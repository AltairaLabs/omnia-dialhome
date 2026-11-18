// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cfgWithModel(model string) *RemoteConfig {
	return &RemoteConfig{Agent: "a", Providers: []RemoteProvider{{
		Group: "default", Name: "llm", Default: true,
		Spec: ProviderSpec{Type: "mock", Model: model},
	}}}
}

// wireConfig is a RemoteConfig as a relay serves it, every field of the
// provider spec set. Decoding it and encoding it again must give the same
// JSON: a hosted runtime that drops a field it does not use would lose it for
// whatever reads the config next.
const wireConfig = `{
  "agent": "remote-vertex-dh",
  "packDigest": "sha256:abc",
  "providers": [{
    "group": "default", "name": "gemini-vertex", "default": true,
    "spec": {
      "type": "gemini", "role": "llm", "model": "gemini-2.5-pro",
      "baseURL": "https://proxy.example", "headers": {"X-Tenant": "t1"},
      "messageMetadataTemplate": "ctx: {{ticket}}",
      "platform": {"type": "vertex", "region": "us-central1", "project": "p1", "endpoint": "https://e"},
      "auth": {"type": "workloadIdentity"},
      "credential": {"envVar": "MY_KEY", "filePath": "/k"},
      "defaults": {"temperature": "0.2", "topP": "0.9", "maxTokens": 512, "thinkingBudgetTokens": 2048,
        "contextWindow": 100000, "truncationStrategy": "sliding", "requestTimeout": "90s",
        "streamIdleTimeout": "2m", "firstTokenTimeout": "30s"},
      "pricing": {"inputCostPer1K": "0.001", "outputCostPer1K": "0.002", "cachedCostPer1K": "0.0001"},
      "capabilities": ["text", "streaming"]
    }
  }, {
    "group": "voice", "name": "tts", "spec": {"type": "openai", "role": "tts",
      "tts": {"voice": "alloy", "sampleRate": 24000, "audioFiles": ["a.wav"], "format": "pcm"}}
  }, {
    "group": "ears", "name": "stt", "spec": {"type": "openai", "role": "stt", "stt": {"sampleRate": 16000, "language": "en"}}
  }, {
    "group": "vectors", "name": "emb", "spec": {"type": "openai", "role": "embedding", "embedding": {"dimensions": 1536}}
  }],
  "withheld": [{"group": "judge", "name": "keyed", "reason": "SecretBackedCredential"}]
}`

func TestRemoteConfig_RoundTripsTheWireForm(t *testing.T) {
	cfg := &RemoteConfig{}
	require.NoError(t, json.Unmarshal([]byte(wireConfig), cfg))
	out, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.JSONEq(t, wireConfig, string(out))

	p, ok := cfg.Default()
	require.True(t, ok)
	assert.Equal(t, "gemini-2.5-pro", p.Spec.Model)
	assert.Equal(t, PlatformVertex, p.Spec.Platform.Type)
	assert.Equal(t, WithheldSecretBacked, cfg.Withheld[0].Reason)
}

// fakeFetcher is a configFetcher whose generation and answer the test sets.
type fakeFetcher struct {
	gen   uint64
	cfg   *RemoteConfig
	err   error
	calls int
}

func (f *fakeFetcher) FetchConfig(context.Context) (*RemoteConfig, error) {
	f.calls++
	return f.cfg, f.err
}
func (f *fakeFetcher) generation() uint64 { return f.gen }

// fakeSource is a ConfigSource over f with a clock the test moves.
func fakeSource(f *fakeFetcher, baked *RemoteConfig) (*ConfigSource, *time.Time) {
	now := time.Unix(1_000_000, 0)
	return &ConfigSource{c: f, baked: baked, log: logr.Discard(), now: func() time.Time { return now }}, &now
}

func model(t *testing.T, s *ConfigSource) (string, ConfigOrigin) {
	t.Helper()
	cfg, origin, err := s.Current(context.Background())
	require.NoError(t, err)
	return cfg.Providers[0].Spec.Model, origin
}

func TestConfigSource_ReadsAtMostOncePerTTLAndAfreshOnReconnect(t *testing.T) {
	f := &fakeFetcher{gen: 1, cfg: cfgWithModel("v1")}
	src, now := fakeSource(f, cfgWithModel("baked"))

	m, origin := model(t, src)
	assert.Equal(t, "v1", m)
	assert.Equal(t, OriginChannel, origin)
	f.cfg = cfgWithModel("v2")
	m, _ = model(t, src)
	assert.Equal(t, "v1", m, "within the TTL the last read stands")

	*now = now.Add(configTTL)
	m, _ = model(t, src)
	assert.Equal(t, "v2", m, "a Provider change is picked up within the TTL")

	f.gen, f.cfg = 2, cfgWithModel("v3")
	m, _ = model(t, src)
	assert.Equal(t, "v3", m, "a new connection reads afresh")
	assert.Equal(t, 3, f.calls)
}

func TestConfigSource_FailedReadKeepsLastServedElseBaked(t *testing.T) {
	f := &fakeFetcher{gen: 1, err: errors.New("relay says 503")}
	src, now := fakeSource(f, cfgWithModel("baked"))

	m, origin := model(t, src)
	assert.Equal(t, "baked", m)
	assert.Equal(t, OriginBaked, origin)
	_, _ = model(t, src)
	assert.Equal(t, 1, f.calls, "a relay that cannot serve is not asked again within the TTL")

	*now = now.Add(configTTL)
	f.err, f.cfg = nil, cfgWithModel("v1")
	m, origin = model(t, src)
	assert.Equal(t, "v1", m)
	assert.Equal(t, OriginChannel, origin)

	*now = now.Add(configTTL)
	f.err = errors.New("blip")
	m, origin = model(t, src)
	assert.Equal(t, "v1", m, "a blip keeps what this connection last served")
	assert.Equal(t, OriginChannel, origin)

	f.gen = 2
	m, origin = model(t, src)
	assert.Equal(t, "baked", m, "another connection's config is not this one's")
	assert.Equal(t, OriginBaked, origin)
}

func TestConfigSource_NoChannelNoBakedIsErrNoConfig(t *testing.T) {
	src, _ := fakeSource(&fakeFetcher{}, nil)
	_, _, err := src.Current(context.Background())
	assert.ErrorIs(t, err, ErrNoConfig)
}

func TestConfigSource_WithheldProviderFilledFromBaked(t *testing.T) {
	served := &RemoteConfig{Agent: "a",
		Providers: []RemoteProvider{{Group: "judge", Name: "wi", Spec: ProviderSpec{Model: "served"}}},
		Withheld: []WithheldProvider{
			{Group: "default", Name: "keyed", Default: true, Reason: WithheldSecretBacked},
			{Group: "other", Name: "x", Reason: WithheldSecretBacked},
		}}
	baked := &RemoteConfig{Providers: []RemoteProvider{
		{Group: "default", Name: "own-key", Spec: ProviderSpec{Model: "baked-default"}},
		{Group: "judge", Name: "old", Spec: ProviderSpec{Model: "baked-judge"}},
	}}
	src, _ := fakeSource(&fakeFetcher{gen: 1, cfg: served}, baked)
	cfg, origin, err := src.Current(context.Background())
	require.NoError(t, err)
	assert.Equal(t, OriginChannel, origin)
	def, ok := cfg.Default()
	require.True(t, ok, "the withheld default comes from the runtime's own baked config")
	assert.Equal(t, "baked-default", def.Spec.Model)
	judge, _ := cfg.Group("judge")
	assert.Equal(t, "served", judge.Spec.Model, "what the channel serves wins")
	assert.Equal(t, []WithheldProvider{{Group: "other", Name: "x", Reason: WithheldSecretBacked}}, cfg.Withheld)
	assert.Len(t, served.Providers, 1, "the served config is not modified")

	assert.Same(t, served, fillWithheld(served, nil))
}

func TestBakedConfigFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvConfig {
				return v
			}
			return ""
		}
	}
	_, ok, err := BakedConfigFromEnv(env(""))
	require.NoError(t, err)
	assert.False(t, ok)

	cfg, ok, err := BakedConfigFromEnv(env(wireConfig))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "gemini-2.5-pro", cfg.Providers[0].Spec.Model)

	_, _, err = BakedConfigFromEnv(env("{not json"))
	assert.Error(t, err)
}

func TestPlatformEndpoint_FoundryOverride(t *testing.T) {
	foundry := func(k string) string {
		if k == EnvFoundryProjectEndpoint {
			return "https://injected.services.ai.azure.com/api/projects/p"
		}
		return ""
	}
	none := func(string) string { return "" }
	provider := func(pt string) RemoteProvider {
		return RemoteProvider{Spec: ProviderSpec{Platform: &PlatformConfig{Type: pt, Endpoint: "https://spec.example"}}}
	}

	assert.Equal(t, "https://injected.services.ai.azure.com/api/projects/p",
		provider(PlatformAzure).PlatformEndpoint(foundry), "Foundry's endpoint wins on azure")
	assert.Equal(t, "https://spec.example", provider(PlatformAzure).PlatformEndpoint(none))
	assert.Equal(t, "https://spec.example", provider(PlatformVertex).PlatformEndpoint(foundry),
		"a Foundry endpoint is not a Vertex one")
	assert.Empty(t, RemoteProvider{}.PlatformEndpoint(foundry))
}

func TestRemoteConfigDefaultAndGroup(t *testing.T) {
	var nilCfg *RemoteConfig
	_, ok := nilCfg.Default()
	assert.False(t, ok)
	_, ok = nilCfg.Group("x")
	assert.False(t, ok)
	_, ok = (&RemoteConfig{Providers: []RemoteProvider{{Name: "x"}}}).Default()
	assert.False(t, ok, "a withheld default leaves none")
}
