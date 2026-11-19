// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"

	"github.com/AltairaLabs/PromptKit/sdk/v2"
)

// governanceOptions are the SDK options a conversation for session id gets
// from Policy and Memory.
func (h *Handler) governanceOptions(ctx context.Context, id string) []sdk.Option {
	var opts []sdk.Option
	if h.cfg.Policy != nil {
		opts = append(opts, sdk.WithToolHook(&policyGate{
			decider: h.cfg.Policy, sessionID: id, registry: h.cfg.PolicyRegistry,
			exempt: h.clientTools(), log: h.cfg.Log,
		}))
	}
	if h.cfg.Memory != nil {
		scope, err := h.cfg.Memory.MemoryScope(ctx, id)
		if err != nil {
			h.cfg.Log.Info("conversation opened without memory: its scope could not be read",
				"sessionID", id, "err", err.Error())
		} else {
			opts = append(opts, memoryOptions(scope, h.cfg.Memory, id)...)
		}
	}
	return opts
}

// clientTools is the set of ClientTools.
func (h *Handler) clientTools() map[string]bool {
	m := make(map[string]bool, len(h.cfg.ClientTools))
	for _, n := range h.cfg.ClientTools {
		m[n] = true
	}
	return m
}
