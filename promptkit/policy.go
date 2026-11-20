// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"errors"

	"github.com/AltairaLabs/PromptKit/runtime/v2/hooks"
	"github.com/AltairaLabs/PromptKit/runtime/v2/memory"
	"github.com/go-logr/logr"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// Decider decides whether a tool call may run. *dialhome.Governance is one.
type Decider interface {
	Decide(ctx context.Context, req dialhome.PolicyRequest) (dialhome.PolicyDecision, error)
}

// relayMemoryTools are PromptKit's memory tools. With Config.Memory set they
// are not put to the tool policy: they reach memory only through the relay,
// which scopes and refuses them itself. Any other tool, whatever its name,
// is put to the policy.
var relayMemoryTools = []string{
	memory.RecallToolName, memory.RememberToolName, memory.ListToolName, memory.ForgetToolName,
}

// policyGate is a PromptKit tool hook that asks the Decider before every tool
// the pack runs in this process, and lets it run only on an allow. One per
// conversation, since it names the conversation's session.
type policyGate struct {
	decider   Decider
	sessionID string
	registry  string
	exempt    map[string]bool
	// hosted are the tools Omnia runs, and decides itself in doing so.
	hosted *hostedRoutes
	log    logr.Logger
}

var _ hooks.ToolHook = (*policyGate)(nil)

// Name implements hooks.ToolHook.
func (g *policyGate) Name() string { return "omnia-dialhome-policy" }

// BeforeExecution implements hooks.ToolHook. A refusal reaches the model as
// the tool's error, carrying the policy's message. No answer is a refusal:
// the channel down, a timeout, a relay that cannot decide.
func (g *policyGate) BeforeExecution(ctx context.Context, req hooks.ToolRequest) hooks.Decision {
	if g.exempt[req.Name] || g.hosted.has(req.Name) {
		return hooks.Allow
	}
	d, err := g.decider.Decide(ctx, dialhome.PolicyRequest{
		SessionID: g.sessionID, CallID: req.CallID, Tool: req.Name,
		Registry: g.registry, Arguments: req.Args,
	})
	if err != nil {
		g.log.Info("tool refused: no policy decision", "sessionID", g.sessionID, "tool", req.Name, "err", err.Error())
		return hooks.Deny(noDecisionReason(err))
	}
	if !d.Allow {
		g.log.V(1).Info("tool refused by policy", "sessionID", g.sessionID, "tool", req.Name,
			"deniedBy", d.DeniedBy, "policy", d.Policy)
		reason := d.Reason
		if reason == "" {
			reason = "refused by policy"
		}
		return hooks.DenyWithMetadata(reason, map[string]any{"denied_by": d.DeniedBy, "policy": d.Policy})
	}
	return hooks.Allow
}

// AfterExecution implements hooks.ToolHook.
func (g *policyGate) AfterExecution(context.Context, hooks.ToolRequest, hooks.ToolResponse) hooks.Decision {
	return hooks.Allow
}

// noDecisionReason is what the model is told when no decision came: that the
// tool did not run, and why in a word, never the transport's internals.
func noDecisionReason(err error) string {
	var re *dialhome.RelayError
	switch {
	case errors.As(err, &re) && re.Message != "":
		return "the tool did not run: no policy decision could be obtained (" + re.Message + ")"
	case errors.Is(err, dialhome.ErrNotConnected):
		return "the tool did not run: the connection to Omnia is down, and a tool runs only when Omnia allows it"
	default:
		// A timeout, an answer that was not a decision, or a relay that does
		// not decide tool calls at all: which one is not knowable here, and
		// the last never recovers on its own, so promise nothing.
		return "the tool did not run: Omnia gave no policy decision for it, and a tool runs only when Omnia allows it"
	}
}
