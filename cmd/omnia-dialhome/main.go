// Copyright 2026 Altaira Labs
// SPDX-License-Identifier: Apache-2.0

// Command omnia-dialhome runs a PromptKit pack as an Omnia agent's runtime
// from outside the cluster. It dials home to the agent's relay with the
// Omnia-issued key, serves omnia.runtime.v1 over that channel, and builds its
// model provider from the configuration the relay serves (falling back to
// OMNIA_DIAL_HOME_CONFIG, then to PromptKit's own choice from the
// environment).
//
//	OMNIA_DIAL_HOME_URL      the relay's wss://…/v1/channel URL (required)
//	OMNIA_DIAL_HOME_KEY      the agent's dial-home key (required)
//	OMNIA_DIAL_HOME_CONFIG   baked provider configuration, JSON (optional)
//	OMNIA_DIALHOME_PACK      the compiled .pack.json (or -pack)
//	OMNIA_DIALHOME_PROMPT    the pack prompt to run (or -prompt)
//	OMNIA_DIALHOME_CLIENT_TOOLS  comma-separated client tools (or -client-tools)
//	OMNIA_DIALHOME_PORT      when set, serve /healthz and /readyz on it
//	PORT                     the same, when OMNIA_DIALHOME_PORT is unset
//
// When it is deployed by a PromptKit deploy adapter in place of the adapter's
// own runtime, the adapter hands it the pack and the agent instead:
//
//	PROMPTPACK_PACK_JSON     the compiled pack, inline (when no pack is given)
//	PROMPTPACK_AGENT         the pack prompt to run (when no prompt is given)
//
// A pack the adapter staged to storage (PROMPTPACK_PACK_URI) is refused: give
// the pack another way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/AltairaLabs/PromptKit/runtime/v2/providers"
	"github.com/go-logr/logr"

	dialhome "github.com/AltairaLabs/omnia-dialhome"
	"github.com/AltairaLabs/omnia-dialhome/promptkit"
	"github.com/AltairaLabs/omnia-dialhome/providerspec"
	"github.com/AltairaLabs/omnia-dialhome/server"
)

// Exit codes.
const (
	exitOK = iota
	exitUsage
	exitConfig
	exitServe
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv))
}

// The deploy handoff: what a PromptKit deploy adapter sets on any container it
// deploys in place of its own runtime.
const (
	envHandoffPackJSON = "PROMPTPACK_PACK_JSON"
	envHandoffPackURI  = "PROMPTPACK_PACK_URI"
	envHandoffAgent    = "PROMPTPACK_AGENT"
)

// options are the command's settings.
type options struct {
	pack, prompt string
	// packJSON is a pack handed over inline, written to a file before use.
	packJSON    string
	clientTools []string
	port        string
}

func parse(args []string, getenv func(string) string) (options, error) {
	fs := flag.NewFlagSet("omnia-dialhome", flag.ContinueOnError)
	pack := fs.String("pack", getenv("OMNIA_DIALHOME_PACK"), "compiled PromptPack (.pack.json)")
	prompt := fs.String("prompt", firstSet(getenv, "OMNIA_DIALHOME_PROMPT", envHandoffAgent), "pack prompt to run")
	tools := fs.String("client-tools", getenv("OMNIA_DIALHOME_CLIENT_TOOLS"), "comma-separated client tools")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	o := options{pack: *pack, prompt: *prompt, port: firstSet(getenv, "OMNIA_DIALHOME_PORT", "PORT")}
	if o.pack == "" {
		if getenv(envHandoffPackJSON) == "" && getenv(envHandoffPackURI) != "" {
			return options{}, fmt.Errorf("the deploy staged the pack at %s=%s, which is not supported: "+
				"give the pack with -pack or OMNIA_DIALHOME_PACK (for example in an image built FROM this one)",
				envHandoffPackURI, getenv(envHandoffPackURI))
		}
		o.packJSON = getenv(envHandoffPackJSON)
	}
	if (o.pack == "" && o.packJSON == "") || o.prompt == "" {
		return options{}, errors.New("a pack and a prompt are required (-pack, -prompt)")
	}
	for _, t := range strings.Split(*tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			o.clientTools = append(o.clientTools, t)
		}
	}
	return o, nil
}

func run(args []string, getenv func(string) string) int {
	log := logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil))
	opts, err := parse(args, getenv)
	if err != nil {
		log.Error(err, "usage")
		return exitUsage
	}
	if opts.packJSON != "" {
		path, cleanup, err := writePack(opts.packJSON)
		if err != nil {
			log.Error(err, "pack not written")
			return exitConfig
		}
		defer cleanup()
		opts.pack = path
	}
	cfg, ok, err := dialhome.ConfigFromEnv(getenv)
	if err != nil || !ok {
		log.Error(err, "dial-home is not configured: set OMNIA_DIAL_HOME_URL and OMNIA_DIAL_HOME_KEY")
		return exitConfig
	}
	cfg.Log = log
	baked, _, err := dialhome.BakedConfigFromEnv(getenv)
	if err != nil {
		log.Error(err, "baked config ignored")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ch, err := dialhome.Dial(ctx, cfg)
	if err != nil {
		log.Error(err, "dial-home failed")
		return exitConfig
	}
	defer func() { _ = ch.Close() }()

	h, err := promptkit.New(promptkit.Config{
		PackPath: opts.pack, PromptName: opts.prompt, ClientTools: opts.clientTools,
		Provider: defaultProvider(dialhome.NewConfigSource(ch, baked), getenv, log),
		Log:      log,
	})
	if err != nil {
		log.Error(err, "runtime not built")
		return exitConfig
	}
	defer func() { _ = h.Close() }()

	if opts.port != "" {
		go serveProbes(ctx, net.JoinHostPort("", opts.port), ch, log)
	}
	go func() {
		<-ctx.Done()
		_ = ch.Close()
	}()
	log.Info("serving over dial-home", "pack", opts.pack, "prompt", opts.prompt)
	if err := server.Serve(ch, h, server.WithLogger(log)); err != nil {
		log.Error(err, "serve failed")
		return exitServe
	}
	return exitOK
}

// firstSet returns the value of the first of names that is set.
func firstSet(getenv func(string) string, names ...string) string {
	for _, name := range names {
		if v := getenv(name); v != "" {
			return v
		}
	}
	return ""
}

// writePack writes a pack handed over inline to a private file, since the
// runtime opens packs by path. cleanup removes it.
func writePack(packJSON string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "omnia-dialhome-")
	if err != nil {
		return "", nil, fmt.Errorf("pack directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "agent.pack.json")
	if err := os.WriteFile(path, []byte(packJSON), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write pack: %w", err)
	}
	return path, cleanup, nil
}

// defaultProvider builds each conversation's provider from the agent's
// default provider, as the channel (or the baked config) serves it. With
// neither, PromptKit chooses from the pack and the environment.
func defaultProvider(src *dialhome.ConfigSource, getenv func(string) string, log logr.Logger) promptkit.ProviderFunc {
	return func(ctx context.Context) (providers.Provider, error) {
		cfg, origin, err := src.Current(ctx)
		if errors.Is(err, dialhome.ErrNoConfig) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		p, ok := cfg.Default()
		if !ok {
			log.Info("no default provider served; PromptKit chooses", "origin", string(origin))
			return nil, nil
		}
		prov, err := providerspec.New(ctx, p, getenv)
		if err != nil {
			return nil, fmt.Errorf("provider %s/%s from %s config: %w", p.Group, p.Name, origin, err)
		}
		return prov, nil
	}
}

// serveProbes answers /healthz while the process runs and /readyz while the
// channel is connected, for platforms that probe a port.
func serveProbes(ctx context.Context, addr string, ch *dialhome.Client, log logr.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ch.Connected() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error(err, "probe server failed")
	}
}
