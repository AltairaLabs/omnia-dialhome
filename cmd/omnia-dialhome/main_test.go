// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	_ "github.com/AltairaLabs/PromptKit/runtime/v2/providers/mock" // registers "mock"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParse(t *testing.T) {
	o, err := parse(nil, env(map[string]string{
		"OMNIA_DIALHOME_PACK": "a.pack.json", "OMNIA_DIALHOME_PROMPT": "chat",
		"OMNIA_DIALHOME_CLIENT_TOOLS": " get_location, ,camera ", "PORT": "8080",
	}))
	require.NoError(t, err)
	assert.Equal(t, options{pack: "a.pack.json", prompt: "chat", clientTools: []string{"get_location", "camera"}, port: "8080"}, o)

	o, err = parse([]string{"-pack", "b.pack.json", "-prompt", "p"}, env(nil))
	require.NoError(t, err)
	assert.Equal(t, "b.pack.json", o.pack)
	assert.Empty(t, o.clientTools)

	_, err = parse(nil, env(nil))
	assert.Error(t, err)
}

func TestRun_RefusesWhatItCannotRun(t *testing.T) {
	assert.Equal(t, exitUsage, run(nil, env(nil)))
	pack := map[string]string{"OMNIA_DIALHOME_PACK": "a.pack.json", "OMNIA_DIALHOME_PROMPT": "chat"}
	assert.Equal(t, exitConfig, run(nil, env(pack)), "not configured to dial home")

	pack["OMNIA_DIAL_HOME_URL"] = "ws://relay.example.com/v1/channel"
	pack["OMNIA_DIAL_HOME_KEY"] = "k"
	assert.Equal(t, exitConfig, run(nil, env(pack)), "ws:// off the cluster")
}

func TestDefaultProvider(t *testing.T) {
	ch, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: "ws://127.0.0.1:1/v1/channel", Key: "k", MinBackoff: time.Hour})
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()
	log := logr.Discard()

	p, err := defaultProvider(dialhome.NewConfigSource(ch, nil), env(nil), log)(context.Background())
	require.NoError(t, err)
	assert.Nil(t, p, "no config at all: PromptKit chooses")

	noDefault := &dialhome.RemoteConfig{Providers: []dialhome.RemoteProvider{{Group: "judge", Name: "j",
		Spec: dialhome.ProviderSpec{Type: "mock"}}}}
	p, err = defaultProvider(dialhome.NewConfigSource(ch, noDefault), env(nil), log)(context.Background())
	require.NoError(t, err)
	assert.Nil(t, p)

	baked := &dialhome.RemoteConfig{Providers: []dialhome.RemoteProvider{{Group: "default", Name: "llm", Default: true,
		Spec: dialhome.ProviderSpec{Type: "mock", Model: "m"}}}}
	p, err = defaultProvider(dialhome.NewConfigSource(ch, baked), env(nil), log)(context.Background())
	require.NoError(t, err)
	require.NotNil(t, p, "the baked default is built while the channel is down")
	assert.Equal(t, "llm", p.ID())
	_ = p.Close()

	bad := &dialhome.RemoteConfig{Providers: []dialhome.RemoteProvider{{Group: "default", Name: "llm", Default: true,
		Spec: dialhome.ProviderSpec{Type: "mock", Defaults: &dialhome.ProviderDefaults{RequestTimeout: "soon"}}}}}
	_, err = defaultProvider(dialhome.NewConfigSource(ch, bad), env(nil), log)(context.Background())
	assert.Error(t, err)
}
