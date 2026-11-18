// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package server serves omnia.runtime.v1 for a Handler: the hello-first
// Converse stream, client tool round-trips, heartbeats, Health and the
// optional RPCs. Serve it on a dialhome.Client to be a hosted runtime, or on
// any net.Listener.
package server

import "context"

// Capability names a runtime advertises in Health and in each stream's
// RuntimeHello. An open set: a consumer ignores a name it does not know.
const (
	// CapabilityClientTools: the runtime sends client tool calls on the
	// Converse stream (ToolCall with execution CLIENT) and reads the
	// client's ClientToolResult back on the same stream.
	CapabilityClientTools = "client_tools"
)

// Handler is what a runtime author implements. The package owns the wire
// protocol; the handler owns the turn.
type Handler interface {
	// Capabilities is the honest capability set. It is advertised in both
	// Health and every RuntimeHello, which must agree, so both come from here.
	Capabilities() []string

	// Converse handles one turn, emitting its output through emit. It ends
	// the turn with emit.Done; when it returns nil without doing so, an empty
	// Done is sent for it. An error ends the turn with an INTERNAL_ERROR
	// frame.
	Converse(ctx context.Context, turn Turn, emit Emitter) error
}

// ConversationProber is an optional Handler extension answering
// HasConversation. Without it every session is reported unavailable.
type ConversationProber interface {
	HasConversation(ctx context.Context, sessionID string) ResumeState
}

// ConversationEnder is an optional Handler extension told when a
// conversation has ended (EndConversation). Best-effort: a facade that never
// sends it leaves the runtime to its own idle handling.
type ConversationEnder interface {
	EndConversation(ctx context.Context, sessionID string)
}

// ResumeState is a HasConversation answer.
type ResumeState int

// Resume states.
const (
	// ResumeUnavailable: unknown (no store, or it could not be read). Not an
	// expiry.
	ResumeUnavailable ResumeState = iota
	// ResumeResumable: the session's context is held.
	ResumeResumable
	// ResumeNotFound: definitively no context for the session.
	ResumeNotFound
)

// Emitter is the output surface of one turn. Its methods are safe to call
// from several goroutines.
type Emitter interface {
	// Chunk streams a fragment of the reply. An empty string is dropped.
	Chunk(text string) error
	// ToolCall sends a client tool call and blocks for the client's result.
	// Call it only when Turn.ClientToolsAvailable: with no client nothing
	// answers it.
	ToolCall(call ClientToolCall) (ClientToolResult, error)
	// Done ends the turn.
	Done(d Done) error
}

// Turn is one inbound conversational turn.
type Turn struct {
	SessionID string
	// Content is the turn's text; Parts take precedence when set.
	Content  string
	Parts    []ContentPart
	Metadata map[string]string
	Identity Identity
	// ClientToolsAvailable is true when the serving surface answers client
	// tool calls. When false nobody can answer one, so a handler must not
	// call Emitter.ToolCall.
	ClientToolsAvailable bool
}

// ContentPart is one part of a multimodal turn or reply.
type ContentPart struct {
	Type       string // "text" | "image" | "audio" | "video"
	Text       string
	Data       string // base64 media
	URL        string
	MimeType   string
	StorageRef string
}

// ClientToolCall asks the client to run a tool.
type ClientToolCall struct {
	ID             string
	Name           string
	ArgumentsJSON  string
	ConsentMessage string
	Categories     []string
}

// ClientToolResult is the client's answer to a ClientToolCall.
type ClientToolResult struct {
	CallID          string
	ResultJSON      string
	IsRejected      bool
	RejectionReason string
	// IsDenied marks a rejection as the client declining (or never
	// answering) rather than its tool failing.
	IsDenied bool
}

// Usage is a turn's token and cost accounting.
type Usage struct {
	InputTokens  int32
	OutputTokens int32
	CostUSD      float32
}

// Done is the end of a turn.
type Done struct {
	// Final is the complete reply text; Parts take precedence when set.
	Final string
	Usage *Usage
	Parts []ContentPart
	// FinishReason is why the last model call stopped ("stop",
	// "max_output_tokens", "tool_use", "safety", "refusal"), if known.
	FinishReason string
}
