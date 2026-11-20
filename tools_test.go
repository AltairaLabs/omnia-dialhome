// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// toolsRelay is a relay's tools stream: it lists tools and answers calls.
func toolsRelay(t *testing.T, call func(body map[string]any) (int, any)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+dialhome.ToolsPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dialhome.HostedToolList{Tools: []dialhome.HostedTool{
			{Name: "lookup_order", Description: "find an order", InputSchema: json.RawMessage(`{"type":"object"}`)},
		}})
	})
	mux.Handle("POST "+dialhome.ToolsCallPath, jsonHandler(t, dialhome.ToolsCallPath, call))
	return mux
}

func TestHostedTools_ListsWhatTheRelayHosts(t *testing.T) {
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: toolsRelay(t, nil)})
	list, err := g.HostedTools(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "lookup_order", list[0].Name)
	assert.Equal(t, "find an order", list[0].Description)
	assert.JSONEq(t, `{"type":"object"}`, string(list[0].InputSchema))
}

func TestHostedTools_ARelayWithoutTheStreamListsNone(t *testing.T) {
	g := governed(t, nil)
	list, err := g.HostedTools(context.Background())
	require.Error(t, err, "a closed stream is no list, never an empty one taken for an answer")
	assert.Nil(t, list)
}

func TestCallTool_CarriesTheCallAndReturnsTheResult(t *testing.T) {
	var got map[string]any
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: toolsRelay(t, func(b map[string]any) (int, any) {
		got = b
		return http.StatusOK, dialhome.HostedToolResult{Result: json.RawMessage(`{"status":"shipped"}`)}
	})})
	res, err := g.CallTool(context.Background(), dialhome.HostedToolCall{
		SessionID: "s1", CallID: "c1", Tool: "lookup_order", Arguments: json.RawMessage(`{"id":"o-7"}`), Round: 2,
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"shipped"}`, string(res.Result))
	assert.Equal(t, "s1", got["session_id"])
	assert.Equal(t, "c1", got["call_id"])
	assert.Equal(t, "lookup_order", got["tool"])
	assert.Equal(t, map[string]any{"id": "o-7"}, got["arguments"])
	assert.Equal(t, float64(2), got["round"])
}

func TestCallTool_ARefusalIsAResultAndAnUnhostedToolAnError(t *testing.T) {
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindTools: toolsRelay(t, func(b map[string]any) (int, any) {
		if b["tool"] == "drop_tables" {
			return http.StatusNotFound, dialhome.ErrorBody{Error: "no such tool is hosted for this agent"}
		}
		return http.StatusOK, dialhome.HostedToolResult{Error: "above the limit", DeniedBy: "deny-above-limit"}
	})})
	res, err := g.CallTool(context.Background(), dialhome.HostedToolCall{SessionID: "s1", Tool: "approve_refund"})
	require.NoError(t, err)
	assert.Equal(t, "above the limit", res.Error)
	assert.Equal(t, "deny-above-limit", res.DeniedBy)

	_, err = g.CallTool(context.Background(), dialhome.HostedToolCall{SessionID: "s1", Tool: "drop_tables"})
	var re *dialhome.RelayError
	require.True(t, errors.As(err, &re))
	assert.Equal(t, http.StatusNotFound, re.Status)
	assert.Equal(t, "no such tool is hosted for this agent", re.Message)
}

func TestStreamKind_ToolsIsNamed(t *testing.T) {
	assert.Equal(t, "tools", dialhome.KindTools.String())
	assert.Equal(t, dialhome.StreamKind(8), dialhome.KindTools, "a wire byte: never renumber")
}
