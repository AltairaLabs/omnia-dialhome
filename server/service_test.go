// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/AltairaLabs/omnia-dialhome/runtimev1"
)

// fake is a Handler whose turn the test scripts.
type fake struct {
	caps []string
	turn func(ctx context.Context, t Turn, emit Emitter) error
}

func (f *fake) Capabilities() []string { return f.caps }
func (f *fake) Converse(ctx context.Context, t Turn, emit Emitter) error {
	return f.turn(ctx, t, emit)
}

// prober also answers HasConversation and EndConversation.
type prober struct {
	fake
	ended []string
}

func (p *prober) HasConversation(_ context.Context, id string) ResumeState {
	switch id {
	case "held":
		return ResumeResumable
	case "gone":
		return ResumeNotFound
	}
	return ResumeUnavailable
}

func (p *prober) EndConversation(_ context.Context, id string) { p.ended = append(p.ended, id) }

func start(t *testing.T, h Handler, opts ...Option) runtimev1.RuntimeServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = Serve(lis, h, opts...) }()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cc.Close(); _ = lis.Close() })
	return runtimev1.NewRuntimeServiceClient(cc)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// recvAll reads a stream to its Done (or end), returning every frame.
func recvAll(t *testing.T, s runtimev1.RuntimeService_ConverseClient) []*runtimev1.ServerMessage {
	t.Helper()
	var out []*runtimev1.ServerMessage
	for {
		m, err := s.Recv()
		if err != nil {
			return out
		}
		out = append(out, m)
		if m.GetDone() != nil {
			return out
		}
	}
}

func TestConverse_HelloFirstThenChunksAndDone(t *testing.T) {
	h := &fake{caps: []string{"x"}, turn: func(_ context.Context, tr Turn, emit Emitter) error {
		require.NoError(t, emit.Chunk("a"))
		require.NoError(t, emit.Chunk(""))
		require.NoError(t, emit.Chunk("b:"+tr.Content+":"+tr.Identity.UserID+":"+tr.Identity.Claims["team"]))
		return emit.Done(Done{Final: "ab", Usage: &Usage{InputTokens: 3, OutputTokens: 4, CostUSD: 0.5},
			FinishReason: "stop", Parts: []ContentPart{{Type: "image", URL: "https://i"}}})
	}}
	c := start(t, h)
	ctx := metadata.AppendToOutgoingContext(ctxT(t), HeaderUserID, "u1", HeaderClaimPrefix+"team", "eng")
	s, err := c.Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "hi"}))
	got := recvAll(t, s)
	require.Len(t, got, 4)
	assert.Equal(t, []string{"x"}, got[0].GetRuntimeHello().GetCapabilities(), "the hello comes first")
	assert.Equal(t, "a", got[1].GetChunk().GetContent())
	assert.Equal(t, "b:hi:u1:eng", got[2].GetChunk().GetContent())
	d := got[3].GetDone()
	assert.Equal(t, "ab", d.GetFinalContent())
	assert.Equal(t, int32(4), d.GetUsage().GetOutputTokens())
	assert.Equal(t, "stop", d.GetFinishReason())
	assert.Equal(t, "https://i", d.GetParts()[0].GetMedia().GetUrl())
}

func TestConverse_ImplicitDoneAndErrorFrame(t *testing.T) {
	fail := false
	h := &fake{turn: func(context.Context, Turn, Emitter) error {
		if fail {
			return errors.New("boom")
		}
		return nil
	}}
	c := start(t, h)
	s, err := c.Converse(ctxT(t))
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "hi"}))
	got := recvAll(t, s)
	require.NotNil(t, got[len(got)-1].GetDone(), "a turn that never said Done gets one")

	fail = true
	s, err = c.Converse(ctxT(t))
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "hi"}))
	_, err = s.Recv() // hello
	require.NoError(t, err)
	m, err := s.Recv()
	require.NoError(t, err)
	assert.Equal(t, CodeInternal, m.GetError().GetCode())
}

func TestConverse_ClientToolRoundTrip(t *testing.T) {
	h := &fake{caps: []string{CapabilityClientTools}, turn: func(_ context.Context, tr Turn, emit Emitter) error {
		if !tr.ClientToolsAvailable {
			return emit.Done(Done{Final: "no client"})
		}
		res, err := emit.ToolCall(ClientToolCall{ID: "c1", Name: "get_location", ArgumentsJSON: `{}`,
			ConsentMessage: "ok?", Categories: []string{"location"}})
		if err != nil {
			return err
		}
		return emit.Done(Done{Final: res.CallID + "=" + res.ResultJSON})
	}}
	c := start(t, h)
	ctx := metadata.AppendToOutgoingContext(ctxT(t), HeaderClientTools, "true")
	s, err := c.Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "where am I"}))
	_, err = s.Recv() // hello
	require.NoError(t, err)
	m, err := s.Recv()
	require.NoError(t, err)
	call := m.GetToolCall()
	require.NotNil(t, call)
	assert.Equal(t, runtimev1.ToolExecution_TOOL_EXECUTION_CLIENT, call.GetExecution())
	assert.Equal(t, "get_location", call.GetName())
	assert.Equal(t, []string{"location"}, call.GetCategories())
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s",
		ClientToolResult: &runtimev1.ClientToolResult{CallId: "c1", ResultJson: `{"lat":1}`}}))
	m, err = s.Recv()
	require.NoError(t, err)
	assert.Equal(t, `c1={"lat":1}`, m.GetDone().GetFinalContent())

	// A result for another call is refused.
	s, err = c.Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "where am I"}))
	_, _ = s.Recv() // hello
	_, err = s.Recv()
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s",
		ClientToolResult: &runtimev1.ClientToolResult{CallId: "other"}}))
	m, err = s.Recv()
	require.NoError(t, err)
	assert.Equal(t, CodeInternal, m.GetError().GetCode())

	// Without the header there is no client to ask.
	s, err = c.Converse(ctxT(t))
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "where am I"}))
	got := recvAll(t, s)
	assert.Equal(t, "no client", got[len(got)-1].GetDone().GetFinalContent())
}

func TestConverse_HeartbeatsWhileATurnWorks(t *testing.T) {
	h := &fake{turn: func(context.Context, Turn, Emitter) error {
		time.Sleep(120 * time.Millisecond)
		return nil
	}}
	c := start(t, h, WithHeartbeat(20*time.Millisecond))
	s, err := c.Converse(ctxT(t))
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "slow"}))
	beats := 0
	for _, m := range recvAll(t, s) {
		if m.GetHeartbeat() != nil {
			beats++
		}
	}
	assert.GreaterOrEqual(t, beats, 2)
}

func TestConverse_RefusesDuplexAndDeclarations(t *testing.T) {
	c := start(t, &fake{turn: func(context.Context, Turn, Emitter) error { return nil }})
	s, err := c.Converse(ctxT(t))
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", DuplexStart: &runtimev1.DuplexStart{Codec: "pcm"}}))
	m, err := s.Recv()
	require.NoError(t, err)
	assert.Equal(t, CodeDuplex, m.GetError().GetCode())

	ctx := metadata.AppendToOutgoingContext(ctxT(t), HeaderSessionOpen, "true", HeaderDeclare, `{"skills":["x"]}`)
	s, err = c.Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "hi"}))
	_, err = s.Recv()
	st := status.Convert(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Len(t, st.Details(), 1)
	info := st.Details()[0].(*errdetails.ErrorInfo)
	assert.Equal(t, SessionDeclarationRefusedReason, info.GetReason())
	assert.Equal(t, ReasonSessionInitUnsupported, info.GetMetadata()["reason"])

	// An empty declaration on the open is no declaration.
	ctx = metadata.AppendToOutgoingContext(ctxT(t), HeaderSessionOpen, "true", HeaderDeclare, " null ")
	s, err = c.Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&runtimev1.ClientMessage{SessionId: "s", Content: "hi"}))
	got := recvAll(t, s)
	require.NotNil(t, got[len(got)-1].GetDone())
}

func TestHealthAndOptionalRPCs(t *testing.T) {
	plain := start(t, &fake{caps: []string{"a"}})
	h, err := plain.Health(ctxT(t), &runtimev1.HealthRequest{})
	require.NoError(t, err)
	assert.True(t, h.GetHealthy())
	assert.Equal(t, runtimev1.ContractVersion, h.GetContractVersion())
	assert.Equal(t, []string{"a"}, h.GetCapabilities())

	r, err := plain.HasConversation(ctxT(t), &runtimev1.HasConversationRequest{SessionId: "s"})
	require.NoError(t, err)
	assert.Equal(t, runtimev1.ResumeState_RESUME_STATE_UNAVAILABLE, r.GetState())
	_, err = plain.HasConversation(ctxT(t), &runtimev1.HasConversationRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = plain.EndConversation(ctxT(t), &runtimev1.EndConversationRequest{SessionId: "s"})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = plain.Invoke(ctxT(t), &runtimev1.InvocationRequest{})
	assert.Equal(t, codes.Unimplemented, status.Code(err), "nothing is advertised that is not served")

	p := &prober{}
	c := start(t, p)
	for id, want := range map[string]runtimev1.ResumeState{
		"held": runtimev1.ResumeState_RESUME_STATE_RESUMABLE,
		"gone": runtimev1.ResumeState_RESUME_STATE_NOT_FOUND,
		"?":    runtimev1.ResumeState_RESUME_STATE_UNAVAILABLE,
	} {
		r, err := c.HasConversation(ctxT(t), &runtimev1.HasConversationRequest{SessionId: id})
		require.NoError(t, err)
		assert.Equal(t, want, r.GetState(), id)
	}
	_, err = c.EndConversation(ctxT(t), &runtimev1.EndConversationRequest{SessionId: "s9"})
	require.NoError(t, err)
	assert.Equal(t, []string{"s9"}, p.ended)
	_, err = c.EndConversation(ctxT(t), &runtimev1.EndConversationRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestStandardHealthService(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := NewServer(&fake{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = cc.Close() }()
	r, err := healthpb.NewHealthClient(cc).Check(ctxT(t), &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, healthpb.HealthCheckResponse_SERVING, r.GetStatus())
}

func TestServe_ClosedListenerIsACleanStop(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, lis.Close())
	assert.NoError(t, Serve(lis, &fake{}))
}

func TestPartsRoundTrip(t *testing.T) {
	in := []ContentPart{{Type: "text", Text: "t"}, {Type: "image", Data: "ZGF0YQ==", MimeType: "image/png", StorageRef: "r"}}
	assert.Equal(t, in, partsFromProto(partsToProto(in)))
	assert.Nil(t, partsFromProto(nil))
	assert.Nil(t, partsToProto(nil))
}

func TestIdentityFromIncoming(t *testing.T) {
	assert.Equal(t, Identity{}, IdentityFromIncoming(context.Background()))
	assert.False(t, ClientToolsAvailable(context.Background()))
	open, _ := SessionOpenFromIncoming(context.Background())
	assert.False(t, open)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		HeaderAgentName, "agent", HeaderWorkspace, "ws", HeaderSubjectID, "sub",
		"X-Omnia-Claim-Role", "admin", HeaderClientTools, "yes"))
	id := IdentityFromIncoming(ctx)
	assert.Equal(t, "agent", id.AgentName)
	assert.Equal(t, "ws", id.Workspace)
	assert.Equal(t, "sub", id.SubjectID)
	assert.Equal(t, map[string]string{"role": "admin"}, id.Claims)
	assert.False(t, ClientToolsAvailable(ctx), "only \"true\" means a client")
}
