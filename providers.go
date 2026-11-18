// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package dialhome

// ProviderSpec is an Omnia Provider's spec as the relay serves it, field for
// field and in the same JSON as the Provider resource's spec. It is a plain
// mirror so a hosted runtime can read it without Omnia's API types; fields it
// does not use are kept so a config passes through unchanged.
//
// The relay serves no secret: a provider whose credential lives in a cluster
// Secret, or rides in Headers, is withheld instead (WithheldProvider), so on a
// served provider SecretRef and CredentialsSecretRef are always empty.
type ProviderSpec struct {
	// Type is the provider's wire protocol / vendor: claude, openai, gemini,
	// ollama, vllm, mock, …
	Type string `json:"type"`
	// Role is llm (the default when empty), embedding, tts, stt, image,
	// inference or rerank.
	Role      string           `json:"role,omitempty"`
	TTS       *TTSConfig       `json:"tts,omitempty"`
	STT       *STTConfig       `json:"stt,omitempty"`
	Embedding *EmbeddingConfig `json:"embedding,omitempty"`
	// Model is the model identifier.
	Model string `json:"model,omitempty"`
	// BaseURL overrides the provider's default API endpoint.
	BaseURL string `json:"baseURL,omitempty"`
	// Headers are sent on every provider request.
	Headers map[string]string `json:"headers,omitempty"`
	// MessageMetadataTemplate renders a user message's metadata as text the
	// model reads.
	MessageMetadataTemplate string `json:"messageMetadataTemplate,omitempty"`
	// Platform is the hyperscaler hosting the provider, if any.
	Platform *PlatformConfig `json:"platform,omitempty"`
	// Auth is how the platform is authenticated to.
	Auth *AuthConfig `json:"auth,omitempty"`
	// Credential is where a direct provider's credential comes from.
	Credential *CredentialConfig `json:"credential,omitempty"`
	// Defaults are the provider's tuning parameters.
	Defaults *ProviderDefaults `json:"defaults,omitempty"`
	// Pricing overrides the built-in cost table.
	Pricing *ProviderPricing `json:"pricing,omitempty"`
	// Capabilities are free-form feature tags (text, streaming, vision, …).
	Capabilities []string `json:"capabilities,omitempty"`
}

// TTSConfig configures a tts-role provider.
type TTSConfig struct {
	Voice      string   `json:"voice,omitempty"`
	SampleRate int32    `json:"sampleRate,omitempty"`
	AudioFiles []string `json:"audioFiles,omitempty"`
	Format     string   `json:"format,omitempty"`
}

// STTConfig configures an stt-role provider.
type STTConfig struct {
	SampleRate int32  `json:"sampleRate,omitempty"`
	Language   string `json:"language,omitempty"`
}

// EmbeddingConfig configures an embedding-role provider.
type EmbeddingConfig struct {
	Dimensions int32 `json:"dimensions,omitempty"`
}

// SecretKeyRef names a key in a cluster Secret. Never set on a provider the
// relay serves.
type SecretKeyRef struct {
	Name string  `json:"name"`
	Key  *string `json:"key,omitempty"`
}

// CredentialConfig says where a direct provider's credential comes from. On a
// served provider only EnvVar or FilePath can be set, naming a variable or
// file in the hosted runtime's own environment.
type CredentialConfig struct {
	SecretRef *SecretKeyRef `json:"secretRef,omitempty"`
	EnvVar    string        `json:"envVar,omitempty"`
	FilePath  string        `json:"filePath,omitempty"`
}

// Platform types.
const (
	PlatformBedrock = "bedrock"
	PlatformVertex  = "vertex"
	PlatformAzure   = "azure"
)

// PlatformConfig is the hyperscaler hosting a provider.
type PlatformConfig struct {
	// Type is bedrock, vertex or azure.
	Type     string `json:"type"`
	Region   string `json:"region,omitempty"`
	Project  string `json:"project,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// AuthConfig is how a platform provider authenticates: workloadIdentity on a
// served provider, the identity the hosted runtime runs with.
type AuthConfig struct {
	Type                 string        `json:"type"`
	CredentialsSecretRef *SecretKeyRef `json:"credentialsSecretRef,omitempty"`
}

// ProviderDefaults are a provider's tuning parameters. Decimals are strings,
// as on the resource.
type ProviderDefaults struct {
	Temperature          *string `json:"temperature,omitempty"`
	TopP                 *string `json:"topP,omitempty"`
	MaxTokens            *int32  `json:"maxTokens,omitempty"`
	ThinkingBudgetTokens *int32  `json:"thinkingBudgetTokens,omitempty"`
	ContextWindow        *int32  `json:"contextWindow,omitempty"`
	TruncationStrategy   string  `json:"truncationStrategy,omitempty"`
	RequestTimeout       string  `json:"requestTimeout,omitempty"`
	StreamIdleTimeout    string  `json:"streamIdleTimeout,omitempty"`
	FirstTokenTimeout    string  `json:"firstTokenTimeout,omitempty"`
}

// ProviderPricing is a provider's cost per 1000 tokens, as decimal strings.
type ProviderPricing struct {
	InputCostPer1K  *string `json:"inputCostPer1K,omitempty"`
	OutputCostPer1K *string `json:"outputCostPer1K,omitempty"`
	CachedCostPer1K *string `json:"cachedCostPer1K,omitempty"`
}
