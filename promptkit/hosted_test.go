// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
	"github.com/AltairaLabs/omnia-dialhome/promptkit"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

const hostedPack = "testdata/hosted.pack.json"

// toolsStandIn is the relay's tools stream: it hosts lookup_order and
// get_location, and answers every call with answer.
type toolsStandIn struct {
	answer dialhome.HostedToolResult

	mu    sync.Mutex
	calls []dialhome.HostedToolCall
}

func (s *toolsStandIn) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+dialhome.ToolsPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dialhome.HostedToolList{Tools: []dialhome.HostedTool{
			{Name: "lookup_order", Description: "find an order",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)},
			{Name: "get_location", Description: "Omnia's own get_location"},
		}})
	})
	mux.HandleFunc("POST "+dialhome.ToolsCallPath, func(w http.ResponseWriter, r *http.Request) {
		var c dialhome.HostedToolCall
		_ = json.NewDecoder(r.Body).Decode(&c)
		s.mu.Lock()
		s.calls = append(s.calls, c)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(s.answer)
	})
	return mux
}

func (s *toolsStandIn) received() []dialhome.HostedToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dialhome.HostedToolCall(nil), s.calls...)
}

// channelGovernance dials a stand-in relay serving upstreams and returns the
// channel's Governance.
func channelGovernance(t *testing.T, upstreams map[dialhome.StreamKind]http.Handler) *dialhome.Governance {
	t.Helper()
	relay := relaytest.New(t, testKey, upstreams)
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: relay.URL(), Key: testKey, Log: logr.Discard(),
		MinBackoff: 20 * time.Millisecond, KeepAlive: relaytest.KeepAlive,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Eventually(t, c.Connected, 5*time.Second, 10*time.Millisecond)
	return dialhome.NewGovernance(c)
}

// hostedHandler serves the hosted test pack with Tools from gov, the policy
// and recording of g, and get_location as a tool of the pack's own.
func hostedHandler(t *testing.T, gov *dialhome.Governance, g *governor, ran *atomic.Int32,
	p providers.Provider) *promptkit.Handler {
	t.Helper()
	h, err := promptkit.New(promptkit.Config{
		PackPath: hostedPack, PromptName: "assistant",
		Provider:  func(context.Context) (providers.Provider, error) { return p, nil },
		Tools:     gov,
		Policy:    g,
		ToolCalls: g,
		Configure: func(conv *sdk.Conversation) error {
			conv.OnTool("get_location", func(map[string]any) (any, error) {
				ran.Add(1)
				return map[string]any{"room": "control"}, nil
			})
			return nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func capturingCall(tool string, args map[string]any) *offering {
	return &offering{capturing: &capturing{ToolSupport: mock.NewToolProviderWithRepository("mock", "mock-model", false,
		callScript{tool: tool, args: args})}}
}

// offering also keeps the names of the tools the model was offered.
type offering struct {
	*capturing
	omu   sync.Mutex
	names map[string]bool
}

func (o *offering) BuildTooling(descs []*providers.ToolDescriptor) (providers.ProviderTools, error) {
	o.omu.Lock()
	if o.names == nil {
		o.names = map[string]bool{}
	}
	for _, d := range descs {
		o.names[d.Name] = true
	}
	o.omu.Unlock()
	return o.capturing.BuildTooling(descs)
}

func (o *offering) offered(name string) bool {
	o.omu.Lock()
	defer o.omu.Unlock()
	return o.names[name]
}

// The model calls a tool Omnia hosts: it goes over the tools stream for the
// turn's session, and the model sees Omnia's result. Omnia decides and
// records it, so the runtime neither asks nor records.
func TestHostedTools_ACallRunsInOmniaAndTheModelSeesTheResult(t *testing.T) {
	relay := &toolsStandIn{answer: dialhome.HostedToolResult{Result: json.RawMessage(`{"status":"shipped"}`)}}
	gov := channelGovernance(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: relay.handler()})
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}}
	var ran atomic.Int32
	c := capturingCall("lookup_order", map[string]any{"id": "o-7"})
	h := hostedHandler(t, gov, g, &ran, c)

	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where is o-7?"}, &collect{}))
	calls := relay.received()
	require.Len(t, calls, 1)
	assert.Equal(t, "s1", calls[0].SessionID)
	assert.Equal(t, "lookup_order", calls[0].Tool)
	assert.NotEmpty(t, calls[0].CallID, "the model's call id travels with it")
	assert.JSONEq(t, `{"id":"o-7"}`, string(calls[0].Arguments))
	assert.True(t, c.offered("lookup_order"), "the model is offered the hosted tool, with Omnia's schema")

	results := c.toolResults()
	require.NotEmpty(t, results)
	assert.Contains(t, results[len(results)-1], "shipped")
	assert.Empty(t, g.asked, "Omnia decides a hosted call itself")
	assert.Never(t, func() bool { return len(g.records()) > 0 }, 200*time.Millisecond, 10*time.Millisecond,
		"and records it")
}

// A refusal from Omnia reaches the model as the tool's error, in the
// policy's words.
func TestHostedTools_TheModelIsToldWhyOmniaRefusedACall(t *testing.T) {
	relay := &toolsStandIn{answer: dialhome.HostedToolResult{Error: "Refunds above 500 need an approver.",
		DeniedBy: "deny-large-refunds"}}
	gov := channelGovernance(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: relay.handler()})
	var ran atomic.Int32
	c := capturingCall("lookup_order", nil)
	h := hostedHandler(t, gov, &governor{}, &ran, c)
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "refund"}, &collect{}))
	results := c.toolResults()
	require.NotEmpty(t, results)
	assert.Contains(t, results[len(results)-1], "Refunds above 500 need an approver.")
}

// A tool the pack defines, run by its own OnTool handler, wins over the
// hosted tool of the same name, and stays under the runtime's policy.
func TestHostedTools_APackToolWithItsOwnHandlerWins(t *testing.T) {
	relay := &toolsStandIn{}
	gov := channelGovernance(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: relay.handler()})
	g := &governor{decision: dialhome.PolicyDecision{Allow: true}}
	var ran atomic.Int32
	h := hostedHandler(t, gov, g, &ran, capturingCall("get_location", nil))
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where?"}, &collect{}))
	assert.Equal(t, int32(1), ran.Load(), "the pack's handler ran")
	assert.Empty(t, relay.received(), "Omnia was not asked to run it")
	require.Len(t, g.asked, 1, "a tool the runtime runs is put to the policy, as ever")
}

// A relay without the tools stream closes it: the conversation opens with no
// hosted tools, and a call to one never leaves the runtime.
func TestHostedTools_ARelayWithoutTheStreamOffersNone(t *testing.T) {
	gov := channelGovernance(t, nil)
	var ran atomic.Int32
	c := capturingCall("lookup_order", nil)
	h := hostedHandler(t, gov, &governor{decision: dialhome.PolicyDecision{Allow: true}}, &ran, c)
	_ = h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "where is o-7?"}, &collect{})
	assert.False(t, c.offered("lookup_order"))
	for _, r := range c.toolResults() {
		assert.NotContains(t, r, "shipped")
	}
}

// Over the channel, with no answer at all, the model is told the call did not
// run rather than given a result.
func TestHostedTools_ACallWithNoAnswerIsAnError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+dialhome.ToolsPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dialhome.HostedToolList{Tools: []dialhome.HostedTool{{Name: "lookup_order"}}})
	})
	mux.HandleFunc("POST "+dialhome.ToolsCallPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(dialhome.ErrorBody{Error: "no such tool is hosted for this agent"})
	})
	gov := channelGovernance(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: mux})
	var ran atomic.Int32
	c := capturingCall("lookup_order", nil)
	h := hostedHandler(t, gov, &governor{}, &ran, c)
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "o-7?"}, &collect{}))
	results := c.toolResults()
	require.NotEmpty(t, results)
	assert.Contains(t, results[len(results)-1], "the tool did not run: no such tool is hosted for this agent")
}
