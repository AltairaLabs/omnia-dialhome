// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/promptkit"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// turnCounter replies with the turn number the mock derives from the
// request: one more than the assistant replies already in the conversation.
// A conversation that carried its history over answers a higher number.
type turnCounter struct{}

func (turnCounter) GetResponse(ctx context.Context, p mock.ResponseParams) (string, error) {
	t, err := turnCounter{}.GetTurn(ctx, p)
	return t.Content, err
}

func (turnCounter) GetTurn(_ context.Context, p mock.ResponseParams) (*mock.Turn, error) {
	return &mock.Turn{Type: "text", Content: fmt.Sprintf("replies so far: %d", p.TurnNumber)}, nil
}

func countingProvider() promptkit.ProviderFunc {
	return func(context.Context) (providers.Provider, error) {
		return mock.NewToolProviderWithRepository("mock", "mock-model", false, turnCounter{}), nil
	}
}

// fakeHistory is a HistorySource with a fixed answer, counting reads.
type fakeHistory struct {
	mu    sync.Mutex
	h     dialhome.History
	err   error
	reads []string
}

func (f *fakeHistory) History(_ context.Context, id string) (dialhome.History, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, id)
	return f.h, f.err
}

func (f *fakeHistory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func twoTurns() dialhome.History {
	return dialhome.History{Messages: []dialhome.HistoryMessage{
		{Role: dialhome.HistoryRoleUser, Content: "my name is Ada"},
		{Role: dialhome.HistoryRoleAssistant, Content: "hello Ada"},
		{Role: "system", Content: "never carried"},
		{Role: dialhome.HistoryRoleUser, Content: ""},
		{Role: dialhome.HistoryRoleUser, Content: "what is 2+2?"},
		{Role: dialhome.HistoryRoleAssistant, Content: "4"},
	}}
}

func TestHistory_ASessionThisProcessDoesNotHoldOpensWithItsEarlierTurns(t *testing.T) {
	src := &fakeHistory{h: twoTurns()}
	h := newHandler(t, promptkit.Config{Provider: countingProvider(), History: src})
	ctx := context.Background()

	emit := &collect{}
	require.NoError(t, h.Converse(ctx, server.Turn{SessionID: "s1", Content: "what is my name?"}, emit))
	assert.Equal(t, "replies so far: 2", emit.done.Final, "the model saw both earlier replies")
	assert.Equal(t, []string{"s1"}, src.reads)

	emit = &collect{}
	require.NoError(t, h.Converse(ctx, server.Turn{SessionID: "s1", Content: "and again?"}, emit))
	assert.Equal(t, "replies so far: 3", emit.done.Final, "the rehydrated conversation goes on in memory")
	assert.Equal(t, 1, src.count(), "a held conversation is not read again")
}

func TestHistory_NoneRecordedOrNoAnswerOpensANewConversation(t *testing.T) {
	for name, src := range map[string]*fakeHistory{
		"none recorded": {},
		"no answer":     {err: errors.New("stream closed")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHandler(t, promptkit.Config{Provider: countingProvider(), History: src})
			emit := &collect{}
			require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "hi"}, emit))
			assert.Equal(t, "replies so far: 1", emit.done.Final)
		})
	}
}

func TestHistory_WithoutASourceNothingIsRead(t *testing.T) {
	h := newHandler(t, promptkit.Config{Provider: countingProvider()})
	emit := &collect{}
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s1", Content: "hi"}, emit))
	assert.Equal(t, "replies so far: 1", emit.done.Final)
	assert.Equal(t, server.ResumeNotFound, h.HasConversation(context.Background(), "other"))
}

func TestHistory_HasConversationAnswersFromTheRecord(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		src  *fakeHistory
		want server.ResumeState
	}{
		"recorded":           {&fakeHistory{h: twoTurns()}, server.ResumeResumable},
		"nothing recorded":   {&fakeHistory{}, server.ResumeNotFound},
		"session unknown":    {&fakeHistory{err: &dialhome.RelayError{Status: http.StatusNotFound}}, server.ResumeNotFound},
		"relay without it":   {&fakeHistory{err: errors.New("EOF")}, server.ResumeNotFound},
		"session-api is out": {&fakeHistory{err: &dialhome.RelayError{Status: http.StatusBadGateway}}, server.ResumeUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHandler(t, promptkit.Config{Provider: countingProvider(), History: c.src})
			assert.Equal(t, c.want, h.HasConversation(ctx, "s1"))
		})
	}
}

// TestHistory_OverTheChannel reads the history from a relay's history stream,
// as the command wires it: a new process (a new container) answers a session
// with its earlier turns.
func TestHistory_OverTheChannel(t *testing.T) {
	var asked map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+dialhome.HistoryPath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		_ = json.NewEncoder(w).Encode(twoTurns())
	})
	gov := channelGovernance(t, map[dialhome.StreamKind]http.Handler{dialhome.KindHistory: mux})
	h := newHandler(t, promptkit.Config{Provider: countingProvider(), History: gov})

	assert.Equal(t, server.ResumeResumable, h.HasConversation(context.Background(), "s9"))
	emit := &collect{}
	require.NoError(t, h.Converse(context.Background(), server.Turn{SessionID: "s9", Content: "what is my name?"}, emit))
	assert.Equal(t, "replies so far: 2", emit.done.Final)
	assert.Equal(t, map[string]any{"session_id": "s9"}, asked)
}
