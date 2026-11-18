// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package providerspec

import (
	"context"
	"testing"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/credentials"
	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	_ "github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock" // registers "mock"
	"github.com/AltairaLabs/PromptKit/runtime/v2/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

func ptr[T any](v T) *T { return &v }

func noEnv(string) string { return "" }

func TestSpec_CopiesDefaultsPricingAndPlatform(t *testing.T) {
	p := dialhome.RemoteProvider{Name: "azure-gpt", Spec: dialhome.ProviderSpec{
		Type: "openai", Model: "gpt-5", BaseURL: "https://b", Headers: map[string]string{"X": "1"},
		MessageMetadataTemplate: "{{.x}}",
		Platform: &dialhome.PlatformConfig{Type: dialhome.PlatformAzure,
			Endpoint: "https://spec.example", Region: "eastus"},
		Auth:     &dialhome.AuthConfig{Type: "workloadIdentity"},
		Defaults: &dialhome.ProviderDefaults{Temperature: ptr("0.5"), TopP: ptr("0.9"), MaxTokens: ptr(int32(256))},
		Pricing:  &dialhome.ProviderPricing{InputCostPer1K: ptr("0.01"), OutputCostPer1K: ptr("0.03")},
	}}
	foundry := func(k string) string {
		if k == dialhome.EnvFoundryProjectEndpoint {
			return "https://injected"
		}
		return ""
	}

	spec, err := Spec(p, foundry)
	require.NoError(t, err)
	assert.Equal(t, "azure-gpt", spec.ID)
	assert.Equal(t, "openai", spec.Type)
	assert.Equal(t, "gpt-5", spec.Model)
	assert.Equal(t, "{{.x}}", spec.MessageMetadataTemplate)
	assert.InDelta(t, 0.5, spec.Defaults.Temperature, 1e-6)
	assert.InDelta(t, 0.9, spec.Defaults.TopP, 1e-6)
	assert.Equal(t, 256, spec.Defaults.MaxTokens)
	assert.InDelta(t, 0.01, spec.Defaults.Pricing.InputCostPer1K, 1e-9)
	assert.InDelta(t, 0.03, spec.Defaults.Pricing.OutputCostPer1K, 1e-9)
	assert.Equal(t, "azure", spec.Platform)
	require.NotNil(t, spec.PlatformConfig)
	assert.Equal(t, "https://injected", spec.PlatformConfig.Endpoint, "Foundry's endpoint wins")
	assert.Equal(t, "eastus", spec.PlatformConfig.Region)

	spec, err = Spec(p, noEnv)
	require.NoError(t, err)
	assert.Equal(t, "https://spec.example", spec.PlatformConfig.Endpoint)
}

func TestSpec_MalformedDecimalsAreErrors(t *testing.T) {
	for _, s := range []dialhome.ProviderSpec{
		{Defaults: &dialhome.ProviderDefaults{Temperature: ptr("hot")}},
		{Defaults: &dialhome.ProviderDefaults{TopP: ptr("x")}},
		{Pricing: &dialhome.ProviderPricing{InputCostPer1K: ptr("x")}},
		{Pricing: &dialhome.ProviderPricing{OutputCostPer1K: ptr("x")}},
	} {
		_, err := Spec(dialhome.RemoteProvider{Spec: s}, noEnv)
		assert.Error(t, err)
		_, err = New(context.Background(), dialhome.RemoteProvider{Spec: s}, noEnv)
		assert.Error(t, err)
	}
}

func TestNew_MockProviderAnswersWithTheServedModel(t *testing.T) {
	p, err := New(context.Background(), dialhome.RemoteProvider{Name: "llm",
		Spec: dialhome.ProviderSpec{Type: "mock", Model: "served-model"}}, noEnv)
	require.NoError(t, err)
	resp, err := p.Predict(context.Background(), providers.PredictionRequest{
		Messages: []types.Message{{Role: "user", Content: "hi"}}})
	require.NoError(t, err)
	assert.Contains(t, resp.Content, "served-model")
}

func TestResolverConfig(t *testing.T) {
	direct := dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{Type: "openai"}}
	spec, _ := Spec(direct, noEnv)
	_, ok := resolverConfig(direct, spec)
	assert.False(t, ok, "no credential block: PromptKit's own env lookup")

	envVar := dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{Type: "openai",
		Credential: &dialhome.CredentialConfig{EnvVar: "MY_KEY"}}}
	spec, _ = Spec(envVar, noEnv)
	rc, ok := resolverConfig(envVar, spec)
	require.True(t, ok)
	assert.Equal(t, "MY_KEY", rc.CredentialConfig.CredentialEnv)

	platform := dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{Type: "openai",
		Platform: &dialhome.PlatformConfig{Type: dialhome.PlatformVertex}}}
	spec, _ = Spec(platform, noEnv)
	rc, ok = resolverConfig(platform, spec)
	require.True(t, ok)
	assert.Equal(t, "vertex", rc.PlatformConfig.Type)
}

func TestNew_UnresolvableCredentialIsAnError(t *testing.T) {
	_, err := New(context.Background(), dialhome.RemoteProvider{Name: "x", Spec: dialhome.ProviderSpec{
		Type: "openai", Credential: &dialhome.CredentialConfig{EnvVar: "OMNIA_DIALHOME_TEST_UNSET_KEY"}}}, noEnv)
	assert.Error(t, err)
}

func TestSpec_MatchesTheInClusterMapping(t *testing.T) {
	spec, err := Spec(dialhome.RemoteProvider{Name: "c", Spec: dialhome.ProviderSpec{
		Type: "claude", Model: "claude-3-5-haiku-20241022",
		Platform: &dialhome.PlatformConfig{Type: dialhome.PlatformBedrock, Region: "us-east-1"},
		Defaults: &dialhome.ProviderDefaults{ThinkingBudgetTokens: ptr(int32(2048)),
			RequestTimeout: "45s", StreamIdleTimeout: "2m"},
	}}, noEnv)
	require.NoError(t, err)
	assert.InDelta(t, 0.7, spec.Defaults.Temperature, 1e-6, "Omnia's default temperature where none is set")
	assert.Equal(t, 2048, spec.AdditionalConfig["thinking_budget"])
	assert.Equal(t, 45*time.Second, spec.RequestTimeout)
	assert.Equal(t, 2*time.Minute, spec.StreamIdleTimeout)
	assert.Equal(t, credentials.BedrockModelMapping["claude-3-5-haiku-20241022"], spec.Model,
		"a Claude release name is mapped to its Bedrock model ID")

	for _, d := range []string{"soon", "-1s"} {
		_, err := Spec(dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{
			Defaults: &dialhome.ProviderDefaults{RequestTimeout: d}}}, noEnv)
		assert.Error(t, err, d)
		_, err = Spec(dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{
			Defaults: &dialhome.ProviderDefaults{StreamIdleTimeout: d}}}, noEnv)
		assert.Error(t, err, d)
	}
	_, err = Spec(dialhome.RemoteProvider{Spec: dialhome.ProviderSpec{
		Defaults: &dialhome.ProviderDefaults{MaxTokens: ptr(int32(5))}}}, noEnv)
	assert.NoError(t, err)
}
