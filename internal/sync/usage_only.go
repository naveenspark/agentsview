package sync

import "go.kenn.io/agentsview/internal/db"

func (e *Engine) sessionForStorage(session db.Session) db.Session {
	if !e.usageOnly {
		return session
	}
	session.FirstMessage = nil
	session.DisplayName = nil
	session.SessionName = nil
	session.SecretLeakCount = 0
	session.SecretsRulesVersion = ""
	return session
}

func (e *Engine) messagesForStorage(messages []db.Message) []db.Message {
	if !e.usageOnly || messages == nil {
		return messages
	}
	stored := make([]db.Message, len(messages))
	copy(stored, messages)
	for i := range stored {
		stored[i].Content = ""
		stored[i].ThinkingText = ""
		stored[i].ToolCalls = nil
		stored[i].ToolResults = nil
	}
	return stored
}
