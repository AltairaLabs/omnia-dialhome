// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"fmt"
	"time"
)

// The history stream (KindHistory): a session's conversation so far, as Omnia
// recorded it. A runtime that keeps conversations in memory loses them when
// its process or container is replaced; it reads the conversation back here
// before the session's next turn, so the conversation continues with its
// earlier turns.
//
// As on the governance streams, the request names the session and nothing
// else. The relay answers only for a session with a call open through it
// right now (a turn in flight, or the facade asking whether the session can
// be resumed), and only from that session's own record.
const (
	// HistoryPath (KindHistory, POST HistoryRequest → History) reads one
	// session's conversation.
	HistoryPath = "/v1/history"
)

// DefaultHistoryTimeout bounds a history read.
const DefaultHistoryTimeout = 10 * time.Second

// History roles.
const (
	HistoryRoleUser      = "user"
	HistoryRoleAssistant = "assistant"
)

// HistoryRequest asks for a session's conversation.
type HistoryRequest struct {
	SessionID string `json:"session_id"`
}

// HistoryMessage is one message of a conversation: the user's text or the
// agent's reply. Tool calls, tool results and media are not carried.
type HistoryMessage struct {
	// Role is HistoryRoleUser or HistoryRoleAssistant.
	Role    string `json:"role"`
	Content string `json:"content"`
}

// History is a session's conversation, oldest first, through the agent's
// last reply: a user message with no reply yet (the turn in flight) is not in
// it. Empty for a session Omnia has not recorded.
type History struct {
	Messages []HistoryMessage `json:"messages"`
	// Truncated is set when the relay left out the oldest messages to keep
	// the answer within its size bound.
	Truncated bool `json:"truncated,omitempty"`
}

// History reads a session's conversation as Omnia recorded it. An error is
// the relay not answering (the channel down, a relay that does not serve
// KindHistory) or refusing (*RelayError, for a session it is not carrying).
func (g *Governance) History(ctx context.Context, sessionID string) (History, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultHistoryTimeout)
	defer cancel()
	var h History
	if err := post(ctx, g.history, HistoryPath, HistoryRequest{SessionID: sessionID}, &h); err != nil {
		return History{}, fmt.Errorf("dial-home: history: %w", err)
	}
	return h, nil
}
