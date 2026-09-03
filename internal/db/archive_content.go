package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// archiveContentRanks orders policies from least to most restrictive. The
// atomic on DB stores an index into this slice.
var archiveContentRanks = []config.ArchiveContent{
	config.ArchiveContentFull,
	config.ArchiveContentTranscripts,
	config.ArchiveContentUsage,
}

func archiveContentRank(policy config.ArchiveContent) int32 {
	index := slices.Index(archiveContentRanks, policy)
	if index < 0 {
		return 0
	}
	return int32(index)
}

// SetArchiveContent tightens this handle's storage boundary. The switch is
// monotonic for the lifetime of a DB handle: once a process promises not to
// persist some content, a later caller cannot silently weaken that promise.
func (db *DB) SetArchiveContent(policy config.ArchiveContent) {
	if db == nil {
		return
	}
	rank := archiveContentRank(policy)
	for {
		current := db.archiveContent.Load()
		if current >= rank || db.archiveContent.CompareAndSwap(current, rank) {
			return
		}
	}
}

// ArchiveContent reports the storage boundary this DB handle enforces.
func (db *DB) ArchiveContent() config.ArchiveContent {
	if db == nil {
		return config.ArchiveContentFull
	}
	return archiveContentRanks[db.archiveContent.Load()]
}

func (db *DB) usageOnlyStorage() bool {
	return db.ArchiveContent().UsageOnly()
}

func (db *DB) sessionForStorage(session Session) Session {
	if !db.usageOnlyStorage() {
		return session
	}
	// Derive automation while the parser/importer preview is still present.
	// The retained session row is authoritative after transcript text is gone.
	session.IsAutomated = sessionIsAutomated(session)
	session.FirstMessage = nil
	session.DisplayName = nil
	session.SessionName = nil
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	return session
}

func (db *DB) sessionAndMessagesForStorage(
	session Session, messages []Message,
) (Session, []Message) {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		// Some importers do not precompute IsAutomated. Classify from the raw
		// messages before the storage projection drops user text.
		session.IsAutomated = sessionIsAutomated(session) ||
			IsAutomatedTranscript(
				session.UserMessageCount, messages, session.FirstMessage,
			)
		return db.sessionForStorage(session), usageOnlyMessages(messages)
	case config.ArchiveContentTranscripts:
		return session, transcriptMessages(messages)
	default:
		return session, messages
	}
}

// ProjectSessionForStorage applies this database handle's storage policy to a
// prepared session without writing it. Report-only callers use the same
// projection as the write boundary before comparing prepared and stored rows.
func (db *DB) ProjectSessionForStorage(
	session Session, messages []Message,
) (Session, []Message) {
	return db.sessionAndMessagesForStorage(session, messages)
}

func (db *DB) messagesForStorage(messages []Message) []Message {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		return usageOnlyMessages(messages)
	case config.ArchiveContentTranscripts:
		return transcriptMessages(messages)
	default:
		return messages
	}
}

func (db *DB) subagentLinksForStorage(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	switch db.ArchiveContent() {
	case config.ArchiveContentUsage:
		return usageOnlySubagentLinks(links)
	case config.ArchiveContentTranscripts:
		return transcriptSubagentLinks(links)
	default:
		return links
	}
}

// transcriptMessages keeps every message row and tool call while dropping
// the payloads that carry file contents and command output. Lengths, tool
// names, categories, statuses, and delegation links stay so tool analytics
// and the session tree keep working.
func transcriptMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	stored := slices.Clone(messages)
	for i := range stored {
		stored[i].ToolResults = nil
		if len(stored[i].ToolCalls) == 0 {
			continue
		}
		calls := slices.Clone(stored[i].ToolCalls)
		for j := range calls {
			calls[j].InputJSON = ""
			calls[j].ResultContent = ""
			if len(calls[j].ResultEvents) == 0 {
				continue
			}
			events := slices.Clone(calls[j].ResultEvents)
			for k := range events {
				events[k].Content = ""
			}
			calls[j].ResultEvents = events
		}
		stored[i].ToolCalls = calls
	}
	return stored
}

func transcriptSubagentLinks(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	if links == nil {
		return nil
	}
	stored := slices.Clone(links)
	for i := range stored {
		stored[i].ResultContent = ""
	}
	return stored
}

func usageOnlyMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	stored := make([]Message, 0, len(messages))
	for _, message := range messages {
		if !usageOnlyMessageRequired(message) {
			continue
		}
		message.Content = ""
		message.ThinkingText = ""
		message.ToolCalls = usageOnlyToolCalls(message.ToolCalls)
		message.ToolResults = nil
		message.HasThinking = false
		message.HasToolUse = len(message.ToolCalls) > 0
		message.ContentLength = 0
		message.IsSystem = false
		message.ContextTokens = 0
		message.OutputTokens = 0
		message.HasContextTokens = false
		message.HasOutputTokens = false
		message.SourceType = ""
		message.SourceSubtype = ""
		message.PromptSource = ""
		message.SourceParentUUID = ""
		message.IsSidechain = false
		message.IsCompactBoundary = false
		stored = append(stored, message)
	}
	return stored
}

func usageOnlyMessageRequired(message Message) bool {
	tokenEligible := len(message.TokenUsage) > 0 && message.Model != "" &&
		message.Model != "<synthetic>"
	activityEligible := message.Role == "assistant" &&
		message.Model != "<synthetic>"
	return tokenEligible || activityEligible ||
		usageOnlyMessageHasSubagentCall(message)
}

func usageOnlyMessageHasSubagentCall(message Message) bool {
	return slices.ContainsFunc(
		message.ToolCalls, usageOnlySubagentCallRequired,
	)
}

func usageOnlySubagentCallRequired(call ToolCall) bool {
	return call.SubagentSessionID != "" || call.Category == "Task" ||
		strings.Contains(call.ToolName, "subagent")
}

// usageOnlyToolCalls retains the opaque identifiers needed to reconstruct
// delegated-session relationships. Everything that can carry transcript or
// tool-result content is replaced or discarded at the storage boundary.
func usageOnlyToolCalls(calls []ToolCall) []ToolCall {
	var stored []ToolCall
	for _, call := range calls {
		if !usageOnlySubagentCallRequired(call) {
			continue
		}
		stored = append(stored, ToolCall{
			ToolName:          "subagent",
			Category:          "Task",
			ToolUseID:         call.ToolUseID,
			SubagentSessionID: call.SubagentSessionID,
		})
	}
	return stored
}

func usageOnlySubagentLinks(
	links []ToolCallSubagentLink,
) []ToolCallSubagentLink {
	var stored []ToolCallSubagentLink
	for _, link := range links {
		if link.ToolUseID == "" || link.SubagentSessionID == "" {
			continue
		}
		stored = append(stored, ToolCallSubagentLink{
			ToolUseID:         link.ToolUseID,
			SubagentSessionID: link.SubagentSessionID,
		})
	}
	return stored
}

func updateUsageOnlyAutomationTx(
	tx transactionQueries, sessionID string, messages []Message,
) error {
	var userMessageCount int
	var automated bool
	var agent, sessionKind string
	err := tx.QueryRow(
		`SELECT user_message_count, is_automated, agent, session_kind
		   FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&userMessageCount, &automated, &agent, &sessionKind)
	if err != nil {
		return err
	}
	if IsAutomatedSessionMetadata(agent, sessionKind) {
		if automated {
			return nil
		}
		return setSessionAutomationTx(tx, sessionID, true)
	}
	if userMessageCount > 1 {
		if !automated {
			return nil
		}
		return setSessionAutomationTx(tx, sessionID, false)
	}
	// An incremental tail may omit the first user message whose text was
	// discarded after the original classification. Preserve an existing
	// one-turn verdict; new raw text can still promote an unclassified row.
	if automated || !IsAutomatedTranscript(userMessageCount, messages, nil) {
		return nil
	}
	return setSessionAutomationTx(tx, sessionID, true)
}

func messagesForSession(messages []Message, sessionID string) []Message {
	selected := make([]Message, 0, len(messages))
	for _, message := range messages {
		if message.SessionID == sessionID {
			selected = append(selected, message)
		}
	}
	return selected
}

// applyArchiveContentToCopiedSessionsTx projects sessions copied verbatim
// from another archive onto this database's storage policy. Resync copies
// archived rows with ATTACH, so the write-time projection never sees them.
// The SQL mirrors usageOnlyMessages and transcriptMessages column for column.
func applyArchiveContentToCopiedSessionsTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
	policy config.ArchiveContent,
) error {
	switch policy {
	case config.ArchiveContentTranscripts:
		return dropCopiedToolContentTx(ctx, tx, tempIDsTable)
	case config.ArchiveContentUsage:
		return compactCopiedSessionsForUsageTx(ctx, tx, tempIDsTable)
	default:
		return nil
	}
}

func dropCopiedToolContentTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE tool_calls SET input_json = NULL, result_content = NULL
		WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)`,
	); err != nil {
		return fmt.Errorf("dropping copied tool payloads: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE tool_result_events SET content = ''
		WHERE session_id IN (SELECT id FROM `+tempIDsTable+`)`,
	); err != nil {
		return fmt.Errorf("dropping copied tool result events: %w", err)
	}
	return nil
}

func compactCopiedSessionsForUsageTx(
	ctx context.Context, tx *sql.Tx, tempIDsTable string,
) error {
	inCopied := ` IN (SELECT id FROM ` + tempIDsTable + `)`
	statements := []struct {
		label string
		sql   string
	}{
		{"tool result events", `
			DELETE FROM tool_result_events WHERE session_id` + inCopied},
		{"non-delegation tool calls", `
			DELETE FROM tool_calls
			WHERE session_id` + inCopied + `
			  AND NOT (COALESCE(subagent_session_id, '') != ''
			       OR category = 'Task'
			       OR tool_name LIKE '%subagent%')`},
		{"delegation tool calls", `
			UPDATE tool_calls
			SET tool_name = 'subagent', category = 'Task',
			    input_json = NULL, skill_name = NULL,
			    result_content_length = NULL, result_content = NULL,
			    file_path = NULL
			WHERE session_id` + inCopied},
		{"messages outside usage accounting", `
			DELETE FROM messages
			WHERE session_id` + inCopied + `
			  AND NOT (
			    (length(token_usage) > 0 AND model != ''
			       AND model != '<synthetic>')
			    OR (role = 'assistant' AND model != '<synthetic>')
			    OR EXISTS (SELECT 1 FROM tool_calls tc
			               WHERE tc.message_id = messages.id))`},
		{"message payloads", `
			UPDATE messages
			SET content = '', thinking_text = '', has_thinking = 0,
			    has_tool_use = EXISTS (SELECT 1 FROM tool_calls tc
			                           WHERE tc.message_id = messages.id),
			    content_length = 0, is_system = 0,
			    context_tokens = 0, output_tokens = 0,
			    has_context_tokens = 0, has_output_tokens = 0,
			    source_type = '', source_subtype = '', prompt_source = '',
			    source_parent_uuid = '', is_sidechain = 0,
			    is_compact_boundary = 0
			WHERE session_id` + inCopied},
		{"session titles", `
			UPDATE sessions
			SET first_message = NULL, display_name = NULL,
			    session_name = NULL, secret_leak_count = 0,
			    secrets_rules_version = ''
			WHERE id` + inCopied},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.sql); err != nil {
			return fmt.Errorf(
				"compacting copied %s: %w", statement.label, err,
			)
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM "+tempIDsTable)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	ids, err := scanStrings(rows)
	if err != nil {
		return fmt.Errorf("listing copied sessions: %w", err)
	}
	for _, id := range ids {
		if err := settleUsageOnlySignalsTx(tx, id); err != nil {
			return fmt.Errorf("settling copied signals for %s: %w", id, err)
		}
	}
	return nil
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}
