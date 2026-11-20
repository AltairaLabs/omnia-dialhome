// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"errors"
	"net/http"

	"github.com/AltairaLabs/PromptKit/runtime/v2/statestore"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/AltairaLabs/PromptKit/sdk/v2"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// HistorySource is where a session's conversation is kept beyond this
// process: the copy the relay keeps of each conversation it carries.
// *dialhome.Governance is one.
type HistorySource interface {
	History(ctx context.Context, sessionID string) (dialhome.History, error)
}

// history reads the session's conversation from the configured source, as
// PromptKit messages. Nil when there is no source, nothing was kept, or
// the source did not answer: the conversation then starts without its
// earlier turns, as it would have before there was a source.
func (h *Handler) history(ctx context.Context, id string) []types.Message {
	if h.cfg.History == nil {
		return nil
	}
	got, err := h.cfg.History.History(ctx, id)
	if err != nil {
		h.cfg.Log.Info("conversation history not read; opening without it", "sessionID", id, "err", err.Error())
		return nil
	}
	msgs := make([]types.Message, 0, len(got.Messages))
	for _, m := range got.Messages {
		if m.Content == "" || (m.Role != dialhome.HistoryRoleUser && m.Role != dialhome.HistoryRoleAssistant) {
			continue
		}
		msgs = append(msgs, types.Message{Role: m.Role, Content: m.Content})
	}
	if got.Truncated {
		h.cfg.Log.Info("conversation history truncated by the relay", "sessionID", id, "messageCount", len(msgs))
	}
	return msgs
}

// resumeOptions seeds a conversation's state store with its history, so the
// conversation PromptKit opens continues from it. The store is the
// conversation's own, as the one PromptKit makes when given none.
func resumeOptions(ctx context.Context, id string, history []types.Message) ([]sdk.Option, error) {
	store := statestore.NewMemoryStore()
	if err := store.Save(ctx, &statestore.ConversationState{ID: id, Messages: history}); err != nil {
		return nil, err
	}
	return []sdk.Option{sdk.WithStateStore(store)}, nil
}

// probeHistory answers HasConversation for a session this process does not
// hold: resumable when the relay kept a conversation for it, which a turn
// will read back. A relay that failed in its own right (a 5xx: its store
// unavailable) is unavailable, never a conversation gone;
// anything else, an older relay without the stream included, is not found,
// as it was before there was a source.
func (h *Handler) probeHistory(ctx context.Context, id string) server.ResumeState {
	if h.cfg.History == nil {
		return server.ResumeNotFound
	}
	got, err := h.cfg.History.History(ctx, id)
	var re *dialhome.RelayError
	switch {
	case errors.As(err, &re) && re.Status >= http.StatusInternalServerError:
		return server.ResumeUnavailable
	case err != nil:
		h.cfg.Log.V(1).Info("resume probe: history not read", "sessionID", id, "err", err.Error())
		return server.ResumeNotFound
	case len(got.Messages) == 0:
		return server.ResumeNotFound
	default:
		return server.ResumeResumable
	}
}
