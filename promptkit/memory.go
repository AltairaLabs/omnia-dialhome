// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package promptkit

import (
	"context"
	"errors"

	"github.com/AltairaLabs/PromptKit/runtime/v2/memory"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/AltairaLabs/PromptKit/sdk/v2"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// MemoryService is the session's memory, kept by the relay.
// *dialhome.Governance is one.
type MemoryService interface {
	MemoryScope(ctx context.Context, sessionID string) (dialhome.MemoryScope, error)
	Remember(ctx context.Context, sessionID string, m dialhome.MemoryRecord) (string, error)
	Recall(ctx context.Context, req dialhome.MemoryRecallRequest) ([]dialhome.MemoryRecord, error)
	ListMemories(ctx context.Context, req dialhome.MemoryListRequest) ([]dialhome.MemoryRecord, error)
	Forget(ctx context.Context, sessionID, id string) error
}

// memorySubject is the value of the memory scope's subject key. PromptKit
// offers the memory tools only when the scope names a subject; who it is is
// the relay's to know, from the session, so the value here only says "a user
// the relay will scope to".
const memorySubject = "session-user"

// retrievalLimit bounds the memories recalled into a turn's context.
const retrievalLimit = 5

// errForgetAll is DeleteAll: erasing a user's memory is not the agent's to do.
var errForgetAll = errors.New("forgetting every memory is not available to the agent")

// memoryOptions are the SDK options that give a conversation the session's
// memory, as the relay scopes it, or nil when it has none.
func memoryOptions(scope dialhome.MemoryScope, svc MemoryService, sessionID string) []sdk.Option {
	if !scope.Enabled {
		return nil
	}
	store := &memoryStore{svc: svc, sessionID: sessionID}
	pkScope := map[string]string{}
	if scope.User {
		pkScope[sdk.DefaultMemorySubjectKey] = memorySubject
	}
	var opts []sdk.MemoryOption
	if !scope.Tools {
		opts = append(opts, sdk.WithMemoryToolsDisabled())
	}
	if scope.Retrieval {
		opts = append(opts, sdk.WithMemoryRetriever(store))
	}
	return []sdk.Option{sdk.WithMemory(store, pkScope, opts...)}
}

// memoryStore is a PromptKit memory.Store and memory.Retriever over the
// session's memory. It ignores the scope PromptKit hands it: the relay
// scopes every request to the session's user and workspace itself.
type memoryStore struct {
	svc       MemoryService
	sessionID string
}

var (
	_ memory.Store     = (*memoryStore)(nil)
	_ memory.Retriever = (*memoryStore)(nil)
)

// Save implements memory.Store.
func (s *memoryStore) Save(ctx context.Context, m *memory.Memory) error {
	id, err := s.svc.Remember(ctx, s.sessionID, dialhome.MemoryRecord{
		Type: m.Type, Content: m.Content, Metadata: m.Metadata, Confidence: m.Confidence,
	})
	if err != nil {
		return err
	}
	m.ID = id
	return nil
}

// Retrieve implements memory.Store.
func (s *memoryStore) Retrieve(
	ctx context.Context, _ map[string]string, query string, opts memory.RetrieveOptions,
) ([]*memory.Memory, error) {
	recs, err := s.svc.Recall(ctx, dialhome.MemoryRecallRequest{
		SessionID: s.sessionID, Query: query, Types: opts.Types,
		Limit: opts.Limit, MinConfidence: opts.MinConfidence,
	})
	return toMemories(recs), err
}

// List implements memory.Store.
func (s *memoryStore) List(ctx context.Context, _ map[string]string, opts memory.ListOptions) ([]*memory.Memory, error) {
	recs, err := s.svc.ListMemories(ctx, dialhome.MemoryListRequest{
		SessionID: s.sessionID, Types: opts.Types, Limit: opts.Limit, Offset: opts.Offset,
	})
	return toMemories(recs), err
}

// Delete implements memory.Store.
func (s *memoryStore) Delete(ctx context.Context, _ map[string]string, id string) error {
	return s.svc.Forget(ctx, s.sessionID, id)
}

// DeleteAll implements memory.Store. It is always refused.
func (s *memoryStore) DeleteAll(context.Context, map[string]string) error { return errForgetAll }

// RetrieveContext implements memory.Retriever: a recall on the latest user
// message.
func (s *memoryStore) RetrieveContext(
	ctx context.Context, _ map[string]string, msgs []types.Message,
) ([]*memory.Memory, error) {
	q := lastUserText(msgs)
	if q == "" {
		return nil, nil
	}
	recs, err := s.svc.Recall(ctx, dialhome.MemoryRecallRequest{SessionID: s.sessionID, Query: q, Limit: retrievalLimit})
	return toMemories(recs), err
}

func lastUserText(msgs []types.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].GetContent()
		}
	}
	return ""
}

func toMemories(recs []dialhome.MemoryRecord) []*memory.Memory {
	if len(recs) == 0 {
		return nil
	}
	out := make([]*memory.Memory, 0, len(recs))
	for _, r := range recs {
		out = append(out, &memory.Memory{
			ID: r.ID, Type: r.Type, Content: r.Content, Metadata: r.Metadata,
			Confidence: r.Confidence, CreatedAt: r.CreatedAt,
		})
	}
	return out
}
