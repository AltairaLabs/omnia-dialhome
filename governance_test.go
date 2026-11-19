// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
)

// governed dials a stand-in relay serving upstreams and returns the channel's
// Governance once the channel is up.
func governed(t *testing.T, upstreams map[dialhome.StreamKind]http.Handler) *dialhome.Governance {
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

func jsonHandler(t *testing.T, path string, answer func(body map[string]any) (int, any)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || r.Method != http.MethodPost {
			http.Error(w, "not here", http.StatusNotFound)
			return
		}
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		status, out := answer(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	})
}

func TestDecide_CarriesTheCallAndReturnsTheRelaysDecision(t *testing.T) {
	var got map[string]any
	g := governed(t, map[dialhome.StreamKind]http.Handler{
		dialhome.KindPolicy: jsonHandler(t, dialhome.PolicyDecisionPath, func(b map[string]any) (int, any) {
			got = b
			return http.StatusOK, dialhome.PolicyDecision{Allow: false, Reason: "above the limit", DeniedBy: "r1"}
		}),
	})
	d, err := g.Decide(context.Background(), dialhome.PolicyRequest{
		SessionID: "s1", CallID: "c1", Tool: "open_ticket", Registry: "site-tools",
		Arguments: json.RawMessage(`{"amount":5000}`),
	})
	require.NoError(t, err)
	assert.False(t, d.Allow)
	assert.Equal(t, "above the limit", d.Reason)
	assert.Equal(t, "r1", d.DeniedBy)
	assert.Equal(t, "s1", got["session_id"])
	assert.Equal(t, "site-tools", got["registry"])
	assert.Equal(t, map[string]any{"amount": float64(5000)}, got["arguments"])
}

func TestDecide_NoAnswerIsAnErrorNeverAnAllow(t *testing.T) {
	t.Run("a relay that does not serve policy", func(t *testing.T) {
		g := governed(t, nil)
		d, err := g.Decide(context.Background(), dialhome.PolicyRequest{SessionID: "s", Tool: "t"})
		require.ErrorIs(t, err, dialhome.ErrNoDecision)
		assert.False(t, d.Allow)
	})
	t.Run("a relay that fails", func(t *testing.T) {
		g := governed(t, map[dialhome.StreamKind]http.Handler{
			dialhome.KindPolicy: jsonHandler(t, dialhome.PolicyDecisionPath, func(map[string]any) (int, any) {
				return http.StatusBadGateway, dialhome.ErrorBody{Error: "decision service unavailable"}
			}),
		})
		_, err := g.Decide(context.Background(), dialhome.PolicyRequest{SessionID: "s", Tool: "t"})
		require.ErrorIs(t, err, dialhome.ErrNoDecision)
		var re *dialhome.RelayError
		require.ErrorAs(t, err, &re)
		assert.Equal(t, "decision service unavailable", re.Message)
	})
	t.Run("a relay too slow to answer", func(t *testing.T) {
		g := governed(t, map[dialhome.StreamKind]http.Handler{
			dialhome.KindPolicy: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }),
		})
		g.PolicyTimeout = 50 * time.Millisecond
		_, err := g.Decide(context.Background(), dialhome.PolicyRequest{SessionID: "s", Tool: "t"})
		require.ErrorIs(t, err, dialhome.ErrNoDecision)
	})
	t.Run("no channel", func(t *testing.T) {
		c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
			URL: "ws://127.0.0.1:1/v1/channel", Key: testKey, Log: logr.Discard(),
		})
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_, err = dialhome.NewGovernance(c).Decide(context.Background(), dialhome.PolicyRequest{SessionID: "s"})
		require.ErrorIs(t, err, dialhome.ErrNoDecision)
		assert.True(t, errors.Is(err, dialhome.ErrNotConnected))
	})
}

func TestRecordToolCall_PostsTheRecord(t *testing.T) {
	got := make(chan map[string]any, 1)
	g := governed(t, map[dialhome.StreamKind]http.Handler{
		dialhome.KindToolCalls: jsonHandler(t, dialhome.ToolCallsPath, func(b map[string]any) (int, any) {
			got <- b
			return http.StatusAccepted, struct{}{}
		}),
	})
	require.NoError(t, g.RecordToolCall(context.Background(), dialhome.ToolCallRecord{
		SessionID: "s1", CallID: "c1", Tool: "read_sensor", Phase: dialhome.ToolCallCompleted,
		Result: "ok", DurationMs: 12, Time: time.Now(),
	}))
	b := <-got
	assert.Equal(t, "completed", b["phase"])
	assert.Equal(t, "ok", b["result"])
}

func TestMemory_EveryOperationNamesItsSession(t *testing.T) {
	mux := http.NewServeMux()
	seen := map[string]string{}
	answer := func(path string, out any) {
		mux.Handle(path, jsonHandler(t, path, func(b map[string]any) (int, any) {
			seen[path], _ = b["session_id"].(string)
			return http.StatusOK, out
		}))
	}
	answer(dialhome.MemoryScopePath, dialhome.MemoryScope{Enabled: true, User: true, Tools: true})
	answer(dialhome.MemoryRememberPath, dialhome.MemoryRememberResponse{ID: "m1"})
	answer(dialhome.MemoryRecallPath, dialhome.MemoryList{Memories: []dialhome.MemoryRecord{{ID: "m1", Content: "likes tea"}}})
	answer(dialhome.MemoryListPath, dialhome.MemoryList{})
	answer(dialhome.MemoryForgetPath, struct{}{})
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindMemoryStore: mux})
	ctx := context.Background()

	scope, err := g.MemoryScope(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, dialhome.MemoryScope{Enabled: true, User: true, Tools: true}, scope)
	id, err := g.Remember(ctx, "s1", dialhome.MemoryRecord{Content: "likes tea"})
	require.NoError(t, err)
	assert.Equal(t, "m1", id)
	got, err := g.Recall(ctx, dialhome.MemoryRecallRequest{SessionID: "s1", Query: "tea"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "likes tea", got[0].Content)
	_, err = g.ListMemories(ctx, dialhome.MemoryListRequest{SessionID: "s1"})
	require.NoError(t, err)
	require.NoError(t, g.Forget(ctx, "s1", "m1"))
	for _, p := range []string{dialhome.MemoryScopePath, dialhome.MemoryRememberPath, dialhome.MemoryRecallPath,
		dialhome.MemoryListPath, dialhome.MemoryForgetPath} {
		assert.Equal(t, "s1", seen[p], p)
	}
}

func TestMemory_ARefusalCarriesTheRelaysWords(t *testing.T) {
	g := governed(t, map[dialhome.StreamKind]http.Handler{
		dialhome.KindMemoryStore: jsonHandler(t, dialhome.MemoryRememberPath, func(map[string]any) (int, any) {
			return http.StatusUnprocessableEntity, dialhome.ErrorBody{Error: "not stored: the user has not consented"}
		}),
	})
	_, err := g.Remember(context.Background(), "s1", dialhome.MemoryRecord{Content: "x"})
	require.Error(t, err)
	assert.Equal(t, "not stored: the user has not consented", err.Error())
}

func TestDecide_AnAnswerThatIsNotADecisionIsNotAnAllow(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"empty object": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) },
		"malformed":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"allow":tr`)) },
		"no content":   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"a redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, dialhome.PolicyDecisionPath+"?again", http.StatusTemporaryRedirect)
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindPolicy: h})
			d, err := g.Decide(context.Background(), dialhome.PolicyRequest{SessionID: "s", Tool: "t"})
			assert.False(t, d.Allow)
			if name != "empty object" {
				assert.ErrorIs(t, err, dialhome.ErrNoDecision)
			}
		})
	}
}
