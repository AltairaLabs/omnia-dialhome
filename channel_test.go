// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/internal/relaytest"
	"github.com/AltairaLabs/omnia-dialhome/runtimev1"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

const testKey = "odh_test-key"

// echo is the stand-in for a hosted runtime.
type echo struct{}

func (echo) Capabilities() []string { return nil }

func (echo) Converse(_ context.Context, t server.Turn, emit server.Emitter) error {
	if err := emit.Chunk("echo:" + t.Content + "|user=" + t.Identity.UserID); err != nil {
		return err
	}
	return emit.Done(server.Done{})
}

// dial connects a client serving echo to relay with key.
func dial(t *testing.T, relay *relaytest.Relay, key string) *dialhome.Client {
	t.Helper()
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: relay.URL(), Key: key, Log: logr.Discard(),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		KeepAlive: relaytest.KeepAlive,
	})
	require.NoError(t, err)
	go func() { _ = server.Serve(c, echo{}) }()
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitConnected(t *testing.T, r *relaytest.Relay, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return r.Connected() == n }, 5*time.Second, 10*time.Millisecond)
}

// waitBothConnected waits for c's one session to be live at both ends. The
// relay holds its end of a session before the client holds its own, so a
// client-side stream opened on the relay's word alone can find none.
func waitBothConnected(t *testing.T, r *relaytest.Relay, c *dialhome.Client) {
	t.Helper()
	require.Eventually(t, func() bool { return r.Connected() == 1 && c.Connected() }, 5*time.Second, 10*time.Millisecond)
}

func converse(ctx context.Context, cc *grpc.ClientConn, text string) (string, error) {
	// gzip, as the facade sends text turns.
	stream, err := runtimev1.NewRuntimeServiceClient(cc).Converse(ctx, grpc.UseCompressor(gzip.Name))
	if err != nil {
		return "", err
	}
	if err := stream.Send(&runtimev1.ClientMessage{SessionId: "s1", Content: text}); err != nil {
		return "", err
	}
	_ = stream.CloseSend()
	var out strings.Builder
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return out.String(), nil
		}
		if err != nil {
			return out.String(), err
		}
		out.WriteString(msg.GetChunk().GetContent())
		if msg.GetDone() != nil {
			return out.String(), nil
		}
	}
}

func TestChannel_ConverseReachesTheRuntimeWithItsMetadata(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)
	dial(t, relay, testKey)
	waitConnected(t, relay, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, server.HeaderUserID, "u-42")
	got, err := converse(ctx, relay.Conn(t), "hello")
	require.NoError(t, err)
	require.Equal(t, "echo:hello|user=u-42", got)
}

func TestClient_RedialsAfterTheChannelDrops(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)
	c := dial(t, relay, testKey)
	waitConnected(t, relay, 1)
	cc := relay.Conn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := converse(ctx, cc, "before")
	require.NoError(t, err)

	// The key is still valid, so a dropped session is a network blip: the
	// runtime must come back on its own, and the facade's next turn reach it.
	relay.Drop()
	require.Eventually(t, func() bool { return relay.Handshakes() >= 2 && relay.Connected() == 1 && c.Connected() },
		5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		got, err := converse(ctx, cc, "again")
		return err == nil && got == "echo:again|user="
	}, 5*time.Second, 50*time.Millisecond)
}

// The revision rides every handshake, re-dials included, so a relay can tell
// revisions of the same agent apart; a client with none sends no header.
func TestClient_HandshakeCarriesTheRevision(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: relay.URL(), Key: testKey, Log: logr.Discard(), Revision: "1.4.0",
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, KeepAlive: relaytest.KeepAlive,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, "1.4.0", c.Revision())
	waitConnected(t, relay, 1)
	relay.Drop()
	require.Eventually(t, func() bool { return relay.Handshakes() >= 2 }, 5*time.Second, 10*time.Millisecond)
	values, sent := relay.PackVersions()
	assert.Equal(t, []string{"1.4.0", "1.4.0"}, values[:2])
	assert.Equal(t, []bool{true, true}, sent[:2])

	bare := relaytest.New(t, testKey, nil)
	c2 := dial(t, bare, testKey)
	assert.Empty(t, c2.Revision())
	waitConnected(t, bare, 1)
	_, sent = bare.PackVersions()
	assert.Equal(t, []bool{false}, sent, "no revision, no header")
}

func TestClient_KeepsRedialingARefusedKey(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer odh_not-the-key")
	hdr.Set(dialhome.HeaderVersion, dialhome.ProtocolVersion)
	_, resp, err := websocket.DefaultDialer.Dial(relay.URL(), hdr)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	hdr.Set("Authorization", "Bearer "+testKey)
	hdr.Set(dialhome.HeaderVersion, "2")
	_, resp, err = websocket.DefaultDialer.Dial(relay.URL(), hdr)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	c := dial(t, relay, "odh_not-the-key")
	require.Never(t, func() bool { return c.Connected() || relay.Connected() > 0 },
		300*time.Millisecond, 20*time.Millisecond)
}

func TestChannel_ForwardCarriesHTTPToItsKindOnly(t *testing.T) {
	events := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "events:"+r.URL.Path)
	})
	relay := relaytest.New(t, testKey, map[dialhome.StreamKind]http.Handler{dialhome.KindEvents: events})
	c := dial(t, relay, testKey)
	waitBothConnected(t, relay, c)

	fwd, err := c.ForwardLocal(dialhome.KindEvents)
	require.NoError(t, err)
	defer func() { _ = fwd.Close() }()
	resp, err := http.Get(fwd.URL() + "/api/v1/sessions")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, "events:/api/v1/sessions", string(body))

	for _, kind := range []dialhome.StreamKind{dialhome.KindMemory, dialhome.KindRuntime, dialhome.StreamKind(9)} {
		f, err := c.ForwardLocal(kind)
		require.NoError(t, err)
		_, err = (&http.Client{Timeout: 2 * time.Second}).Get(f.URL() + "/anything")
		require.Error(t, err, "kind %s must not be served", kind)
		_ = f.Close()
	}
}

func TestClient_WithoutAChannel(t *testing.T) {
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: "ws://127.0.0.1:1/v1/channel", Key: testKey, MinBackoff: time.Hour})
	require.NoError(t, err)
	require.Equal(t, "dial-home", c.Addr().Network())
	require.Contains(t, c.Addr().String(), "127.0.0.1:1")
	_, err = c.OpenStream(dialhome.KindEvents)
	require.ErrorIs(t, err, dialhome.ErrNotConnected)

	f, err := c.ForwardLocal(dialhome.KindEvents)
	require.NoError(t, err)
	_, err = (&http.Client{Timeout: 2 * time.Second}).Get(f.URL())
	require.Error(t, err)
	require.NoError(t, f.Close())

	go func() { _ = c.Close() }()
	_, err = c.Accept()
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestDial_RejectsAnInvalidConfig(t *testing.T) {
	_, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{URL: "ws://relay.example.com/v1/channel", Key: "k"})
	require.Error(t, err)
}

func configUpstream(cfg *dialhome.RemoteConfig, reads *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path != dialhome.ConfigPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(cfg)
	})
}

func modelCfg(m string) *dialhome.RemoteConfig {
	return &dialhome.RemoteConfig{Agent: "a", Providers: []dialhome.RemoteProvider{{
		Group: "default", Name: "llm", Default: true, Spec: dialhome.ProviderSpec{Type: "mock", Model: m}}}}
}

func TestConfig_OverTheChannelThenBaked(t *testing.T) {
	var reads atomic.Int32
	relay := relaytest.New(t, testKey, map[dialhome.StreamKind]http.Handler{
		dialhome.KindConfig: configUpstream(modelCfg("from-channel"), &reads),
	})
	c := dial(t, relay, testKey)
	waitBothConnected(t, relay, c)

	got, err := c.FetchConfig(context.Background())
	require.NoError(t, err)
	p, ok := got.Default()
	require.True(t, ok)
	assert.Equal(t, "from-channel", p.Spec.Model)

	src := dialhome.NewConfigSource(c, modelCfg("baked"))
	for range 3 {
		cfg, origin, err := src.Current(context.Background())
		require.NoError(t, err)
		assert.Equal(t, dialhome.OriginChannel, origin)
		assert.Equal(t, "from-channel", cfg.Providers[0].Spec.Model)
	}
	assert.Equal(t, int32(2), reads.Load(), "one FetchConfig, then one read per TTL")

	require.NoError(t, c.Close())
	cfg, origin, err := src.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, dialhome.OriginBaked, origin)
	assert.Equal(t, "baked", cfg.Providers[0].Spec.Model)
}

func TestConfig_RelayWithoutConfigKindFallsBack(t *testing.T) {
	relay := relaytest.New(t, testKey, nil)
	c := dial(t, relay, testKey)
	waitConnected(t, relay, 1)

	_, err := c.FetchConfig(context.Background())
	require.Error(t, err)
	_, origin, err := dialhome.NewConfigSource(c, modelCfg("baked")).Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, dialhome.OriginBaked, origin)
	_, _, err = dialhome.NewConfigSource(c, nil).Current(context.Background())
	assert.ErrorIs(t, err, dialhome.ErrNoConfig)
}
