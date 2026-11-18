# omnia-dialhome

Run an agent anywhere and have Omnia treat it as one of its own.

`omnia-dialhome` is the hosted side of Omnia's **dial-home channel**. An agent
hosted outside the cluster — on Vertex Agent Runtime, AgentCore, Azure AI
Foundry, or a machine on site — opens one outbound connection to its
`AgentRuntime`'s relay. Nothing ever dials into it. Over that connection Omnia
sends it conversations exactly as it sends them to an in-cluster runtime, so
policy, sessions, evals and memory stay in Omnia while the agent and its tools
run where they need to.

This module contains:

| Package | What it is |
|---|---|
| `dialhome` (module root) | The channel client: dial out, handshake, keepalive, re-dial; the stream kinds; provider configuration served over the channel (`RemoteConfig`, `ConfigSource`) |
| `runtimev1` | The `omnia.runtime.v1` gRPC contract the channel carries, generated from `proto/omnia/runtime/v1/runtime.proto` |
| `server` | Serves `omnia.runtime.v1` for any `Handler`: hello first, streamed chunks, client tool round-trips, heartbeats, Health |
| `promptkit` | A `Handler` backed by a [PromptKit](https://github.com/AltairaLabs/PromptKit) pack: one SDK conversation per Omnia session |
| `providerspec` | Builds a PromptKit provider from a provider the relay serves |
| `cmd/omnia-dialhome` | The entrypoint the container image runs |

It depends on PromptKit. PromptKit does not depend on it, or know about Omnia.

## Run the image

You need the dial-home URL and key Omnia issued for the agent (Omnia keeps
them in the `<agent>-dial-home-key` Secret, keys `url` and `key`), and a
compiled PromptPack.

```bash
docker build -t omnia-dialhome .
docker run --rm \
  -v "$PWD/agent.pack.json:/pack/agent.pack.json:ro" \
  -e OMNIA_DIALHOME_PACK=/pack/agent.pack.json \
  -e OMNIA_DIALHOME_PROMPT=assistant \
  -e OMNIA_DIAL_HOME_URL=wss://my-agent-dial-home.example.com/v1/channel \
  -e OMNIA_DIAL_HOME_KEY="$KEY" \
  omnia-dialhome
```

| Variable | Flag | Meaning |
|---|---|---|
| `OMNIA_DIAL_HOME_URL` | | The relay's `wss://…/v1/channel` URL. Required |
| `OMNIA_DIAL_HOME_KEY` | | The agent's dial-home key. Required |
| `OMNIA_DIAL_HOME_CONFIG` | | Provider configuration, as JSON, to use while the channel is down. Optional |
| `OMNIA_DIALHOME_PACK` | `-pack` | The compiled `.pack.json`. Required |
| `OMNIA_DIALHOME_PROMPT` | `-prompt` | The pack prompt to run. Required |
| `OMNIA_DIALHOME_CLIENT_TOOLS` | `-client-tools` | Comma-separated pack tools the caller's client runs |
| `PORT` | | When set, serves `/healthz` (process up) and `/readyz` (channel connected) |

The model provider comes from Omnia: while the channel is up the relay serves
the agent's providers, and the runtime builds the default one, with its
credential taken from where it runs (its platform identity, or its own
environment). A provider whose credential lives in a cluster Secret is never
sent; it is listed as withheld, and `OMNIA_DIAL_HOME_CONFIG` can supply it.
With no configuration at all PromptKit chooses from the pack and the
environment, as it does anywhere else.

`ws://` is refused unless the host is loopback or an in-cluster Service name
(`*.svc`, `*.svc.cluster.local`): the key rides every handshake.

## Use it as a library

Serve your own handler over the channel:

```go
cfg, ok, err := dialhome.ConfigFromEnv(os.Getenv)
if err != nil || !ok {
	log.Fatal("set OMNIA_DIAL_HOME_URL and OMNIA_DIAL_HOME_KEY")
}
ch, err := dialhome.Dial(ctx, cfg) // connects, and reconnects, in the background
if err != nil {
	log.Fatal(err)
}
defer ch.Close()

// ch is a net.Listener: the relay's runtime streams arrive on Accept.
log.Fatal(server.Serve(ch, myHandler))
```

or a PromptKit pack, with your own tools:

```go
h, _ := promptkit.New(promptkit.Config{
	PackPath:    "agent.pack.json",
	PromptName:  "assistant",
	ClientTools: []string{"get_location"},
	Configure: func(conv *sdk.Conversation) error {
		conv.OnTool("read_scada", readSCADA) // runs here, on site
		return nil
	},
})
log.Fatal(server.Serve(ch, h))
```

A pack tool with an `OnTool` handler runs in this process and never crosses
the channel. A tool listed in `ClientTools` is sent to the caller's client as
a client tool call and its answer goes back to the model.

`ch.ForwardLocal(dialhome.KindEvents)` and `ch.ForwardLocal(dialhome.KindMemory)`
give a loopback URL whose requests ride the channel to the agent's session and
memory services in the cluster, for an HTTP client that should reach them.

## The channel, version 1

- **Transport.** A WebSocket (`wss://`) carrying a [yamux](https://github.com/hashicorp/yamux)
  session in binary messages.
- **Handshake.** `GET /v1/channel` with `Authorization: Bearer <key>` and
  `X-Omnia-Dial-Home-Version: 1`. A key the relay does not accept is refused
  with 401 before the upgrade, an unsupported version with 400.
- **Streams.** The opener of every stream writes two bytes, `{1, kind}`, and
  the stream is then the protocol `kind` names. A kind the receiver does not
  serve is closed at once.

  | Kind | Byte | Opened by | Carries |
  |---|---|---|---|
  | runtime | 1 | relay | `omnia.runtime.v1` gRPC, as the facade speaks it |
  | memory | 2 | runtime | HTTP/1.1 to the agent's memory service |
  | events | 3 | runtime | HTTP/1.1 to the agent's session service |
  | config | 4 | runtime | HTTP/1.1 `GET /v1/config`: the agent's provider configuration |

- **Liveness.** yamux keepalives. A dropped session is re-dialled with a
  capped, jittered exponential backoff (1s to 30s).
- **Revocation.** When the key is revoked the relay closes every session it
  opened; the runtime keeps re-dialling and is refused with 401.

## The runtime contract

`proto/omnia/runtime/v1/runtime.proto` is the contract between Omnia's facade
and an agent's runtime, at the version its `Contract-Version` marker states
(`runtimev1.ContractVersion`). `make generate` regenerates `runtimev1` from it.

## Develop

```bash
make test   # go test -race -count=1 ./...
make lint   # golangci-lint
make image  # docker build
```

The tests run a stand-in relay in process (`internal/relaytest`) and drive
turns through the real channel with PromptKit's mock provider; nothing calls a
model.

See [CONTRIBUTING.md](CONTRIBUTING.md). Every commit needs a DCO sign-off.

## Licence

Apache 2.0. See [LICENSE](LICENSE).
