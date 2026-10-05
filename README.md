# WB2Hub Lite

Android app that hosts an OpenAI-compatible gateway for CodeBuddy / WorkBuddy,
with no external server: the gateway runs on the phone.

```
MainActivity                  status board: accounts, endpoint, key
GatewayService                foreground service that owns the gateway's lifetime
      ↓ ProcessBuilder
libwb2hub.so                  the Go gateway, shipped inside jniLibs/arm64-v8a/
      ↓
127.0.0.1:7863                /v1/*  OpenAI-compatible API
                              /admin/*  JSON API for this UI
```

## Why the gateway is a `.so`

Android 10+ applies W^X to app-private storage, so a plain file cannot be
executed. Only `nativeLibraryDir` (`/data/app/<pkg>/lib/arm64/`) may be exec'd,
and the packager only puts things there if they look like a shared library.

Android 6+ also normally packages `.so` files compressed and page-aligned for
mmap at install time, which leaves no standalone file on disk — so that is not
executable either. Two settings fix it, and both are required:

- `packaging { jniLibs { useLegacyPackaging = true } }` in `app/build.gradle`
- `android:extractNativeLibs="true"` in `AndroidManifest.xml`

The Go binary is statically linked (`CGO_ENABLED=0`), so it needs no NDK sysroot.

## Features

**Account pool** — weighted selection across several upstream identities, with
per-account cooldowns, soft rate limits, error circuit breaking, and a manual
disable that survives restarts.

**Dual realm** — the international build (workbuddy.ai) and the China build
(copilot.tencent.com / codebuddy.cn) have different hosts, User-Agents and
feature sets. The pair is kept together per account so the wrong UA never reaches
the wrong host.

**Proxy slots** — each slot is a named outbound proxy. Accounts are bound to
slots, so different clients can take different routes without a global switch.
Probing reports the exit IP plus its country, ASN, ISP and whether it is a
residential or datacenter address (via ip-api.com).

**Per-key egress and usage attribution** — multiple API keys, each optionally
bound to its own realm, each with its own request/token/credit counters.

**Daily quotas** — a daily credit limit (past which only free models are
served), an account-wide daily token limit, a per-model daily token limit that
blocks only the offending model, and a reserve floor that keeps an account from
being drained to zero.

**Gateway-run web tools** — the upstream has no search service, so the gateway
executes `web_search` (DuckDuckGo HTML) and `web_fetch` itself and feeds the
results back to the model.

**Growth centre** (China realm) — task list, accept, claim, energy, streak,
heatmap, lottery, tier redemption, makeup cards, the cat's buddy/travel cycle,
and the night-owl window. Tasks that only a real desktop client can complete are
reported as skippable rather than silently retried: faking the report event does
not advance them, and a panel that shows them as failures trains the operator to
ignore failures.

**Trial and quota healing** — the daily check-in, the IDE trial grant, the
compensation and gift claims, plus a resource view. A healer tracks per-account
back-off so a claim that upstream says was already taken is settled once instead
of being retried on every pass.

**Daily scheduler** — check-in and the cat's trip at 09:00/21:00, token
keepalive at 22:00, the night-owl task at 01:00. It runs on wall-clock hours
rather than an interval because the rewards are time-boxed windows: a missed
window is a day of credit gone, not a retryable error. Each slot carries its own
last-fired stamp, so a job that shares an hour with another does not suppress it,
and a slot missed while the phone was asleep runs late instead of being skipped.

**Huawei CodeArts upstream** — the third upstream, alongside the international
and China builds. It authenticates with an AK/SK pair exchanged for a short-lived
STS credential, signs every request with `SDK-HMAC-SHA256`, and binds each call
to a DPoP proof (`ES256`, RFC 9449) plus PKCE `S256` for the browser flow. Both
an interactive browser login and a direct AK/SK import are supported.

**Log ring** — a bounded, sequence-numbered log buffer the panel polls
incrementally, with each line classified by level and subsystem. The standard
logger is tee'd into it, so a startup failure or panic traceback is visible in
the app instead of only on a stderr the Android host discards.

## Build

```bash
make gateway     # cross-compile the Go service into jniLibs
make apk         # build a signed release APK
make test vet fmt
```

Output: `app/build/outputs/apk/release/app-release.apk`

`make apk` depends on `make gateway` on purpose — packaging a stale gateway is
the one failure this layout is designed to prevent. Pushing a tag builds and
attaches the APK via `.github/workflows/build-apk.yml`.

### Building on an arm64 host

The Android SDK and AGP ship **x86-64** `aapt2`, so on an arm64 machine the
build fails with `AAPT2 Daemon startup failed`. Point AGP at a shim that
dispatches through qemu-user:

```properties
# gradle.properties
android.aapt2FromMavenOverride=/opt/aapt2-shim/aapt2
```

where the shim runs `qemu-x86_64-static` with an x86-64 sysroot
(`QEMU_LD_PREFIX`). Editing the binary inside Gradle's transform cache does not
work: AGP re-extracts it into a fresh content-hashed directory on every build.

## Install

The APK is self-contained — no server, no config file, no account setup screen
beyond pasting a credential. On first launch it generates an API key, starts the
gateway on `127.0.0.1:7863`, and shows both on screen for a client to use.

The signing key is committed at `keystore/wb2hub.p12` so successive builds share
one certificate and later versions install straight over earlier ones.

## Known limitations

- **No login flow for the WorkBuddy realms.** Accounts are imported by pasting an
  `access_token`; the OAuth / device-code flows for those two are not
  implemented. CodeArts is the exception — it has both a browser PKCE flow and a
  direct AK/SK import.
- **Upstream contracts are unverified against a live account.** The local logic
  (pool, slots, keys, quotas, the scheduler's firing rules, the signing and DPoP
  encodings) is covered by tests and was exercised end-to-end; the upstream
  request shapes were ported from a reference implementation and have not been
  run against real credentials. The CodeArts ticket-polling field names, the STS
  request body and the DPoP scope are the least certain of these — see
  `docs/CODEARTS-REVERSE-NOTES.md`, which lists each unknown explicitly.
- The gateway listens on loopback only. Binding it anywhere else forces API key
  authentication on automatically, but nothing here configures TLS or reverse
  proxying.

## API surface

Beyond the OpenAI-compatible `/v1/*` routes, the gateway exposes an admin
surface. Every route requires the API key as a bearer token.

| Route | Purpose |
| --- | --- |
| `GET /admin/overview` | Accounts, counts, quotas, uptime |
| `GET/POST /admin/accounts` | List accounts / import a credential |
| `GET/POST /admin/proxy/slots` | List or create egress slots |
| `GET /admin/proxy/discover` | Probe the usual local proxy ports |
| `GET/POST /admin/keys` | List or mint API keys |
| `GET /admin/limits` | Quota configuration |
| `GET /admin/webtools` | Local web-tool executor state |
| `GET /admin/logs` | Log ring, with a `limit` parameter |
| `POST /admin/gateway/restart` | Record a restart request and exit |
| `GET /admin/scheduler` | Live schedule and last batch |
| `POST /admin/scheduler/run?job=` | Run one job now |
| `POST /admin/scheduler/config` | Replace the schedule |
| `GET /admin/growth` | Growth-centre view |
| `POST /admin/growth/<action>` | Accept, claim, draw, redeem, travel, buddy |
| `GET /admin/trial` | Trial and resource view |
| `POST /admin/trial/<action>` | Claim, check-in, compensation, gift |
| `POST /admin/codearts/login/start` | Begin the PKCE browser flow |
| `GET /admin/codearts/login/status` | Poll that flow |
| `POST /admin/codearts/import` | Import an AK/SK pair or a ticket id |
| `GET /admin/codearts/status` | Which account holds a credential |
| `GET /admin/codearts/models` | Models the credential can reach |
| `POST /admin/codearts/checkin` | Claim the CodeArts daily check-in |

### Environment

`TW2H_*` variables override the JSON config file, which overrides the defaults —
the environment wins because that is how the Android host passes its writable
paths in. Beyond the path and quota variables:

| Variable | Meaning |
| --- | --- |
| `TW2H_SCHEDULER` | `1` enables the daily scheduler (default off) |
| `TW2H_CHECKIN_HOURS` | Comma-separated hours, e.g. `9,21` |
| `TW2H_TRAVEL_HOURS` | Comma-separated hours |
| `TW2H_KEEPALIVE_HOURS` | Comma-separated hours |
| `TW2H_CAT_HOURS` | Comma-separated hours |
| `TW2H_LOG_CAPACITY` | Log ring size, default 500 |

The scheduler is off by default on purpose: it touches every account on a timer,
so enabling it should be a decision rather than a discovery.
