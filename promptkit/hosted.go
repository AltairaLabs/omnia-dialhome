// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	pktools "github.com/AltairaLabs/PromptKit/runtime/v2/tools"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/go-logr/logr"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// HostedToolService lists the tools Omnia hosts for the agent and runs them
// in the cluster. *dialhome.Governance is one.
type HostedToolService interface {
	HostedTools(ctx context.Context) ([]dialhome.HostedTool, error)
	CallTool(ctx context.Context, call dialhome.HostedToolCall) (dialhome.HostedToolResult, error)
}

// hostedMode is the PromptKit executor mode of a tool Omnia runs.
const hostedMode = "omnia-hosted"

// hostedRoutes is the set of a conversation's tools that Omnia runs. It is
// filled when the conversation opens, before its first turn, and read by the
// policy gate and the tool call recorder: Omnia decides and records those
// calls itself, so they are neither asked about nor recorded here.
type hostedRoutes struct {
	mu    sync.RWMutex
	names map[string]bool
}

func (r *hostedRoutes) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names == nil {
		r.names = map[string]bool{}
	}
	r.names[name] = true
}

func (r *hostedRoutes) has(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.names[name]
}

// hostedTools is the list of tools Omnia hosts, or none when it cannot be
// had: a runtime offers no hosted tool it could not list.
func (h *Handler) hostedTools(ctx context.Context, id string) []dialhome.HostedTool {
	if h.cfg.Tools == nil {
		return nil
	}
	list, err := h.cfg.Tools.HostedTools(ctx)
	if err != nil {
		h.cfg.Log.Info("conversation opened without Omnia-hosted tools: they could not be listed",
			"sessionID", id, "err", err.Error())
		return nil
	}
	return list
}

// installHosted registers the hosted tools on conv, routed to Omnia, and
// records which they are in routes.
//
// A tool the pack itself defines is the pack's: its OnTool handler runs it,
// and the hosted tool of the same name is not offered, so a local handler is
// never displaced. A client tool is the client's. A hosted tool reaches the
// model only if the prompt's tools list names it, as for any tool; the pack
// need not define it, since Omnia supplies its description and schema.
func installHosted(
	conv *sdk.Conversation, list []dialhome.HostedTool, svc HostedToolService, sessionID string,
	routes *hostedRoutes, client map[string]bool, log logr.Logger,
) {
	if len(list) == 0 {
		return
	}
	reg := conv.ToolRegistry()
	reg.RegisterExecutor(&hostedExecutor{svc: svc, sessionID: sessionID})
	for _, t := range list {
		if reg.Get(t.Name) != nil || client[t.Name] {
			log.V(1).Info("Omnia-hosted tool not offered: the pack has a tool of that name",
				"sessionID", sessionID, "tool", t.Name)
			continue
		}
		desc := &pktools.ToolDescriptor{
			Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, OutputSchema: t.OutputSchema,
			Mode: hostedMode,
		}
		if err := reg.Register(desc); err != nil {
			log.Info("Omnia-hosted tool not registered", "sessionID", sessionID, "tool", t.Name, "err", err.Error())
			continue
		}
		routes.add(t.Name)
	}
}

// hostedExecutor runs a conversation's hosted tools over the tools stream.
type hostedExecutor struct {
	svc       HostedToolService
	sessionID string
}

var _ pktools.Executor = (*hostedExecutor)(nil)

// Name implements tools.Executor.
func (e *hostedExecutor) Name() string { return hostedMode }

// Execute implements tools.Executor: Omnia runs the call, and its result, or
// why it failed or was refused, is what the model sees.
func (e *hostedExecutor) Execute(ctx context.Context, desc *pktools.ToolDescriptor, args json.RawMessage) (json.RawMessage, error) {
	res, err := e.svc.CallTool(ctx, dialhome.HostedToolCall{
		SessionID: e.sessionID, CallID: pktools.CallIDFromContext(ctx), Tool: desc.Name, Arguments: args,
	})
	if err != nil {
		return nil, errors.New(noResultReason(err))
	}
	if res.Error != "" {
		return nil, errors.New(res.Error)
	}
	if len(res.Result) == 0 {
		return json.RawMessage("null"), nil
	}
	return res.Result, nil
}

// noResultReason is what the model is told when Omnia gave no result: why in
// a word, never the transport's internals. A call whose answer was lost may
// still have run in the cluster, so it promises nothing either way.
func noResultReason(err error) string {
	var re *dialhome.RelayError
	switch {
	case errors.As(err, &re) && re.Message != "":
		return "the tool did not run: " + re.Message
	case errors.Is(err, dialhome.ErrNotConnected):
		return "the tool did not run: the connection to Omnia is down"
	default:
		return "no result came back from Omnia for this tool call"
	}
}
