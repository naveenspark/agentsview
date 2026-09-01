package sync_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestUsageOnlyStoragePreservesUsageWithoutTranscriptContent(t *testing.T) {
	codexRoot := t.TempDir()
	sessionID := "019eb791-cf7d-75c1-8439-9ed74c1229e1"
	path := filepath.Join(
		codexRoot,
		"2026", "08", "31",
		"rollout-2026-08-31T10-00-00-"+sessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			sessionID, "/workspace/private-project", "codex_cli_rs",
			"2026-08-31T10:00:00Z",
		),
		testjsonl.CodexTurnContextJSON(
			"gpt-5.5", "2026-08-31T10:00:01Z",
		),
		testjsonl.CodexMsgJSON(
			"user", "private prompt that must stay in the source transcript",
			"2026-08-31T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call-private",
			map[string]any{"cmd": "read /workspace/private-project/secret.txt"},
			"2026-08-31T10:00:03Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call-private", "private tool output",
			"2026-08-31T10:00:04Z",
		),
		testjsonl.CodexTokenCountJSON(
			"2026-08-31T10:00:05Z", 100_000, 250, 64_000,
		),
	)), 0o600))

	claudeRoot := t.TempDir()
	claudeSessionID := "claude-private-session"
	claudePath := filepath.Join(
		claudeRoot, "private-project", claudeSessionID+".jsonl",
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(claudePath), 0o755))
	claudeBuilder := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			"2026-08-31T11:00:00Z",
			"You are a code reviewer. Review the code changes shown below. private Claude prompt",
			claudeSessionID,
			"/workspace/private-project",
		).
		AddClaudeAssistant(
			"2026-08-31T11:00:00.500Z",
			"private unbilled Claude response",
		).
		AddClaudeAssistantUsage(
			"2026-08-31T11:00:01Z",
			"private Claude response",
			testjsonl.ClaudeAssistantUsage{
				MessageID:    "msg-private-1",
				RequestID:    "req-private-1",
				Model:        "claude-sonnet-4-6",
				InputTokens:  1_000,
				OutputTokens: 200,
			},
		)
	require.NoError(t, os.WriteFile(
		claudePath, []byte(claudeBuilder.String()), 0o600,
	))

	agentDirs := map[parser.AgentType][]string{
		parser.AgentClaude: {claudeRoot},
		parser.AgentCodex:  {codexRoot},
	}
	fullDB := dbtest.OpenTestDB(t)
	fullEngine := sync.NewEngine(fullDB, sync.EngineConfig{
		AgentDirs: agentDirs,
		Machine:   "local",
	})
	t.Cleanup(fullEngine.Close)
	usageDB := dbtest.OpenTestDB(t)
	usageEngine := sync.NewEngine(usageDB, sync.EngineConfig{
		AgentDirs: agentDirs,
		Machine:   "local",
		UsageOnly: true,
	})
	t.Cleanup(usageEngine.Close)

	require.Equal(t, 2, fullEngine.SyncAll(t.Context(), nil).Synced)
	require.Equal(t, 2, usageEngine.SyncAll(t.Context(), nil).Synced)

	appendBuilder := testjsonl.NewSessionBuilder().AddClaudeAssistantUsage(
		"2026-08-31T11:00:02Z",
		"private incrementally appended Claude response",
		testjsonl.ClaudeAssistantUsage{
			MessageID:    "msg-private-2",
			RequestID:    "req-private-2",
			Model:        "claude-sonnet-4-6",
			InputTokens:  1_250,
			OutputTokens: 250,
		},
	)
	appendFile, err := os.OpenFile(claudePath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = appendFile.WriteString(appendBuilder.String())
	require.NoError(t, err)
	require.NoError(t, appendFile.Close())
	fullEngine.SyncPathsContext(t.Context(), []string{claudePath})
	usageEngine.SyncPathsContext(t.Context(), []string{claudePath})
	resyncStats := usageEngine.ResyncAll(t.Context(), nil)
	require.False(t, resyncStats.Aborted)
	require.Zero(t, resyncStats.Failed)

	for _, agent := range []string{"claude", "codex"} {
		filter := db.UsageFilter{
			From: "2026-08-31", To: "2026-08-31",
			Agent: agent, Timezone: "UTC", Breakdowns: true,
		}
		fullUsage, err := fullDB.GetDailyUsage(context.Background(), filter)
		require.NoError(t, err)
		usageOnlyUsage, err := usageDB.GetDailyUsage(
			context.Background(), filter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullUsage.Totals, usageOnlyUsage.Totals)
		assert.Equal(t, fullUsage.Daily, usageOnlyUsage.Daily)
		assert.Equal(t, fullUsage.SessionCounts, usageOnlyUsage.SessionCounts)

		fullMatching, err := fullDB.GetUsageMatchingSessionCount(
			context.Background(), filter,
		)
		require.NoError(t, err)
		usageOnlyMatching, err := usageDB.GetUsageMatchingSessionCount(
			context.Background(), filter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullMatching, usageOnlyMatching)

		automatedFilter := filter
		automatedFilter.AutomatedScope = "automated"
		fullAutomated, err := fullDB.GetDailyUsage(
			context.Background(), automatedFilter,
		)
		require.NoError(t, err)
		usageOnlyAutomated, err := usageDB.GetDailyUsage(
			context.Background(), automatedFilter,
		)
		require.NoError(t, err)
		assert.Equal(t, fullAutomated, usageOnlyAutomated)
	}

	messages, err := usageDB.GetAllMessages(context.Background(), "codex:"+sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	for _, message := range messages {
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}

	session, err := usageDB.GetSessionFull(
		context.Background(), "codex:"+sessionID,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.FirstMessage)
	assert.Nil(t, session.DisplayName)
	assert.Nil(t, session.SessionName)
	assert.Equal(t, 0, session.SecretLeakCount)

	fullClaudeMessages, err := fullDB.GetAllMessages(
		context.Background(), claudeSessionID,
	)
	require.NoError(t, err)
	require.NotEmpty(t, fullClaudeMessages)
	var fullClaudeText strings.Builder
	for _, message := range fullClaudeMessages {
		fullClaudeText.WriteString(message.Content)
	}
	assert.Contains(t, fullClaudeText.String(), "private Claude prompt")
	assert.Contains(t, fullClaudeText.String(), "incrementally appended")

	usageClaudeMessages, err := usageDB.GetAllMessages(
		context.Background(), claudeSessionID,
	)
	require.NoError(t, err)
	require.Less(t, len(usageClaudeMessages), len(fullClaudeMessages),
		"usage-only storage must omit rows unrelated to accounting")
	for _, message := range usageClaudeMessages {
		assert.True(t,
			(message.TokenUsage != nil && message.Model != "" &&
				message.Model != "<synthetic>") ||
				(message.Role == "assistant" && message.Model != "<synthetic>"),
			"stored message %d is unrelated to usage accounting", message.Ordinal,
		)
		assert.Empty(t, message.Content)
		assert.Empty(t, message.ThinkingText)
		assert.Empty(t, message.ToolCalls)
		assert.Empty(t, message.ToolResults)
	}
}
