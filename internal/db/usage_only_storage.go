package db

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
		message.ToolCalls = nil
		message.ToolResults = nil
		message.HasThinking = false
		message.HasToolUse = false
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
	return tokenEligible || activityEligible
}

func promoteUsageOnlyAutomationTx(
	tx transactionQueries, sessionID string, messages []Message,
) error {
	var userMessageCount int
	var automated bool
	err := tx.QueryRow(
		`SELECT user_message_count, is_automated FROM sessions WHERE id = ?`,
		sessionID,
	).Scan(&userMessageCount, &automated)
	if err != nil {
		return err
	}
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
