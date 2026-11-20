// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

func historyRelay(t *testing.T, answer func(body map[string]any) (int, any)) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST "+dialhome.HistoryPath, jsonHandler(t, dialhome.HistoryPath, answer))
	return mux
}

func TestHistory_NamesTheSessionAndReturnsItsConversation(t *testing.T) {
	var got map[string]any
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindHistory: historyRelay(t, func(b map[string]any) (int, any) {
		got = b
		return http.StatusOK, dialhome.History{Messages: []dialhome.HistoryMessage{
			{Role: dialhome.HistoryRoleUser, Content: "my name is Ada"},
			{Role: dialhome.HistoryRoleAssistant, Content: "hello Ada"},
		}, Truncated: true}
	})})
	h, err := g.History(context.Background(), "s1")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"session_id": "s1"}, got, "the session and nothing else")
	require.Len(t, h.Messages, 2)
	assert.Equal(t, dialhome.HistoryMessage{Role: "user", Content: "my name is Ada"}, h.Messages[0])
	assert.Equal(t, dialhome.HistoryMessage{Role: "assistant", Content: "hello Ada"}, h.Messages[1])
	assert.True(t, h.Truncated)
}

func TestHistory_ARefusalIsARelayError(t *testing.T) {
	g := governed(t, map[dialhome.StreamKind]http.Handler{dialhome.KindHistory: historyRelay(t, func(map[string]any) (int, any) {
		return http.StatusNotFound, dialhome.ErrorBody{Error: "no such session on this relay"}
	})})
	_, err := g.History(context.Background(), "other")
	var re *dialhome.RelayError
	require.True(t, errors.As(err, &re))
	assert.Equal(t, http.StatusNotFound, re.Status)
}

func TestHistory_ARelayWithoutTheStreamIsAnError(t *testing.T) {
	g := governed(t, nil)
	h, err := g.History(context.Background(), "s1")
	require.Error(t, err, "a closed stream is no answer, never an empty conversation")
	assert.Empty(t, h.Messages)
}

func TestStreamKind_HistoryIsNamed(t *testing.T) {
	assert.Equal(t, "history", dialhome.KindHistory.String())
	assert.Equal(t, dialhome.StreamKind(9), dialhome.KindHistory, "a wire byte: never renumber")
}
