// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// Handshake contract.
const (
	// ChannelPath is where the relay accepts the dial-home connection.
	ChannelPath = "/v1/channel"
	// HeaderVersion carries the channel protocol version on the handshake.
	HeaderVersion = "X-Omnia-Dial-Home-Version"
	// ProtocolVersion is the only channel version this package speaks.
	ProtocolVersion = "1"
	// HandshakeTimeout bounds the WebSocket handshake, on either end.
	HandshakeTimeout = 15 * time.Second
)

// Runtime-side configuration, read from the hosted runtime's environment
// (ConfigFromEnv, BakedConfigFromEnv).
const (
	// EnvURL is the relay's connect-back URL (wss://.../v1/channel).
	EnvURL = "OMNIA_DIAL_HOME_URL"
	// EnvKey is the Omnia-issued dial-home key.
	EnvKey = "OMNIA_DIAL_HOME_KEY"
	// EnvConfig carries the baked RemoteConfig, as JSON, that a hosted runtime
	// falls back to when the channel is down. Optional.
	EnvConfig = "OMNIA_DIAL_HOME_CONFIG"
)

// BearerKey extracts the key from an Authorization header value. The scheme
// is matched case-insensitively and surrounding space is trimmed.
func BearerKey(h string) (string, bool) {
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	k := strings.TrimSpace(h[len(p):])
	return k, k != ""
}

// StreamKind names what a stream on the channel carries.
type StreamKind byte

// Stream kinds. Values are wire bytes; never renumber.
const (
	KindRuntime StreamKind = 1
	KindMemory  StreamKind = 2
	KindEvents  StreamKind = 3
	// KindConfig (runtime → relay, HTTP) serves the agent's resolved
	// configuration at ConfigPath. Added without a version bump: a relay that
	// predates it closes the stream, as the contract requires for any kind it
	// does not serve, and the runtime keeps its baked config.
	KindConfig StreamKind = 4
	// KindPolicy (runtime → relay, HTTP) decides one tool call the runtime is
	// about to run itself, at PolicyDecisionPath. Fail closed: a relay that
	// predates it closes the stream, and a tool with no decision does not run.
	KindPolicy StreamKind = 5
	// KindToolCalls (runtime → relay, HTTP) records a tool call the runtime
	// ran itself on the session it belongs to, at ToolCallsPath.
	KindToolCalls StreamKind = 6
	// KindMemoryStore (runtime → relay, HTTP) is the session's memory: the
	// Memory*Path operations, scoped by the relay to the session's user, not
	// by the runtime. KindMemory is the raw HTTP forward and stays as it was.
	KindMemoryStore StreamKind = 7
)

func (k StreamKind) String() string {
	switch k {
	case KindRuntime:
		return "runtime"
	case KindMemory:
		return "memory"
	case KindEvents:
		return "events"
	case KindConfig:
		return "config"
	case KindPolicy:
		return "policy"
	case KindToolCalls:
		return "tool-calls"
	case KindMemoryStore:
		return "memory-store"
	default:
		return fmt.Sprintf("unknown(%d)", byte(k))
	}
}

// streamVersion is the first byte of every stream header.
const streamVersion byte = 1

// headerTimeout bounds how long a peer may take to name a stream's kind.
const headerTimeout = 10 * time.Second

// ErrBadStreamHeader is returned for a stream header of an unknown version.
var ErrBadStreamHeader = errors.New("dial-home: bad stream header")

// WriteStreamHeader names a new stream's kind. The opener of every stream
// writes it before anything else.
func WriteStreamHeader(w io.Writer, k StreamKind) error {
	_, err := w.Write([]byte{streamVersion, byte(k)})
	return err
}

// ReadStreamHeader reads a stream's kind, bounded by a ten-second deadline.
func ReadStreamHeader(c net.Conn) (StreamKind, error) {
	_ = c.SetReadDeadline(time.Now().Add(headerTimeout))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return 0, err
	}
	if h[0] != streamVersion {
		return 0, ErrBadStreamHeader
	}
	return StreamKind(h[1]), nil
}
