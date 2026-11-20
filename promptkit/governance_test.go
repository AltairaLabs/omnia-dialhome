// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
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
	assert.Never(t, func() bool { return len(g.records()) > 0 }, 200*time.Millisecond, 10*time.Millisecond,
		"a refused call is the relay's to record")
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
	remember := calling("memory__remember", map[string]any{"content": "the user prefers metric units"})

	t.Run("a user the relay will keep memory for", func(t *testing.T) {
		g := &governor{scope: dialhome.MemoryScope{Enabled: true, User: true, Tools: true}}
		var ran atomic.Int32
		h := siteHandler(t, g, &ran, remember)
		require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "remember"}, &collect{}))
		require.Len(t, g.saved, 1)
		assert.Equal(t, "the user prefers metric units", g.saved[0].Content)
		assert.Empty(t, g.asked, "memory tools are not put to the tool policy")
	})
	t.Run("a recall that matches nothing finds nothing, and does not fail", func(t *testing.T) {
		g := &governor{scope: dialhome.MemoryScope{Enabled: true, User: true, Tools: true}}
		var ran atomic.Int32
		h := siteHandler(t, g, &ran, calling("memory__recall", map[string]any{"query": "favourite colour"}))
		require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "recall"}, &collect{}))
		require.Eventually(t, func() bool { return len(g.records()) == 2 }, 5*time.Second, 10*time.Millisecond)
		done := g.records()[1]
		assert.Equal(t, dialhome.ToolCallCompleted, done.Phase, "error: %s", done.Error)
		assert.JSONEq(t, `{"count":0,"memories":[]}`, done.Result)
	})
	t.Run("no user: no memory tools", func(t *testing.T) {
		g := &governor{scope: dialhome.MemoryScope{Enabled: true, Tools: true}}
		var ran atomic.Int32
		h := siteHandler(t, g, &ran, remember)
		_ = h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "remember"}, &collect{})
		assert.Empty(t, g.saved)
	})
}

// capturing wraps the mock tool provider and keeps the messages of every
// model call, so a test can read what the model was told.
type capturing struct {
	providers.ToolSupport
	mu   sync.Mutex
	seen [][]types.Message
}

func (c *capturing) keep(req providers.PredictionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, append([]types.Message(nil), req.Messages...))
}

func (c *capturing) PredictWithTools(ctx context.Context, req providers.PredictionRequest, tools providers.ProviderTools,
	choice string) (providers.PredictionResponse, []types.MessageToolCall, error) {
	c.keep(req)
	return c.ToolSupport.PredictWithTools(ctx, req, tools, choice)
}

func (c *capturing) PredictStreamWithTools(ctx context.Context, req providers.PredictionRequest,
	tools providers.ProviderTools, choice string) (<-chan providers.StreamChunk, error) {
	c.keep(req)
	return c.ToolSupport.PredictStreamWithTools(ctx, req, tools, choice)
}

// toolResults is every tool result the model was shown.
func (c *capturing) toolResults() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, msgs := range c.seen {
		for i := range msgs {
			if msgs[i].Role == "tool" {
				out = append(out, msgs[i].GetContent())
			}
		}
	}
	return out
}

func TestGovernance_TheModelIsToldWhyACallWasRefused(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Reason: "Above the 25,000 limit: ask an approver."}}
	var ran atomic.Int32
	c := &capturing{ToolSupport: mock.NewToolProviderWithRepository("mock", "mock-model", false,
		callScript{tool: "get_location"})}
	h := siteHandler(t, g, &ran, func(context.Context) (providers.Provider, error) { return c, nil })
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.Zero(t, ran.Load())
	results := c.toolResults()
	require.NotEmpty(t, results, "the model saw the call's result")
	assert.Contains(t, results[len(results)-1], "Above the 25,000 limit: ask an approver.")
}

// Over a real channel to a relay that does not decide tool calls, a local
// tool does not run.
func TestGovernance_ARelayThatDoesNotDecideRunsNoTools(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: relay.URL(), Key: testKey, Log: logr.Discard(),
		MinBackoff: 20 * time.Millisecond, KeepAlive: relaytest.KeepAlive,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Eventually(t, c.Connected, 5*time.Second, 10*time.Millisecond)

	var ran atomic.Int32
	h := newHandler(t, promptkit.Config{
		Provider: calling("get_location", nil), Policy: dialhome.NewGovernance(c),
		Configure: func(conv *sdk.Conversation) error {
			conv.OnTool("get_location", func(map[string]any) (any, error) { ran.Add(1); return "x", nil })
			return nil
		},
	})
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.Zero(t, ran.Load())
}

// Only PromptKit's own four memory tools skip the policy, and only when the
// relay keeps the memory: any other tool is asked about, whatever its name.
func TestGovernance_AToolNamedLikeMemoryIsStillPutToThePolicy(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true},
		scope: dialhome.MemoryScope{Enabled: true, User: true, Tools: true}}
	var ran atomic.Int32
	h := siteHandler(t, g, &ran, calling("memory__export", nil))
	_ = h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "export"}, &collect{})
	g.mu.Lock()
	defer g.mu.Unlock()
	require.Len(t, g.asked, 1)
	assert.Equal(t, "memory__export", g.asked[0].Tool)
}

// blockingRecorder holds every record until release closes.
type blockingRecorder struct {
	governor
	release chan struct{}
}

func (b *blockingRecorder) RecordToolCall(ctx context.Context, r dialhome.ToolCallRecord) error {
	<-b.release
	return b.governor.RecordToolCall(ctx, r)
}

// A slow relay never holds the turn up, and its records still arrive, in
// order, once it answers.
func TestGovernance_ASlowRecorderNeitherHoldsTheTurnNorLosesRecords(t *testing.T) {
	b := &blockingRecorder{governor: governor{decision: dialhome.PolicyDecision{Allow: true}}, release: make(chan struct{})}
	var ran atomic.Int32
	h := newHandler(t, promptkit.Config{
		Provider: calling("get_location", nil), Policy: &b.governor, ToolCalls: b,
		Configure: func(conv *sdk.Conversation) error {
			conv.OnTool("get_location", func(map[string]any) (any, error) { ran.Add(1); return "x", nil })
			return nil
		},
	})
	done := make(chan error, 1)
	go func() {
		done <- h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the turn waited on the recorder")
	}
	close(b.release)
	require.Eventually(t, func() bool { return len(b.records()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, dialhome.ToolCallStarted, b.records()[0].Phase)
}

func TestGovernance_AToolThatFailsIsRecordedAsFailed(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}}
	h := newHandler(t, promptkit.Config{
		Provider: calling("get_location", nil), Policy: g, ToolCalls: g,
		Configure: func(conv *sdk.Conversation) error {
			conv.OnTool("get_location", func(map[string]any) (any, error) { return nil, errors.New("sensor offline") })
			return nil
		},
	})
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	require.Eventually(t, func() bool { return len(g.records()) == 2 }, 5*time.Second, 10*time.Millisecond)
	last := g.records()[1]
	assert.Equal(t, dialhome.ToolCallFailed, last.Phase)
	assert.Contains(t, last.Error, "sensor offline")
}

// An allow's injected headers reach the tool's handler through its context,
// for that call only, and nothing of them outlives the turn.
func TestGovernance_InjectedHeadersReachTheToolsHandler(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true,
		InjectedHeaders: map[string]string{"X-Site-Token": "tok"}}}
	var got []map[string]string
	var mu sync.Mutex
	h := newHandler(t, promptkit.Config{
		Provider: calling("get_location", nil), Policy: g,
		Configure: func(conv *sdk.Conversation) error {
			conv.OnToolCtx("get_location", func(ctx context.Context, _ map[string]any) (any, error) {
				mu.Lock()
				got = append(got, promptkit.InjectedHeaders(ctx))
				mu.Unlock()
				return map[string]any{"room": "control"}, nil
			})
			return nil
		},
	})
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1)
	assert.Equal(t, map[string]string{"X-Site-Token": "tok"}, got[0])

	assert.Nil(t, promptkit.InjectedHeaders(context.Background()), "outside a tool call there are none")
}

// A relay that sends no injected headers (an older one, or a tool not opted
// in) gives the handler none.
func TestGovernance_NoInjectedHeadersIsNil(t *testing.T) {
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}}
	var got map[string]string
	called := false
	h := newHandler(t, promptkit.Config{
		Provider: calling("get_location", nil), Policy: g,
		Configure: func(conv *sdk.Conversation) error {
			conv.OnToolCtx("get_location", func(ctx context.Context, _ map[string]any) (any, error) {
				called, got = true, promptkit.InjectedHeaders(ctx)
				return "ok", nil
			})
			return nil
		},
	})
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.True(t, called)
	assert.Nil(t, got)
}
