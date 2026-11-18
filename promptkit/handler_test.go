// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
	"github.com/AltairaLabs/omnia-dialhome/promptkit"
	"github.com/AltairaLabs/omnia-dialhome/runtimev1"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

const (
	testKey  = "odh_test-key"
	testPack = "testdata/assistant.pack.json"
	reply    = "Hello from the hosted pack, streamed in several pieces."
)

// script is a mock response repository: a tool call on the first model call
// of a turn when tools is set, then text.
type script struct{ tools bool }

func (s script) GetResponse(ctx context.Context, p mock.ResponseParams) (string, error) {
	t, err := s.GetTurn(ctx, p)
	return t.Content, err
}

func (s script) GetTurn(_ context.Context, p mock.ResponseParams) (*mock.Turn, error) {
	if s.tools && p.TurnNumber == 1 {
		return &mock.Turn{Type: "tool_calls", ToolCalls: []mock.ToolCall{
			{Name: "get_location", Arguments: map[string]any{"accuracy": "high"}}}}, nil
	}
	if s.tools {
		return &mock.Turn{Type: "text", Content: "You are in the control room."}, nil
	}
	return &mock.Turn{Type: "text", Content: reply}, nil
}

func provider(tools bool) promptkit.ProviderFunc {
	return func(context.Context) (providers.Provider, error) {
		return mock.NewToolProviderWithRepository("mock", "mock-model", false, script{tools: tools}), nil
	}
}

// hosted runs h as a hosted runtime: dialled home to a relay stand-in, which
// it returns with the facade's connection to it.
func hosted(t *testing.T, h server.Handler) (*relaytest.Relay, *grpc.ClientConn) {
	t.Helper()
	relay := relaytest.New(t, testKey, nil)
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: relay.URL(), Key: testKey, Log: logr.Discard(),
		MinBackoff: 20 * time.Millisecond, KeepAlive: relaytest.KeepAlive,
	})
	require.NoError(t, err)
	go func() { _ = server.Serve(c, h) }()
	t.Cleanup(func() { _ = c.Close() })
	require.Eventually(t, func() bool { return relay.Connected() == 1 }, 5*time.Second, 10*time.Millisecond)
	return relay, relay.Conn(t)
}

func newHandler(t *testing.T, cfg promptkit.Config) *promptkit.Handler {
	t.Helper()
	cfg.PackPath, cfg.PromptName = testPack, "assistant"
	h, err := promptkit.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestEndToEnd_ATurnStreamsTheModelsReplyThroughTheChannel(t *testing.T) {
	h := newHandler(t, promptkit.Config{Provider: provider(false)})
	_, cc := hosted(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream, err := runtimev1.NewRuntimeServiceClient(cc).Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&runtimev1.ClientMessage{SessionId: "s1", Content: "hello"}))

	first, err := stream.Recv()
	require.NoError(t, err)
	require.NotNil(t, first.GetRuntimeHello(), "the hello comes first")
	assert.Empty(t, first.GetRuntimeHello().GetCapabilities(), "no client tools, no capability claimed")

	var chunks []string
	var done *runtimev1.Done
	for done == nil {
		m, err := stream.Recv()
		require.NoError(t, err)
		if c := m.GetChunk(); c != nil {
			chunks = append(chunks, c.GetContent())
		}
		require.Nil(t, m.GetError())
		done = m.GetDone()
	}
	assert.Greater(t, len(chunks), 1, "the reply streams in pieces")
	assert.Equal(t, reply, strings.Join(chunks, ""))
	assert.Equal(t, reply, done.GetFinalContent())
	assert.Positive(t, done.GetUsage().GetOutputTokens())

	health, err := runtimev1.NewRuntimeServiceClient(cc).Health(ctx, &runtimev1.HealthRequest{})
	require.NoError(t, err)
	assert.Equal(t, runtimev1.ContractVersion, health.GetContractVersion())
}

func TestEndToEnd_AClientToolGoesToTheClientAndItsAnswerToTheModel(t *testing.T) {
	h := newHandler(t, promptkit.Config{Provider: provider(true), ClientTools: []string{"get_location"}})
	assert.Equal(t, []string{server.CapabilityClientTools}, h.Capabilities())
	_, cc := hosted(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, server.HeaderClientTools, "true")

	stream, err := runtimev1.NewRuntimeServiceClient(cc).Converse(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&runtimev1.ClientMessage{SessionId: "s2", Content: "where am I?"}))

	var final string
	var sawCall bool
	for final == "" {
		m, err := stream.Recv()
		require.NoError(t, err)
		require.Nil(t, m.GetError())
		if call := m.GetToolCall(); call != nil {
			sawCall = true
			assert.Equal(t, "get_location", call.GetName())
			assert.Equal(t, runtimev1.ToolExecution_TOOL_EXECUTION_CLIENT, call.GetExecution())
			assert.JSONEq(t, `{"accuracy":"high"}`, call.GetArgumentsJson())
			require.NoError(t, stream.Send(&runtimev1.ClientMessage{SessionId: "s2",
				ClientToolResult: &runtimev1.ClientToolResult{CallId: call.GetId(), ResultJson: `{"room":"control"}`}}))
		}
		if d := m.GetDone(); d != nil {
			final = d.GetFinalContent()
		}
	}
	assert.True(t, sawCall, "the client was asked to run the tool")
	assert.Equal(t, "You are in the control room.", final)
}

func TestConversationsArePerSessionAndEnd(t *testing.T) {
	h := newHandler(t, promptkit.Config{Provider: provider(false)})
	ctx := context.Background()
	assert.Equal(t, server.ResumeNotFound, h.HasConversation(ctx, "s3"))

	emit := &collect{}
	require.NoError(t, h.Converse(ctx, server.Turn{SessionID: "s3", Content: "hi"}, emit))
	assert.Equal(t, reply, emit.done.Final)
	assert.Equal(t, server.ResumeResumable, h.HasConversation(ctx, "s3"))

	h.EndConversation(ctx, "s3")
	assert.Equal(t, server.ResumeNotFound, h.HasConversation(ctx, "s3"))

	err := h.Converse(ctx, server.Turn{SessionID: "s4", Parts: []server.ContentPart{{Type: "file"}}}, emit)
	assert.Error(t, err, "a part the pack cannot take is refused")
}

func TestNew_RequiresAPackAndPrompt(t *testing.T) {
	_, err := promptkit.New(promptkit.Config{PackPath: testPack})
	assert.Error(t, err)
}

func TestOpenFailuresAreTurnErrors(t *testing.T) {
	h, err := promptkit.New(promptkit.Config{PackPath: "testdata/missing.pack.json", PromptName: "x",
		Provider: provider(false)})
	require.NoError(t, err)
	assert.Error(t, h.Converse(context.Background(), server.Turn{SessionID: "s"}, &collect{}))
}

// collect is an Emitter that keeps what it is sent.
type collect struct {
	chunks []string
	done   server.Done
}

func (c *collect) Chunk(s string) error { c.chunks = append(c.chunks, s); return nil }
func (c *collect) ToolCall(server.ClientToolCall) (server.ClientToolResult, error) {
	return server.ClientToolResult{}, nil
}
func (c *collect) Done(d server.Done) error { c.done = d; return nil }
