// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// wsConn presents a WebSocket as the byte stream yamux runs over. Each Write
// is one binary message; Read concatenates them. Text messages are ignored.
type wsConn struct {
	ws  *websocket.Conn
	r   io.Reader
	wmu sync.Mutex
}

var _ net.Conn = (*wsConn)(nil)

// NewWSConn presents ws as the byte stream both ends run yamux over: each
// Write is one binary message, and Read concatenates them, skipping any text
// message.
func NewWSConn(ws *websocket.Conn) net.Conn { return &wsConn{ws: ws} }

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.r == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error                       { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}

// DefaultKeepAlive is how often each end proves the session alive.
const DefaultKeepAlive = 15 * time.Second

// YamuxConfig is the session config both ends use: keepalives every
// keepAlive (DefaultKeepAlive when zero or less), and the same bound on a
// write to the connection.
func YamuxConfig(keepAlive time.Duration) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	if keepAlive <= 0 {
		keepAlive = DefaultKeepAlive
	}
	cfg.KeepAliveInterval = keepAlive
	cfg.ConnectionWriteTimeout = keepAlive
	cfg.LogOutput = io.Discard
	return cfg
}
