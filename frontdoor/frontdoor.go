// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package frontdoor serves an agent's turns through its hosting platform's own
// invoke API, the "front door", and has Omnia govern each one over the
// dial-home channel.
//
// A turn arrives on the platform's route (AgentCore and Foundry
// POST /invocations, Foundry POST /responses, Vertex Agent Runtime
// POST /api/reasoning_engine and /api/stream_reasoning_engine). The server
// opens it with the relay, forwarding the caller's identity token unread; the
// relay verifies the token and names the session the turn runs under. The turn
// then runs here, through the same server.Handler the channel serves, so every
// tool, memory call and history read it makes is governed by the relay for
// that session. The reply goes back to the caller and to the relay, which
// records it.
//
// The runtime never decides who the caller is. A caller the relay refuses gets
// the relay's status (401 or 403) and nothing runs; with the channel down, or
// a relay that does not serve the front door, the answer is 503.
package frontdoor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-logr/logr"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// Where the caller's identity token is read from, first present wins. The
// platforms differ in which headers reach the container: AgentCore passes
// Authorization when its JWT authorizer allowlists it, and custom headers
// under its prefix; Foundry strips Authorization and passes x-client-*;
// Vertex passes no header, so its token rides the request body.
const (
	HeaderAgentCoreIdentity = "X-Amzn-Bedrock-AgentCore-Runtime-Custom-Omnia-Identity"
	HeaderFoundryIdentity   = "X-Client-Omnia-Identity"
	HeaderIdentity          = "X-Omnia-Identity"
	// HeaderAgentCoreSession is AgentCore's runtime session id.
	HeaderAgentCoreSession = "X-Amzn-Bedrock-AgentCore-Runtime-Session-Id"
)

// Platforms, as the front door names them to the relay.
const (
	PlatformAgentCore = "agentcore"
	PlatformVertex    = "vertex"
	PlatformFoundry   = "foundry"
)

// KickField is the body field of the relay's session start (the "kick"): an
// invocation that only starts the container so that it dials home. It runs no
// turn.
const KickField = "omnia_dial_home"

// maxBody bounds a request body.
const maxBody = 4 << 20

// TurnOpener is the relay side of a front-door turn. *dialhome.Governance is
// one.
type TurnOpener interface {
	BeginFrontDoorTurn(ctx context.Context, req dialhome.FrontDoorBeginRequest) (dialhome.FrontDoorBeginResponse, error)
	EndFrontDoorTurn(ctx context.Context, turnID string, req dialhome.FrontDoorEndRequest) error
}

// Server serves the front door.
type Server struct {
	// Handler runs the turn: the same handler the channel serves.
	Handler server.Handler
	// Relay opens and ends each turn.
	Relay TurnOpener
	// Platform names the platform when a route does not (Foundry's and
	// AgentCore's /invocations are the same route); "" infers it.
	Platform string
	// SessionFallback is the platform session when a request names none
	// (Foundry's FOUNDRY_AGENT_SESSION_ID: one container per session).
	SessionFallback string
	Log             logr.Logger

	inflight atomic.Int64
	kicked   atomic.Bool
	once     sync.Once
}

// Busy reports whether a turn is running here now.
func (s *Server) Busy() bool { return s.inflight.Load() > 0 }

// Kicked reports whether the relay started this container (a session start
// arrived on it). Such a container holds the channel for Omnia's own turns.
func (s *Server) Kicked() bool { return s.kicked.Load() }

// Register adds the front-door routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	s.once.Do(func() {
		if s.Log.GetSink() == nil {
			s.Log = logr.Discard()
		}
	})
	mux.HandleFunc("POST /invocations", s.serveInvocations)
	mux.HandleFunc("POST /responses", s.serveResponses)
	mux.HandleFunc("POST /api/reasoning_engine", s.serveVertex(false))
	mux.HandleFunc("POST /api/stream_reasoning_engine", s.serveVertex(true))
}

// turn is one front-door turn as a route parsed it.
type turn struct {
	platform, session, token, text string
}

// result is a finished turn.
type result struct {
	sessionID string
	reply     string
	usage     *server.Usage
}

// errTurn is a turn that failed after it was opened, or before.
type errTurn struct {
	status int
	msg    string
}

func (e *errTurn) Error() string { return e.msg }

// run opens the turn with the relay, runs it, ends it, and returns the reply.
// chunk, when set, is given each fragment as it streams.
func (s *Server) run(ctx context.Context, t turn, chunk func(string)) (result, error) {
	if strings.TrimSpace(t.text) == "" {
		return result{}, &errTurn{status: http.StatusBadRequest, msg: "the request has no message"}
	}
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	begin, err := s.Relay.BeginFrontDoorTurn(ctx, dialhome.FrontDoorBeginRequest{
		Platform: t.platform, PlatformSessionID: t.session, IdentityToken: t.token,
		Message: dialhome.FrontDoorMessage{Text: t.text},
	})
	if err != nil {
		return result{}, refusal(err)
	}
	em := &collector{chunk: chunk}
	turnErr := s.Handler.Converse(ctx, server.Turn{
		SessionID: begin.SessionID, Content: t.text,
		Identity: server.Identity{SessionID: begin.SessionID, UserID: begin.UserID, Origin: begin.Origin,
			Claims: begin.Claims},
	}, em)
	reply, usage := em.result()
	end := dialhome.FrontDoorEndRequest{Reply: reply}
	if usage != nil {
		end.Usage = &dialhome.FrontDoorUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CostUSD: float64(usage.CostUSD)}
	}
	if turnErr != nil {
		end = dialhome.FrontDoorEndRequest{Error: turnErr.Error()}
	}
	// Detached: the caller hanging up does not leave the relay holding the turn.
	if err := s.Relay.EndFrontDoorTurn(context.WithoutCancel(ctx), begin.TurnID, end); err != nil {
		s.Log.Info("front-door turn end not recorded", "err", err.Error())
	}
	if turnErr != nil {
		s.Log.Error(turnErr, "front-door turn failed", "sessionID", begin.SessionID)
		return result{}, &errTurn{status: http.StatusInternalServerError, msg: "the turn failed"}
	}
	return result{sessionID: begin.SessionID, reply: reply, usage: usage}, nil
}

// refusal maps the relay's answer to the caller's: its own 4xx (the caller
// refused), else 503 (no governed turn is possible now).
func refusal(err error) error {
	var re *dialhome.RelayError
	if errors.As(err, &re) && re.Status >= 400 && re.Status < 500 {
		return &errTurn{status: re.Status, msg: re.Message}
	}
	return &errTurn{status: http.StatusServiceUnavailable, msg: "the agent cannot take a turn right now"}
}

// collector is the Emitter of a front-door turn: no client is on the other
// end of a platform invocation, so it answers no client tool call.
type collector struct {
	mu    sync.Mutex
	chunk func(string)
	text  strings.Builder
	done  *server.Done
}

func (c *collector) Chunk(text string) error {
	if text == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text.WriteString(text)
	if c.chunk != nil {
		c.chunk(text)
	}
	return nil
}

func (c *collector) ToolCall(server.ClientToolCall) (server.ClientToolResult, error) {
	return server.ClientToolResult{}, errors.New("frontdoor: no client answers a client tool call")
}

func (c *collector) Done(d server.Done) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = &d
	return nil
}

func (c *collector) result() (string, *server.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done != nil && c.done.Final != "" {
		return c.done.Final, c.done.Usage
	}
	var u *server.Usage
	if c.done != nil {
		u = c.done.Usage
	}
	return c.text.String(), u
}

// identityToken is the caller's token from the headers, first present wins.
func identityToken(h http.Header) string {
	if v := h.Get("Authorization"); v != "" {
		if tok, ok := dialhome.BearerKey(v); ok {
			return tok
		}
	}
	for _, name := range []string{HeaderAgentCoreIdentity, HeaderFoundryIdentity, HeaderIdentity} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
		}
	}
	return ""
}

// readJSON reads a JSON object body into a map; a body that is not JSON is
// returned as text (raw is set).
func readJSON(r *http.Request) (map[string]any, string, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		return nil, "", err
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil, string(b), nil
	}
	return m, "", nil
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers a failed turn with its status and a JSON error.
func writeErr(w http.ResponseWriter, err error) {
	var e *errTurn
	if !errors.As(err, &e) {
		e = &errTurn{status: http.StatusInternalServerError, msg: "the turn failed"}
	}
	writeJSON(w, e.status, map[string]string{"status": "error", "error": e.msg})
}
