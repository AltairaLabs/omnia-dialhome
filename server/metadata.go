// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"
)

// gRPC metadata the facade attaches to every runtime call. The caller's
// identity travels here, never in a message field, and the raw bearer token
// is never sent.
const (
	HeaderAgentName = "x-omnia-agent-name"
	HeaderNamespace = "x-omnia-namespace"
	HeaderSessionID = "x-omnia-session-id"
	HeaderRequestID = "x-omnia-request-id"
	// HeaderUserID is the pseudonymised end user.
	HeaderUserID = "x-omnia-user-id"
	// HeaderSubjectID is the pseudonymised token holder, when it differs from
	// the end user.
	HeaderSubjectID = "x-omnia-subject-id"
	HeaderUserEmail = "x-omnia-user-email"
	HeaderProvider  = "x-omnia-provider"
	HeaderModel     = "x-omnia-model"
	HeaderOrigin    = "x-omnia-origin"
	HeaderWorkspace = "x-omnia-workspace"
	// HeaderClaimPrefix prefixes each mapped JWT claim.
	HeaderClaimPrefix = "x-omnia-claim-"
	// HeaderClientTools is "true" on a Converse call whose serving surface
	// forwards client tool calls to a client and returns their results.
	HeaderClientTools = "x-omnia-client-tools"
	// HeaderSessionOpen is "true" on the one Converse stream whose message
	// opened its session.
	HeaderSessionOpen = "x-omnia-session-open"
	// HeaderDeclare carries the client's session declaration, on the session
	// open stream only.
	HeaderDeclare = "x-omnia-declare-bin"
)

// Identity is the caller identity the facade attached to a call.
type Identity struct {
	AgentName string
	Namespace string
	SessionID string
	RequestID string
	UserID    string
	SubjectID string
	UserEmail string
	Provider  string
	Model     string
	Origin    string
	Workspace string
	// Claims are the mapped JWT claims, keyed by the name after
	// HeaderClaimPrefix.
	Claims map[string]string
}

// IdentityFromIncoming reads the caller identity from a call's incoming
// metadata. It is the zero Identity when there is none.
func IdentityFromIncoming(ctx context.Context) Identity {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return Identity{}
	}
	return Identity{
		AgentName: first(md, HeaderAgentName),
		Namespace: first(md, HeaderNamespace),
		SessionID: first(md, HeaderSessionID),
		RequestID: first(md, HeaderRequestID),
		UserID:    first(md, HeaderUserID),
		SubjectID: first(md, HeaderSubjectID),
		UserEmail: first(md, HeaderUserEmail),
		Provider:  first(md, HeaderProvider),
		Model:     first(md, HeaderModel),
		Origin:    first(md, HeaderOrigin),
		Workspace: first(md, HeaderWorkspace),
		Claims:    claims(md),
	}
}

// ClientToolsAvailable reports whether the facade marked the call as served
// by a surface that answers client tools. Anything but "true" — including a
// facade that never sends the header — reads as no client.
func ClientToolsAvailable(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	return ok && first(md, HeaderClientTools) == "true"
}

// SessionOpenFromIncoming reports whether a stream is its session's open, and
// the declaration it carries (nil for none).
func SessionOpenFromIncoming(ctx context.Context) (open bool, declare []byte) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || first(md, HeaderSessionOpen) != "true" {
		return false, nil
	}
	if v := md.Get(HeaderDeclare); len(v) > 0 {
		declare = []byte(v[0])
	}
	return true, declare
}

func first(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func claims(md metadata.MD) map[string]string {
	out := map[string]string{}
	for k, v := range md {
		if len(v) == 0 {
			continue
		}
		if name, ok := strings.CutPrefix(strings.ToLower(k), HeaderClaimPrefix); ok {
			out[name] = v[0]
		}
	}
	return out
}
