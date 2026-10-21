# BoP (Broken on Purpose)

[![Publish](https://github.com/jackspiering/bop/actions/workflows/publish.yml/badge.svg?branch=main)](https://github.com/jackspiering/bop/actions/workflows/publish.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A container that fails on demand: unhealthy, crash, crash-loop, OOM, hang and
SIGTERM behaviours, switchable at runtime. It knows nothing about any
orchestrator, so you can point it at anything that watches, heals or
orchestrates containers: autoheal-style tools, Compose `depends_on`, restart
policies, Swarm, sidecars.

> **Not for production.** BoP is built to misbehave. Do not put it behind
> anything you care about, and do not expose its ports to untrusted networks.

- Image: `ghcr.io/jackspiering/bop` (linux/amd64, linux/arm64), see [Releases](#releases)
- Go standard library only, static binary, distroless nonroot (uid 65532)
- MIT licensed

## Quick start

```sh
docker run -d --name bop -p 8080:8080 ghcr.io/jackspiering/bop:edge
curl -i localhost:8080/healthz            # 200 ok

docker exec bop bop unhealthy             # /healthz now returns 503
docker inspect -f '{{.State.Health.Status}}' bop   # unhealthy, after a few probes
docker exec bop bop healthy               # and back
```

The healthcheck in the image uses `--start-interval`, which needs **Docker
Engine 25.0 or later** (older engines reject the image's `HEALTHCHECK`; on
those, override it).

## One grammar

Boot and runtime use the same grammar: `<mode> [key=value ...]`. Durations are
Go durations (`500ms`, `10s`, `2m`). Invalid input exits `2` with a usage message.

```sh
docker run -e BOP_MODE="crash exit=3 after=10s" ghcr.io/jackspiering/bop:edge   # at boot
docker exec <container> bop crash exit=3 after=10s                               # at runtime
```

At runtime the CLI sends the mode to PID 1 over the control listener, prints
the resulting state and exits non-zero on failure. `docker exec` works because
the image has `bop` on `PATH`.

### Modes

| Mode | Arguments | Behaviour |
|---|---|---|
| `healthy` | | `/healthz` returns 200. The default. |
| `unhealthy` | `after=<dur>` (0) | Healthy until `after`, then `/healthz` returns 503. The process keeps running. |
| `crash` | `exit=<code>` (1), `after=<dur>` (0) | Exits with `code` after `after`. Via exec it responds first, then exits. |
| `crash-loop` | `interval=<dur>` (5s), `exit=<code>` (1) | Each process start exits with `code` after `interval`. Only as boot mode or with `sticky=true`; a plain exec is rejected. |
| `oom` | `rate=<MiB/s>` (64) | Allocates and writes every page until the cgroup kills the process (exit 137, `OOMKilled=true`). Refuses (exit 2) when `/sys/fs/cgroup/memory.max` is `max`, unless `BOP_OOM_UNSAFE=1`. Needs cgroup v2. |
| `hang` | | `/healthz` and `/info` accept the connection and never answer. The process stays alive and the control listener keeps working, so `bop healthy` recovers it. |

Any mode accepts `sticky=true`, see [Sticky](#sticky-restart-vs-recreate).
`exit` is 0 to 255.

### Other commands

| Command | |
|---|---|
| `bop` / `bop serve` | The main process. |
| `bop status` | Print the running process's state. |
| `bop clear` | Back to `healthy` and remove the sticky mode. |
| `bop healthcheck [--url=URL]` | Exit 0 if `URL` returns 2xx, else 1. Default `http://127.0.0.1:<port of BOP_ADDR>/healthz`, 1.5s timeout. |
| `bop sidecar --target=URL [--interval=2s]` | See [Sidecar](#sidecar). |
| `bop version` | |

### Knobs (environment, boot only)

| Variable | Default | |
|---|---|---|
| `BOP_MODE` | `healthy` | Initial mode, same grammar as above. |
| `BOP_START_DELAY` | `0` | Don't bind the data listener until this has passed. The control listener binds immediately. |
| `BOP_ON_SIGTERM` | `exit:0` | `exit:<code>`, `ignore`, or `delay:<dur>` (exit 0 once the delay has passed). `ignore` exists to test the stop timeout and SIGKILL (137). SIGTERM and SIGINT are handled explicitly because PID 1 ignores unhandled signals. |
| `BOP_ADDR` | `:8080` | Data listener. The image's healthcheck follows its port. |
| `BOP_CONTROL_ADDR` | `127.0.0.1:9901` | Control listener. `docker exec` inherits it from the container's environment. |
| `BOP_STATE_DIR` | `/tmp/bop` | Where sticky modes are stored. |
| `BOP_OOM_UNSAFE` | | `1` allows `oom` without a memory limit. It can take the host down. |

## Endpoints

- `GET /healthz`: `200 ok`, or `503` with a short reason.
- `GET /info`: JSON with `instance_id` (random per process start), `pid`,
  `hostname`, `started_at`, `mode`, `sticky`, `version`.
- Control listener (`BOP_CONTROL_ADDR`): a small JSON API (`GET /status`,
  `POST /mode`, `POST /clear`) used by the CLI only.
  **Caveat:** anything that shares the container's network namespace can reach
  it and switch the mode: sidecars using `network_mode: service:` or
  `container:`, and `--network host`. It binds to loopback by default.

One structured `log/slog` text line is written to stdout per state change.

## Sticky: restart vs recreate

`sticky=true` persists the mode to `$BOP_STATE_DIR/mode` (default `/tmp/bop`),
inside the container's writable layer:

- `docker restart` keeps the writable layer, so the mode survives.
- Recreating the container (`docker rm`, `compose up --force-recreate`) starts
  a fresh layer, so it does not.

On start, a sticky file overrides `BOP_MODE`. `bop healthy sticky=true` and
`bop clear` remove it. A non-sticky switch does not touch the file, so the
sticky mode returns on the next restart. Sticky cannot work with
`read_only: true`; bop detects the write failure, says so, and leaves the mode
unchanged. Note that `bop crash sticky=true` makes the container crash on every
restart; recreate it to get out.

```sh
docker exec bop bop unhealthy sticky=true
docker restart bop                      # still unhealthy
docker rm -f bop && docker run ...      # healthy again
```

## Sidecar

`bop sidecar --target=http://127.0.0.1:8080` binds nothing. It idles, handles
signals, polls the target's `/info` (a bare URL gets `/info` appended) and logs
when the target becomes unreachable or its `instance_id` changes.

In a sidecar on `network_mode: service:<parent>` (or `container:`), its
healthcheck is `bop healthcheck --url=http://127.0.0.1:8080/info`, which proves
it still shares the parent's network namespace. When the parent restarts, the
sidecar keeps the old namespace, the probe fails and the sidecar goes
unhealthy. That makes a handy test for tools that must also restart or
re-attach dependents. See [`examples/sidecar`](examples/sidecar/compose.yaml).

## Compose examples

Every example uses `${BOP_IMAGE:-ghcr.io/jackspiering/bop:edge}`.

| Mode or knob | Example |
|---|---|
| `healthy` | [`examples/healthy`](examples/healthy/compose.yaml) |
| `unhealthy` | [`examples/unhealthy`](examples/unhealthy/compose.yaml) |
| `crash` | [`examples/crash`](examples/crash/compose.yaml) |
| `crash-loop` | [`examples/crash-loop`](examples/crash-loop/compose.yaml) |
| `oom` | [`examples/oom`](examples/oom/compose.yaml) |
| `hang` | [`examples/hang`](examples/hang/compose.yaml) |
| `BOP_START_DELAY` | [`examples/start-delay`](examples/start-delay/compose.yaml) |
| `BOP_ON_SIGTERM` | [`examples/sigterm`](examples/sigterm/compose.yaml) |
| `BOP_ADDR`, `BOP_CONTROL_ADDR` | [`examples/custom-addr`](examples/custom-addr/compose.yaml) |
| sidecar | [`examples/sidecar`](examples/sidecar/compose.yaml) |
| sticky | [`examples/restart-vs-recreate`](examples/restart-vs-recreate/compose.yaml) |

The distroless image has no shell, so write healthchecks in exec form
(`test: ["CMD", "/bop", "healthcheck"]`). `docker run --health-cmd` always uses
a shell and cannot run `bop`; the image's own `HEALTHCHECK` is already set.

## Image

Multi-stage build, `CGO_ENABLED=0`, `-trimpath`, stripped, on
`gcr.io/distroless/static-debian13:nonroot` pinned by digest. It runs as
`65532:65532` and exposes `8080`. The image is about 17 MB unpacked
(about 4 MB as a gzipped `docker save` of one platform), most of it the
distroless base and the Go HTTP stack.

```sh
docker build -t bop:dev .
```

Releases are signed with keyless [cosign](https://github.com/sigstore/cosign)
and carry an SBOM and max-mode provenance:

```sh
cosign verify ghcr.io/jackspiering/bop:edge \
  --certificate-identity-regexp '^https://github.com/jackspiering/bop/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

That accepts a signature from any workflow in this repository. To accept only
the publish workflow, running on `main` or on a release tag, use the stricter
form (CI runs it against every image it publishes):

```sh
cosign verify ghcr.io/jackspiering/bop:0.1.0 \
  --certificate-identity-regexp '^https://github\.com/jackspiering/bop/\.github/workflows/publish\.yml@refs/(heads/main|tags/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Releases

| Tag | Meaning |
|---|---|
| `:0.1.0` | An immutable release. Pin this (or its digest) in tests. |
| `:0.1` | The highest patch of a minor version. |
| `:latest` | The highest stable release. |
| `:edge` | The tip of `main`. Moves; `bop version` shows `edge-<sha>`. |
| `:sha-<short>` | One specific commit on `main`, pushed by that commit's `main` build. Release tags do not touch it. |

Pushing a `vX.Y.Z` tag on a commit that is on `main` runs the same CI, then
publishes the versioned tags, signs the digest and creates the GitHub Release
with generated notes. Tags like `v1.0.0-rc.1` publish only their exact version
and mark the release as a pre-release. A release tag must be strict
[SemVer](https://semver.org/), `vMAJOR.MINOR.PATCH` or
`vMAJOR.MINOR.PATCH-PRERELEASE`, with no leading zeros and no `+build`
metadata. The workflow refuses any other tag before it pushes anything.

`:X.Y.Z` never changes once published. The workflow refuses to publish a
version that already exists in the registry. A re-run of a release run ("Re-run
all jobs" or "Re-run failed jobs") is safe: it reuses the published digest
instead of rebuilding, and skips a GitHub Release that already exists.
`:latest`, `:X.Y` and `:edge` only move forward: tagging an older version, or
re-running an older workflow run, leaves them where they are.

All tags are signed the same way, so the `cosign verify` command above works
for any of them. The image is pushed by digest first, and a tag only moves to
that digest after it is signed and the strict `cosign verify` passes.

## Development

```sh
go vet ./... && go test -race ./...                 # unit tests
go test -tags integration -timeout 15m ./test/integration/   # needs Docker
```

The integration tests build `bop:test`, drive real Docker through the `docker`
CLI and assert on `docker inspect` (health, exit code, `OOMKilled`, restart
count). They need Docker Engine 25.0+ and cgroup v2.

## Maintainer: one-time steps

After the first publish on `main`, set the ghcr package visibility to **Public**
in the package settings (github.com → your profile → Packages → `bop` →
Package settings), and confirm the package is linked to this repository.

## Prior art

- [bretfisher/browncoat](https://github.com/BretFisher/browncoat): a Node.js
  (Express) image that misbehaves in simple ways, configured by environment
  variables plus a few HTTP toggles (slow or failed startup, exit with a code,
  failing or slow healthchecks).
- [hernancete/docker-healthcheck](https://github.com/hernancete/docker-healthcheck)
  (`healthcheck-test`): a tiny image whose health you flip with
  `docker exec <c> getSick` and `docker exec <c> heal`.

What BoP adds: one grammar that switches every mode at runtime, plus `hang`,
OOM, crash-loop and SIGTERM behaviours (exit code, ignore, delay), sticky
restart-versus-recreate semantics, the network-namespace sidecar check, a small
static nonroot image, and signed images with SBOM and provenance.

## License

MIT, see [LICENSE](LICENSE).
