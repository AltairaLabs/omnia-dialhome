// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package dialhome is the hosted side of Omnia's dial-home channel: the
// connection a runtime hosted outside the cluster (Vertex Agent Runtime,
// AgentCore, Foundry, a VM on site) opens to the relay that stands in for it
// as its AgentRuntime's runtime container.
//
// The hosted runtime opens ONE outbound connection; nothing ever dials into
// it. Over that connection both ends run a yamux session, and every stream on
// it begins with a two-byte header naming its kind:
//
//	relay  → runtime   KindRuntime   omnia.runtime.v1, exactly as the facade
//	                                 speaks it (the relay forwards it unparsed)
//	runtime → relay    KindMemory    HTTP/1.1 to the agent's memory-api
//	runtime → relay    KindEvents    HTTP/1.1 to the agent's session-api
//	runtime → relay    KindConfig    HTTP/1.1, GET ConfigPath: the agent's
//	                                 resolved provider config (RemoteConfig)
//
// # Wire contract, version 1
//
//   - Transport: a WebSocket (wss:// in production) carrying the yamux byte
//     stream in binary messages. WebSocket, not a raw upgrade, because it is
//     the one upgrade every platform egress path and every Gateway passes.
//   - Handshake: GET ChannelPath with `Authorization: Bearer <key>` and
//     HeaderVersion: ProtocolVersion. The key is an Omnia-issued dial-home key
//     scoped to one AgentRuntime. A missing or unknown key is refused with 401
//     before the upgrade; an unsupported version with 400.
//   - Streams: the opener writes {streamVersion, kind} and then the stream is
//     the protocol named by kind. A kind the receiver does not serve is closed
//     at once — the channel is not a general-purpose tunnel.
//   - Liveness: yamux keepalives. A session that misses them closes, the
//     relay reports the runtime unavailable, and the runtime re-dials with a
//     capped exponential backoff.
//   - Revocation: when a key stops being valid the relay closes every live
//     session that authenticated with it.
//
// KindConfig was added after version 1 shipped, without a version bump: the
// rule above already makes a relay that predates it close the stream, and the
// runtime then keeps its baked configuration.
//
// # Configuration over the channel
//
// While the channel is up, the provider configuration the relay serves wins
// over whatever was baked into the hosted deployment; the baked config
// (EnvConfig) is the fallback when the channel is down or the relay serves
// none, and supplies a provider the relay withheld when it has one for the
// same group (ConfigSource). The relay serves only providers that need no
// in-cluster secret, and names the rest as withheld.
//
// Client is the runtime side. It is a net.Listener: serve omnia.runtime.v1 on
// it (package server does that for any Handler, package promptkit for a
// PromptKit pack) exactly as on a TCP listener.
package dialhome
