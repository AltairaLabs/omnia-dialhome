// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package frontdoor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// serveInvocations is AgentCore's invoke route and Foundry's invocations
// protocol. The body is JSON naming the message as prompt, message or input
// (a body that is not JSON is the message). The session is AgentCore's
// header, else Foundry's agent_session_id query parameter, else a session_id
// body field, else the container's own session. It answers
// {"response", "session_id"}, or an SSE stream when the caller accepts one.
func (s *Server) serveInvocations(w http.ResponseWriter, r *http.Request) {
	body, raw, err := readJSON(r)
	if err != nil {
		writeErr(w, &errTurn{status: http.StatusBadRequest, msg: "the request body could not be read"})
		return
	}
	if body != nil && str(body, KickField) != "" {
		// The relay starting this container so that it dials home.
		s.kicked.Store(true)
		s.Log.Info("front door: session start", "hasPlatformSession", r.Header.Get(HeaderAgentCoreSession) != "")
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
		return
	}
	platform := s.Platform
	session := r.Header.Get(HeaderAgentCoreSession)
	if session != "" && platform == "" {
		platform = PlatformAgentCore
	}
	if session == "" {
		session = r.URL.Query().Get("agent_session_id")
	}
	if session == "" {
		session = str(body, "session_id")
	}
	if session == "" {
		session = s.SessionFallback
	}
	if platform == "" {
		platform = PlatformFoundry
	}
	text := str(body, "prompt", "message", "input")
	if body == nil {
		text = raw
	}
	t := turn{platform: platform, session: session, token: identityToken(r.Header), text: text}

	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamSSE(w, r, t, func(chunk string) any { return map[string]string{"type": "chunk", "text": chunk} },
			func(res result) any {
				return map[string]string{"type": "done", "response": res.reply, "session_id": res.sessionID}
			})
		return
	}
	res, err := s.run(r.Context(), t, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"response": res.reply, "session_id": res.sessionID})
}

// serveResponses is Foundry's responses protocol: an OpenAI Responses
// request. The message is input (a string, or the text of its last user
// item); the session is agent_session_id, else conversation, else the
// container's own. It answers a Responses object, or its SSE events when
// stream is set.
func (s *Server) serveResponses(w http.ResponseWriter, r *http.Request) {
	body, raw, err := readJSON(r)
	if err != nil || body == nil {
		writeErr(w, &errTurn{status: http.StatusBadRequest, msg: "the request is not a Responses request: " + raw})
		return
	}
	session := str(body, "agent_session_id")
	if session == "" {
		session = conversationID(body["conversation"])
	}
	if session == "" {
		session = s.SessionFallback
	}
	platform := s.Platform
	if platform == "" {
		platform = PlatformFoundry
	}
	t := turn{platform: platform, session: session, token: identityToken(r.Header), text: responsesInput(body["input"])}
	id := "resp_" + randomHex(12)
	if stream, _ := body["stream"].(bool); stream {
		s.streamSSE(w, r, t, func(chunk string) any {
			return map[string]any{"type": "response.output_text.delta", "item_id": id, "delta": chunk}
		}, func(res result) any {
			return map[string]any{"type": "response.completed", "response": responsesObject(id, res)}
		})
		return
	}
	res, err := s.run(r.Context(), t, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, responsesObject(id, res))
}

// serveVertex is Agent Runtime's query (:query) and stream query
// (:streamQuery) on a custom container. The body is
// {"class_method", "input": {"message", "session_id", "identity_token"}}: no
// header reaches the container, so the caller's identity token rides input.
// query answers {"output"}; stream query answers NDJSON {"output"} lines.
func (s *Server) serveVertex(stream bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _, err := readJSON(r)
		if err != nil || body == nil {
			writeErr(w, &errTurn{status: http.StatusBadRequest, msg: "the request is not a query"})
			return
		}
		in, _ := body["input"].(map[string]any)
		token := str(in, "identity_token")
		if token == "" {
			token = identityToken(r.Header)
		}
		t := turn{platform: PlatformVertex, session: str(in, "session_id"), token: token,
			text: str(in, "message", "prompt", "input")}
		if stream {
			s.vertexStream(w, r, t)
			return
		}
		res, err := s.run(r.Context(), t, nil)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"output": res.reply, "session_id": res.sessionID})
	}
}

// vertexStream answers a stream query: an NDJSON {"output"} line per chunk,
// then {"session_id", "done"}. A turn refused before it starts is answered
// with its status.
func (s *Server) vertexStream(w http.ResponseWriter, r *http.Request, t turn) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	started := false
	line := func(v any) {
		if !started {
			w.WriteHeader(http.StatusOK)
			started = true
		}
		_ = enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, err := s.run(r.Context(), t, func(chunk string) { line(map[string]string{"output": chunk}) })
	switch {
	case err != nil && !started:
		writeErr(w, err)
	case err != nil:
		line(map[string]string{"error": err.Error()})
	default:
		if !started {
			line(map[string]string{"output": res.reply})
		}
		line(map[string]any{"session_id": res.sessionID, "done": true})
	}
}

// streamSSE runs t, sending each chunk as an SSE data event built by chunkEv
// and the end by doneEv. A turn refused before it starts is answered with
// its status, not a stream.
func (s *Server) streamSSE(w http.ResponseWriter, r *http.Request, t turn,
	chunkEv func(string) any, doneEv func(result) any) {
	flusher, _ := w.(http.Flusher)
	started := false
	send := func(v any) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	res, err := s.run(r.Context(), t, func(chunk string) { send(chunkEv(chunk)) })
	if err != nil {
		if !started {
			writeErr(w, err)
			return
		}
		send(map[string]string{"type": "error", "error": err.Error()})
		return
	}
	send(doneEv(res))
}

// responsesInput is the text of a Responses request's input: a string, or
// the text of its last user message item.
func responsesInput(v any) string {
	switch in := v.(type) {
	case string:
		return in
	case []any:
		for i := len(in) - 1; i >= 0; i-- {
			item, ok := in[i].(map[string]any)
			if !ok {
				continue
			}
			if role, _ := item["role"].(string); role != "" && role != "user" {
				continue
			}
			if text := itemText(item["content"]); text != "" {
				return text
			}
		}
	}
	return ""
}

// itemText is the text of a Responses message item's content.
func itemText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if m, ok := p.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// conversationID is a Responses request's conversation: an id, or {"id"}.
func conversationID(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case map[string]any:
		id, _ := c["id"].(string)
		return id
	}
	return ""
}

// responsesObject is a minimal Responses API response for a finished turn.
func responsesObject(id string, res result) map[string]any {
	out := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed",
		"output": []any{map[string]any{
			"id": "msg_" + randomHex(12), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": res.reply, "annotations": []any{}}},
		}},
		"output_text": res.reply,
		"metadata":    map[string]string{"omnia_session_id": res.sessionID},
	}
	if u := res.usage; u != nil {
		out["usage"] = map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
			"total_tokens": u.InputTokens + u.OutputTokens}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
