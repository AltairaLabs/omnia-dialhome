// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"maps"
	"sync"

	"github.com/AltairaLabs/PromptKit/runtime/v2/tools"
)

// injectedHeaders keeps, for each call of a conversation's current turn, the
// headers the tool policy added to it, until the turn ends. One per
// conversation.
type injectedHeaders struct {
	mu     sync.Mutex
	byCall map[string]map[string]string
}

func (h *injectedHeaders) put(callID string, headers map[string]string) {
	if h == nil || callID == "" || len(headers) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byCall == nil {
		h.byCall = map[string]map[string]string{}
	}
	h.byCall[callID] = maps.Clone(headers)
}

func (h *injectedHeaders) get(callID string) map[string]string {
	if h == nil || callID == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.byCall[callID])
}

// clear forgets every call's headers: nothing of a turn outlives it.
func (h *injectedHeaders) clear() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.byCall = nil
}

type injectedHeadersKey struct{}

func withInjectedHeaders(ctx context.Context, h *injectedHeaders) context.Context {
	return context.WithValue(ctx, injectedHeadersKey{}, h)
}

// InjectedHeaders returns the headers Omnia's tool policy added to the tool
// call being run, for the handler to send to the tool's backend, or nil when
// there are none. Call it in a context-aware tool handler (OnToolCtx,
// OnToolCall) with the context it is given.
//
// Omnia sends them only with an allow, and only for a tool its operator has
// opted in. They are usually credentials: send them where the policy meant
// them to go, and never log them, return them to the model or keep them past
// the call. They are forgotten when the turn ends. A relay that predates them
// sends none.
func InjectedHeaders(ctx context.Context) map[string]string {
	h, _ := ctx.Value(injectedHeadersKey{}).(*injectedHeaders)
	return h.get(tools.CallIDFromContext(ctx))
}
