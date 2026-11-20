// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"encoding/json"
	"time"
)

// The governance streams: what a runtime asks of the relay about the tools it
// runs itself and the memory its conversations use. Each is HTTP/1.1 with a
// JSON body over a stream of its kind, one request per stream.
//
// Every request names the session it belongs to, and nothing else about who
// is calling. The relay answers from the identity it carried to the runtime
// for that session (the caller, their workspace, their consent), never from
// what the runtime says: a runtime cannot widen what a session may do by
// describing it differently. A session the relay is not carrying is refused.
const (
	// PolicyDecisionPath (KindPolicy, POST PolicyRequest → PolicyDecision)
	// decides one tool call before it runs.
	PolicyDecisionPath = "/v1/policy/decision"
	// ToolCallsPath (KindToolCalls, POST ToolCallRecord → 202) records one
	// phase of a tool call on its session.
	ToolCallsPath = "/v1/tool-calls"

	// MemoryScopePath (KindMemoryStore, POST MemoryScopeRequest → MemoryScope)
	// says what memory a session has.
	MemoryScopePath = "/v1/memory/scope"
	// MemoryRememberPath (POST MemoryRememberRequest → MemoryRememberResponse)
	// saves a memory.
	MemoryRememberPath = "/v1/memory/remember"
	// MemoryRecallPath (POST MemoryRecallRequest → MemoryList) searches.
	MemoryRecallPath = "/v1/memory/recall"
	// MemoryListPath (POST MemoryListRequest → MemoryList) lists.
	MemoryListPath = "/v1/memory/list"
	// MemoryForgetPath (POST MemoryForgetRequest → 204) forgets one memory.
	MemoryForgetPath = "/v1/memory/forget"
)

// PolicyRequest asks whether one tool call may run.
type PolicyRequest struct {
	// SessionID is the session whose turn made the call.
	SessionID string `json:"session_id"`
	// CallID is the model's id for the call.
	CallID string `json:"call_id"`
	// Tool is the tool's name.
	Tool string `json:"tool"`
	// Registry is the name the runtime's own tools were governed under. It
	// is deprecated: a current relay ignores it, and decides each tool under
	// the ToolRegistry that declares it as a remote handler, refusing a tool
	// no such handler declares. It is still sent, for an older relay that
	// reads it.
	Registry string `json:"registry,omitempty"`
	// Arguments are the call's arguments, as the model gave them.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// PolicyDecision is the relay's answer. Anything but Allow true means the
// tool does not run.
type PolicyDecision struct {
	Allow bool `json:"allow"`
	// Reason is the policy's message on a refusal, written for the model to
	// pass on.
	Reason string `json:"reason,omitempty"`
	// DeniedBy names the rule that refused the call.
	DeniedBy string `json:"denied_by,omitempty"`
	// Policy names the policy that decided, when the relay knows it.
	Policy string `json:"policy,omitempty"`
	// InjectedHeaders are headers the tool policy adds to this call, for the
	// runtime to send with it to the tool's backend. Only on an allow, and
	// only for a tool its operator has opted in: they are usually
	// credentials. An older relay never sends them.
	InjectedHeaders map[string]string `json:"injected_headers,omitempty"`
}

// Tool call phases, ToolCallRecord.Phase.
const (
	// ToolCallStarted: the call was allowed and is running. Arguments are set.
	ToolCallStarted = "started"
	// ToolCallCompleted: the tool returned. Result is set.
	ToolCallCompleted = "completed"
	// ToolCallFailed: the tool failed. Error is set.
	ToolCallFailed = "failed"
)

// ToolCallRecord is one phase of a tool call the runtime ran itself. A call
// the relay refused is recorded by the relay, so a runtime never sends one.
type ToolCallRecord struct {
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
	Tool      string `json:"tool"`
	// Phase is ToolCallStarted, ToolCallCompleted or ToolCallFailed.
	Phase     string          `json:"phase"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	// DurationMs is how long the tool ran, on completed and failed.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// Round is the 1-based tool-loop round of the turn that made the call.
	Round int       `json:"round,omitempty"`
	Time  time.Time `json:"time"`
}

// MemoryScopeRequest asks what memory a session has.
type MemoryScopeRequest struct {
	SessionID string `json:"session_id"`
}

// MemoryScope is what memory a session has. With Enabled false it has none.
type MemoryScope struct {
	Enabled bool `json:"enabled"`
	// User is true when the session's caller is a user memory may be kept
	// for. Without one only shared memory is read, and the memory tools are
	// not offered.
	User bool `json:"user"`
	// Tools offers the model the memory tools.
	Tools bool `json:"tools"`
	// Retrieval recalls memories into each turn's context.
	Retrieval bool `json:"retrieval"`
}

// MemoryRecord is one memory.
//
// On a remember, every field is the runtime's assertion, Metadata included
// (PromptKit puts the provenance and the consent category there). A relay
// treats them as claims to check or classify again, never as facts about the
// user or their consent.
type MemoryRecord struct {
	ID         string         `json:"id,omitempty"`
	Type       string         `json:"type,omitempty"`
	Content    string         `json:"content"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	CreatedAt  time.Time      `json:"created_at,omitzero"`
	// ExpiresAt, when set, is when the memory should be forgotten.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// MemoryRememberRequest saves a memory for the session's user.
type MemoryRememberRequest struct {
	SessionID string       `json:"session_id"`
	Memory    MemoryRecord `json:"memory"`
}

// MemoryRememberResponse names the saved memory.
type MemoryRememberResponse struct {
	ID string `json:"id"`
}

// MemoryRecallRequest searches the session's memory.
type MemoryRecallRequest struct {
	SessionID     string   `json:"session_id"`
	Query         string   `json:"query"`
	Types         []string `json:"types,omitempty"`
	Limit         int      `json:"limit,omitempty"`
	MinConfidence float64  `json:"min_confidence,omitempty"`
}

// MemoryListRequest lists the session's memory.
type MemoryListRequest struct {
	SessionID string   `json:"session_id"`
	Types     []string `json:"types,omitempty"`
	Limit     int      `json:"limit,omitempty"`
	Offset    int      `json:"offset,omitempty"`
}

// MemoryList is the memories a recall or list found.
type MemoryList struct {
	Memories []MemoryRecord `json:"memories"`
}

// MemoryForgetRequest forgets one of the session user's memories.
type MemoryForgetRequest struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
}

// ErrorBody is the body of any governance response that is not a success:
// the reason, worded for the model where the model will see it (a memory the
// user's consent refused, say).
type ErrorBody struct {
	Error string `json:"error"`
}
