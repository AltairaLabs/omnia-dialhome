// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package frontdoor_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/frontdoor"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// echo is a handler that replies "echo: <text>" in two chunks, and records
// the turns it ran.
type echo struct {
	mu    sync.Mutex
	turns []server.Turn
	fail  bool
}

func (e *echo) Capabilities() []string { return nil }

func (e *echo) Converse(_ context.Context, t server.Turn, emit server.Emitter) error {
	e.mu.Lock()
	e.turns = append(e.turns, t)
	e.mu.Unlock()
	if e.fail {
		return errors.New("model down")
	}
	_ = emit.Chunk("echo: ")
	_ = emit.Chunk(t.Content)
	return emit.Done(server.Done{Final: "echo: " + t.Content, Usage: &server.Usage{InputTokens: 4, OutputTokens: 2}})
}

// fakeRelay is the relay's side of the front door.
type fakeRelay struct {
	mu     sync.Mutex
	begins []dialhome.FrontDoorBeginRequest
	ends   map[string]dialhome.FrontDoorEndRequest
	err    error
}

func (f *fakeRelay) BeginFrontDoorTurn(_ context.Context, req dialhome.FrontDoorBeginRequest) (dialhome.FrontDoorBeginResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins = append(f.begins, req)
	if f.err != nil {
		return dialhome.FrontDoorBeginResponse{}, f.err
	}
	return dialhome.FrontDoorBeginResponse{SessionID: "omnia-" + req.PlatformSessionID, TurnID: "turn-1",
		UserID: "u-1", Origin: "oidc", Claims: map[string]string{"groups": "ops"}}, nil
}

func (f *fakeRelay) EndFrontDoorTurn(_ context.Context, turnID string, req dialhome.FrontDoorEndRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ends == nil {
		f.ends = map[string]dialhome.FrontDoorEndRequest{}
	}
	f.ends[turnID] = req
	return nil
}

func newServer(t *testing.T, platform string) (*frontdoor.Server, *echo, *fakeRelay, http.Handler) {
	t.Helper()
	h, relay := &echo{}, &fakeRelay{}
	fd := &frontdoor.Server{Handler: h, Relay: relay, Platform: platform, SessionFallback: "container-session",
		Log: logr.Discard()}
	mux := http.NewServeMux()
	fd.Register(mux)
	return fd, h, relay, mux
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// AgentCore: the session header, a bearer token the JWT authorizer passed on,
// the prompt; the turn runs under the relay's session with the relay's
// identity, and the reply is recorded.
func TestInvocations_AgentCore(t *testing.T) {
	_, h, relay, mux := newServer(t, "")
	rec := do(mux, http.MethodPost, "/invocations", `{"prompt":"hi"}`, map[string]string{
		frontdoor.HeaderAgentCoreSession: "ac-1", "Authorization": "Bearer jwt-1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "echo: hi", body["response"])
	assert.Equal(t, "omnia-ac-1", body["session_id"])

	require.Len(t, relay.begins, 1)
	assert.Equal(t, dialhome.FrontDoorBeginRequest{Platform: frontdoor.PlatformAgentCore, PlatformSessionID: "ac-1",
		IdentityToken: "jwt-1", Message: dialhome.FrontDoorMessage{Text: "hi"}}, relay.begins[0])
	require.Len(t, h.turns, 1)
	assert.Equal(t, "omnia-ac-1", h.turns[0].SessionID)
	assert.Equal(t, "u-1", h.turns[0].Identity.UserID, "the relay's identity, never the request's")
	assert.False(t, h.turns[0].ClientToolsAvailable)
	end := relay.ends["turn-1"]
	assert.Equal(t, "echo: hi", end.Reply)
	require.NotNil(t, end.Usage)
	assert.Equal(t, int32(2), end.Usage.OutputTokens)
}

// The identity token is read from the first header present, in order.
func TestIdentityTokenSources(t *testing.T) {
	cases := map[string]map[string]string{
		"agentcore custom":    {frontdoor.HeaderAgentCoreIdentity: "t1"},
		"foundry x-client":    {frontdoor.HeaderFoundryIdentity: "Bearer t1"},
		"generic":             {frontdoor.HeaderIdentity: "t1"},
		"authorization first": {"Authorization": "Bearer t1", frontdoor.HeaderFoundryIdentity: "t2"},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, relay, mux := newServer(t, "")
			require.Equal(t, http.StatusOK, do(mux, http.MethodPost, "/invocations", `{"message":"x"}`, hdr).Code)
			assert.Equal(t, "t1", relay.begins[0].IdentityToken)
		})
	}
	_, _, relay, mux := newServer(t, "")
	require.Equal(t, http.StatusOK, do(mux, http.MethodPost, "/invocations", `{"message":"x"}`, nil).Code)
	assert.Empty(t, relay.begins[0].IdentityToken, "none: the relay decides")
}

// Foundry invocations: the session from the query, else the container's own;
// a body that is not JSON is the message.
func TestInvocations_Foundry(t *testing.T) {
	_, _, relay, mux := newServer(t, "")
	require.Equal(t, http.StatusOK, do(mux, http.MethodPost, "/invocations?agent_session_id=fs-1", "plain text", nil).Code)
	assert.Equal(t, dialhome.FrontDoorBeginRequest{Platform: frontdoor.PlatformFoundry, PlatformSessionID: "fs-1",
		Message: dialhome.FrontDoorMessage{Text: "plain text"}}, relay.begins[0])
	require.Equal(t, http.StatusOK, do(mux, http.MethodPost, "/invocations", `{"input":"x"}`, nil).Code)
	assert.Equal(t, "container-session", relay.begins[1].PlatformSessionID)
}

// The relay's refusal is the caller's answer; a relay that does not answer
// is 503; nothing runs either way. A failed turn is 500, ended with its error.
func TestInvocations_Refusals(t *testing.T) {
	for status, relayErr := range map[int]error{
		http.StatusUnauthorized:       &dialhome.RelayError{Status: 401, Message: "an identity token is required"},
		http.StatusForbidden:          &dialhome.RelayError{Status: 403, Message: "this agent has no front door"},
		http.StatusServiceUnavailable: errors.New("channel down"),
	} {
		_, h, relay, mux := newServer(t, "")
		relay.err = relayErr
		rec := do(mux, http.MethodPost, "/invocations", `{"prompt":"x"}`, nil)
		assert.Equal(t, status, rec.Code)
		assert.Empty(t, h.turns)
	}
	_, _, _, mux := newServer(t, "")
	assert.Equal(t, http.StatusBadRequest, do(mux, http.MethodPost, "/invocations", `{"prompt":""}`, nil).Code)

	_, h, relay, mux := newServer(t, "")
	h.fail = true
	assert.Equal(t, http.StatusInternalServerError, do(mux, http.MethodPost, "/invocations", `{"prompt":"x"}`, nil).Code)
	assert.Equal(t, "model down", relay.ends["turn-1"].Error)
}

// The relay's session start runs no turn and marks the container as the one
// that holds the channel.
func TestInvocations_SessionStart(t *testing.T) {
	fd, h, relay, mux := newServer(t, "")
	assert.False(t, fd.Kicked())
	rec := do(mux, http.MethodPost, "/invocations", `{"omnia_dial_home":"start"}`, map[string]string{
		frontdoor.HeaderAgentCoreSession: "omnia-dh-1"})
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.True(t, fd.Kicked())
	assert.Empty(t, h.turns)
	assert.Empty(t, relay.begins)
}

// SSE when the caller accepts it: a chunk event per fragment, then done.
func TestInvocations_SSE(t *testing.T) {
	_, _, _, mux := newServer(t, "")
	rec := do(mux, http.MethodPost, "/invocations", `{"prompt":"hi"}`, map[string]string{"Accept": "text/event-stream"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	events := sseEvents(t, rec.Body.String())
	require.Len(t, events, 3)
	assert.Equal(t, "chunk", events[0]["type"])
	assert.Equal(t, "done", events[2]["type"])
	assert.Equal(t, "echo: hi", events[2]["response"])

	_, _, relay, mux := newServer(t, "")
	relay.err = &dialhome.RelayError{Status: 401}
	rec = do(mux, http.MethodPost, "/invocations", `{"prompt":"hi"}`, map[string]string{"Accept": "text/event-stream"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "refused before the stream starts: a status, not a stream")
}

func sseEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var m map[string]any
			require.NoError(t, json.Unmarshal([]byte(data), &m))
			out = append(out, m)
		}
	}
	return out
}

// Foundry responses: input as a string or message items, the session from
// agent_session_id or conversation; a Responses object back, or its events.
func TestResponses(t *testing.T) {
	_, _, relay, mux := newServer(t, "")
	rec := do(mux, http.MethodPost, "/responses", `{"input":"hi","agent_session_id":"fs-2"}`,
		map[string]string{frontdoor.HeaderFoundryIdentity: "t1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var obj map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &obj))
	assert.Equal(t, "response", obj["object"])
	assert.Equal(t, "echo: hi", obj["output_text"])
	out := obj["output"].([]any)[0].(map[string]any)
	assert.Equal(t, "echo: hi", out["content"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, dialhome.FrontDoorBeginRequest{Platform: frontdoor.PlatformFoundry, PlatformSessionID: "fs-2",
		IdentityToken: "t1", Message: dialhome.FrontDoorMessage{Text: "hi"}}, relay.begins[0])

	items := `{"conversation":{"id":"conv-1"},"input":[{"role":"user","content":"old"},` +
		`{"role":"assistant","content":"x"},{"role":"user","content":[{"type":"input_text","text":"new"}]}]}`
	require.Equal(t, http.StatusOK, do(mux, http.MethodPost, "/responses", items, nil).Code)
	assert.Equal(t, "new", relay.begins[1].Message.Text)
	assert.Equal(t, "conv-1", relay.begins[1].PlatformSessionID)

	rec = do(mux, http.MethodPost, "/responses", `{"input":"hi","stream":true}`, nil)
	events := sseEvents(t, rec.Body.String())
	require.NotEmpty(t, events)
	assert.Equal(t, "response.output_text.delta", events[0]["type"])
	assert.Equal(t, "response.completed", events[len(events)-1]["type"])

	assert.Equal(t, http.StatusBadRequest, do(mux, http.MethodPost, "/responses", "nope", nil).Code)
}

// Vertex: the token and session ride input; query answers output, stream
// query NDJSON lines.
func TestVertex(t *testing.T) {
	_, _, relay, mux := newServer(t, "")
	q := `{"class_method":"query","input":{"message":"hi","session_id":"vx-1","identity_token":"t1"}}`
	rec := do(mux, http.MethodPost, "/api/reasoning_engine", q, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "echo: hi", body["output"])
	assert.Equal(t, dialhome.FrontDoorBeginRequest{Platform: frontdoor.PlatformVertex, PlatformSessionID: "vx-1",
		IdentityToken: "t1", Message: dialhome.FrontDoorMessage{Text: "hi"}}, relay.begins[0])

	rec = do(mux, http.MethodPost, "/api/stream_reasoning_engine", q, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	require.Len(t, lines, 3)
	assert.JSONEq(t, `{"output":"echo: "}`, lines[0])
	assert.Contains(t, lines[2], `"done":true`)

	relay.err = &dialhome.RelayError{Status: 401}
	assert.Equal(t, http.StatusUnauthorized, do(mux, http.MethodPost, "/api/stream_reasoning_engine", q, nil).Code)
	assert.Equal(t, http.StatusBadRequest, do(mux, http.MethodPost, "/api/reasoning_engine", "x", nil).Code)
}

// Busy while a turn runs, and not after.
func TestBusy(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	relay := &fakeRelay{}
	fd := &frontdoor.Server{Handler: blocking{started: started, release: release}, Relay: relay}
	mux := http.NewServeMux()
	fd.Register(mux)
	go do(mux, http.MethodPost, "/invocations", `{"prompt":"x"}`, nil)
	<-started
	assert.True(t, fd.Busy())
	close(release)
	assert.Eventually(t, func() bool { return !fd.Busy() }, time.Second, 5*time.Millisecond)
}

type blocking struct{ started, release chan struct{} }

func (blocking) Capabilities() []string { return nil }
func (b blocking) Converse(_ context.Context, _ server.Turn, emit server.Emitter) error {
	close(b.started)
	<-b.release
	return emit.Done(server.Done{Final: "ok"})
}

// End to end over a real channel: the turn is opened and ended on the
// front-door stream.
func TestFrontDoorOverTheChannel(t *testing.T) {
	var mu sync.Mutex
	var ended dialhome.FrontDoorEndRequest
	upstream := http.NewServeMux()
	upstream.HandleFunc("POST "+dialhome.FrontDoorTurnsPath, func(w http.ResponseWriter, r *http.Request) {
		var req dialhome.FrontDoorBeginRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(dialhome.FrontDoorBeginResponse{SessionID: "s-" + req.PlatformSessionID,
			TurnID: "t/1", Origin: "oidc"})
	})
	upstream.HandleFunc("POST "+dialhome.FrontDoorTurnsPath+"/{turn}/end", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewDecoder(r.Body).Decode(&ended)
		assert.Equal(t, "t/1", r.PathValue("turn"))
		w.WriteHeader(http.StatusNoContent)
	})
	relay := relaytest.New(t, "k", map[dialhome.StreamKind]http.Handler{dialhome.KindFrontDoor: upstream})
	ch, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{URL: relay.URL(), Key: "k", Log: logr.Discard()})
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()
	require.Eventually(t, ch.Connected, 5*time.Second, 10*time.Millisecond)

	h := &echo{}
	fd := &frontdoor.Server{Handler: h, Relay: dialhome.NewGovernance(ch)}
	mux := http.NewServeMux()
	fd.Register(mux)
	rec := do(mux, http.MethodPost, "/invocations", `{"prompt":"hi"}`, map[string]string{frontdoor.HeaderAgentCoreSession: "ac"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "s-ac", h.turns[0].SessionID)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "echo: hi", ended.Reply)
}

func TestFrontDoorEndPath(t *testing.T) {
	assert.Equal(t, "/v1/front-door/turns/a%2Fb/end", dialhome.FrontDoorEndPath("a/b"))
	assert.Equal(t, "front-door", dialhome.KindFrontDoor.String())
}
