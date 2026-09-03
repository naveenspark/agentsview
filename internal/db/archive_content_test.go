package db

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestOpenUsageOnlyPreservesStoredAutomationClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	database, err := OpenWithArchiveContent(path, config.ArchiveContentUsage)
	require.NoError(t, err)

	prompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "automated", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &prompt, StartedAt: &startedAt, UserMessageCount: 1,
	}))
	require.NoError(t, database.Close())

	reopened, err := OpenWithArchiveContent(path, config.ArchiveContentUsage)
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
	database.SetArchiveContent(config.ArchiveContentUsage)
	require.Equal(t, config.ArchiveContentUsage, database.ArchiveContent())

	privateTitle := "private conversation title"
	privatePrompt := "You are a code reviewer. Review the code changes shown below."
	startedAt := "2026-08-31T10:00:00Z"
	session := Session{
		ID: "direct", Project: "project", Agent: "claude", Machine: "local",
		FirstMessage: &privatePrompt, DisplayName: &privateTitle,
		SessionName: &privateTitle, StartedAt: &startedAt,
		MessageCount: 4, UserMessageCount: 1,
		SecretLeakCount: 2, SecretsRulesVersion: "private-rules",
		ToolFailureSignalCount: 3, Outcome: "failure",
		QualitySignalVersion: CurrentQualitySignalVersion,
	}
	require.NoError(t, database.UpsertSession(session))
	require.NoError(t, database.ReplaceSessionMessages(session.ID, []Message{
		{SessionID: session.ID, Ordinal: 0, Role: "user", Content: privatePrompt},
		{SessionID: session.ID, Ordinal: 1, Role: "tool", Content: "private tool output"},
		{SessionID: session.ID, Ordinal: 2, Role: "assistant", Model: "model-a", Content: "private response"},
		{SessionID: session.ID, Ordinal: 3, Role: "assistant", Model: "model-a", Content: "private billed response", TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`)},
	}))
	require.NoError(t, database.UpdateSessionSignals(
		session.ID, SessionSignalUpdate{
			ToolFailureSignalCount: 4,
			Outcome:                "failure",
			QualitySignals: QualitySignals{
				ShortPromptCount: 3,
			},
		},
	))
	require.NoError(t, database.ReplaceSessionSecretFindings(
		session.ID,
		[]SecretFinding{{
			SessionID: session.ID,
			RuleName:  "private-secret-rule",
		}},
		1,
		"private-rules",
	))

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
	database.SetArchiveContent(config.ArchiveContentUsage)

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
	assert.Zero(t, session.ToolFailureSignalCount)
	assert.Empty(t, session.Outcome)
	assert.Equal(t, CurrentQualitySignalVersion, session.QualitySignalVersion)
	findings, err := database.SessionSecretFindings(
		context.Background(), sessionID,
	)
	require.NoError(t, err)
	assert.Empty(t, findings)

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

func TestTranscriptsArchiveContentKeepsTextAndDropsToolPayloads(t *testing.T) {
	database := testDB(t)
	database.SetArchiveContent(config.ArchiveContentTranscripts)

	title := "conversation title"
	prompt := "please run the build"
	startedAt := "2026-08-31T10:00:00Z"
	require.NoError(t, database.UpsertSession(Session{
		ID: "transcripts", Project: "project", Agent: "claude",
		Machine: "local", FirstMessage: &prompt, SessionName: &title,
		StartedAt: &startedAt, MessageCount: 2, UserMessageCount: 1,
	}))
	require.NoError(t, database.ReplaceSessionMessages("transcripts", []Message{
		{SessionID: "transcripts", Ordinal: 0, Role: "user", Content: prompt},
		{
			SessionID: "transcripts", Ordinal: 1, Role: "assistant",
			Model: "model-a", Content: "running it now", HasToolUse: true,
			ThinkingText: "the build script is make",
			ToolCalls: []ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
				InputJSON:           `{"command":"make build"}`,
				ResultContent:       "build output line",
				ResultContentLength: 17,
				ResultEvents: []ToolResultEvent{{
					Source: "tool_result", Status: "ok",
					Content: "build output line", ContentLength: 17,
				}},
			}},
		},
	}))

	stored, err := database.GetSessionFull(context.Background(), "transcripts")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.FirstMessage)
	assert.Equal(t, prompt, *stored.FirstMessage)
	require.NotNil(t, stored.SessionName)
	assert.Equal(t, title, *stored.SessionName)

	messages, err := database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, prompt, messages[0].Content)
	assert.Equal(t, "running it now", messages[1].Content)
	assert.Equal(t, "the build script is make", messages[1].ThinkingText)
	require.Len(t, messages[1].ToolCalls, 1)
	call := messages[1].ToolCalls[0]
	assert.Equal(t, "Bash", call.ToolName)
	assert.Equal(t, "tool-use-1", call.ToolUseID)
	assert.Empty(t, call.InputJSON)
	assert.Empty(t, call.ResultContent)
	assert.Equal(t, 17, call.ResultContentLength)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "ok", call.ResultEvents[0].Status)
	assert.Empty(t, call.ResultEvents[0].Content)
	assert.Equal(t, 17, call.ResultEvents[0].ContentLength)

	require.NoError(t, database.WriteSessionIncremental(
		"transcripts",
		[]Message{{
			SessionID: "transcripts", Ordinal: 2, Role: "assistant",
			Model: "model-a", Content: "delegating", HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-2",
				InputJSON: `{"prompt":"private subagent prompt"}`,
			}},
		}},
		IncrementalSessionUpdate{
			MsgCount: 3,
			SubagentLinks: []ToolCallSubagentLink{{
				ToolUseID: "tool-use-2", SubagentSessionID: "child",
				ResultContent: "subagent result", ResultContentLen: 15,
				HasResult: true,
			}},
		},
	))
	messages, err = database.GetAllMessages(context.Background(), "transcripts")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	assert.Equal(t, "delegating", messages[2].Content)
	require.Len(t, messages[2].ToolCalls, 1)
	link := messages[2].ToolCalls[0]
	assert.Equal(t, "child", link.SubagentSessionID)
	assert.Empty(t, link.InputJSON)
	assert.Empty(t, link.ResultContent)
	assert.Equal(t, 15, link.ResultContentLength)
}

// TestCopyOrphanedDataProjectsArchiveContent covers the resync copy path,
// which reads a full archive with ATTACH and therefore bypasses the
// write-time projection.
func TestCopyOrphanedDataProjectsArchiveContent(t *testing.T) {
	prompt := "please inspect the repository"
	startedAt := "2026-08-31T10:00:00Z"
	seedSource := func(t *testing.T, sourcePath string) {
		t.Helper()
		source := testDBAtPath(t, sourcePath, "source")
		require.NoError(t, source.UpsertSession(Session{
			ID: "archived", Project: "project", Agent: "claude",
			Machine: "local", FirstMessage: &prompt, StartedAt: &startedAt,
			MessageCount: 3, UserMessageCount: 1,
		}))
		require.NoError(t, source.InsertMessages([]Message{
			{SessionID: "archived", Ordinal: 0, Role: "user", Content: prompt},
			{
				SessionID: "archived", Ordinal: 1, Role: "assistant",
				Model: "model-a", Content: "listing files", HasToolUse: true,
				TokenUsage: []byte(`{"input_tokens":10,"output_tokens":2}`),
				ToolCalls: []ToolCall{{
					ToolName: "Bash", Category: "Bash", ToolUseID: "tool-use-1",
					InputJSON: `{"command":"ls"}`, ResultContent: "README.md",
					ResultContentLength: 9,
					ResultEvents: []ToolResultEvent{{
						Source: "tool_result", Status: "ok",
						Content: "README.md", ContentLength: 9,
					}},
				}},
			},
			{
				SessionID: "archived", Ordinal: 2, Role: "assistant",
				Model: "model-a", Content: "delegating", HasToolUse: true,
				ToolCalls: []ToolCall{{
					ToolName: "Agent", Category: "Task", ToolUseID: "tool-use-2",
					InputJSON:         `{"prompt":"private delegated prompt"}`,
					SubagentSessionID: "child",
				}},
			},
		}))
		require.NoError(t, source.ReplaceSessionSecretFindings(
			"archived",
			[]SecretFinding{{SessionID: "archived", RuleName: "aws-access-key"}},
			1, "rules-v1",
		))
		require.NoError(t, source.Close())
	}

	t.Run("transcripts", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		seedSource(t, sourcePath)
		destination := testDBAtPath(t, filepath.Join(dir, "dest.db"), "dest")
		t.Cleanup(func() { require.NoError(t, destination.Close()) })
		destination.SetArchiveContent(config.ArchiveContentTranscripts)

		copied, err := destination.CopyOrphanedDataFrom(sourcePath)
		require.NoError(t, err)
		require.Equal(t, 1, copied)

		stored, err := destination.GetSessionFull(context.Background(), "archived")
		require.NoError(t, err)
		require.NotNil(t, stored)
		require.NotNil(t, stored.FirstMessage)
		assert.Equal(t, prompt, *stored.FirstMessage)
		assert.Equal(t, 1, stored.SecretLeakCount)

		messages, err := destination.GetAllMessages(context.Background(), "archived")
		require.NoError(t, err)
		require.Len(t, messages, 3)
		assert.Equal(t, prompt, messages[0].Content)
		assert.Equal(t, "listing files", messages[1].Content)
		require.Len(t, messages[1].ToolCalls, 1)
		call := messages[1].ToolCalls[0]
		assert.Equal(t, "Bash", call.ToolName)
		assert.Empty(t, call.InputJSON)
		assert.Empty(t, call.ResultContent)
		assert.Equal(t, 9, call.ResultContentLength)
		require.Len(t, call.ResultEvents, 1)
		assert.Empty(t, call.ResultEvents[0].Content)
		assert.Equal(t, "ok", call.ResultEvents[0].Status)
		require.Len(t, messages[2].ToolCalls, 1)
		assert.Equal(t, "child", messages[2].ToolCalls[0].SubagentSessionID)
		assert.Empty(t, messages[2].ToolCalls[0].InputJSON)
	})

	t.Run("usage", func(t *testing.T) {
		dir := t.TempDir()
		sourcePath := filepath.Join(dir, "source.db")
		seedSource(t, sourcePath)
		destination := testDBAtPath(t, filepath.Join(dir, "dest.db"), "dest")
		t.Cleanup(func() { require.NoError(t, destination.Close()) })
		destination.SetArchiveContent(config.ArchiveContentUsage)

		copied, err := destination.CopyOrphanedDataFrom(sourcePath)
		require.NoError(t, err)
		require.Equal(t, 1, copied)

		stored, err := destination.GetSessionFull(context.Background(), "archived")
		require.NoError(t, err)
		require.NotNil(t, stored)
		assert.Nil(t, stored.FirstMessage)
		assert.Zero(t, stored.SecretLeakCount)
		assert.Empty(t, stored.SecretsRulesVersion)
		assert.Equal(t, CurrentQualitySignalVersion, stored.QualitySignalVersion)
		findings, err := destination.SessionSecretFindings(
			context.Background(), "archived",
		)
		require.NoError(t, err)
		assert.Empty(t, findings)

		messages, err := destination.GetAllMessages(context.Background(), "archived")
		require.NoError(t, err)
		require.Len(t, messages, 2)
		assert.Equal(t, []int{1, 2}, []int{messages[0].Ordinal, messages[1].Ordinal})
		assert.Empty(t, messages[0].Content)
		assert.Empty(t, messages[1].Content)
		assert.JSONEq(t, `{"input_tokens":10,"output_tokens":2}`,
			string(messages[0].TokenUsage))
		assert.False(t, messages[0].HasToolUse)
		assert.True(t, messages[1].HasToolUse)
		require.Len(t, messages[1].ToolCalls, 1)
		call := messages[1].ToolCalls[0]
		assert.Equal(t, "subagent", call.ToolName)
		assert.Equal(t, "Task", call.Category)
		assert.Equal(t, "tool-use-2", call.ToolUseID)
		assert.Equal(t, "child", call.SubagentSessionID)
		assert.Empty(t, call.InputJSON)
		assert.Empty(t, call.ResultEvents)
	})
}
