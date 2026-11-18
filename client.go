// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Backoff defaults: konnectivity's 1s → 10s is the prior art, with a longer
// ceiling because a refused key will not fix itself in ten seconds.
const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = 30 * time.Second
	// stableSession is how long a session must have lived for the next
	// re-dial to start again from the minimum backoff.
	stableSession = 30 * time.Second
)

// ErrNotConnected is returned when the channel has no live session.
var ErrNotConnected = errors.New("dial-home: channel not connected")

// RefusedError is a handshake the relay answered with an HTTP status rather
// than an upgrade: 401 for a key it does not accept, 400 for a version it does
// not speak.
type RefusedError struct{ Status int }

func (e *RefusedError) Error() string {
	return fmt.Sprintf("dial-home: relay refused the channel: HTTP %d", e.Status)
}

// ClientConfig configures the runtime side of the channel.
type ClientConfig struct {
	// URL is the relay's connect-back URL, ws:// or wss://.
	URL string
	// Key is the Omnia-issued dial-home key.
	Key string
	// Log receives connection lifecycle events. Zero value discards.
	Log logr.Logger
	// TLSConfig overrides the TLS client config for wss:// URLs.
	TLSConfig *tls.Config
	// MinBackoff and MaxBackoff bound the re-dial delay.
	MinBackoff, MaxBackoff time.Duration
	// KeepAlive is the yamux keepalive interval.
	KeepAlive time.Duration
}

// ConfigFromEnv reads the runtime-side config from EnvURL and EnvKey. ok is
// false when neither is set — the runtime is not configured to dial home,
// which is a normal deployment, not an error.
func ConfigFromEnv(getenv func(string) string) (cfg ClientConfig, ok bool, err error) {
	u, k := getenv(EnvURL), getenv(EnvKey)
	if u == "" && k == "" {
		return ClientConfig{}, false, nil
	}
	cfg = ClientConfig{URL: u, Key: k}
	if err := cfg.Validate(); err != nil {
		return ClientConfig{}, false, err
	}
	return cfg, true, nil
}

// Validate checks the config: a key, and an absolute ws:// or wss:// URL,
// with ws:// only to a loopback or in-cluster host.
func (c ClientConfig) Validate() error {
	if c.Key == "" {
		return fmt.Errorf("dial-home: %s is required", EnvKey)
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("dial-home: %s must be an absolute ws:// or wss:// URL", EnvURL)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return fmt.Errorf("dial-home: %s scheme %q: want ws or wss", EnvURL, u.Scheme)
	}
	if u.Scheme == "ws" && !CleartextAllowed(u.Hostname()) {
		// The key rides the handshake's Authorization header on every
		// re-dial; in clear text over the internet anyone on the path could
		// take it and become the agent's runtime.
		return fmt.Errorf("dial-home: %s: ws:// is allowed only to loopback or in-cluster hosts; use wss://", EnvURL)
	}
	return nil
}

// CleartextAllowed reports whether ws:// may be used to host: only when its
// traffic never leaves the machine or the cluster — loopback, and
// cluster-local Service names (*.svc, *.svc.cluster.local).
func CleartextAllowed(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Client is the runtime side of the channel. It dials the relay, keeps the
// session up across drops, and is a net.Listener: the relay's runtime streams
// arrive on Accept, so a runtime serves omnia.runtime.v1 on it exactly as it
// would on a TCP listener.
type Client struct {
	cfg    ClientConfig
	log    logr.Logger
	accept chan net.Conn
	done   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup

	closeOnce sync.Once
	mu        sync.RWMutex
	sess      *yamux.Session
	// gen counts the sessions this client has had; 0 while none is live.
	gen, sessions uint64
}

var _ net.Listener = (*Client)(nil)

// Dial validates cfg and starts dialling home in the background. It returns
// at once; the channel connects (and reconnects) on its own until Close.
func Dial(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(defaultMaxBackoff, cfg.MinBackoff)
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &Client{
		cfg:    cfg,
		log:    cfg.Log,
		accept: make(chan net.Conn),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	c.wg.Add(1)
	go c.run(ctx)
	return c, nil
}

// Accept returns the next runtime stream the relay opened.
func (c *Client) Accept() (net.Conn, error) {
	select {
	case conn := <-c.accept:
		return conn, nil
	case <-c.done:
		return nil, net.ErrClosed
	}
}

// Close stops dialling and closes the live session.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.cancel()
		if s := c.session(); s != nil {
			_ = s.Close()
		}
	})
	c.wg.Wait()
	return nil
}

// Addr reports the relay URL as the listener address.
func (c *Client) Addr() net.Addr { return Addr(c.cfg.URL) }

// Connected reports whether a session is live.
func (c *Client) Connected() bool { return c.session() != nil }

// OpenStream opens a stream of kind to the relay on the live session.
func (c *Client) OpenStream(kind StreamKind) (net.Conn, error) {
	s := c.session()
	if s == nil {
		return nil, ErrNotConnected
	}
	st, err := s.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := WriteStreamHeader(st, kind); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

func (c *Client) session() *yamux.Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sess
}

func (c *Client) setSession(s *yamux.Session) {
	c.mu.Lock()
	c.sess = s
	c.gen = 0
	if s != nil {
		c.sessions++
		c.gen = c.sessions
	}
	c.mu.Unlock()
}

// generation identifies the live session: a number that changes on every
// reconnect, or 0 when no session is live.
func (c *Client) generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen
}

// run dials, serves the session until it drops, and re-dials with a capped,
// jittered exponential backoff.
func (c *Client) run(ctx context.Context) {
	defer c.wg.Done()
	backoff := c.cfg.MinBackoff
	for {
		start := time.Now()
		err := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) >= stableSession {
			backoff = c.cfg.MinBackoff
		}
		c.logDrop(err, backoff)
		t := time.NewTimer(jitter(backoff))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(backoff*2, c.cfg.MaxBackoff)
	}
}

func (c *Client) logDrop(err error, backoff time.Duration) {
	var refused *RefusedError
	switch {
	case errors.As(err, &refused):
		c.log.Info("dial-home refused", "status", refused.Status, "retryIn", backoff.String())
	case err != nil:
		c.log.Info("dial-home failed", "err", err.Error(), "retryIn", backoff.String())
	default:
		c.log.Info("dial-home channel closed", "retryIn", backoff.String())
	}
}

// jitter spreads re-dials over [d/2, d) so many instances do not reconnect in
// lockstep after a relay restart.
func jitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half) //nolint:gosec // jitter, not security
}

// connectOnce performs one handshake and blocks for the life of the session.
func (c *Client) connectOnce(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.cfg.Key)
	hdr.Set(HeaderVersion, ProtocolVersion)
	d := websocket.Dialer{
		HandshakeTimeout: HandshakeTimeout,
		TLSClientConfig:  c.cfg.TLSConfig,
		Proxy:            http.ProxyFromEnvironment,
	}
	ws, resp, err := d.DialContext(ctx, c.cfg.URL, hdr)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return &RefusedError{Status: resp.StatusCode}
		}
		return err
	}
	sess, err := yamux.Client(NewWSConn(ws), YamuxConfig(c.cfg.KeepAlive))
	if err != nil {
		_ = ws.Close()
		return err
	}
	c.setSession(sess)
	c.log.Info("dial-home channel connected", "url", c.cfg.URL)
	defer func() {
		c.setSession(nil)
		_ = sess.Close()
	}()
	go c.acceptLoop(sess)
	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}
	return nil
}

// acceptLoop hands the relay's runtime streams to Accept. Any other kind is
// closed: the relay has no business opening one.
func (c *Client) acceptLoop(sess *yamux.Session) {
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go c.route(st)
	}
}

func (c *Client) route(st net.Conn) {
	kind, err := ReadStreamHeader(st)
	if err != nil || kind != KindRuntime {
		c.log.V(1).Info("dial-home stream refused", "kind", kind.String())
		_ = st.Close()
		return
	}
	select {
	case c.accept <- st:
	case <-c.done:
		_ = st.Close()
	}
}

// Addr is the net.Addr of a channel endpoint: network "dial-home", and the
// relay URL (or another label) as its string.
type Addr string

// Network implements net.Addr.
func (a Addr) Network() string { return "dial-home" }

// String implements net.Addr.
func (a Addr) String() string { return string(a) }
