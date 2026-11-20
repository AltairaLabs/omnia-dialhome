// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The tools stream (KindTools): the tools Omnia hosts for the agent, which
// run in the cluster rather than in the runtime. The runtime lists them and
// offers them to its model; when the model calls one, the relay runs it with
// the agent's tool policy and credentials, records it on the session, and
// returns only the result. A credential a tool needs never reaches the
// runtime.
const (
	// ToolsPath (KindTools, GET → HostedToolList) lists the hosted tools.
	ToolsPath = "/v1/tools"
	// ToolsCallPath (KindTools, POST HostedToolCall → HostedToolResult) runs
	// one.
	ToolsCallPath = "/v1/tools/call"
)

// Default bounds on the tools stream.
const (
	// DefaultToolListTimeout bounds HostedTools.
	DefaultToolListTimeout = 10 * time.Second
	// DefaultToolCallTimeout bounds CallTool. A hosted tool call is bounded at
	// two minutes in the cluster; this leaves it the channel's latency.
	DefaultToolCallTimeout = 125 * time.Second
)

// HostedTool is one tool Omnia hosts for the agent, as the model is offered
// it. How it is reached (endpoint, headers, credentials) stays in the cluster.
type HostedTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
}

// HostedToolList is the answer to a list.
type HostedToolList struct {
	Tools []HostedTool `json:"tools"`
}

// HostedToolCall asks the relay to run one hosted tool. As on the governance
// streams, it names the session and nothing else about who is calling: the
// relay runs the tool as the caller it carried to the runtime for that
// session, and only while a turn of that session is in flight.
type HostedToolCall struct {
	SessionID string `json:"session_id"`
	// CallID is the model's id for the call; the session's record of the call
	// carries it.
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Round is the 1-based tool-loop round of the turn that made the call,
	// when the runtime knows it.
	Round int `json:"round,omitempty"`
}

// HostedToolResult is a call's outcome. Error set is a call that failed or
// was refused, and DeniedBy set one the relay or the agent's tool policy
// refused (the rule that did); Result is then empty.
type HostedToolResult struct {
	// Result is the tool's answer as JSON (a backend's text answer arrives as
	// a JSON string).
	Result   json.RawMessage `json:"result,omitempty"`
	Error    string          `json:"error,omitempty"`
	DeniedBy string          `json:"denied_by,omitempty"`
}

// HostedTools lists the tools Omnia hosts for the agent. Any failure to get
// the list (the channel down, a relay that does not serve KindTools, a
// timeout) is an error, and the runtime should offer none.
func (g *Governance) HostedTools(ctx context.Context) ([]HostedTool, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultToolListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dial-home"+ToolsPath, nil)
	if err != nil {
		return nil, err
	}
	var l HostedToolList
	if err := do(g.tools, req, ToolsPath, &l); err != nil {
		return nil, fmt.Errorf("dial-home: hosted tools: %w", err)
	}
	return l.Tools, nil
}

// CallTool runs one hosted tool in the cluster. A tool that failed or was
// refused is a result with Error set, not an error: the error return is the
// relay not answering, or answering with something other than a result
// (*RelayError, for a tool the agent does not host).
func (g *Governance) CallTool(ctx context.Context, call HostedToolCall) (HostedToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultToolCallTimeout)
	defer cancel()
	var res HostedToolResult
	if err := post(ctx, g.tools, ToolsCallPath, call, &res); err != nil {
		return HostedToolResult{}, err
	}
	return res, nil
}
