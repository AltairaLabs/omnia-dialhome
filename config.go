// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// ConfigPath is where the relay serves RemoteConfig on a KindConfig stream.
const ConfigPath = "/v1/config"

// EnvFoundryProjectEndpoint is the project endpoint Azure AI Foundry injects
// into a hosted agent's container. On Foundry it wins over a provider's
// spec.platform.endpoint: it is the project inference proxy, which is what
// lets the agent's own identity call the model with no extra RBAC.
const EnvFoundryProjectEndpoint = "FOUNDRY_PROJECT_ENDPOINT"

// RemoteConfig is the configuration the relay resolves for its AgentRuntime
// and serves to the hosted runtime: what Omnia would have materialised for an
// in-cluster runtime, less anything secret.
type RemoteConfig struct {
	// Agent is the AgentRuntime's name.
	Agent string `json:"agent"`
	// PackDigest is the content digest the agent's PromptPack published, so
	// the hosted runtime can tell whether it runs the pack Omnia serves.
	PackDigest string `json:"packDigest,omitempty"`
	// Providers are the agent's providers the hosted runtime may use.
	Providers []RemoteProvider `json:"providers"`
	// Withheld are the agent's providers that were not served, and why.
	Withheld []WithheldProvider `json:"withheld,omitempty"`
}

// RemoteProvider is one of the agent's spec.providers entries, resolved.
type RemoteProvider struct {
	// Group is the entry's spec.providers[].name.
	Group string `json:"group"`
	// Name is the Provider object's name.
	Name string `json:"name"`
	// Default marks the entry the agent's default LLM comes from.
	Default bool `json:"default,omitempty"`
	// Spec is the Provider's spec with no Secret reference in it.
	Spec ProviderSpec `json:"spec"`
}

// WithheldProvider names a provider the relay did not serve.
type WithheldProvider struct {
	Group string `json:"group"`
	Name  string `json:"name"`
	// Default marks the agent's default provider.
	Default bool   `json:"default,omitempty"`
	Reason  string `json:"reason"`
}

// Withheld reasons.
const (
	// WithheldSecretBacked: the credential lives in a cluster Secret, which
	// never leaves the cluster.
	WithheldSecretBacked = "SecretBackedCredential"
	// WithheldCredentialHeader: spec.headers carries a credential
	// (Authorization, an API-key header), which is a secret even though it is
	// not in a Secret.
	WithheldCredentialHeader = "CredentialInHeaders" //nolint:gosec // a reason, not a credential
	// WithheldUnresolved: the Provider could not be read.
	WithheldUnresolved = "ProviderUnresolved"
)

// Default returns the default provider, if the relay served it.
func (c *RemoteConfig) Default() (RemoteProvider, bool) {
	if c == nil {
		return RemoteProvider{}, false
	}
	for _, p := range c.Providers {
		if p.Default {
			return p, true
		}
	}
	return RemoteProvider{}, false
}

// Group returns the provider served under group, if any.
func (c *RemoteConfig) Group(group string) (RemoteProvider, bool) {
	if c == nil {
		return RemoteProvider{}, false
	}
	for _, p := range c.Providers {
		if p.Group == group {
			return p, true
		}
	}
	return RemoteProvider{}, false
}

// PlatformEndpoint is the endpoint the hosted runtime should call for p. For
// an azure-platform provider on Foundry the platform-injected project
// endpoint wins over spec.platform.endpoint; everywhere else it is the spec's.
func (p RemoteProvider) PlatformEndpoint(getenv func(string) string) string {
	pl := p.Spec.Platform
	if pl == nil {
		return ""
	}
	if pl.Type == PlatformAzure {
		if e := getenv(EnvFoundryProjectEndpoint); e != "" {
			return e
		}
	}
	return pl.Endpoint
}

// BakedConfigFromEnv reads the baked fallback config from EnvConfig. ok is
// false when none is set.
func BakedConfigFromEnv(getenv func(string) string) (cfg *RemoteConfig, ok bool, err error) {
	raw := getenv(EnvConfig)
	if raw == "" {
		return nil, false, nil
	}
	cfg = &RemoteConfig{}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		return nil, false, fmt.Errorf("dial-home: %s is not a valid config: %w", EnvConfig, err)
	}
	return cfg, true, nil
}

// configFetchTimeout bounds one config read over the channel.
const configFetchTimeout = 10 * time.Second

// maxConfigBytes bounds a config body; an agent's providers are a few KB.
const maxConfigBytes = 4 << 20

// FetchConfig reads the agent's configuration from the relay over a
// KindConfig stream.
func (c *Client) FetchConfig(ctx context.Context) (*RemoteConfig, error) {
	hc := &http.Client{
		Timeout: configFetchTimeout,
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return c.OpenStream(KindConfig)
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://dial-home"+ConfigPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dial-home: fetch config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dial-home: fetch config: HTTP %d", resp.StatusCode)
	}
	cfg := &RemoteConfig{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxConfigBytes)).Decode(cfg); err != nil {
		return nil, fmt.Errorf("dial-home: decode config: %w", err)
	}
	return cfg, nil
}

// ConfigOrigin says where a ConfigSource's answer came from.
type ConfigOrigin string

// Config origins.
const (
	OriginChannel ConfigOrigin = "channel"
	OriginBaked   ConfigOrigin = "baked"
)

// ErrNoConfig is returned when the channel serves none and nothing was baked.
var ErrNoConfig = errors.New("dial-home: no configuration: channel unavailable and none baked")

// configFetcher is what ConfigSource needs of a Client.
type configFetcher interface {
	FetchConfig(ctx context.Context) (*RemoteConfig, error)
	generation() uint64
}

// configTTL is how long a read of the channel's config — or a failed one —
// stands before the next Current reads again. It bounds how stale a served
// config can be after a Provider changes, and how often a relay that cannot
// serve one is asked.
const configTTL = 30 * time.Second

// ConfigSource is a hosted runtime's configuration: the channel's config wins
// while the channel is connected, and the baked config is the fallback. A
// provider the relay withheld (its credential stays in the cluster) is filled
// from the baked config's entry for the same group, when it has one: that is
// a provider the hosted runtime holds its own credential for.
//
// The channel's config is read on demand, at most once per 30 seconds per
// connection; no background reads. A failed read keeps the last config that
// connection served, else falls back to the baked one, and is logged.
type ConfigSource struct {
	c     configFetcher
	baked *RemoteConfig
	log   logr.Logger
	now   func() time.Time

	mu     sync.Mutex
	gen    uint64        // the connection the fields below belong to
	cached *RemoteConfig // the last config that connection served, if any
	readAt time.Time     // when it was last read, or a read last failed
}

// NewConfigSource builds a ConfigSource over c. baked may be nil.
func NewConfigSource(c *Client, baked *RemoteConfig) *ConfigSource {
	return &ConfigSource{c: c, baked: baked, log: c.log, now: time.Now}
}

// Current returns the configuration to use now and where it came from.
func (s *ConfigSource) Current(ctx context.Context) (*RemoteConfig, ConfigOrigin, error) {
	if gen := s.c.generation(); gen != 0 {
		if cfg := s.fromChannel(ctx, gen); cfg != nil {
			return fillWithheld(cfg, s.baked), OriginChannel, nil
		}
	}
	if s.baked != nil {
		return s.baked, OriginBaked, nil
	}
	return nil, "", ErrNoConfig
}

// fromChannel returns the channel's config for connection gen, reading it
// when this connection's last read is older than configTTL; nil when the
// connection has served none. The lock is not held across the read.
func (s *ConfigSource) fromChannel(ctx context.Context, gen uint64) *RemoteConfig {
	s.mu.Lock()
	if s.gen == gen && s.now().Sub(s.readAt) < configTTL {
		cfg := s.cached
		s.mu.Unlock()
		return cfg
	}
	s.mu.Unlock()

	cfg, err := s.c.FetchConfig(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen {
		s.cached = nil // another connection's config says nothing of this one
	}
	s.gen, s.readAt = gen, s.now()
	if err != nil {
		s.log.Info("dial-home config unavailable", "err", err.Error(),
			"hasLastServed", s.cached != nil, "hasBaked", s.baked != nil)
		return s.cached
	}
	s.cached = cfg
	return cfg
}

// fillWithheld is ch with each withheld provider replaced by baked's entry
// for the same group, when baked has one. The relay's default flag wins.
func fillWithheld(ch, baked *RemoteConfig) *RemoteConfig {
	if baked == nil || len(ch.Withheld) == 0 {
		return ch
	}
	out := *ch
	out.Providers = append([]RemoteProvider(nil), ch.Providers...)
	out.Withheld = nil
	for _, w := range ch.Withheld {
		b, ok := baked.Group(w.Group)
		if !ok {
			out.Withheld = append(out.Withheld, w)
			continue
		}
		b.Default = w.Default
		out.Providers = append(out.Providers, b)
	}
	return &out
}
