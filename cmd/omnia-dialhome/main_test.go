// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

// A PromptKit deploy adapter that runs this image in place of its runtime
// hands over the pack and agent under its own names.
func TestParse_DeployHandoff(t *testing.T) {
	o, err := parse(nil, env(map[string]string{
		"PROMPTPACK_PACK_JSON": `{"id":"p"}`, "PROMPTPACK_AGENT": "assistant",
		"OMNIA_DIALHOME_PORT": "8080", "PORT": "9090",
	}))
	require.NoError(t, err)
	assert.Equal(t, options{packJSON: `{"id":"p"}`, prompt: "assistant", port: "8080"}, o)

	// The command's own settings win over the handoff.
	o, err = parse([]string{"-prompt", "other"}, env(map[string]string{
		"OMNIA_DIALHOME_PACK": "a.pack.json", "PROMPTPACK_PACK_JSON": `{"id":"p"}`, "PROMPTPACK_AGENT": "assistant",
	}))
	require.NoError(t, err)
	assert.Equal(t, "a.pack.json", o.pack)
	assert.Empty(t, o.packJSON)
	assert.Equal(t, "other", o.prompt)

	_, err = parse(nil, env(map[string]string{
		"PROMPTPACK_PACK_URI": "gs://b/p.json", "PROMPTPACK_AGENT": "assistant",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gs://b/p.json")
	assert.Contains(t, err.Error(), "not supported")
}

func TestWritePack(t *testing.T) {
	path, cleanup, err := writePack(`{"id":"p"}`)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, `{"id":"p"}`, string(got))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	cleanup()
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "cleanup removes the pack")
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

func probe(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(`{"prompt":"hi"}`)))
	return rec
}

// Every platform's liveness check passes while the process runs, connected
// or not: a platform that restarts an unhealthy container must not kill one
// that is re-dialling an unreachable relay.
func TestProbeHandler_LiveWhetherOrNotConnected(t *testing.T) {
	for _, connected := range []bool{false, true} {
		h := probeHandler(func() bool { return connected })
		for _, path := range []string{"/healthz", "/", "/readiness", "/ping"} {
			assert.Equal(t, http.StatusOK, probe(t, h, http.MethodGet, path).Code, "GET %s, connected=%v", path, connected)
		}
	}
}

func TestProbeHandler_Readyz(t *testing.T) {
	assert.Equal(t, http.StatusServiceUnavailable,
		probe(t, probeHandler(func() bool { return false }), http.MethodGet, "/readyz").Code)
	assert.Equal(t, http.StatusOK, probe(t, probeHandler(func() bool { return true }), http.MethodGet, "/readyz").Code)
}

// AgentCore's /ping: Healthy while dialling, HealthyBusy while connected, so
// the session is kept alive while it carries conversations.
func TestProbeHandler_AgentCorePing(t *testing.T) {
	for connected, want := range map[bool]string{false: "Healthy", true: "HealthyBusy"} {
		rec := probe(t, probeHandler(func() bool { return connected }), http.MethodGet, "/ping")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, map[string]any{"status": want}, body, "connected=%v; no time_of_last_update", connected)
	}
}

func TestProbeHandler_InvocationsRefused(t *testing.T) {
	rec := probe(t, probeHandler(func() bool { return true }), http.MethodPost, "/invocations")
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "error", body["status"])
	assert.Contains(t, body["error"], "dial-home")
	assert.Contains(t, body["error"], "through Omnia")
}

func TestProbeHandler_UnknownRoutes(t *testing.T) {
	h := probeHandler(func() bool { return true })
	assert.Equal(t, http.StatusNotFound, probe(t, h, http.MethodGet, "/nope").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, probe(t, h, http.MethodGet, "/invocations").Code)
	assert.Equal(t, http.StatusMethodNotAllowed, probe(t, h, http.MethodPost, "/ping").Code)
}

// serveProbes answers on the real port and stops with its context.
func TestServeProbes(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()

	ch, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: "ws://127.0.0.1:1/v1/channel", Key: "k", MinBackoff: time.Hour, Log: logr.Discard()})
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { serveProbes(ctx, l, ch, logr.Discard()); close(done) }()

	resp, err := http.Get("http://" + addr + "/ping") //nolint:noctx // test
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "Healthy", body["status"], "the relay is unreachable")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveProbes did not stop with its context")
	}
}

// A probe port that cannot be bound stops the process, rather than leaving it
// up for a platform to fail every probe against.
func TestRun_ProbePortNotBound(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)

	assert.Equal(t, exitServe, run(nil, env(map[string]string{
		"OMNIA_DIALHOME_PACK": "a.pack.json", "OMNIA_DIALHOME_PROMPT": "chat",
		"OMNIA_DIAL_HOME_URL": "ws://127.0.0.1:1/v1/channel", "OMNIA_DIAL_HOME_KEY": "k",
		"OMNIA_DIALHOME_PORT": port,
	})))
}

func TestParse_Governance(t *testing.T) {
	o, err := parse(nil, env(map[string]string{
		"OMNIA_DIALHOME_PACK": "a.pack.json", "OMNIA_DIALHOME_PROMPT": "chat",
		"OMNIA_DIALHOME_POLICY_REGISTRY": "site-tools", "OMNIA_DIALHOME_MEMORY": "true",
	}))
	require.NoError(t, err)
	assert.Equal(t, "site-tools", o.registry)
	assert.True(t, o.memory)

	o, err = parse([]string{"-pack", "p", "-prompt", "q", "-policy-registry", "r"}, env(nil))
	require.NoError(t, err)
	assert.Equal(t, "r", o.registry)
	assert.False(t, o.memory, "memory is opt-in")
}

// Every tool the pack runs here is put to the policy and recorded; memory
// only when asked for.
func TestHandlerConfig_GovernsEveryConversation(t *testing.T) {
	c, err := dialhome.Dial(context.Background(), dialhome.ClientConfig{
		URL: "ws://127.0.0.1:1/v1/channel", Key: "k", Log: logr.Discard(),
	})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	src := dialhome.NewConfigSource(c, nil)

	cfg := handlerConfig(options{pack: "p", prompt: "q", registry: "site-tools"}, c, src, env(nil), logr.Discard())
	assert.NotNil(t, cfg.Policy)
	assert.NotNil(t, cfg.ToolCalls)
	assert.Nil(t, cfg.Memory)
	assert.Equal(t, "site-tools", cfg.PolicyRegistry)

	cfg = handlerConfig(options{pack: "p", prompt: "q", memory: true}, c, src, env(nil), logr.Discard())
	assert.NotNil(t, cfg.Memory)
}
