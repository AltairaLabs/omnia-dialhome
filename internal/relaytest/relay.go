// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package relaytest is an in-process stand-in for Omnia's relay, the cluster
// end of the dial-home channel, for tests. It speaks the handshake and stream
// headers the real relay does, and reaches the connected runtime over gRPC
// as the facade would.
package relaytest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// KeepAlive is the yamux keepalive the stand-in uses: short, so a test sees
// a dead session quickly.
const KeepAlive = 500 * time.Millisecond

// Relay is the stand-in.
type Relay struct {
	key       string
	srv       *httptest.Server
	upgrader  websocket.Upgrader
	listeners map[dialhome.StreamKind]*chanListener

	mu         sync.Mutex
	sessions   []*yamux.Session
	handshakes atomic.Int32
}

// New starts a relay that accepts key, and terminates the runtime's streams
// of each kind in upstreams with its handler. It stops at test cleanup.
func New(t testing.TB, key string, upstreams map[dialhome.StreamKind]http.Handler) *Relay {
	t.Helper()
	r := &Relay{
		key:       key,
		upgrader:  websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		listeners: map[dialhome.StreamKind]*chanListener{},
	}
	for kind, h := range upstreams {
		l := newChanListener()
		r.listeners[kind] = l
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() {
			_ = l.Close()
			_ = srv.Close()
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc(dialhome.ChannelPath, r.serve)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		r.Drop()
		r.srv.Close()
	})
	return r
}

// URL is the channel URL a client dials.
func (r *Relay) URL() string {
	return "ws" + strings.TrimPrefix(r.srv.URL, "http") + dialhome.ChannelPath
}

// HTTPURL is the relay's plain base URL, for raw handshake tests.
func (r *Relay) HTTPURL() string { return r.srv.URL }

// Handshakes is how many handshakes the relay accepted.
func (r *Relay) Handshakes() int { return int(r.handshakes.Load()) }

// Connected is the number of live sessions.
func (r *Relay) Connected() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.sessions {
		if !s.IsClosed() {
			n++
		}
	}
	return n
}

// Drop closes every live session, as a network blip would.
func (r *Relay) Drop() {
	r.mu.Lock()
	sessions := r.sessions
	r.sessions = nil
	r.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

// Conn is a gRPC connection to the connected runtime, as the facade has
// through the relay. Each new transport opens a runtime stream on the
// newest live session, so the connection follows the runtime across
// re-dials.
func (r *Relay) Conn(t testing.TB) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///dial-home",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			s := r.live()
			if s == nil {
				return nil, dialhome.ErrNotConnected
			}
			st, err := s.OpenStream()
			if err != nil {
				return nil, err
			}
			if err := dialhome.WriteStreamHeader(st, dialhome.KindRuntime); err != nil {
				_ = st.Close()
				return nil, err
			}
			return st, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func (r *Relay) live() *yamux.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.sessions) - 1; i >= 0; i-- {
		if !r.sessions[i].IsClosed() {
			return r.sessions[i]
		}
	}
	return nil
}

func (r *Relay) serve(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get(dialhome.HeaderVersion) != dialhome.ProtocolVersion {
		http.Error(w, "unsupported channel version", http.StatusBadRequest)
		return
	}
	if k, ok := dialhome.BearerKey(req.Header.Get("Authorization")); !ok || k != r.key {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ws, err := r.upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	sess, err := yamux.Server(dialhome.NewWSConn(ws), dialhome.YamuxConfig(KeepAlive))
	if err != nil {
		_ = ws.Close()
		return
	}
	r.handshakes.Add(1)
	r.mu.Lock()
	r.sessions = append(r.sessions, sess)
	r.mu.Unlock()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go r.dispatch(st)
	}
}

// dispatch hands a runtime-opened stream to its kind's upstream, or closes
// it: as the real relay does, it serves no kind it was not given, and never
// accepts a runtime stream from the runtime.
func (r *Relay) dispatch(st net.Conn) {
	kind, err := dialhome.ReadStreamHeader(st)
	l, ok := r.listeners[kind]
	if err != nil || !ok || kind == dialhome.KindRuntime {
		_ = st.Close()
		return
	}
	select {
	case l.conns <- st:
	case <-l.done:
		_ = st.Close()
	}
}

type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return dialhome.Addr("relaytest") }
