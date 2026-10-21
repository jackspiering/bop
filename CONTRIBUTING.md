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
Engine 25.0 or later (for `--start-interval`), Docker Compose v2 and cgroup v2
(for `oom`). They take about a minute.

## Changes

- Every new mode or knob needs a table-driven unit test, an integration test and
  an `examples/<name>/compose.yaml` that `TestExamplesRun` brings up.
- Pin GitHub Actions to a full commit SHA with the version as a trailing
  comment, and base images by digest. Dependabot keeps both current.
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
