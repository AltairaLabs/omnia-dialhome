// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/events"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/go-logr/logr"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// Recorder records a tool call the pack ran in this process on its session.
// *dialhome.Governance is one.
type Recorder interface {
	RecordToolCall(ctx context.Context, rec dialhome.ToolCallRecord) error
}

// recordTimeout bounds one record. Recording never holds a turn up: it runs
// off the conversation's event bus, after the fact.
const recordTimeout = 15 * time.Second

// toolStatusFailed is PromptKit's completed-with-failure status. A tool that
// returns a failure is reported on tool.call.completed, not tool.call.failed.
const toolStatusFailed = "failed"

// toolStatusPending is a client tool handed to the client: not an outcome.
const toolStatusPending = "pending"

// recordToolCalls subscribes to conv's tool call events and records each one
// through rec, except the client tools (the client's answer is its own
// record). A call the policy refused raises no event: the relay that refused
// it records it.
func recordToolCalls(conv *sdk.Conversation, sessionID string, rec Recorder, client map[string]bool, log logr.Logger) {
	bus := conv.EventBus()
	if bus == nil {
		log.Info("tool calls not recorded: the conversation has no event bus", "sessionID", sessionID)
		return
	}
	// One subscription for all three kinds, so a call's phases are recorded
	// in the order they happened.
	unsubscribe := bus.SubscribeAll(func(e *events.Event) {
		r, ok := toolCallRecord(e, sessionID)
		if !ok || client[r.Tool] {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		defer cancel()
		if err := rec.RecordToolCall(ctx, r); err != nil {
			log.Info("tool call not recorded", "sessionID", sessionID, "tool", r.Tool,
				"phase", r.Phase, "err", err.Error())
		}
	})
	conv.OwnSubscription(unsubscribe)
}

// toolCallRecord converts a tool call event, or reports false for any other.
func toolCallRecord(e *events.Event, sessionID string) (dialhome.ToolCallRecord, bool) {
	var phase string
	switch e.Type {
	case events.EventToolCallStarted:
		phase = dialhome.ToolCallStarted
	case events.EventToolCallCompleted:
		phase = dialhome.ToolCallCompleted
	case events.EventToolCallFailed:
		phase = dialhome.ToolCallFailed
	default:
		return dialhome.ToolCallRecord{}, false
	}
	d := toolCallData(e.Data)
	if d == nil || d.Status == toolStatusPending {
		return dialhome.ToolCallRecord{}, false
	}
	r := dialhome.ToolCallRecord{
		SessionID: sessionID, CallID: d.CallID, Tool: d.ToolName, Phase: phase,
		Round: d.Round, Time: e.Timestamp,
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	switch phase {
	case dialhome.ToolCallStarted:
		if len(d.Args) > 0 {
			r.Arguments, _ = json.Marshal(d.Args)
		}
	case dialhome.ToolCallCompleted:
		r.DurationMs = d.Duration.Milliseconds()
		text := partsText(d)
		if d.Status == toolStatusFailed {
			r.Phase, r.Error = dialhome.ToolCallFailed, text
		} else {
			r.Result = text
		}
	case dialhome.ToolCallFailed:
		r.DurationMs = d.Duration.Milliseconds()
		r.Error = "unknown error"
		if d.Error != nil {
			r.Error = d.Error.Error()
		}
	}
	return r, true
}

func toolCallData(data events.EventData) *events.ToolCallEventData {
	switch d := data.(type) {
	case *events.ToolCallEventData:
		return d
	case events.ToolCallEventData:
		return &d
	default:
		return nil
	}
}

// partsText is a result's text parts, joined.
func partsText(d *events.ToolCallEventData) string {
	var b strings.Builder
	for _, p := range d.Parts {
		if p.Text != nil {
			b.WriteString(*p.Text)
		}
	}
	return b.String()
}
