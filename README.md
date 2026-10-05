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

- **No login flow.** Accounts are imported by pasting an `access_token`. The
  OAuth / device-code flows are not implemented.
- **Upstream contracts are unverified against a live account.** The local logic
  (pool, slots, keys, quotas) is covered by tests and was exercised end-to-end;
  the upstream request shapes were ported from a reference implementation and
  have not been run against real credentials.
- **CodeArts upstream is absent.** The DPOP/PKCE/STS chain for Huawei CodeArts
  is not implemented.
- The gateway listens on loopback only. Binding it anywhere else forces API key
  authentication on automatically, but nothing here configures TLS or reverse
  proxying.
