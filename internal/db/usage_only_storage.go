package db

import "strings"

// EnableUsageOnlyStorage makes usage accounting the database's storage
// boundary. The switch is monotonic for the lifetime of a DB handle: once a
// process promises not to persist transcript content, a later caller cannot
// silently weaken that promise.
func (db *DB) EnableUsageOnlyStorage() {
	if db != nil {
		db.usageOnlyStorage.Store(true)
	}
}

// UsageOnlyStorageEnabled reports whether this DB handle enforces the
// usage-only storage boundary.
func (db *DB) UsageOnlyStorageEnabled() bool {
	return db != nil && db.usageOnlyStorage.Load()
}

func (db *DB) sessionForStorage(session Session) Session {
	if !db.UsageOnlyStorageEnabled() {
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
	if !db.UsageOnlyStorageEnabled() {
		return session, messages
	}
	// Some importers do not precompute IsAutomated. Classify from the raw
	// messages before the storage projection drops user text.
	session.IsAutomated = sessionIsAutomated(session) ||
		IsAutomatedTranscript(
			session.UserMessageCount, messages, session.FirstMessage,
		)
	return db.sessionForStorage(session), usageOnlyMessages(messages)
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
	if !db.UsageOnlyStorageEnabled() {
		return messages
	}
	return usageOnlyMessages(messages)
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
	for _, call := range message.ToolCalls {
		if usageOnlySubagentCallRequired(call) {
			return true
		}
	}
	return false
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
