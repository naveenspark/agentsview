package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenUsageOnlyPreservesStoredAutomationClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := OpenUsageOnly(path)
	require.NoError(t, err)

	prompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "automated", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &prompt, StartedAt: &startedAt, UserMessageCount: 1,
	}))
	require.NoError(t, database.Close())

	reopened, err := OpenUsageOnly(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	stored, err := reopened.GetSessionFull(context.Background(), "automated")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated,
		"startup migrations cannot reclassify discarded transcript text")
	assert.Nil(t, stored.FirstMessage)
}

func TestUsageOnlyStoragePolicyOwnsDirectAndBatchWrites(t *testing.T) {
	database := testDB(t)
	database.EnableUsageOnlyStorage()
	require.True(t, database.UsageOnlyStorageEnabled())

	privateTitle := "private conversation title"
	privatePrompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	session := Session{
		ID: "direct", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &privatePrompt, DisplayName: &privateTitle,
		SessionName: &privateTitle, StartedAt: &startedAt,
		MessageCount: 4, UserMessageCount: 1,
		SecretLeakCount: 2, SecretsRulesVersion: "private-rules",
	}
	require.NoError(t, database.UpsertSession(session))
	require.NoError(t, database.ReplaceSessionMessages(session.ID, []Message{
		{SessionID: session.ID, Ordinal: 0, Role: "user", Content: privatePrompt},
		{SessionID: session.ID, Ordinal: 1, Role: "tool", Content: "private tool output"},
		{SessionID: session.ID, Ordinal: 2, Role: "assistant", Model: "model-a", Content: "private response"},
		{SessionID: session.ID, Ordinal: 3, Role: "assistant", Model: "model-a", Content: "private billed response", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`)},
	}))

	assertUsageOnlyStoredSession(t, database, session.ID, []int{2, 3})
	replacementTitle := "title added after the initial import"
	require.NoError(t, database.RefreshSessionName(session.ID, &replacementTitle))
	require.NoError(t, database.RenameSession(session.ID, &replacementTitle))
	stored, err := database.GetSessionFull(context.Background(), session.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.IsAutomated,
		"classification must be derived before private prompt text is discarded")
	assert.Nil(t, stored.SessionName)
	assert.Nil(t, stored.DisplayName)

	batchSession := session
	batchSession.ID = "batch"
	batchSession.IsAutomated = true
	result, err := database.WriteSessionBatch([]SessionBatchWrite{{
		Session: batchSession,
		Messages: []Message{
			{SessionID: batchSession.ID, Ordinal: 0, Role: "user", Content: "private batch prompt"},
			{SessionID: batchSession.ID, Ordinal: 1, Role: "assistant", Model: "model-b", Content: "private batch response", TokenUsage: []byte(`{"input_tokens":20,"output_tokens":4}`)},
		},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.Equal(t, 1, result.WrittenSessions)
	assertUsageOnlyStoredSession(t, database, batchSession.ID, []int{1})

	incrementalSession := session
	incrementalSession.ID = "incremental"
	incrementalSession.FirstMessage = nil
	incrementalSession.IsAutomated = false
	incrementalSession.MessageCount = 0
	incrementalSession.UserMessageCount = 0
	require.NoError(t, database.UpsertSession(incrementalSession))
	require.NoError(t, database.WriteSessionIncremental(
		incrementalSession.ID,
		[]Message{{
			SessionID: incrementalSession.ID, Ordinal: 0,
			Role: "user", Content: privatePrompt,
		}},
		IncrementalSessionUpdate{MsgCount: 1, UserMsgCount: 1},
	))
	assertUsageOnlyStoredSession(t, database, incrementalSession.ID, []int{})
	incrementalStored, err := database.GetSessionFull(
		context.Background(), incrementalSession.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, incrementalStored)
	assert.True(t, incrementalStored.IsAutomated,
		"incremental classification must use text before it is discarded")

	require.NoError(t, database.WriteSessionIncremental(
		incrementalSession.ID,
		[]Message{{
			SessionID: incrementalSession.ID, Ordinal: 1,
			Role: "user", Content: "a second interactive turn",
		}},
		IncrementalSessionUpdate{MsgCount: 2, UserMsgCount: 2},
	))
	incrementalStored, err = database.GetSessionFull(
		context.Background(), incrementalSession.ID,
	)
	require.NoError(t, err)
	require.NotNil(t, incrementalStored)
	assert.False(t, incrementalStored.IsAutomated,
		"a second user turn conclusively demotes text-derived automation")
}

func TestUsageOnlyStoragePreservesContentFreeIncrementalSubagentEdge(
	t *testing.T,
) {
	database := testDB(t)
	database.EnableUsageOnlyStorage()

	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "parent", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt,
	}))
	require.NoError(t, database.UpsertSession(Session{
		ID: "child", Project: "project", Agent: "claude", Machine: "local",
		StartedAt: &startedAt,
	}))

	require.NoError(t, database.WriteSessionIncremental(
		"parent",
		[]Message{{
			SessionID: "parent", Ordinal: 0, Role: "assistant",
			Model: "model-a", Content: "private delegated work",
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-1",
				InputJSON: `{"prompt":"private subagent prompt"}`,
			}},
		}},
		IncrementalSessionUpdate{
			MsgCount: 1,
			SubagentLinks: []ToolCallSubagentLink{{
				ToolUseID: "tool-use-1", SubagentSessionID: "child",
				ResultContent: "private subagent result", ResultContentLen: 23,
				HasResult: true,
			}},
		},
	))
	require.NoError(t, database.LinkSubagentSessions())

	child, err := database.GetSession(context.Background(), "child")
	require.NoError(t, err)
	require.NotNil(t, child)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "parent", *child.ParentSessionID)
	assert.Equal(t, "subagent", child.RelationshipType)

	messages, err := database.GetAllMessages(context.Background(), "parent")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].ToolCalls, 1)
	call := messages[0].ToolCalls[0]
	assert.Equal(t, "tool-use-1", call.ToolUseID)
	assert.Equal(t, "child", call.SubagentSessionID)
	assert.Equal(t, "subagent", call.ToolName)
	assert.Equal(t, "Task", call.Category)
	assert.Empty(t, call.InputJSON)
	assert.Empty(t, call.SkillName)
	assert.Empty(t, call.ResultContent)
	assert.Zero(t, call.ResultContentLength)
	assert.Empty(t, call.ResultEvents)
}

func assertUsageOnlyStoredSession(
	t *testing.T, database *DB, sessionID string, wantOrdinals []int,
) {
	t.Helper()
	session, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.FirstMessage)
	assert.Nil(t, session.DisplayName)
	assert.Nil(t, session.SessionName)
	assert.Zero(t, session.SecretLeakCount)
	assert.Empty(t, session.SecretsRulesVersion)

	messages, err := database.GetAllMessages(context.Background(), sessionID)
	require.NoError(t, err)
	ordinals := make([]int, len(messages))
	for index, message := range messages {
		ordinals[index] = message.Ordinal
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}
	assert.Equal(t, wantOrdinals, ordinals)
}
