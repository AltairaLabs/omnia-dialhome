// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestBearerKey(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":   "abc",
		"bearer  abc ": "abc",
		"Bearer ":      "",
		"Basic abc":    "",
		"":             "",
	} {
		got, ok := BearerKey(in)
		require.Equal(t, want, got, in)
		require.Equal(t, want != "", ok, in)
	}
}

func TestStreamKindString(t *testing.T) {
	require.Equal(t, "runtime", KindRuntime.String())
	require.Equal(t, "memory", KindMemory.String())
	require.Equal(t, "events", KindEvents.String())
	require.Equal(t, "config", KindConfig.String())
	require.Equal(t, "unknown(7)", StreamKind(7).String())
}

// The wire bytes are the contract with every relay already deployed.
func TestStreamKinds_AreTheWireBytes(t *testing.T) {
	require.Equal(t, []byte{1, 2, 3, 4},
		[]byte{byte(KindRuntime), byte(KindMemory), byte(KindEvents), byte(KindConfig)})
	require.Equal(t, "/v1/channel", ChannelPath)
	require.Equal(t, "/v1/config", ConfigPath)
	require.Equal(t, "X-Omnia-Dial-Home-Version", HeaderVersion)
	require.Equal(t, "1", ProtocolVersion)
}

func TestStreamHeader_RoundTripsAndRejectsAnUnknownVersion(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	for _, kind := range []StreamKind{KindRuntime, KindMemory, KindEvents, KindConfig} {
		var got []byte
		done := make(chan struct{})
		go func() {
			defer close(done)
			got = make([]byte, 2)
			_, _ = io.ReadFull(b, got)
		}()
		require.NoError(t, WriteStreamHeader(a, kind))
		<-done
		require.Equal(t, []byte{1, byte(kind)}, got, "header for %s", kind)

		go func() { _ = WriteStreamHeader(a, kind) }()
		k, err := ReadStreamHeader(b)
		require.NoError(t, err)
		require.Equal(t, kind, k)
	}

	go func() { _, _ = a.Write([]byte{9, 1}) }()
	_, err := ReadStreamHeader(b)
	require.True(t, errors.Is(err, ErrBadStreamHeader))
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	_, ok, err := ConfigFromEnv(env(nil))
	require.NoError(t, err)
	require.False(t, ok, "neither set is a runtime that does not dial home")

	cfg, ok, err := ConfigFromEnv(env(map[string]string{EnvURL: "wss://relay.example/v1/channel", EnvKey: "k"}))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "k", cfg.Key)

	for _, bad := range []map[string]string{
		{EnvURL: "wss://relay.example/v1/channel"},
		{EnvKey: "k"},
		{EnvURL: "https://relay.example", EnvKey: "k"},
		{EnvURL: "relay", EnvKey: "k"},
		{EnvURL: "ws://relay.example.com/v1/channel", EnvKey: "k"},
	} {
		_, _, err := ConfigFromEnv(env(bad))
		require.Error(t, err, bad)
	}
}

func TestWSOnlyToLoopbackOrInClusterHosts(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "localhost": true,
		"agent-dial-home.ns.svc": true, "agent-dial-home.ns.svc.cluster.local": true,
		"relay.example.com": false, "10.0.0.1": false, "svc.example.com": false,
		"localhost.example.com": false,
	} {
		require.Equal(t, want, CleartextAllowed(host), host)
	}
	for url, ok := range map[string]bool{
		"ws://127.0.0.1:9002/v1/channel":                       true,
		"ws://[::1]:9002/v1/channel":                           true,
		"ws://agent-dial-home.ns.svc.cluster.local/v1/channel": true,
		"ws://relay.example.com/v1/channel":                    false,
		"ws://10.1.2.3/v1/channel":                             false,
		"wss://relay.example.com/v1/channel":                   true,
	} {
		err := ClientConfig{URL: url, Key: "k"}.Validate()
		require.Equal(t, ok, err == nil, "%s: %v", url, err)
	}
}

func TestJitter(t *testing.T) {
	for range 50 {
		j := jitter(time.Second)
		require.GreaterOrEqual(t, j, 500*time.Millisecond)
		require.Less(t, j, time.Second)
	}
	require.Equal(t, time.Duration(1), jitter(1))
}

func TestRefusedError(t *testing.T) {
	require.Contains(t, (&RefusedError{Status: 401}).Error(), "401")
}

// wsPair returns both ends of a real WebSocket as wsConns.
func wsPair(t *testing.T) (*wsConn, *wsConn) {
	t.Helper()
	srvSide := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err == nil {
			srvSide <- ws
		}
	}))
	t.Cleanup(srv.Close)
	cli, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	a, b := NewWSConn(cli).(*wsConn), NewWSConn(<-srvSide).(*wsConn)
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestWSConn_StreamsBinaryIgnoresTextAndHonoursDeadlines(t *testing.T) {
	a, b := wsPair(t)
	require.NotNil(t, a.LocalAddr())
	require.NotNil(t, a.RemoteAddr())

	require.NoError(t, b.ws.WriteMessage(websocket.TextMessage, []byte("ignored")))
	_, err := b.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 3)
	n, err := io.ReadFull(a, buf)
	require.NoError(t, err)
	require.Equal(t, "hel", string(buf[:n]))
	n, err = io.ReadFull(a, buf[:2])
	require.NoError(t, err)
	require.Equal(t, "lo", string(buf[:n]))

	require.NoError(t, a.SetDeadline(time.Now().Add(20*time.Millisecond)))
	require.NoError(t, a.SetWriteDeadline(time.Time{}))
	_, err = a.Read(buf)
	require.Error(t, err, "a passed read deadline ends the read")
}

func TestYamuxConfig(t *testing.T) {
	require.Equal(t, DefaultKeepAlive, YamuxConfig(0).KeepAliveInterval)
	c := YamuxConfig(time.Second)
	require.True(t, c.EnableKeepAlive)
	require.Equal(t, time.Second, c.KeepAliveInterval)
	require.Equal(t, time.Second, c.ConnectionWriteTimeout)
}

func TestClient_RefusesANonRuntimeStreamFromTheRelay(t *testing.T) {
	c := &Client{log: logr.Discard(), accept: make(chan net.Conn), done: make(chan struct{})}
	x, y := net.Pipe()
	defer func() { _ = x.Close() }()
	go func() { _ = WriteStreamHeader(x, KindMemory) }()
	c.route(y)
	_, err := x.Read(make([]byte, 1))
	require.Error(t, err, "the stream is closed")

	// A runtime stream arriving after Close is closed too.
	x2, y2 := net.Pipe()
	defer func() { _ = x2.Close() }()
	close(c.done)
	go func() { _ = WriteStreamHeader(x2, KindRuntime) }()
	c.route(y2)
	_, err = x2.Read(make([]byte, 1))
	require.Error(t, err)
}

func TestClientGeneration(t *testing.T) {
	c := &Client{}
	require.Zero(t, c.generation())
	c.setSession(nil)
	require.Zero(t, c.generation())
}

func TestAddr(t *testing.T) {
	require.Equal(t, "dial-home", Addr("x").Network())
	require.Equal(t, "x", Addr("x").String())
}
