// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"fmt"
	"net/url"
)

// The front-door stream (KindFrontDoor): a turn that reached the runtime
// through its platform's own invoke API (AgentCore InvokeAgentRuntime, Vertex
// Agent Runtime query, a Foundry hosted agent's invocations or responses
// protocol) rather than over the channel.
//
// The runtime opens the turn before running it, forwarding the caller's
// identity token as the platform passed it, unread. The relay verifies the
// token itself, decides the session the turn runs under, and holds that
// session open, so the governance streams answer for it while the turn runs.
// The runtime never says who the caller is: it runs the turn under the session
// the relay names, with the identity the relay returns.
const (
	// FrontDoorTurnsPath (KindFrontDoor, POST FrontDoorBeginRequest →
	// FrontDoorBeginResponse) opens a turn.
	FrontDoorTurnsPath = "/v1/front-door/turns"
)

// FrontDoorEndPath (KindFrontDoor, POST FrontDoorEndRequest → 204) ends the
// turn turnID.
func FrontDoorEndPath(turnID string) string {
	return FrontDoorTurnsPath + "/" + url.PathEscape(turnID) + "/end"
}

// FrontDoorBeginRequest opens a front-door turn.
type FrontDoorBeginRequest struct {
	// Platform is the platform the turn arrived on ("agentcore", "vertex",
	// "foundry"); informational, the relay names the session by its own.
	Platform string `json:"platform,omitempty"`
	// PlatformSessionID is the platform's session for the caller's
	// conversation; empty starts a new conversation.
	PlatformSessionID string `json:"platform_session_id,omitempty"`
	// IdentityToken is the caller's identity token, as the platform passed
	// it; empty for a caller who sent none.
	IdentityToken string `json:"identity_token,omitempty"`
	// Message is the caller's message.
	Message FrontDoorMessage `json:"message"`
}

// FrontDoorMessage is a turn's text.
type FrontDoorMessage struct {
	Text string `json:"text"`
}

// FrontDoorBeginResponse is the relay's answer to an opened turn: the session
// to run it under, and who it verified the caller to be.
type FrontDoorBeginResponse struct {
	SessionID string            `json:"session_id"`
	TurnID    string            `json:"turn_id"`
	UserID    string            `json:"user_id,omitempty"`
	Origin    string            `json:"origin"`
	Claims    map[string]string `json:"claims,omitempty"`
}

// FrontDoorEndRequest ends a turn.
type FrontDoorEndRequest struct {
	Reply string          `json:"reply"`
	Usage *FrontDoorUsage `json:"usage,omitempty"`
	// Error is set when the turn failed; Reply is then empty.
	Error string `json:"error,omitempty"`
}

// FrontDoorUsage is a turn's accounting.
type FrontDoorUsage struct {
	InputTokens  int32   `json:"input_tokens,omitempty"`
	OutputTokens int32   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// BeginFrontDoorTurn opens a front-door turn. A *RelayError is the relay
// refusing the caller (401: the token was not accepted, or none was given and
// the agent requires one; 403: the agent has no front door); any other error
// is the relay not answering, and the turn must not run.
func (g *Governance) BeginFrontDoorTurn(ctx context.Context, req FrontDoorBeginRequest) (FrontDoorBeginResponse, error) {
	var resp FrontDoorBeginResponse
	if err := g.call(ctx, g.frontDoor, FrontDoorTurnsPath, req, &resp); err != nil {
		return FrontDoorBeginResponse{}, fmt.Errorf("dial-home: front door: %w", err)
	}
	return resp, nil
}

// EndFrontDoorTurn ends a front-door turn: the relay records the reply and
// releases the session.
func (g *Governance) EndFrontDoorTurn(ctx context.Context, turnID string, req FrontDoorEndRequest) error {
	if err := g.call(ctx, g.frontDoor, FrontDoorEndPath(turnID), req, nil); err != nil {
		return fmt.Errorf("dial-home: front door: %w", err)
	}
	return nil
}
