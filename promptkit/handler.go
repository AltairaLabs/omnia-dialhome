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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	lastUsed time.Time // guarded by Handler.mu

	// turn holds one turn at a time; a turn waiting for it gives up when its
	// own stream ends.
	turn chan struct{}
	// ended is set once the conversation is closed.
	ended atomic.Bool

	mu      sync.Mutex
	emit    server.Emitter
	clients bool // the current turn has a client to answer tool calls
}

func newSession(now time.Time) *session {
	return &session{lastUsed: now, turn: make(chan struct{}, 1)}
}

func (s *session) acquire(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) tryAcquire() bool {
	select {
	case s.turn <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *session) release() { <-s.turn }

func (s *session) setTurn(emit server.Emitter, clients bool) {
	s.mu.Lock()
	s.emit, s.clients = emit, clients
	s.mu.Unlock()
}

func (s *session) current() (server.Emitter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit, s.clients
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

// EndConversation closes the session's conversation once any turn on it has
// finished.
func (h *Handler) EndConversation(ctx context.Context, sessionID string) {
	h.mu.Lock()
	s := h.convs[sessionID]
	delete(h.convs, sessionID)
	h.mu.Unlock()
	if s != nil {
		h.end(ctx, sessionID, s)
	}
}

// closeWait bounds how long Close waits for a turn in progress.
const closeWait = 5 * time.Second

// Close closes every conversation, waiting up to five seconds for each turn
// in progress.
func (h *Handler) Close() error {
	h.mu.Lock()
	convs := h.convs
	h.convs = map[string]*session{}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	for id, s := range convs {
		h.end(ctx, id, s)
	}
	return nil
}

// end closes s once it holds no turn (or ctx ends), and marks it ended so a
// turn that was waiting for it does not run on a closed conversation.
func (h *Handler) end(ctx context.Context, id string, s *session) {
	if s.acquire(ctx) == nil {
		defer s.release()
	}
	s.ended.Store(true)
	h.close(id, s)
}

// errEnded is a turn on a conversation that ended while it waited.
var errEnded = errors.New("promptkit: the conversation has ended")

// Converse runs one turn on the session's conversation, streaming the reply.
func (h *Handler) Converse(ctx context.Context, t server.Turn, emit server.Emitter) error {
	if t.SessionID == "" {
		return errors.New("promptkit: a turn needs a session id")
	}
	msg, err := userMessage(t)
	if err != nil {
		return err
	}
	var opts []sdk.SendOption
	md, err := forwardedMetadata(t.Metadata)
	if err != nil {
		return err
	}
	if md != nil {
		opts = append(opts, sdk.WithMessageMetadata(md))
	}
	s, err := h.session(ctx, t.SessionID)
	if err != nil {
		return err
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.ended.Load() {
		return errEnded
	}
	s.setTurn(emit, t.ClientToolsAvailable)
	defer s.setTurn(nil, false)
	return stream(ctx, s.conv, msg, opts, emit)
}

// stream runs the turn and relays it. It always drains the SDK's stream, so
// the conversation is free for the next turn, and nothing of this turn
// outlives it.
func stream(ctx context.Context, conv *sdk.Conversation, msg any, opts []sdk.SendOption, emit server.Emitter) error {
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var result error
	finished := false
	for chunk := range conv.Stream(turnCtx, msg, opts...) {
		if finished {
			continue
		}
		switch {
		case chunk.Error != nil:
			result, finished = chunk.Error, true
		case chunk.Type == sdk.ChunkText:
			if err := emit.Chunk(chunk.Text); err != nil {
				result, finished = err, true
			}
		case chunk.Type == sdk.ChunkDone:
			result, finished = emit.Done(done(chunk.Message)), true
		default:
			// Server-side tool calls run inside PromptKit and are not part of
			// the protocol; client tools are answered by clientTool.
		}
		if finished {
			cancel()
		}
	}
	if !finished {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("promptkit: the turn ended without a reply")
	}
	return result
}

// Limits on the metadata a turn forwards to the model, and the keys that are
// the platform's: as Omnia's own runtime applies them.
const (
	maxMetadataKeys        = 32
	maxMetadataKeyLen      = 64
	maxMetadataValueBytes  = 8 << 10
	maxMetadataTotalBytes  = 32 << 10
	reservedMetadataPrefix = "omnia."
)

var (
	reservedMetadataKeys = map[string]bool{"mock_scenario": true, "content_type": true}
	metadataKeyPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	errMessageMetadata   = errors.New("invalid message metadata")
)

// forwardedMetadata is the part of a turn's metadata the model may see, or
// nil: never a reserved key, and an error when the rest breaks a limit.
func forwardedMetadata(in map[string]string) (map[string]any, error) {
	var out map[string]any
	total := 0
	for k, v := range in {
		if reservedMetadataKeys[k] || strings.HasPrefix(k, reservedMetadataPrefix) {
			continue
		}
		if len(k) > maxMetadataKeyLen || !metadataKeyPattern.MatchString(k) {
			return nil, fmt.Errorf("%w: a key is longer than %d bytes or not [A-Za-z0-9_.-]", errMessageMetadata, maxMetadataKeyLen)
		}
		if len(v) > maxMetadataValueBytes {
			return nil, fmt.Errorf("%w: %s is longer than %d bytes", errMessageMetadata, k, maxMetadataValueBytes)
		}
		total += len(k) + len(v)
		if out == nil {
			out = map[string]any{}
		}
		out[k] = v
	}
	if len(out) > maxMetadataKeys || total > maxMetadataTotalBytes {
		return nil, fmt.Errorf("%w: at most %d keys and %d bytes", errMessageMetadata, maxMetadataKeys, maxMetadataTotalBytes)
	}
	return out, nil
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
		if h.now().Sub(s.lastUsed) < h.cfg.IdleTTL || !s.tryAcquire() {
			continue
		}
		s.ended.Store(true)
		s.release()
		if idle == nil {
			idle = map[string]*session{}
		}
		idle[id] = s
		delete(h.convs, id)
	}
	return idle
}

func (h *Handler) open(ctx context.Context, id string) (*session, error) {
	s := newSession(h.now())
	opts := append([]sdk.Option{sdk.WithConversationID(id)}, h.cfg.Options...)
	if h.cfg.Provider != nil {
		// Not the stream's ctx: the provider lives with the conversation, and
		// a credential source may keep its ctx to refresh tokens with.
		p, err := h.cfg.Provider(context.WithoutCancel(ctx))
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
	emit, clients := s.current()
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
