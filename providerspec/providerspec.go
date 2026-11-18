// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Package providerspec turns a provider served over the dial-home channel
// into a PromptKit provider, for a hosted runtime built on PromptKit.
//
// The spec arrives from Omnia; the credential comes from where the runtime
// runs — its platform identity for a workloadIdentity provider, its own
// environment otherwise — and on Foundry the platform-injected project
// endpoint wins over the spec's. It is kept out of package dialhome so the
// channel itself does not depend on PromptKit.
//
// It maps what a PromptKit ProviderSpec can carry, as Omnia's in-cluster
// runtime does: model (Bedrock release names mapped to model IDs), base URL,
// headers, metadata template, sampling defaults (temperature defaulting to
// 0.7), maxTokens, input/output pricing, request and stream-idle timeouts,
// the thinking budget and platform. Settings Omnia's runtime applies outside
// the provider — contextWindow, truncationStrategy, firstTokenTimeout,
// cachedCostPer1K — are the hosted runtime's to apply.
package providerspec

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/credentials"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

// thinkingBudgetKey is the AdditionalConfig key PromptKit's claude provider
// reads to enable extended thinking. An unrecognised key is ignored, so a
// typo here would be silent.
const thinkingBudgetKey = "thinking_budget"

// defaultTemperature is Omnia's in-cluster runtime default, applied where a
// Provider sets none, so a hosted agent samples the same way.
const defaultTemperature = float32(0.7)

// Spec maps p to a PromptKit ProviderSpec with no credential resolved.
// getenv supplies the platform-injected endpoint (os.Getenv in production).
func Spec(p dialhome.RemoteProvider, getenv func(string) string) (providers.ProviderSpec, error) {
	s := p.Spec
	spec := providers.ProviderSpec{
		ID:                      p.Name,
		Type:                    s.Type,
		Model:                   s.Model,
		BaseURL:                 s.BaseURL,
		Headers:                 s.Headers,
		MessageMetadataTemplate: s.MessageMetadataTemplate,
	}
	if err := applyDefaults(&spec, p); err != nil {
		return providers.ProviderSpec{}, err
	}
	if d := s.Defaults; d != nil && d.ThinkingBudgetTokens != nil && *d.ThinkingBudgetTokens > 0 {
		spec.AdditionalConfig = map[string]any{thinkingBudgetKey: int(*d.ThinkingBudgetTokens)}
	}
	if s.Platform != nil {
		spec.Platform = s.Platform.Type
		if s.Platform.Type == dialhome.PlatformBedrock {
			if id, ok := credentials.BedrockModelMapping[spec.Model]; ok {
				spec.Model = id
			}
		}
		spec.PlatformConfig = &providers.PlatformConfig{
			Type:     s.Platform.Type,
			Region:   s.Platform.Region,
			Project:  s.Platform.Project,
			Endpoint: p.PlatformEndpoint(getenv),
		}
	}
	return spec, nil
}

// New builds the PromptKit provider for p, resolving its credential where the
// runtime runs: the platform's credential chain for a platform provider, the
// runtime's own environment or file for a direct one.
func New(ctx context.Context, p dialhome.RemoteProvider, getenv func(string) string) (providers.Provider, error) {
	spec, err := Spec(p, getenv)
	if err != nil {
		return nil, err
	}
	if rc, ok := resolverConfig(p, spec); ok {
		cred, err := credentials.Resolve(ctx, rc)
		if err != nil {
			return nil, fmt.Errorf("provider %s: resolve credential: %w", p.Name, err)
		}
		spec.Credential = cred
	}
	return providers.CreateProviderFromSpec(spec)
}

// resolverConfig is the credential resolution p needs, if any. A direct
// provider with no credential block is left to PromptKit's own lookup of its
// type's API-key variable.
func resolverConfig(p dialhome.RemoteProvider, spec providers.ProviderSpec) (credentials.ResolverConfig, bool) {
	rc := credentials.ResolverConfig{ProviderType: spec.Type}
	switch c := p.Spec.Credential; {
	case spec.PlatformConfig != nil:
		rc.PlatformConfig = spec.PlatformConfig
	case c != nil && (c.EnvVar != "" || c.FilePath != ""):
		rc.CredentialConfig = &credentials.CredentialConfig{CredentialEnv: c.EnvVar, CredentialFile: c.FilePath}
	default:
		return rc, false
	}
	return rc, true
}

// applyDefaults copies spec.defaults and spec.pricing. A malformed decimal is
// an error: silently meaning "provider default" is the failure it would
// otherwise be.
func applyDefaults(spec *providers.ProviderSpec, p dialhome.RemoteProvider) error {
	if d := p.Spec.Defaults; d != nil {
		if err := applySampling(spec, d); err != nil {
			return err
		}
	}
	if spec.Defaults.Temperature <= 0 {
		spec.Defaults.Temperature = defaultTemperature
	}
	if pr := p.Spec.Pricing; pr != nil {
		if err := parseFloat64("inputCostPer1K", pr.InputCostPer1K, &spec.Defaults.Pricing.InputCostPer1K); err != nil {
			return err
		}
		return parseFloat64("outputCostPer1K", pr.OutputCostPer1K, &spec.Defaults.Pricing.OutputCostPer1K)
	}
	return nil
}

// applySampling copies the sampling defaults and timeouts.
func applySampling(spec *providers.ProviderSpec, d *dialhome.ProviderDefaults) error {
	if err := parseFloat32("temperature", d.Temperature, &spec.Defaults.Temperature); err != nil {
		return err
	}
	if err := parseFloat32("topP", d.TopP, &spec.Defaults.TopP); err != nil {
		return err
	}
	if d.MaxTokens != nil {
		spec.Defaults.MaxTokens = int(*d.MaxTokens)
	}
	if err := parseDuration("requestTimeout", d.RequestTimeout, &spec.RequestTimeout); err != nil {
		return err
	}
	return parseDuration("streamIdleTimeout", d.StreamIdleTimeout, &spec.StreamIdleTimeout)
}

func parseDuration(field, raw string, dst *time.Duration) error {
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return fmt.Errorf("parse %s %q: must be a non-negative duration", field, raw)
	}
	*dst = d
	return nil
}

func parseFloat32(field string, raw *string, dst *float32) error {
	var v float64
	if err := parseFloat64(field, raw, &v); err != nil {
		return err
	}
	if raw != nil {
		*dst = float32(v)
	}
	return nil
}

func parseFloat64(field string, raw *string, dst *float64) error {
	if raw == nil {
		return nil
	}
	v, err := strconv.ParseFloat(*raw, 64)
	if err != nil {
		return fmt.Errorf("parse %s %q: %w", field, *raw, err)
	}
	*dst = v
	return nil
}
