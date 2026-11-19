// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// StreamOpener opens a stream of one kind to the relay. *Client is one.
type StreamOpener interface {
	OpenStream(kind StreamKind) (net.Conn, error)
}

// Default bounds on governance requests.
const (
	// DefaultPolicyTimeout bounds a policy decision. The relay's own call to
	// the decision service is shorter; this leaves it the channel's latency.
	DefaultPolicyTimeout = 5 * time.Second
	// defaultGovernanceTimeout bounds a tool call record or a memory request.
	defaultGovernanceTimeout = 15 * time.Second
	// maxGovernanceBody bounds a governance response body.
	maxGovernanceBody = 4 << 20
)

// ErrNoDecision wraps every way a policy decision can fail to arrive: the
// channel down, a relay that does not serve KindPolicy, a timeout, an answer
// that is not a decision. A caller treats it as a refusal.
var ErrNoDecision = errors.New("dial-home: no policy decision")

// Governance is the runtime's client for the governance streams: policy
// decisions, tool call records and the session's memory. Its methods are safe
// for concurrent use.
type Governance struct {
	policy, toolCalls, memory *http.Client
	// PolicyTimeout bounds Decide; DefaultPolicyTimeout when zero.
	PolicyTimeout time.Duration
}

// NewGovernance returns a Governance over c's streams.
func NewGovernance(c StreamOpener) *Governance {
	return &Governance{
		policy:    streamHTTPClient(c, KindPolicy),
		toolCalls: streamHTTPClient(c, KindToolCalls),
		memory:    streamHTTPClient(c, KindMemoryStore),
	}
}

// streamHTTPClient is an HTTP client whose every request rides a new stream
// of kind: no keep-alive, so a request never lands on a stream of a session
// that has since dropped.
func streamHTTPClient(c StreamOpener, kind StreamKind) *http.Client {
	return &http.Client{
		// A redirect is not an answer: it fails the request, and a policy
		// request that fails is a refusal.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return c.OpenStream(kind)
			},
			DisableKeepAlives: true,
		},
	}
}

// Decide asks the relay whether a tool call may run. Any failure to get an
// answer is an error wrapping ErrNoDecision, and the tool must not run.
func (g *Governance) Decide(ctx context.Context, req PolicyRequest) (PolicyDecision, error) {
	timeout := g.PolicyTimeout
	if timeout <= 0 {
		timeout = DefaultPolicyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d PolicyDecision
	if err := post(ctx, g.policy, PolicyDecisionPath, req, &d); err != nil {
		return PolicyDecision{}, fmt.Errorf("%w: %w", ErrNoDecision, err)
	}
	return d, nil
}

// RecordToolCall records one phase of a tool call on its session.
func (g *Governance) RecordToolCall(ctx context.Context, rec ToolCallRecord) error {
	return g.call(ctx, g.toolCalls, ToolCallsPath, rec, nil)
}

// MemoryScope says what memory a session has.
func (g *Governance) MemoryScope(ctx context.Context, sessionID string) (MemoryScope, error) {
	var s MemoryScope
	err := g.call(ctx, g.memory, MemoryScopePath, MemoryScopeRequest{SessionID: sessionID}, &s)
	return s, err
}

// Remember saves a memory for the session's user and returns its id.
func (g *Governance) Remember(ctx context.Context, sessionID string, m MemoryRecord) (string, error) {
	var resp MemoryRememberResponse
	err := g.call(ctx, g.memory, MemoryRememberPath, MemoryRememberRequest{SessionID: sessionID, Memory: m}, &resp)
	return resp.ID, err
}

// Recall searches the session's memory.
func (g *Governance) Recall(ctx context.Context, req MemoryRecallRequest) ([]MemoryRecord, error) {
	var l MemoryList
	err := g.call(ctx, g.memory, MemoryRecallPath, req, &l)
	return l.Memories, err
}

// ListMemories lists the session's memory.
func (g *Governance) ListMemories(ctx context.Context, req MemoryListRequest) ([]MemoryRecord, error) {
	var l MemoryList
	err := g.call(ctx, g.memory, MemoryListPath, req, &l)
	return l.Memories, err
}

// Forget forgets one of the session user's memories.
func (g *Governance) Forget(ctx context.Context, sessionID, id string) error {
	return g.call(ctx, g.memory, MemoryForgetPath, MemoryForgetRequest{SessionID: sessionID, ID: id}, nil)
}

func (g *Governance) call(ctx context.Context, hc *http.Client, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, defaultGovernanceTimeout)
	defer cancel()
	return post(ctx, hc, path, in, out)
}

// RelayError is a governance request the relay answered with an error. Its
// message is the relay's, worded for whoever reads the result.
type RelayError struct {
	Status  int
	Message string
}

func (e *RelayError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("dial-home: relay answered HTTP %d", e.Status)
	}
	return e.Message
}

// post sends in as JSON to path and decodes a 2xx answer into out (when
// out is set). Any other status is a *RelayError.
func post(ctx context.Context, hc *http.Client, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://dial-home"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	r := io.LimitReader(resp.Body, maxGovernanceBody)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e ErrorBody
		_ = json.NewDecoder(r).Decode(&e)
		return &RelayError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(r).Decode(out); err != nil {
		return fmt.Errorf("dial-home: decode %s: %w", path, err)
	}
	return nil
}
