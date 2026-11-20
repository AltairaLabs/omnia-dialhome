// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/events"
	"github.com/AltairaLabs/PromptKit/runtime/v2/hooks"
	"github.com/AltairaLabs/PromptKit/runtime/v2/memory"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

type decideFunc func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error)

func (f decideFunc) Decide(ctx context.Context, r dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
	return f(ctx, r)
}

func gate(d decideFunc) *policyGate {
	return &policyGate{decider: d, sessionID: "s1", registry: "site-tools",
		exempt: map[string]bool{"get_location": true}, log: logr.Discard()}
}

func TestPolicyGate_AsksForEveryLocalToolAndFollowsTheAnswer(t *testing.T) {
	var asked dialhome.PolicyRequest
	g := gate(func(_ context.Context, r dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		asked = r
		return dialhome.PolicyDecision{Allow: true}, nil
	})
	d := g.BeforeExecution(context.Background(), hooks.ToolRequest{
		Name: "open_ticket", CallID: "c1", Args: json.RawMessage(`{"amount":100}`)})
	assert.True(t, d.Allow)
	assert.Equal(t, dialhome.PolicyRequest{SessionID: "s1", CallID: "c1", Tool: "open_ticket",
		Registry: "site-tools", Arguments: json.RawMessage(`{"amount":100}`)}, asked)
}

func TestPolicyGate_ARefusalCarriesThePolicysMessage(t *testing.T) {
	g := gate(func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		return dialhome.PolicyDecision{Allow: false, Reason: "above the limit", DeniedBy: "deny-above-limit"}, nil
	})
	d := g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "open_ticket"})
	assert.False(t, d.Allow)
	assert.Equal(t, "above the limit", d.Reason)
	assert.Equal(t, "deny-above-limit", d.Metadata["denied_by"])

	g = gate(func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		return dialhome.PolicyDecision{}, nil
	})
	d = g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "open_ticket"})
	assert.False(t, d.Allow, "a decision that does not say allow is a refusal")
	assert.NotEmpty(t, d.Reason)
}

func TestPolicyGate_NoDecisionIsARefusal(t *testing.T) {
	for name, err := range map[string]error{
		"channel down": fmt.Errorf("%w: %w", dialhome.ErrNoDecision, dialhome.ErrNotConnected),
		"timeout":      fmt.Errorf("%w: %w", dialhome.ErrNoDecision, context.DeadlineExceeded),
		"relay error": fmt.Errorf("%w: %w", dialhome.ErrNoDecision,
			&dialhome.RelayError{Status: 502, Message: "decision service unavailable"}),
	} {
		t.Run(name, func(t *testing.T) {
			g := gate(func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
				return dialhome.PolicyDecision{Allow: true}, err // an allow beside an error is not an allow
			})
			d := g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "read_sensor"})
			assert.False(t, d.Allow)
			assert.Contains(t, d.Reason, "did not run")
		})
	}
}

func TestPolicyGate_OnlyTheExemptToolsAreNotPutToIt(t *testing.T) {
	var asked []string
	g := gate(func(_ context.Context, r dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		asked = append(asked, r.Tool)
		return dialhome.PolicyDecision{}, nil
	})
	for _, n := range relayMemoryTools {
		g.exempt[n] = true
	}
	assert.True(t, g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "get_location"}).Allow)
	assert.True(t, g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "memory__recall"}).Allow)
	assert.False(t, g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "memory__export"}).Allow)
	assert.Equal(t, []string{"memory__export"}, asked)
	assert.True(t, g.AfterExecution(context.Background(), hooks.ToolRequest{}, hooks.ToolResponse{}).Allow)
	assert.NotEmpty(t, g.Name())
}

func TestToolCallRecord_Phases(t *testing.T) {
	ts := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	text := "Raised ticket 1042"
	ev := func(typ events.EventType, d events.ToolCallEventData) *events.Event {
		return &events.Event{Type: typ, Timestamp: ts, Data: &d}
	}

	r, ok := toolCallRecord(ev(events.EventToolCallStarted, events.ToolCallEventData{
		ToolName: "open_ticket", CallID: "c1", Args: map[string]any{"asset": "pump-2"}, Round: 1}), "s1")
	require.True(t, ok)
	assert.Equal(t, dialhome.ToolCallStarted, r.Phase)
	assert.JSONEq(t, `{"asset":"pump-2"}`, string(r.Arguments))
	assert.Equal(t, 1, r.Round)
	assert.Equal(t, ts, r.Time)

	r, ok = toolCallRecord(ev(events.EventToolCallCompleted, events.ToolCallEventData{
		ToolName: "open_ticket", CallID: "c1", Status: "success", Duration: 30 * time.Millisecond,
		Parts: []types.ContentPart{{Type: "text", Text: &text}}}), "s1")
	require.True(t, ok)
	assert.Equal(t, dialhome.ToolCallCompleted, r.Phase)
	assert.Equal(t, text, r.Result)
	assert.Equal(t, int64(30), r.DurationMs)

	r, ok = toolCallRecord(ev(events.EventToolCallCompleted, events.ToolCallEventData{
		ToolName: "read_sensor", CallID: "c2", Status: "failed", Parts: []types.ContentPart{{Type: "text", Text: &text}}}), "s1")
	require.True(t, ok)
	assert.Equal(t, dialhome.ToolCallFailed, r.Phase, "a completed call that failed is recorded as failed")
	assert.Equal(t, text, r.Error)

	r, ok = toolCallRecord(ev(events.EventToolCallFailed, events.ToolCallEventData{
		ToolName: "read_sensor", CallID: "c3", Error: errors.New("sensor unreachable")}), "s1")
	require.True(t, ok)
	assert.Equal(t, "sensor unreachable", r.Error)

	_, ok = toolCallRecord(ev(events.EventToolCallCompleted, events.ToolCallEventData{Status: "pending"}), "s1")
	assert.False(t, ok, "a client tool handed to the client is not an outcome")
	_, ok = toolCallRecord(&events.Event{Type: events.EventProviderCallStarted}, "s1")
	assert.False(t, ok)
	d := events.ToolCallEventData{ToolName: "x"}
	_, ok = toolCallRecord(&events.Event{Type: events.EventToolCallStarted, Data: d}, "s1")
	assert.True(t, ok, "event data by value is read too")
}

// memoryFake is a MemoryService that keeps what it is asked.
type memoryFake struct {
	scope    dialhome.MemoryScope
	scopeErr error
	saved    []dialhome.MemoryRecord
	recalled []dialhome.MemoryRecallRequest
	listed   []dialhome.MemoryListRequest
	forgot   []string
	err      error
}

func (m *memoryFake) MemoryScope(context.Context, string) (dialhome.MemoryScope, error) {
	return m.scope, m.scopeErr
}

func (m *memoryFake) Remember(_ context.Context, _ string, r dialhome.MemoryRecord) (string, error) {
	m.saved = append(m.saved, r)
	return "m1", m.err
}

func (m *memoryFake) Recall(_ context.Context, r dialhome.MemoryRecallRequest) ([]dialhome.MemoryRecord, error) {
	m.recalled = append(m.recalled, r)
	return []dialhome.MemoryRecord{{ID: "m1", Content: "prefers metric units", Confidence: 0.9}}, m.err
}

func (m *memoryFake) ListMemories(_ context.Context, r dialhome.MemoryListRequest) ([]dialhome.MemoryRecord, error) {
	m.listed = append(m.listed, r)
	return nil, m.err
}

func (m *memoryFake) Forget(_ context.Context, _, id string) error {
	m.forgot = append(m.forgot, id)
	return m.err
}

func TestMemoryStore_EveryOperationGoesToTheSession(t *testing.T) {
	f := &memoryFake{}
	s := &memoryStore{svc: f, sessionID: "s1"}
	ctx := context.Background()

	m := &memory.Memory{Type: "preference", Content: "prefers metric units", Confidence: 0.9,
		Scope: map[string]string{"user_id": "anyone"}}
	require.NoError(t, s.Save(ctx, m))
	assert.Equal(t, "m1", m.ID)
	assert.Equal(t, "prefers metric units", f.saved[0].Content)

	got, err := s.Retrieve(ctx, map[string]string{"user_id": "someone-else"}, "units",
		memory.RetrieveOptions{Limit: 3, Types: []string{"preference"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, dialhome.MemoryRecallRequest{SessionID: "s1", Query: "units", Limit: 3,
		Types: []string{"preference"}}, f.recalled[0], "the scope PromptKit passes is not sent: the relay scopes")

	_, err = s.List(ctx, nil, memory.ListOptions{Limit: 10, Offset: 5})
	require.NoError(t, err)
	assert.Equal(t, dialhome.MemoryListRequest{SessionID: "s1", Limit: 10, Offset: 5}, f.listed[0])

	require.NoError(t, s.Delete(ctx, nil, "m1"))
	assert.Equal(t, []string{"m1"}, f.forgot)
	assert.Error(t, s.DeleteAll(ctx, nil), "forgetting everything is not the agent's to do")

	msgs := []types.Message{{Role: "user", Content: "what units?"}, {Role: "assistant", Content: "…"}}
	got, err = s.RetrieveContext(ctx, nil, msgs)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, "what units?", f.recalled[1].Query)
	got, err = s.RetrieveContext(ctx, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got, "no user message, no recall")

	f.err = &dialhome.RelayError{Status: 422, Message: "not stored: no consent"}
	assert.EqualError(t, s.Save(ctx, &memory.Memory{Content: "x"}), "not stored: no consent",
		"the relay's words reach the model")
	f.err = errors.New(`Post "http://dial-home/v1/memory/recall": dial-home: channel not connected`)
	_, err = s.Retrieve(ctx, nil, "x", memory.RetrieveOptions{})
	assert.ErrorIs(t, err, errMemoryUnavailable, "the transport's never do")
}

func TestMemoryOptions_FollowTheScope(t *testing.T) {
	f := &memoryFake{}
	assert.Nil(t, memoryOptions(dialhome.MemoryScope{}, f, "s1"), "no memory")
	assert.Len(t, memoryOptions(dialhome.MemoryScope{Enabled: true}, f, "s1"), 1)
	assert.Len(t, memoryOptions(dialhome.MemoryScope{Enabled: true, User: true, Tools: true, Retrieval: true}, f, "s1"), 1)
}

func TestInjectedHeaders_KeptPerCallUntilTheTurnEnds(t *testing.T) {
	var h injectedHeaders
	h.put("", map[string]string{"A": "1"})
	h.put("c1", nil)
	assert.Nil(t, h.get("c1"), "nothing to keep")
	in := map[string]string{"X-Site-Token": "tok"}
	h.put("c1", in)
	in["X-Site-Token"] = "changed"
	assert.Equal(t, map[string]string{"X-Site-Token": "tok"}, h.get("c1"), "kept as given")
	out := h.get("c1")
	out["X-Site-Token"] = "changed"
	assert.Equal(t, map[string]string{"X-Site-Token": "tok"}, h.get("c1"), "a reader cannot change them")
	assert.Nil(t, h.get("c2"))
	h.clear()
	assert.Nil(t, h.get("c1"))

	var none *injectedHeaders
	none.put("c1", in)
	none.clear()
	assert.Nil(t, none.get("c1"))
}

func TestPolicyGate_KeepsAnAllowsInjectedHeaders(t *testing.T) {
	g := gate(func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		return dialhome.PolicyDecision{Allow: true, InjectedHeaders: map[string]string{"X-T": "v"}}, nil
	})
	g.injected = &injectedHeaders{}
	require.True(t, g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "open_ticket", CallID: "c1"}).Allow)
	assert.Equal(t, map[string]string{"X-T": "v"}, g.injected.get("c1"))

	g = gate(func(context.Context, dialhome.PolicyRequest) (dialhome.PolicyDecision, error) {
		return dialhome.PolicyDecision{Allow: false, InjectedHeaders: map[string]string{"X-T": "v"}}, nil
	})
	g.injected = &injectedHeaders{}
	require.False(t, g.BeforeExecution(context.Background(), hooks.ToolRequest{Name: "open_ticket", CallID: "c1"}).Allow)
	assert.Nil(t, g.injected.get("c1"), "never on a refusal")
}
