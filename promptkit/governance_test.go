// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/promptkit"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// callScript calls one tool on a turn's first model call, then answers.
type callScript struct {
	tool string
	args map[string]any
}

func (s callScript) GetResponse(ctx context.Context, p mock.ResponseParams) (string, error) {
	t, err := s.GetTurn(ctx, p)
	return t.Content, err
}

func (s callScript) GetTurn(_ context.Context, p mock.ResponseParams) (*mock.Turn, error) {
	if p.TurnNumber == 1 {
		return &mock.Turn{Type: "tool_calls", ToolCalls: []mock.ToolCall{{Name: s.tool, Arguments: s.args}}}, nil
	}
	return &mock.Turn{Type: "text", Content: "done"}, nil
}

func calling(tool string, args map[string]any) promptkit.ProviderFunc {
	return func(context.Context) (providers.Provider, error) {
		return mock.NewToolProviderWithRepository("mock", "mock-model", false, callScript{tool: tool, args: args}), nil
	}
}

// governor is a fake relay: it decides, records and remembers in memory.
type governor struct {
	decision dialhome.PolicyDecision
	err      error
	scope    dialhome.MemoryScope

	mu       sync.Mutex
	asked    []dialhome.PolicyRequest
	recorded []dialhome.ToolCallRecord
	saved    []dialhome.MemoryRecord
}

func (g *governor) Decide(_ context.Context, r dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, r)
	return g.decision, g.err
}

func (g *governor) RecordToolCall(_ context.Context, r dialhome.ToolCallRecord) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recorded = append(g.recorded, r)
	return nil
}

func (g *governor) records() []dialhome.ToolCallRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]dialhome.ToolCallRecord(nil), g.recorded...)
}

func (g *governor) MemoryScope(context.Context, string) (dialhome.MemoryScope, error) {
	return g.scope, nil
}

func (g *governor) Remember(_ context.Context, _ string, m dialhome.MemoryRecord) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.saved = append(g.saved, m)
	return "m1", nil
}

func (g *governor) Recall(context.Context, dialhome.MemoryRecallRequest) ([]dialhome.MemoryRecord, error) {
	return nil, nil
}

func (g *governor) ListMemories(context.Context, dialhome.MemoryListRequest) ([]dialhome.MemoryRecord, error) {
	return nil, nil
}

func (g *governor) Forget(context.Context, string, string) error { return nil }

// siteHandler serves the test pack with get_location as a tool that runs in
// this process, counted in ran.
func siteHandler(t *testing.T, g *governor, ran *atomic.Int32, provider promptkit.ProviderFunc) *promptkit.Handler {
	t.Helper()
	return newHandler(t, promptkit.Config{
		Provider: provider, Policy: g, ToolCalls: g, Memory: g, PolicyRegistry: "site-tools",
		Configure: func(conv *sdk.Conversation) error {
			conv.OnTool("get_location", func(map[string]any) (any, error) {
				ran.Add(1)
				return map[string]any{"room": "control"}, nil
			})
			return nil
		},
	})
}

func TestGovernance_ARefusedToolDoesNotRun(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: false, Reason: "not here"}}
	var ran atomic.Int32
	h := siteHandler(t, g, &ran, calling("get_location", map[string]any{"accuracy": "high"}))

	emit := &collect{}
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, emit))
	assert.Equal(t, "done", emit.done.Final)
	assert.Zero(t, ran.Load(), "the tool did not run")
	require.Len(t, g.asked, 1)
	assert.Equal(t, "s1", g.asked[0].SessionID)
	assert.Equal(t, "get_location", g.asked[0].Tool)
	assert.Equal(t, "site-tools", g.asked[0].Registry)
	assert.JSONEq(t, `{"accuracy":"high"}`, string(g.asked[0].Arguments))
	time.Sleep(50 * time.Millisecond) // any record would be on the bus by now
	assert.Empty(t, g.records(), "a refused call is the relay's to record")
}

func TestGovernance_NoDecisionMeansTheToolDoesNotRun(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}, err: dialhome.ErrNoDecision}
	var ran atomic.Int32
	h := siteHandler(t, g, &ran, calling("get_location", nil))
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.Zero(t, ran.Load())
}

func TestGovernance_AnAllowedToolRunsAndIsRecorded(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}}
	var ran atomic.Int32
	h := siteHandler(t, g, &ran, calling("get_location", map[string]any{"accuracy": "low"}))
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.Equal(t, int32(1), ran.Load())

	require.Eventually(t, func() bool { return len(g.records()) == 2 }, 5*time.Second, 10*time.Millisecond)
	recs := g.records()
	assert.Equal(t, dialhome.ToolCallStarted, recs[0].Phase)
	assert.JSONEq(t, `{"accuracy":"low"}`, string(recs[0].Arguments))
	assert.Equal(t, dialhome.ToolCallCompleted, recs[1].Phase)
	assert.JSONEq(t, `{"room":"control"}`, recs[1].Result)
	for _, r := range recs {
		assert.Equal(t, "s1", r.SessionID)
		assert.Equal(t, "get_location", r.Tool)
		assert.Equal(t, recs[0].CallID, r.CallID)
	}
}

func TestGovernance_MemoryToolsReachTheSessionsMemory(t *testing.T) {
	remember := calling("memory__remember", map[string]any{"content": "the engineer prefers metric units"})

	t.Run("a user the relay will keep memory for", func(t *testing.T) {
		g := &governor{scope: dialhome.MemoryScope{Enabled: true, User: true, Tools: true}}
		var ran atomic.Int32
		h := siteHandler(t, g, &ran, remember)
		require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "remember"}, &collect{}))
		require.Len(t, g.saved, 1)
		assert.Equal(t, "the engineer prefers metric units", g.saved[0].Content)
		assert.Empty(t, g.asked, "memory tools are not put to the tool policy")
	})
	t.Run("no user: no memory tools", func(t *testing.T) {
		g := &governor{scope: dialhome.MemoryScope{Enabled: true, Tools: true}}
		var ran atomic.Int32
		h := siteHandler(t, g, &ran, remember)
		_ = h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "remember"}, &collect{})
		assert.Empty(t, g.saved)
	})
}
