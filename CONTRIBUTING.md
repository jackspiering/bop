# Contributing

BoP stays small: Go standard library only, no third-party modules, and it must
not know anything about specific orchestrators or tools.

## Tests

```sh
gofmt -l .                                   # must print nothing
go vet ./... && go vet -tags integration ./...
go test -race ./...                          # unit tests
go test -tags integration -timeout 15m ./test/integration/   # needs Docker
```

The integration tests build `bop:test` from the working tree, drive real Docker
through the `docker` CLI and assert on `docker inspect`. They need Docker
Engine 25.0+ (to build the image), Docker Compose 2.18+ (the example tests drop
published ports with `!reset`) and cgroup v2 (for `oom`). They take about a
minute. A timed-out or interrupted run cannot clean up after itself, so the
suite removes leftover `bop-it-*` containers, networks and volumes when it
starts and again when it ends. Do not run two integration suites against the
same Docker daemon at once.

## Changes

- Every new mode or knob needs a table-driven unit test, an integration test and
  an `examples/<name>/compose.yaml` that `TestExamplesRun` brings up.
- Pin GitHub Actions to a full commit SHA with the version as a trailing
  comment, and base images by digest. Dependabot keeps both current.
- The Go version in `go.mod` and the `golang:` tag in the `Dockerfile` must
  match; `go test ./test/release/` fails otherwise, so a Dependabot golang tag
  PR needs a matching `go.mod` edit.
- The build tools are pinned by digest by hand, because Dependabot does not
  track them: the BuildKit daemon (`driver-opts: image=moby/buildkit:...` on
  every `setup-buildx-action`, the same reference in `ci.yml` and
  `publish.yml`) and the SBOM scanner
  (`sbom: generator=docker/buildkit-syft-scanner:...` in `publish.yml`). To
  bump one, pick a release tag, run `docker buildx imagetools inspect
  <image>:<tag>` and pin `<image>:<tag>@sha256:<index digest>`.
  `go test ./test/release/` fails if either is unpinned. Do not add a
  `# syntax=` line to the Dockerfile: the pinned daemon's built-in frontend
  is used.
- Workflows default to `permissions: contents: read`; widen per job only.
- Open a pull request against `main`. The four CI checks must pass.

## Releases

A maintainer pushes a `vX.Y.Z` (or `vX.Y.Z-rc.N`) tag on a commit that is on
`main`; the publish workflow does the rest. Tags must be strict SemVer with no
`+build` metadata, or the workflow refuses them. Release notes are generated
from merged pull requests, so there is no separate changelog.

A version can be published only once. If a release run fails, re-run it from
the Actions tab instead of deleting and re-pushing the tag: once `:X.Y.Z`
exists, a re-run reuses its digest instead of rebuilding. If a release is
wrong, cut a new version.
