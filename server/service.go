// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip" // the facade gzips text turns
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/AltairaLabs/omnia-dialhome/runtimev1"
)

// Error codes on an Error frame.
const (
	CodeInternal = "INTERNAL_ERROR"
	CodeDuplex   = "DUPLEX_UNSUPPORTED"
)

// The wire form of a refused session open, which the facade reports to its
// client as SESSION_DECLARATION_REFUSED.
const (
	SessionDeclarationRefusedReason = "SessionDeclarationRefused"
	SessionInitErrorDomain          = "session-init.omnia.altairalabs.ai"
	// ReasonSessionInitUnsupported: the runtime has no session init, so it
	// cannot honour any declaration.
	ReasonSessionInitUnsupported = "SessionInitUnsupported"
)

// DefaultMaxMessageSize is the gRPC message bound: 16MB, to carry
// base64-encoded images.
const DefaultMaxMessageSize = 16 * 1024 * 1024

// DefaultHeartbeat is how often a turn in progress tells the facade it is
// still working, so a turn that is silent while it calls a slow tool is not
// taken for a stalled runtime.
const DefaultHeartbeat = 10 * time.Second

type config struct {
	maxMessageSize int
	heartbeat      time.Duration
	opts           []grpc.ServerOption
}

// Option configures NewServer and Serve.
type Option func(*config)

// WithMaxMessageSize overrides the gRPC message bound, in bytes.
func WithMaxMessageSize(n int) Option { return func(c *config) { c.maxMessageSize = n } }

// WithHeartbeat overrides the heartbeat interval.
func WithHeartbeat(d time.Duration) Option { return func(c *config) { c.heartbeat = d } }

// WithServerOptions appends gRPC server options (interceptors, a stats
// handler).
func WithServerOptions(o ...grpc.ServerOption) Option {
	return func(c *config) { c.opts = append(c.opts, o...) }
}

// NewServer returns a gRPC server with h registered as omnia.runtime.v1's
// RuntimeService, and the standard health service SERVING.
func NewServer(h Handler, opts ...Option) *grpc.Server {
	cfg := config{maxMessageSize: DefaultMaxMessageSize, heartbeat: DefaultHeartbeat}
	for _, o := range opts {
		o(&cfg)
	}
	so := append([]grpc.ServerOption{
		grpc.MaxRecvMsgSize(cfg.maxMessageSize),
		grpc.MaxSendMsgSize(cfg.maxMessageSize),
	}, cfg.opts...)
	srv := grpc.NewServer(so...)
	runtimev1.RegisterRuntimeServiceServer(srv, &service{h: h, heartbeat: cfg.heartbeat})
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return srv
}

// Serve serves h on lis until lis is closed. A closed listener is a clean
// stop and returns nil.
func Serve(lis net.Listener, h Handler, opts ...Option) error {
	err := NewServer(h, opts...).Serve(lis)
	if err == nil || errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

type service struct {
	runtimev1.UnimplementedRuntimeServiceServer
	h         Handler
	heartbeat time.Duration
}

// Health advertises readiness, the contract version and the capabilities.
func (s *service) Health(context.Context, *runtimev1.HealthRequest) (*runtimev1.HealthResponse, error) {
	return &runtimev1.HealthResponse{
		Healthy:         true,
		Status:          "ready",
		ContractVersion: runtimev1.ContractVersion,
		Capabilities:    s.h.Capabilities(),
	}, nil
}

// HasConversation answers from a ConversationProber, else unavailable.
func (s *service) HasConversation(ctx context.Context, req *runtimev1.HasConversationRequest) (*runtimev1.HasConversationResponse, error) {
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	p, ok := s.h.(ConversationProber)
	if !ok {
		return &runtimev1.HasConversationResponse{
			State:  runtimev1.ResumeState_RESUME_STATE_UNAVAILABLE,
			Detail: "resume is not supported by this runtime",
		}, nil
	}
	st := runtimev1.ResumeState_RESUME_STATE_UNAVAILABLE
	switch p.HasConversation(ctx, req.GetSessionId()) {
	case ResumeResumable:
		st = runtimev1.ResumeState_RESUME_STATE_RESUMABLE
	case ResumeNotFound:
		st = runtimev1.ResumeState_RESUME_STATE_NOT_FOUND
	}
	return &runtimev1.HasConversationResponse{State: st}, nil
}

// EndConversation tells a ConversationEnder; without one it is Unimplemented,
// which the facade ignores.
func (s *service) EndConversation(ctx context.Context, req *runtimev1.EndConversationRequest) (*runtimev1.EndConversationResponse, error) {
	e, ok := s.h.(ConversationEnder)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "end conversation is not supported by this runtime")
	}
	if req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	e.EndConversation(ctx, req.GetSessionId())
	return &runtimev1.EndConversationResponse{}, nil
}

// Converse sends RuntimeHello first, then serves each turn on the stream.
func (s *service) Converse(stream runtimev1.RuntimeService_ConverseServer) error {
	ctx := stream.Context()
	if err := refuseDeclaration(ctx); err != nil {
		return err
	}
	c := &conversation{
		s: s, ctx: ctx, stream: stream, out: &sender{stream: stream},
		id: IdentityFromIncoming(ctx), clientTools: ClientToolsAvailable(ctx),
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "failed to receive message: %v", err)
		}
		if err := c.handle(msg); err != nil {
			return err
		}
	}
}

// conversation is one Converse stream's state.
type conversation struct {
	s           *service
	ctx         context.Context
	stream      runtimev1.RuntimeService_ConverseServer
	out         *sender
	id          Identity
	clientTools bool
	helloSent   bool
}

// handle serves one inbound message. Its error ends the stream.
func (c *conversation) handle(msg *runtimev1.ClientMessage) error {
	if msg.GetDuplexStart() != nil {
		_ = c.out.send(errorFrame(CodeDuplex, "duplex audio is not supported by this runtime"))
		return nil
	}
	if !c.helloSent {
		hello := &runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_RuntimeHello{
			RuntimeHello: &runtimev1.RuntimeHello{Capabilities: c.s.h.Capabilities()}}}
		if err := c.out.send(hello); err != nil {
			return status.Errorf(codes.Internal, "failed to send runtime hello: %v", err)
		}
		c.helloSent = true
	}
	if msg.GetClientToolResult() != nil {
		return nil // an answer to a call no turn is waiting on
	}
	turn := Turn{
		SessionID: msg.GetSessionId(), Content: msg.GetContent(), Parts: partsFromProto(msg.GetParts()),
		Metadata: msg.GetMetadata(), Identity: c.id, ClientToolsAvailable: c.clientTools,
	}
	if err := c.s.turn(c.ctx, c.stream, c.out, turn); err != nil {
		_ = c.out.send(errorFrame(CodeInternal, "an internal error occurred while processing the message"))
	}
	return nil
}

// turn runs one turn, heartbeating while it is in progress.
func (s *service) turn(ctx context.Context, stream runtimev1.RuntimeService_ConverseServer, out *sender, t Turn) error {
	stop := out.heartbeat(s.heartbeat)
	defer stop()
	emit := &emitter{out: out, stream: stream}
	if err := s.h.Converse(ctx, t, emit); err != nil {
		return err
	}
	if !emit.isDone() {
		return emit.Done(Done{})
	}
	return nil
}

// refuseDeclaration refuses a session open that declares something: this
// package has no session init to honour it with, and a client that declared
// something and got a session without it would have no way to tell.
func refuseDeclaration(ctx context.Context) error {
	open, declare := SessionOpenFromIncoming(ctx)
	trimmed := bytes.TrimSpace(declare)
	if !open || len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	msg := "this agent's runtime does not accept session declarations"
	st := status.New(codes.FailedPrecondition,
		fmt.Sprintf("session declaration refused: %s: %s", ReasonSessionInitUnsupported, msg))
	if d, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: SessionDeclarationRefusedReason, Domain: SessionInitErrorDomain,
		Metadata: map[string]string{"type": "", "handle": "", "reason": ReasonSessionInitUnsupported, "message": msg},
	}); err == nil {
		return d.Err()
	}
	return st.Err()
}

func errorFrame(code, msg string) *runtimev1.ServerMessage {
	return &runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_Error{
		Error: &runtimev1.Error{Code: code, Message: msg}}}
}

// sender serialises sends on one stream: a gRPC stream allows one sender at
// a time, and a turn's chunks, client tool calls and heartbeats come from
// different goroutines.
type sender struct {
	mu     sync.Mutex
	stream runtimev1.RuntimeService_ConverseServer
}

func (s *sender) send(m *runtimev1.ServerMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(m)
}

// heartbeat sends a Heartbeat every d until the returned stop is called.
func (s *sender) heartbeat(d time.Duration) (stop func()) {
	if d <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_ = s.send(&runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_Heartbeat{
					Heartbeat: &runtimev1.Heartbeat{}}})
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// emitter is one turn's Emitter.
type emitter struct {
	out    *sender
	stream runtimev1.RuntimeService_ConverseServer

	mu   sync.Mutex
	done bool
	// recvMu holds one client tool round-trip at a time: the stream has one
	// reader.
	recvMu sync.Mutex
}

func (e *emitter) isDone() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.done
}

func (e *emitter) Chunk(text string) error {
	if text == "" {
		return nil
	}
	return e.out.send(&runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_Chunk{
		Chunk: &runtimev1.Chunk{Content: text}}})
}

func (e *emitter) Done(d Done) error {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
	var usage *runtimev1.Usage
	if d.Usage != nil {
		usage = &runtimev1.Usage{InputTokens: d.Usage.InputTokens, OutputTokens: d.Usage.OutputTokens, CostUsd: d.Usage.CostUSD}
	}
	return e.out.send(&runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_Done{Done: &runtimev1.Done{
		FinalContent: d.Final, Usage: usage, Parts: partsToProto(d.Parts), FinishReason: d.FinishReason,
	}}})
}

// ToolCall sends a CLIENT ToolCall and reads the matching ClientToolResult
// off the same stream.
func (e *emitter) ToolCall(call ClientToolCall) (ClientToolResult, error) {
	e.recvMu.Lock()
	defer e.recvMu.Unlock()
	err := e.out.send(&runtimev1.ServerMessage{Message: &runtimev1.ServerMessage_ToolCall{ToolCall: &runtimev1.ToolCall{
		Id: call.ID, Name: call.Name, ArgumentsJson: call.ArgumentsJSON,
		Execution:      runtimev1.ToolExecution_TOOL_EXECUTION_CLIENT,
		ConsentMessage: call.ConsentMessage, Categories: call.Categories,
	}}})
	if err != nil {
		return ClientToolResult{}, err
	}
	msg, err := e.stream.Recv()
	if err != nil {
		return ClientToolResult{}, err
	}
	res := msg.GetClientToolResult()
	if res == nil {
		return ClientToolResult{}, status.Error(codes.FailedPrecondition, "expected ClientToolResult after ToolCall")
	}
	return ClientToolResult{
		CallID: res.GetCallId(), ResultJSON: res.GetResultJson(), IsRejected: res.GetIsRejected(),
		RejectionReason: res.GetRejectionReason(), IsDenied: res.GetIsDenied(),
	}, nil
}

func partsFromProto(parts []*runtimev1.ContentPart) []ContentPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]ContentPart, 0, len(parts))
	for _, p := range parts {
		cp := ContentPart{Type: p.GetType(), Text: p.GetText()}
		if m := p.GetMedia(); m != nil {
			cp.Data, cp.URL, cp.MimeType, cp.StorageRef = m.GetData(), m.GetUrl(), m.GetMimeType(), m.GetStorageRef()
		}
		out = append(out, cp)
	}
	return out
}

func partsToProto(parts []ContentPart) []*runtimev1.ContentPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]*runtimev1.ContentPart, 0, len(parts))
	for _, p := range parts {
		cp := &runtimev1.ContentPart{Type: p.Type, Text: p.Text}
		if p.Data != "" || p.URL != "" || p.MimeType != "" || p.StorageRef != "" {
			cp.Media = &runtimev1.MediaContent{Data: p.Data, Url: p.URL, MimeType: p.MimeType, StorageRef: p.StorageRef}
		}
		out = append(out, cp)
	}
	return out
}
