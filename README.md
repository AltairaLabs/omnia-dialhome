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
| `dialhome` (module root) | The channel client: dial out, handshake, keepalive, re-dial; the stream kinds; provider configuration served over the channel (`RemoteConfig`, `ConfigSource`); policy decisions, tool call records, memory and the tools Omnia hosts for the agent (`Governance`) |
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

Each `v*` tag publishes `ghcr.io/altairalabs/omnia-dialhome:<tag>` for
`linux/amd64` and `linux/arm64`. To build it yourself, `docker build -t
omnia-dialhome .`.

```bash
docker run --rm \
  -v "$PWD/agent.pack.json:/pack/agent.pack.json:ro" \
  -e OMNIA_DIALHOME_PACK=/pack/agent.pack.json \
  -e OMNIA_DIALHOME_PROMPT=assistant \
  -e OMNIA_DIAL_HOME_URL=wss://my-agent-dial-home.example.com/v1/channel \
  -e OMNIA_DIAL_HOME_KEY="$KEY" \
  ghcr.io/altairalabs/omnia-dialhome:v0.2.1
```

| Variable | Flag | Meaning |
|---|---|---|
| `OMNIA_DIAL_HOME_URL` | | The relay's `wss://…/v1/channel` URL. Required |
| `OMNIA_DIAL_HOME_KEY` | | The agent's dial-home key. Required |
| `OMNIA_DIAL_HOME_CONFIG` | | Provider configuration, as JSON, to use while the channel is down. Optional |
| `OMNIA_DIALHOME_PACK` | `-pack` | The compiled `.pack.json`. Required, unless a deploy hands the pack over (below) |
| `OMNIA_DIALHOME_PROMPT` | `-prompt` | The pack prompt to run. Required, unless a deploy hands the agent over (below) |
| `OMNIA_DIALHOME_CLIENT_TOOLS` | `-client-tools` | Comma-separated pack tools the caller's client runs |
| `OMNIA_DIALHOME_POLICY_REGISTRY` | `-policy-registry` | The name this runtime's tools are governed under: Omnia's tool policies select them by it |
| `OMNIA_DIALHOME_MEMORY` | `-memory` | `true` gives conversations the memory Omnia keeps for the session's user |
| `OMNIA_DIALHOME_PORT` | | When set, serves the [platform probes](#platform-probes) on this port |
| `PORT` | | The same, when `OMNIA_DIALHOME_PORT` is not set. Azure AI Foundry sets it |

### Platform probes

With a port set, the container answers what the platforms that host it probe.
Every liveness check passes while the process runs, **whether or not the
channel is connected**: a platform that restarts an unhealthy container would
otherwise kill it while it re-dials a relay it cannot yet reach, and it would
never get the chance to connect.

| Route | Answer | For |
|---|---|---|
| `GET /healthz` | `200` while the process runs | Kubernetes-style liveness |
| `GET /readyz` | `200` while the channel is connected, else `503` | Readiness, where a platform should route nothing to a runtime that cannot serve |
| `GET /` | `200` while the process runs | Vertex Agent Runtime's health check |
| `GET /readiness` | `200` while the process runs | Azure AI Foundry hosted agents' readiness probe |
| `GET /ping` | `200`, `{"status":"HealthyBusy"}` while the channel is connected, else `{"status":"Healthy"}` | AWS Bedrock AgentCore's health check |
| `POST /invocations` | `501`, `{"status":"error","error":"…invoke the agent through Omnia"}` | AgentCore's invoke route, and Foundry's Invocations protocol |

Conversations reach the runtime over the channel, never through the platform's
own invoke API, so `/invocations` answers with an error rather than serving a
second path. On AgentCore, `InvokeAgentRuntime` therefore returns
`424 RuntimeClientError`; that is expected.

- **AgentCore.** Serve the default port: set `OMNIA_DIALHOME_PORT=8080`, and
  leave `protocolConfiguration.path` at `/invocations`. AgentCore starts a
  container for a runtime session, which an `InvokeAgentRuntime` call opens,
  so something must make one invocation to start it; that call returns the
  `424` above while the container starts and dials home. `HealthyBusy` marks
  the session active, so AgentCore keeps a connected runtime up past its idle
  timeout, to the session's maximum lifetime, although no further invocation
  reaches it. The session is billed for that time. The response omits the
  optional `time_of_last_update`.
- **Vertex Agent Runtime.** Set `OMNIA_DIALHOME_PORT=8080`. Do not point a
  health check at `/readyz`: while the relay is unreachable it answers `503`,
  and a runtime that is reported unhealthy for that is never left up long
  enough to connect. `/readyz` does suit the optional keep-alive probe
  (`keep_alive_probe.http_get.path: /readyz`), whose `2xx` means busy: an idle
  instance whose channel is connected is then not scaled down, up to the
  probe's `max_seconds` after the last request.
- **Azure AI Foundry.** Foundry passes `PORT` and probes `/readiness`, so no
  port setting is needed. `OMNIA_DIALHOME_PORT`, when set, takes precedence.

### Deployed by PromptArena

PromptArena's deploy adapters can run this image in place of their own runtime
through the deploy config's `runtime_override` block. The adapter then hands
the container the pack and the agent, and the command reads them when its own
settings are absent:

| Variable | Used as |
|---|---|
| `PROMPTPACK_PACK_JSON` | The pack, when neither `-pack` nor `OMNIA_DIALHOME_PACK` is set |
| `PROMPTPACK_AGENT` | The prompt, when neither `-prompt` nor `OMNIA_DIALHOME_PROMPT` is set |

A pack the adapter staged to storage instead (`PROMPTPACK_PACK_URI`, for packs
too large to pass inline) is refused with an error naming the URI. Build an
image `FROM` this one with the pack copied in, and set `OMNIA_DIALHOME_PACK`.
The adapter's other variables are ignored: the model provider comes from
Omnia, as below. The [Vertex adapter](https://github.com/AltairaLabs/promptarena-deploy-vertex#run-omnia-dialhome) README shows a complete configuration.

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
		conv.OnTool("read_sensor", readSensor) // runs here, on site
		return nil
	},
})
log.Fatal(server.Serve(ch, h))
```

A pack tool with an `OnTool` handler runs in this process. A tool listed in
`ClientTools` is sent to the caller's client as a client tool call and its
answer goes back to the model.

### Governed like an agent in the cluster

Give the handler the channel's `Governance` and the tools that run here are
governed by Omnia, the way its own runtime governs the tools it runs:

```go
gov := dialhome.NewGovernance(ch)
h, _ := promptkit.New(promptkit.Config{
	PackPath:       "agent.pack.json",
	PromptName:     "assistant",
	Policy:         gov,          // ask before every tool runs here
	PolicyRegistry: "local-tools", // the name Omnia's policies select these tools by
	ToolCalls:      gov,          // record each call on the session
	Memory:         gov,          // the session user's memory, as Omnia scopes it
	Tools:          gov,          // the agent's tools that Omnia hosts, run in the cluster
	Configure: func(conv *sdk.Conversation) error {
		conv.OnTool("open_ticket", openTicket)
		return nil
	},
})
```

- **Policy.** Before each tool the pack runs here, the handler asks Omnia
  whether it may, and runs it only on an allow. A refusal goes back to the
  model as the tool's error, carrying the policy's message, so the agent can
  tell the person why. No answer is a refusal too: with the channel down, a
  timeout, or a relay that cannot decide, the tool does not run. Client tools
  are not put to it, nor, when `Memory` is set, the four memory tools.

  **Upgrading from v0.1.** A relay that predates policy decisions gives none,
  so from v0.2.0 every tool the pack runs in this process is refused until the
  relay is upgraded too. Client tools are unaffected.
- **Tool calls.** Each call the pack ran here is recorded on its session:
  arguments when it starts, then its result or its error. A call Omnia
  refused is recorded by Omnia.
- **Tools Omnia hosts.** The agent's tools in Omnia (its ToolRegistries:
  HTTP, MCP, gRPC and OpenAPI tools) are listed when a conversation opens and
  offered to the model with the description and schema Omnia gives them. When
  the model calls one, the call goes to Omnia and runs in the cluster: Omnia
  decides it under the agent's tool policy, adds the credentials the tool
  needs, records it on the session, and sends back only the result. Those
  credentials never reach this process, and the call is not put to `Policy`
  or recorded through `ToolCalls`, since Omnia does both. A refusal goes back
  to the model as the tool's error, in the policy's words. With no list (the
  channel down when the conversation opens, or a relay without the tools
  stream) the conversation offers none.

  **A tool the pack defines wins.** If the pack defines a tool with the same
  name as a hosted one (run here by an `OnTool` handler), or lists it in
  `ClientTools`, the pack's tool is the one offered and run, and the hosted
  tool is not offered. To use a hosted tool, name it in the prompt's `tools`
  list and leave it out of the pack's tool definitions: as with any tool, the
  model is offered it only if the prompt names it.
- **Memory.** Conversations get the memory Omnia keeps for the session's user:
  the memory tools, and recall into each turn, as the agent is configured.
  Who the user is, and what they consented to, is Omnia's to decide from the
  session; nothing the runtime sends can name another user. A caller Omnia
  keeps no user memory for gets no memory tools.

Every request names only its session. Omnia answers from the identity it
carried for that session, never from anything the runtime says about it.

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
  | policy | 5 | runtime | HTTP/1.1 `POST /v1/policy/decision`: may this tool call run? |
  | tool-calls | 6 | runtime | HTTP/1.1 `POST /v1/tool-calls`: a tool call the runtime ran |
  | memory-store | 7 | runtime | HTTP/1.1 `POST /v1/memory/{scope,remember,recall,list,forget}`: the session's memory |
  | tools | 8 | runtime | HTTP/1.1 `GET /v1/tools`: the tools Omnia hosts for the agent; `POST /v1/tools/call`: run one in the cluster |

  Kinds 4 to 8 were added within version 1. A relay that does not serve one
  closes its streams, and the runtime fails closed: no decision, no tool; no
  tool list, no hosted tools.

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
