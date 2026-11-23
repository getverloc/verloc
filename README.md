# verloc

[![Release](https://img.shields.io/github/v/release/getverloc/verloc?display_name=tag&sort=semver)](https://github.com/getverloc/verloc/releases)
[![CI](https://github.com/getverloc/verloc/actions/workflows/ci.yml/badge.svg)](https://github.com/getverloc/verloc/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/getverloc/verloc)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A self-hosted, OpenAI-compatible LLM gateway with an embedded operations dashboard. Point applications at one endpoint; manage upstream providers, credentials, model routes, client access, and usage from the browser.

verloc runs as a single Go binary. SQLite stores management state and analytics; the React dashboard is embedded in the binary. Node.js is a build dependency, not a runtime dependency. No external database, Redis, or separate dashboard server is required.

![verloc dashboard — Overview](assets/overview.png)

## Capabilities

- **Low-overhead engine:** zero-allocation model patching, zero-copy SSE streaming, lock-free atomic auth/metrics hot paths, pooled gzip/buffers, and tuned HTTP/2 connection pooling.
- **Adaptive routing & resilience:** public model aliases, ordered/Peak-EWMA latency/smooth-weighted routing, opt-in speculative hedging, per-provider concurrency bulkheads, TLS keep-warm probes, proactive rate-limit header steering, error-class jittered backoff, and half-open single-probe circuit recovery.
- **Inference:** Chat Completions, Responses, Anthropic Messages, Embeddings, Image generations/edits, and native System One typed questions for TypeSafe Jev and Cloudflare Clef. Failover happens strictly before response bytes reach the client; interrupted streams are not silently resumed.
- **Deterministic caching:** opt-in, tenant-isolated in-memory LRU response cache for `temperature=0` non-streaming chat, embeddings, and System One decisions, honoring `Cache-Control: no-store`/`no-cache`.
- **Access control:** one owner account for administration; separate application API keys with model scopes, expiry, rotation, revocation, and sliding-window RPM/TPM plus concurrency limits.
- **Live management:** add providers, discover/test upstream models, tune routing/bulkheads/caching, edit pricing and routes, and change gateway limits without restarting.
- **Analytics & persistence:** durable metadata-only SQLite analytics, operational alerts, encrypted upstream credentials, hashed application keys, audit history, and consistent backup/restore commands.

## Deployment scope

Designed for a **single gateway instance on a single host**, with local persistent storage and one administrative owner. For a production deployment, use HTTPS, protect the data directory and encryption key, verify backups, monitor disk/telemetry health, and validate capacity against your own workload.

This is not a clustered gateway or a billing system. There is no built-in multi-owner RBAC, SSO, distributed rate limiting, or cross-instance coordination. Read the [operating limits](#operating-limits) before exposing it to other users.

## Quick start (one-command setup)

### Option 1: One-command installer (Linux & macOS)

Install `verloc` into `~/.local/bin`, initialize a private loopback-bound runtime directory (`~/.local/share/verloc`, mode `0700`), and optionally start it as a background service:

```sh
# Install and start as a background service (systemd user unit on Linux, launchd on macOS)
curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh -s -- --service
```

Or install without a background service and run it in the foreground:

```sh
curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sh
~/.local/bin/verloc -config ~/.local/share/verloc/bootstrap.json
```

Already cloned the repository? Run directly from the checkout:

```sh
./install.sh --start        # build, install, and start immediately (or: make quickstart)
./install.sh --service      # build, install, and enable as a background service
```

For a system-wide Linux server install (`/usr/local/bin/verloc`, dedicated `verloc` user, `/etc/verloc/bootstrap.json`, `/var/lib/verloc`, and a hardened `systemd` unit):

```sh
curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sudo sh -s -- --system
```

**How `install.sh` works:**
- Downloads a checksum-verified binary from GitHub Releases when available for your OS/arch (`linux`/`darwin`, `amd64`/`arm64`), and automatically falls back to building from source with Go **1.26+** (no C compiler or Node.js required because embedded dashboard assets are committed).
- Re-running the installer upgrades only the binary and preserves your existing `bootstrap.json`, databases, and `master.key`.
- Common flags: `--port 9000`, `--listen 127.0.0.1:9000`, `--dir /path/to/data`, `--from-source`, `--build-dash`, `--uninstall` (preserves data), `--uninstall --purge`.

Open **<http://127.0.0.1:8080/dash/>** and continue with [First-run setup](#first-run-setup).

### Option 2: Docker & Docker Compose

Run the prebuilt multi-arch image (`linux/amd64`, `linux/arm64`) from GitHub Container Registry:

```sh
docker run -d --name verloc \
  -p 127.0.0.1:8080:8080 \
  -v verloc_data:/var/lib/verloc \
  --read-only --tmpfs /tmp \
  --security-opt no-new-privileges:true \
  ghcr.io/getverloc/verloc:latest
```

Or build and run locally with Docker Compose from the repository root:

```sh
docker compose up -d
```

Read the private first-run setup token and open **<http://127.0.0.1:8080/dash/>**:

```sh
docker compose exec verloc cat /var/lib/verloc/verloc-data/setup.token
```

The Compose stack binds to `127.0.0.1:8080` by default, stores state in the persistent `verloc_data` volume under a non-root user (`uid 10001`), mounts the container root filesystem read-only, and includes a `/healthz` health check. Override defaults with environment variables:

- `VERLOC_PORT` (default `8080`), `VERLOC_BIND` (default `127.0.0.1`)
- `VERLOC_SECURE_COOKIES=true` when terminating TLS at a reverse proxy
- `VERLOC_ALLOWED_HOSTS` and `VERLOC_TRUSTED_PROXIES` (comma-separated lists)

### Option 3: Manual build from source

#### Requirements

- Go **1.26 or newer**.
- Node.js **22.12 or newer** and npm (only needed when rebuilding the dashboard; `CGO_ENABLED=0 go build ./cmd/verloc` works using the committed embedded assets).
- A writable local filesystem for SQLite and private key files.
- An upstream provider implementing the endpoint you intend to use.

From the repository root:

```sh
npm --prefix dashboard ci
make build
./verloc -version
```

The build produces `./verloc`, including the dashboard. CGO is disabled; a C compiler is not required.

Run from a dedicated directory so runtime state stays outside the source checkout:

```sh
install -d -m 700 "$HOME/.local/share/verloc"
install -m 755 ./verloc "$HOME/.local/share/verloc/verloc"
cd "$HOME/.local/share/verloc"

umask 077
printf '{"listen":"127.0.0.1:8080"}\n' > bootstrap.json
./verloc -config bootstrap.json
```

Open **<http://127.0.0.1:8080/dash/>**.

A bootstrap file is optional. Running `./verloc` in an empty directory creates the default databases and key files without `config.json`. **The default listener is `:8080` on all interfaces**; `install.sh`, `docker-compose.yml`, and the example above bind to loopback explicitly. Do not expose plain HTTP credentials on an untrusted network.

### First-run setup

1. Read the private `setup.token` file on the gateway host (default: `~/.local/share/verloc/verloc-data/setup.token` with `install.sh`, or `verloc-data/setup.token` in your runtime directory). The server logs its path, not its value. Do not share or commit it.
2. Enter that token in the dashboard and create the owner password: at least 12 characters, at most 1,024 UTF-8 bytes. Setup can be completed only once.
3. Open **Providers** and add an upstream. Supply its API base URL and credential, or explicitly select no authentication for a keyless provider. Use discovery and a test request to check the connection; tests can incur upstream charges.
4. Add a model with a public ID such as `coding`, choose its provider, and set the upstream model ID. Review pricing instead of assuming that missing prices mean free usage.
5. Open **API Keys**, create an application key, set its model scope and limits, and copy the secret. It is shown once.
6. Use that application key with the gateway URL. The owner password and upstream provider credentials are not gateway API keys.

The gateway is not ready for inference until owner setup is complete and at least one enabled model has an enabled route. Configuration readiness does not prove that the upstream is reachable or has available quota; run a model test as well.

## Use with applications

Set an application's OpenAI-compatible **base URL** to `http://127.0.0.1:8080/v1`, its API key to the application key issued by verloc, and its model to a configured **public model ID**.

For the examples below, set `VERLOC_API_KEY` privately in your shell and use a model ID you created. Do not commit application keys to client configuration files.

```sh
export VERLOC_BASE_URL="http://127.0.0.1:8080/v1"
export VERLOC_MODEL="coding"

curl --fail-with-body "$VERLOC_BASE_URL/models" \
  -H "Authorization: Bearer $VERLOC_API_KEY"

curl --fail-with-body "$VERLOC_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$VERLOC_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with hello.\"}]}"
```

Streaming:

```sh
curl --fail-with-body --no-buffer "$VERLOC_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"$VERLOC_MODEL\",\"stream\":true,\"stream_options\":{\"include_usage\":true},\"messages\":[{\"role\":\"user\",\"content\":\"Reply with hello.\"}]}"
```

With the Python OpenAI SDK (`pip install openai`):

```python
import os
from openai import OpenAI

client = OpenAI(
    base_url=os.environ["VERLOC_BASE_URL"],
    api_key=os.environ["VERLOC_API_KEY"],
)
response = client.chat.completions.create(
    model=os.environ["VERLOC_MODEL"],
    messages=[{"role": "user", "content": "Reply with hello."}],
)
print(response.choices[0].message.content)
```

### Typed decisions: TypeSafe Jev and Cloudflare Clef

These models accept `state` and typed `questions`, not chat messages:

- **TypeSafe:** choose the **TypeSafe Jev (System One)** provider preset, or set
  `base_url` to `https://api.typesafe.ai/v1` and `api_format` to `systemone`.
  Map a public ID such as `ts/jev-latest` to `jev-latest`. Discovery accepts
  TypeSafe's `models[].name` IDs.
- **Cloudflare:** keep the Workers AI provider's OpenAI-compatible base
  `https://api.cloudflare.com/client/v4/accounts/%ACCOUNT%/ai/v1` and its
  per-key account IDs. Map `cf/clef` to `@cf/cloudflare/clef`, or
  `cf/clef-flash` to `@cf/cloudflare/clef-flash`. These models alone use the
  native `/ai/run/...` path; other Cloudflare models retain chat behavior.

```sh
curl --fail-with-body "$VERLOC_BASE_URL/systemone" \
  -H "Authorization: Bearer $VERLOC_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "ts/jev-latest",
    "state": {"incident": "Checkout has failed for the last hour."},
    "questions": {
      "urgent": {"type": "noul", "instructions": "Is this incident urgent?"},
      "team": {
        "type": "choice",
        "instructions": "Which team should handle it?",
        "criteria": {"billing": "Payments and invoices", "technical": "Outages and bugs"}
      },
      "severity": {
        "type": "score",
        "instructions": "Rate customer impact.",
        "criteria": ["None", "Minor", "Major", "Critical"]
      }
    }
  }'
```

The response retains native `answers`, probabilities, confidence, actual model
version, and token usage. Cloudflare's REST envelope is unwrapped. Clef image
inputs are forwarded unchanged; upstream media limits still apply. Native models
are labeled `type: "systemone"` in the scoped `/v1/models` response.

System One is non-streaming and accepts **uncompressed JSON** through verloc.
Disable SDK body compression for this route. Gateway body limits still apply
(8 MiB by default); adjust Settings when your provider permits larger requests.
TPM-limited application keys require a configured model context window and reserve
that bound plus request bytes before reconciling actual usage. Native models
cannot be called through Chat Completions or Responses.

See the upstream [TypeSafe API](https://docs.typesafe.ai/api.md) and
[Cloudflare Clef contract](https://developers.cloudflare.com/workers-ai/models/clef/)
for question and media constraints. Add native providers through the dashboard
(provider presets) or a local `config.json`; the committed root
`config.example.json` is a minimal, key-less nvidia example. Review current
provider prices.

### HTTP surface

| Endpoint | Authentication | Purpose |
| --- | --- | --- |
| `POST /v1/chat/completions` | Application API key | Chat Completions, including streaming |
| `POST /v1/responses` | Application API key | OpenAI Responses API translated to Chat Completions, including streaming |
| `POST /v1/systemone` | Application API key | Native Jev/Clef typed-question evaluation; no streaming |
| `POST /v1/messages` | Application API key | Anthropic Messages translated to Chat Completions, including streaming |
| `POST /v1/messages/count_tokens` | Application API key | Local, approximate token count; no upstream inference |
| `POST /v1/embeddings` | Application API key | Embeddings generation |
| `POST /v1/images/generations` | Application API key | Image generation |
| `POST /v1/images/edits` | Application API key | Image edits (multipart/form-data) |
| `GET /v1/models` | None (key optional) | Enabled models; a presented key scopes the list to its access, anonymous callers see all enabled models |
| `GET /v1/pricing` | Application API key | Scoped pricing catalog in USD per million tokens |
| `GET /healthz` | None | Process liveness; does not test upstreams or storage |
| `GET /readyz` | Owner session | Storage/owner/model-configuration readiness |
| `GET /metrics` | Owner session | Live **JSON**, not Prometheus exposition |
| `/dash/` | Owner login within the app | Embedded operations dashboard |
| `/dash/api/*` | Owner session | Dashboard and management APIs; writes require CSRF protection |

Anthropic messages support base64 and URL images without gateway-side URL fetching.
Image-bearing tool results retain their call association in subsequent user content;
malformed or unsupported image sources return a client error rather than disappearing.
Token counting uses a character-based estimate, not a provider tokenizer. It enforces
model access, per-key RPM/concurrency, global admission, and bounded body reads, but
does not consume upstream TPM or incur upstream usage.

Responses requests are translated to Chat Completions and back, so every chat-capable
model serves `/v1/responses` without its backend implementing it. `model`, `input`
(string or item array), `instructions`, `tools`/`tool_choice`, `text.format`,
`max_output_tokens`, sampling fields, `reasoning.effort` and `stream` are mapped.
Responses features with no chat equivalent are handled deliberately rather than
silently dropped: `previous_response_id` and `conversation` are rejected with a 400
because answering them statelessly would return a reply that lost the transcript;
hosted tools (`web_search`, `file_search`, `code_interpreter`, `computer_use`, `mcp`,
`image_generation`) are rejected by name for the same reason. `store` is accepted and
ignored — it defaults to true in the official SDKs and the gateway keeps no response
state, so responses report `store: false` and `previous_response_id: null` honestly.
Stateful reads (`GET`/`DELETE /v1/responses/{id}`) are not implemented.

For providers retaining reasoning, replay is isolated by application-key identity,
public model, provider/upstream route, and conversation/tool-turn content. Reused
tool-call IDs alone cannot select another turn's reasoning. Stream accumulation
is bounded to 512 KiB per choice.

Authentication endpoints under `/dash/api/auth/` are reachable before login for setup/session/login. Application API keys never grant administrative access. Other OpenAI APIs, including audio, are not implemented.

## Dashboard

The same binary serves all nine screens; there is no separate analytics service.

| Screen | What it exposes |
| --- | --- |
| **Overview** | Request/token totals, known spend, error rate, current request rate, latency p95, average TTFT, traffic history, and provider spend allocation with full provider details |
| **Requests** | Time/model/provider/client/status/search filters; cursor-paginated durable history; per-request usage, cost, outcome, client attribution, and upstream-attempt details |
| **Analytics** | Client/model/provider comparisons and full ledgers, status breakdown, latency distribution, p50/p95/p99 latency and TTFT, cache usage, cost history, and data-coverage notes |
| **Alerts** | Configured thresholds/scopes/windows/cooldowns and a bounded recent history of persisted/live triggers |
| **Providers** | Connection management, discovery/testing, credentials, provider health, cooldowns, per-provider concurrency bulkheads, TLS keep-warm intervals, and live diagnostics |
| **Models** | Model metadata and capabilities, upstream mappings, pricing, routing strategies (`ordered` / `latency` / `weighted`), speculative hedging, weighted fallbacks, and bulk actions |
| **API Keys** | Application key lifecycle, model access, expiry, sliding-window RPM/TPM limits, and concurrency limits |
| **Settings** | Owner password, global admission/retry/body/stream limits, deterministic response cache policy & live stats, alert rules, version & automatic software updates, server-side backups, connection details, and audit events |
| **Docs** | In-app gateway API reference: endpoints, schemas, errors, routing, and configuration |

**Allow private / LAN upstream** is an explicit provider-editor and guided-add
option for intentionally local endpoints; it is off by default and saved with
the provider. Existing-provider probes honor that saved permission. Upstream
discovery has a 15-second overall deadline; connectivity tests have 30 seconds,
including DNS and response bodies. Connectivity tests send a real upstream request.

### Automatic model metadata

Discovery preserves rich fields returned by the upstream `/models` endpoint,
including explicit zero prices. It fills missing fields from live catalogs:
[Vercel](https://ai-gateway.vercel.sh/v1/models),
[OpenRouter](https://openrouter.ai/api/v1/models), and
[models.dev](https://models.dev/api.json?type=all), with the existing LiteLLM
catalog as the final fallback. Vercel's public catalog works without a configured
Vercel credential. No local endpoint-capture files are imported or embedded.

**Lookup catalog** in the model editor and manually entered onboarding rows use
the same resolver. Available names, descriptions, ownership, capabilities/tags,
context/output limits, and per-million-token prices are carried through save.
Vercel metadata is cached for one hour; other catalogs for 24 hours. A failed
catalog source does not fail otherwise successful upstream discovery.

Qualified model identities do not match unrelated organizations; ambiguous
bare names and conflicting provider fields remain unresolved. Free variants
remain separate from paid variants. Blank prices mean unknown; explicit `0`
means free. Catalog prices are suggestions across providers: review them before
applying. When no cached-input discount is supplied, newly saved models use the
input rate instead of treating cached tokens as free.

The legacy seed generator also distinguishes missing prices from explicit zero:
free canonical rates are not replaced by paid fallbacks, and partial pricing
remains unknown rather than becoming free.

Model metadata also supports `reasoning`, `attachment`, `temperature`, and a
`modalities` object with `input`/`output` arrays. Owner model upserts
(`PUT /dash/api/admin/models/{id}` or `POST /dash/api/admin/onboard`) persist these
fields in SQLite and live-apply them to `/v1/models`. Explicit managed values
override embedded capability defaults, including an explicit `false`. Omitted
capability fields retain saved overrides on later model edits; capabilities
without a managed value or embedded default remain omitted rather than being
reported as unsupported. The model editor exposes descriptions, ownership, tags,
explicit supported/not-supported capability choices, and input/output modality
lists. Untouched missing fields retain inheritance or unknown status; clearing an
edited modality list saves an explicit empty list. Saved capability overrides
cannot be reset to unknown in the editor.

### Accounting and privacy

- Normal analytics stores **metadata**, not prompts, completions, or request/response bodies. Metadata includes model/provider/client identity, timestamps, status, timings, tokens, known cost, client IP/user agent, and attempt summaries. Protect the database accordingly. Payloads are still sent to your chosen upstreams; their policies apply.
- Missing usage and missing pricing are **unknown**, not zero. Known-cost totals exclude requests that cannot be priced; the dashboard reports coverage gaps.
- Cached input is a subset of input tokens. Total tokens are input + output; cached tokens are not added twice. Prices are USD per million tokens; request-time pricing is preserved when catalog prices change later.
- Percentiles are histogram-bucket estimates over recorded request distributions, not averages of snapshot percentiles.
- Snapshot traffic/cost charts and filtered request analytics are different sources. The dashboard labels their windows; dimensional request filters do not apply to the separate snapshot panels.
- New history is retained indefinitely. There is **no automatic expiry or compaction**. Legacy aggregate-only history remains readable, but deleted detail and missing historical attribution cannot be recovered.
- The analytics writer uses a bounded queue. Under overload or storage failures, metadata can be dropped. Queue pressure, dropped records, and write errors are surfaced in the dashboard. Analytics is not a lossless financial ledger.

## Performance, adaptive routing, and resilience

verloc pairs a low-overhead Go hot path with opt-in traffic controls designed to stay 100% backwards-compatible by default.

### Always-on engine optimizations

- **Hot-path efficiency:** in-place JSON field patching without full-body `map[string]any` unmarshaling, zero-copy SSE pass-through streaming, lock-free atomic auth/metrics/histogram paths, pooled gzip writers and I/O buffers, and tuned HTTP/2 upstream transports.
- **Smart key pools & circuit recovery:** error-class jittered backoff (auth errors quarantined at 5m, 429s honoring `Retry-After`, transient 5xx using exponential jitter), proactive soft-steering from upstream `x-ratelimit-remaining-*` / `anthropic-ratelimit-*` headers, and half-open single-probe recovery (one probe at a time; two consecutive successes restore full traffic) to prevent thundering herds.
- **Sliding-window client limits:** per-key RPM and TPM ceilings use weighted 60-second sliding windows with cross-boundary token reconciliation, eliminating 2× minute-boundary bursts.

### Opt-in controls (Dashboard & JSON)

All controls below default to off/`ordered` and live-apply immediately when saved in the dashboard (or via bootstrap `config.json`):

| Feature | Dashboard location | JSON key | Default |
| --- | --- | --- | --- |
| Per-provider bulkhead | **Providers → Edit → Limits** | `providers.<name>.max_in_flight` (`0–4096`) | `0` (unbounded) |
| TLS keep-warm probes | **Providers → Edit → Limits** | `providers.<name>.keep_warm_seconds` (`0–300`) | `0` (off) |
| Adaptive / weighted routing | **Models → Edit → Routing** | `model_meta.<id>.routing.strategy` (`ordered` \| `latency` \| `weighted`), `primary_weight`, `model_routes.<id>[].weight` | `ordered` |
| Speculative hedging | **Models → Edit → Routing** | `model_meta.<id>.routing.hedge_delay_ms` (`0–30000`), `max_hedges` (`1–2`) | `0` (off) |
| Deterministic response cache | **Settings → Response cache** | `cache.{enabled, ttl_seconds, max_entries, max_bytes, models}` | `enabled: false` |

- **`latency` routing** uses Peak EWMA latency weighted by live in-flight concurrency and error penalties to steer toward the fastest healthy route.
- **`weighted` routing** uses smooth weighted round-robin across primary and fallback targets and automatically bypasses unhealthy or bulkhead-saturated providers.
- **Speculative hedging** applies to chat, Responses, and Anthropic Messages requests (both non-streaming and streaming, racing until response headers arrive): if the primary route has not returned headers within `hedge_delay_ms`, verloc launches a backup route and cancels the loser immediately once a winner commits.
- **Response cache** is strictly isolated per client API key, caches only non-streaming `temperature=0` chat, `/v1/embeddings`, and `/v1/systemone`, sets `X-Verloc-Cache: HIT|MISS`, and bypasses whenever a request sends `Cache-Control: no-store` or `no-cache`.

## State and configuration

**SQLite is the source of truth for managed configuration. A database file alone is not the complete installation.** Keep the external encryption key and any bootstrap options as well.

Default paths below are relative to the directory containing the `-config` file. With no flag, `config.json` is looked up in the process working directory; it may be absent.

| File | Role | Recovery requirement |
| --- | --- | --- |
| `verloc-data/control.db` | Providers, encrypted upstream credentials, models, routes, owner auth, application-key hashes, limits, alert rules, and audit events | Back up |
| `verloc-data/master.key` | External encryption key for upstream credentials | Back up **separately and securely**; loss prevents credential recovery |
| `verloc.db` | Request history, snapshots, alerts, and upstream credential events | Back up to retain analytics |
| `verloc-data/setup.token` | Private first-run owner-setup token | Treat as a secret; not a login or password-reset mechanism |
| Optional bootstrap JSON | Listener, database/key/token paths, secure-cookie mode, host allowlist, trusted proxies | Preserve deployment options; contains no provider keys in a new installation |

SQLite also creates `-wal`/`-shm` files while running. Do not move an active database in isolation or back it up with a naive file copy. Use the backup commands below.

Changes made in the dashboard are transactional, saved in `control.db`, and live-applied. The server does not write `config.json`. Changing bootstrap listener/paths/cookies/host or trusted-proxy policies requires a restart; editing legacy provider/model entries in JSON does not override an already-imported management database.

### Existing `config.json` installations

On the first start with a new management database, verloc imports the legacy provider/model configuration once. Do not point an existing install at an empty management path unintentionally.

To produce a secret-free bootstrap from an imported installation, stop the normal process, run as the same OS user, and then update the service to use the new file:

```sh
./verloc -config config.json -export-bootstrap bootstrap.json
./verloc -config bootstrap.json
```

The export requires a new destination file and records absolute storage paths plus the bootstrap-only `allowed_hosts` and `trusted_proxies` policies. It does not remove the old secret-bearing file. Archive it securely or remove it after verifying the migration; do not deploy it over the live databases.

## Production deployment

### HTTPS and storage

Terminate TLS at a trusted reverse proxy and bind verloc to loopback. For example, `/etc/verloc/bootstrap.json`:

```json
{
  "listen": "127.0.0.1:8080",
  "management_db": "/var/lib/verloc/control.db",
  "master_key_file": "/var/lib/verloc/master.key",
  "setup_token_file": "/var/lib/verloc/setup.token",
  "secure_cookies": true,
  "dashboard": {
    "db_path": "/var/lib/verloc/analytics.db"
  }
}
```

`secure_cookies: true` is required for owner sessions when TLS terminates at the proxy. Forwarded headers do not enable it automatically. Preserve the external `Host`, serve the dashboard and its API from the same origin, and allow long-lived SSE/streaming responses. Open the HTTPS URL, not the internal HTTP listener, to sign in.

Example Caddy configuration; replace the domain and configure DNS:

```caddyfile
gateway.example.com {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }
}
```

Use persistent **local** storage. Do not share SQLite files across replicas or put them on a network filesystem. Provider/model discovery and connection tests are owner-authorized outbound requests; grant owner access only to trusted operators.

### systemd example

You can provision the hardened systemd deployment automatically with `sudo ./install.sh --system` (or `curl -fsSL https://raw.githubusercontent.com/getverloc/verloc/master/install.sh | sudo sh -s -- --system`), or configure it manually below.

On a Linux host with systemd, install the binary as `/usr/local/bin/verloc`, create a dedicated `verloc` system user/group, and make `/var/lib/verloc` writable only by that user. Make the bootstrap file readable by the service user. Never run the gateway as root.

Save the following as `/etc/systemd/system/verloc.service`:

```ini
[Unit]
Description=verloc LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=verloc
Group=verloc
WorkingDirectory=/var/lib/verloc
ExecStart=/usr/local/bin/verloc -config /etc/verloc/bootstrap.json
Restart=on-failure
RestartSec=3
TimeoutStopSec=40
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/verloc

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now verloc
sudo systemctl status verloc
journalctl -u verloc -f
```

Read the private setup token on that host and complete setup over HTTPS. `/healthz` is suitable for an unauthenticated liveness probe. `/readyz` and `/metrics` require an owner session; an application key is not accepted. Do not disable their authentication to accommodate a monitor.

### Production checklist

- Complete owner setup; use separate, scoped application keys per application.
- Serve HTTPS; enable secure cookies; keep the internal listener behind the proxy/firewall.
- Restrict access to databases, setup token, backups, and `master.key`; keep secrets out of git and logs.
- Configure global and per-key limits for expected concurrency and upstream quotas; run representative JSON, streaming, and failure scenarios.
- Confirm model availability with an actual upstream test, not only `/readyz`.
- Monitor disk space, request errors, latency, queue pressure, dropped telemetry, and write failures. History does not expire automatically.
- Create and restore a backup in an isolated directory before relying on it. Retain the original master key separately.
- Keep `VERLOC_REQ_DUMP` unset: it is an opt-in payload-debugging path, not production telemetry.

## Backups, recovery, and upgrades

### Back up

**Settings → Backups** creates database snapshots on the gateway host under `backups/` beside the management database. It returns a server-side path; it is not a browser download or an off-host backup.

The CLI can also create a backup and exit. Run it as the service user with the same bootstrap and storage paths. For a simple offline backup, stop the running service first:

```sh
./verloc -config bootstrap.json -backup ./backup-20261016
```

The destination must not already exist; its parent directory must exist. A completed backup contains `management.db`, `analytics.db`, and a checksum manifest. **It does not include `master.key`, the setup-token file, or bootstrap JSON.** Secure the backup, copy it off-host, and secure the master key separately. Application-key hashes and owner auth state in the backup are still sensitive even though upstream credentials are encrypted.

### Restore

Stop any process using the target paths. Choose new, unused database paths and point at the **original** master key. For a default local install, a separate `restore.json` could contain:

```json
{
  "listen": "127.0.0.1:8080",
  "management_db": "recovered/control.db",
  "master_key_file": "verloc-data/master.key",
  "setup_token_file": "recovered/setup.token",
  "dashboard": {
    "db_path": "recovered/analytics.db"
  }
}
```

```sh
./verloc -config restore.json -restore ./backup-20261016
./verloc -config restore.json
```

Restore validates checksums and credential decryption before publishing the new databases. It refuses existing destination databases or their WAL/SHM files. Reapply the appropriate HTTPS/bootstrap options for the destination host. A restored management backup includes the owner account and application keys; it does not initiate owner setup again.

### Versioning and automatic updates

verloc follows **Semantic Versioning 2.0.0** (`vMAJOR.MINOR.PATCH`, tracked in [`VERSION`](VERSION) and injected at build time alongside commit SHA and UTC build timestamp). The dashboard displays the running version and channel (`stable`, `prerelease`, or `dev`) in the header, sidebar, and **Settings → Software updates**.

#### Built-in update checker & smooth self-update

- **Automatic release detection:** verloc periodically checks GitHub Releases using ETag caching and shows a notification banner whenever a newer release is published.
- **One-click & automatic updates:** In **Settings → Software updates**, click **Update to vX.Y.Z** or toggle **Automatically install new releases** (configurable check cadence and optional pre-release opt-in).
- **Zero data loss & cryptographic verification:**
  1. Downloads `checksums.txt` and `verloc_<os>_<arch>.tar.gz` exclusively over HTTPS from GitHub release hosts and verifies the SHA-256 digest in constant time.
  2. Extracts the binary into the same directory and validates it with a `-version` smoke test before touching the live executable.
  3. Saves a rollback copy of the previous binary (`verloc.bak.<prev>-<timestamp>`), atomically swaps the executable via `rename`, records a `system.updated` audit event, drains active HTTP requests, closes SQLite cleanly, and restarts in place (`exec`) while preserving the PID.
  4. **Never modifies or deletes** `verloc.db`, `verloc-data/control.db`, `master.key`, `setup.token`, or `config.json`.
- **Containers & read-only installs:** When running in Docker or a locked-down system unit where the binary directory is read-only (or when `VERLOC_DISABLE_SELF_UPDATE=1` is set), the dashboard still checks for new releases and displays the exact upgrade command (`docker compose pull && docker compose up -d` or `install.sh --system`).

#### Manual upgrade

1. Back up the databases and separately secure the master key.
2. Build and test the new binary, including its embedded dashboard (or re-run `install.sh` to atomically replace the binary and restart the service).
3. Stop the service, replace **only the binary**, and restart using the existing bootstrap and data paths.
4. Check liveness, sign in, inspect readiness, and exercise an inference request.

Keep the previous binary for recovery, but do not assume older binaries can read a newer database schema. A full rollback can require restoring the pre-upgrade backup to new paths with its original master key. Never replace production state with a development `config.json` or database.

## Operating limits

- **Single owner and single instance.** No clustered availability or distributed admission guarantees.
- **Compatibility is upstream-dependent.** Endpoint support, tools, structured output, streaming usage, and provider restrictions vary. Routing does not make an unsupported upstream API available.
- **Retries are bounded.** Credential/provider failover stops when attempts or the retry budget are exhausted; no failover occurs after response bytes have been sent.
- **Limits are not billing budgets.** Spend alerts notify in the dashboard; they do not stop paid traffic. Rate/admission counters and alert evaluation windows are process-local and reset on restart.
- **History grows with use.** No scheduled pruning, automatic compaction, or hard database-size cap. Plan storage and backup capacity.
- **Recent operational views are bounded.** Alerts shows at most 200 recent loaded/live events; Settings audit and credential diagnostics are not exhaustive historical export tools. Request history is separately cursor-paginated.
- **Analytics is best-effort under pressure.** Missing upstream usage/pricing and dropped telemetry limit accounting accuracy. This is not a payment or compliance audit ledger.

## Development

| Command | Purpose |
| --- | --- |
| `./install.sh --service` | One-command install and background service setup |
| `make install` / `make quickstart` | Local install (`~/.local/bin`) or install + start immediately |
| `docker compose up -d` | Build and run the containerized gateway on `127.0.0.1:8080` |
| `npm --prefix dashboard ci` | Install locked frontend dependencies |
| `make build` | Build the dashboard, then the CGO-free binary |
| `./scripts/build.sh` | Build with version/commit/build-time metadata |
| `go test ./... -count=1` | Run Go tests |
| `go test -race ./... -count=1` | Run Go tests with race detection |
| `node --test scripts/sync-model-details.test.mjs` | Check catalog pricing with synthetic inputs; no private catalog files needed |
| `npm --prefix dashboard run lint` | Run dashboard lint |
| `npm --prefix dashboard run build` | Type-check and rebuild embedded dashboard assets |
| `go vet ./...` | Run Go static checks |
| `npm --prefix dashboard run dev` | Start Vite for UI development; use embedded same-origin mode for deployment checks |
| `node scripts/sync-model-details.mjs` | Regenerate the embedded catalog from local catalog sources; not needed for dashboard-managed models |

`internal/server/static/` is committed so a Go-only build can use the existing dashboard: `CGO_ENABLED=0 go build -o verloc ./cmd/verloc`. After frontend changes, rebuild assets and the binary; an old binary cannot pick up new assets from disk.

CI uses Node 24 with the locked dashboard dependencies, runs the catalog
regressions and frontend lint/type-check/build, then vets/tests Go and builds
the CGO-free binary with the freshly generated assets. Pushing a `v*` tag triggers
[`.github/workflows/release.yml`](.github/workflows/release.yml) to build and
publish cross-platform release tarballs with SHA-256 checksums, multi-arch
container images (`linux/amd64`, `linux/arm64`) to `ghcr.io/getverloc/verloc`,
and SLSA build provenance attestations (`gh attestation verify`).

```text
VERSION                  Canonical SemVer version (MAJOR.MINOR.PATCH)
install.sh               One-command installer, updater, and service manager (Linux/macOS)
Dockerfile               Multi-stage container image (non-root, CGO-free)
docker-compose.yml       One-command loopback-safe Compose stack
config.example.json      Minimal key-less nvidia bootstrap example
cmd/verloc/              CLI entry point
internal/server/         HTTP gateway, auth, management, analytics, embedded assets
dashboard/               React dashboard source (builds → internal/server/static)
scripts/                 Release build, container entrypoint, and catalog tooling
```

## License

Released under the [MIT License](LICENSE).

Copyright (c) 2026 Kunal Kongkan Kashyap.
