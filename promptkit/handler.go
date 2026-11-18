// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package promptkit is a server.Handler backed by a PromptKit pack: each
// Omnia session is one PromptKit SDK conversation, its replies stream as
// they are generated, and the pack's client tools travel the protocol as
// client tool calls.
package promptkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/AltairaLabs/PromptKit/sdk/v2"
	"github.com/go-logr/logr"

	"github.com/AltairaLabs/omnia-dialhome/server"
)

// DefaultIdleTTL is how long a conversation nobody has used is kept before a
// new conversation's open closes it.
const DefaultIdleTTL = time.Hour

// ProviderFunc builds the provider a new conversation uses. A nil provider
// and nil error leaves the choice to PromptKit (the pack's default, with a
// credential from the environment).
type ProviderFunc func(ctx context.Context) (providers.Provider, error)

// Config configures a Handler.
type Config struct {
	// PackPath is the compiled PromptPack (.pack.json). Required.
	PackPath string
	// PromptName is the pack prompt each conversation runs. Required.
	PromptName string
	// Provider builds each conversation's provider. Optional.
	Provider ProviderFunc
	// ClientTools are the pack tools the caller's client runs. Each call is
	// sent on the Converse stream as a client tool call and the turn waits
	// for the client's answer. Listing any makes the runtime advertise
	// client_tools.
	ClientTools []string
	// Options are added to every conversation's sdk.Open.
	Options []sdk.Option
	// Configure is called on every new conversation, for what an option
	// cannot do — registering OnTool handlers, for one.
	Configure func(conv *sdk.Conversation) error
	// IdleTTL bounds how long an unused conversation is kept;
	// DefaultIdleTTL when zero.
	IdleTTL time.Duration
	// Log receives conversation lifecycle events. Zero value discards.
	Log logr.Logger
}

// Handler serves a PromptKit pack over omnia.runtime.v1.
type Handler struct {
	cfg Config
	now func() time.Time

	mu    sync.Mutex
	convs map[string]*session
}

var (
	_ server.Handler            = (*Handler)(nil)
	_ server.ConversationProber = (*Handler)(nil)
	_ server.ConversationEnder  = (*Handler)(nil)
)

// session is one Omnia session's conversation.
type session struct {
	conv     *sdk.Conversation
	provider providers.Provider

	mu       sync.Mutex // one turn at a time
	emit     server.Emitter
	clients  bool // the current turn has a client to answer tool calls
	lastUsed time.Time
}

// New returns a Handler for cfg. The pack is opened per conversation, so a
// pack that does not load fails the first turn; validate it beforehand with
// sdk.ValidatePack if that is too late.
func New(cfg Config) (*Handler, error) {
	if cfg.PackPath == "" || cfg.PromptName == "" {
		return nil, errors.New("promptkit: PackPath and PromptName are required")
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = DefaultIdleTTL
	}
	return &Handler{cfg: cfg, now: time.Now, convs: map[string]*session{}}, nil
}

// Capabilities is client_tools when the pack has client tools, else none.
func (h *Handler) Capabilities() []string {
	if len(h.cfg.ClientTools) > 0 {
		return []string{server.CapabilityClientTools}
	}
	return nil
}

// HasConversation reports whether the session's conversation is held. The
// conversation lives in this process only, so one that is not held is gone.
func (h *Handler) HasConversation(_ context.Context, sessionID string) server.ResumeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.convs[sessionID]; ok {
		return server.ResumeResumable
	}
	return server.ResumeNotFound
}

// EndConversation closes the session's conversation.
func (h *Handler) EndConversation(_ context.Context, sessionID string) {
	h.mu.Lock()
	s := h.convs[sessionID]
	delete(h.convs, sessionID)
	h.mu.Unlock()
	if s != nil {
		h.close(sessionID, s)
	}
}

// Close closes every conversation.
func (h *Handler) Close() error {
	h.mu.Lock()
	convs := h.convs
	h.convs = map[string]*session{}
	h.mu.Unlock()
	for id, s := range convs {
		h.close(id, s)
	}
	return nil
}

// Converse runs one turn on the session's conversation, streaming the reply.
func (h *Handler) Converse(ctx context.Context, t server.Turn, emit server.Emitter) error {
	s, err := h.session(ctx, t.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit, s.clients = emit, t.ClientToolsAvailable
	defer func() { s.emit, s.clients = nil, false }()

	msg, err := userMessage(t)
	if err != nil {
		return err
	}
	var opts []sdk.SendOption
	if len(t.Metadata) > 0 {
		md := make(map[string]any, len(t.Metadata))
		for k, v := range t.Metadata {
			md[k] = v
		}
		opts = append(opts, sdk.WithMessageMetadata(md))
	}
	for chunk := range s.conv.Stream(ctx, msg, opts...) {
		if chunk.Error != nil {
			return chunk.Error
		}
		switch chunk.Type {
		case sdk.ChunkText:
			if err := emit.Chunk(chunk.Text); err != nil {
				return err
			}
		case sdk.ChunkDone:
			return emit.Done(done(chunk.Message))
		default:
			// Server-side tool calls run inside PromptKit and are not part of
			// the protocol; client tools are answered by clientTool.
		}
	}
	return ctx.Err()
}

func done(r *sdk.Response) server.Done {
	if r == nil {
		return server.Done{}
	}
	d := server.Done{Final: r.Text(), Usage: &server.Usage{
		InputTokens: int32(r.InputTokens()), OutputTokens: int32(r.OutputTokens()), CostUSD: float32(r.Cost()), //nolint:gosec // token counts fit
	}}
	if m := r.Message(); m != nil {
		d.FinishReason = m.FinishReason
	}
	return d
}

// userMessage builds the turn's user message: its parts when it has any,
// else its text.
func userMessage(t server.Turn) (any, error) {
	if len(t.Parts) == 0 {
		return t.Content, nil
	}
	m := &types.Message{Role: "user"}
	for _, p := range t.Parts {
		switch {
		case p.Type == "text":
			m.AddTextPart(p.Text)
		case p.Type == "image" && p.Data != "":
			m.AddPart(types.NewImagePartFromData(p.Data, p.MimeType, nil))
		case p.Type == "image" && p.URL != "":
			m.AddImagePartFromURL(p.URL, nil)
		case p.Type == "audio" && p.Data != "":
			m.AddPart(types.NewAudioPartFromData(p.Data, p.MimeType))
		case p.Type == "video" && p.Data != "":
			m.AddPart(types.NewVideoPartFromData(p.Data, p.MimeType))
		default:
			return nil, fmt.Errorf("promptkit: unsupported content part %q", p.Type)
		}
	}
	return m, nil
}

// session returns the session's conversation, opening it when there is none.
func (h *Handler) session(ctx context.Context, id string) (*session, error) {
	h.mu.Lock()
	if s, ok := h.convs[id]; ok {
		s.lastUsed = h.now()
		h.mu.Unlock()
		return s, nil
	}
	idle := h.takeIdle()
	h.mu.Unlock()
	for sid, s := range idle {
		h.close(sid, s)
	}

	s, err := h.open(ctx, id)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if prior, ok := h.convs[id]; ok {
		// Another turn opened it first; keep that one.
		go h.close(id, s)
		return prior, nil
	}
	h.convs[id] = s
	return s, nil
}

// takeIdle removes and returns the conversations unused for IdleTTL. The
// caller holds h.mu.
func (h *Handler) takeIdle() map[string]*session {
	var idle map[string]*session
	for id, s := range h.convs {
		if h.now().Sub(s.lastUsed) < h.cfg.IdleTTL || !s.mu.TryLock() {
			continue
		}
		s.mu.Unlock()
		if idle == nil {
			idle = map[string]*session{}
		}
		idle[id] = s
		delete(h.convs, id)
	}
	return idle
}

func (h *Handler) open(ctx context.Context, id string) (*session, error) {
	s := &session{lastUsed: h.now()}
	opts := append([]sdk.Option{sdk.WithConversationID(id)}, h.cfg.Options...)
	if h.cfg.Provider != nil {
		p, err := h.cfg.Provider(ctx)
		if err != nil {
			return nil, fmt.Errorf("promptkit: provider: %w", err)
		}
		if p != nil {
			s.provider = p
			opts = append(opts, sdk.WithProvider(p))
		}
	}
	conv, err := sdk.Open(h.cfg.PackPath, h.cfg.PromptName, opts...)
	if err != nil {
		closeProvider(s.provider)
		return nil, fmt.Errorf("promptkit: open %s: %w", h.cfg.PackPath, err)
	}
	s.conv = conv
	for _, name := range h.cfg.ClientTools {
		conv.OnClientTool(name, s.clientTool)
	}
	if h.cfg.Configure != nil {
		if err := h.cfg.Configure(conv); err != nil {
			h.close(id, s)
			return nil, fmt.Errorf("promptkit: configure: %w", err)
		}
	}
	h.cfg.Log.V(1).Info("conversation opened", "sessionID", id)
	return s, nil
}

func (h *Handler) close(id string, s *session) {
	if s.conv != nil {
		if err := s.conv.Close(); err != nil {
			h.cfg.Log.Info("conversation close failed", "sessionID", id, "err", err.Error())
		}
	}
	closeProvider(s.provider)
	h.cfg.Log.V(1).Info("conversation closed", "sessionID", id)
}

func closeProvider(p providers.Provider) {
	if p != nil {
		_ = p.Close()
	}
}

// errNoClient is the tool result when no client can answer a client tool.
var errNoClient = errors.New("no client is connected to run this tool")

// clientTool carries one client tool call to the turn's client and returns
// its answer to the model. It runs inside the conversation's turn, so the
// turn's emitter is set.
func (s *session) clientTool(_ context.Context, req sdk.ClientToolRequest) (any, error) {
	emit, clients := s.emit, s.clients
	if emit == nil || !clients {
		return nil, errNoClient
	}
	args, err := json.Marshal(req.Args)
	if err != nil {
		return nil, err
	}
	res, err := emit.ToolCall(server.ClientToolCall{
		ID: req.CallID, Name: req.ToolName, ArgumentsJSON: string(args),
		ConsentMessage: req.ConsentMsg, Categories: req.Categories,
	})
	if err != nil {
		return nil, err
	}
	if res.IsRejected {
		return nil, fmt.Errorf("the client did not run the tool: %s", res.RejectionReason)
	}
	var out any
	if err := json.Unmarshal([]byte(res.ResultJSON), &out); err != nil {
		return res.ResultJSON, nil //nolint:nilerr // a result that is not JSON goes to the model as text
	}
	return out, nil
}
