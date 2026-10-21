# Security

BoP is a test fixture that fails on purpose. Do not run it in production or
expose it to networks you do not trust.

## Known, by design

- **The control listener has no authentication.** It binds to
  `127.0.0.1:9901` by default, but anything that shares the container's network
  namespace can reach it and switch the mode: sidecars using
  `network_mode: service:` or `container:`, and `--network host`. If you change
  `BOP_CONTROL_ADDR` to a non-loopback address, any host that can reach that
  address can crash the container.
- **Browsers are out of scope for the control listener, and are rejected.**
  The control API is for the `bop` CLI only. Every request that carries an
  `Origin` or any `Sec-Fetch-*` header gets `403`, as does one whose `Host` is not
  an IP address, `localhost` or the host in `BOP_CONTROL_ADDR`, and a `POST`
  that is not `Content-Type: application/json` gets `415`. A web page therefore
  cannot switch the mode, or read `GET /status` through DNS rebinding, even with
  `--network host`. This applies to the whole listener, so any future endpoint
  must keep it. If you find a way for a browser to reach the control API,
  report it as a vulnerability.
- **`oom` can take a host down.** It refuses to run without a cgroup memory
  limit unless `BOP_OOM_UNSAFE=1` is set. Do not set it on a machine you care about.

## Reporting a vulnerability

For anything beyond the behaviour above, such as an issue in the image build,
the release signing or the workflows, please use GitHub's private reporting:
<https://github.com/jackspiering/bop/security/advisories/new>. Please do not
open a public issue for it.

## Verifying images

Release and `:edge` images are signed with keyless cosign and carry an SBOM and
provenance. See "Image" in the [README](README.md) for the `cosign verify`
command.

The strict identity in the README proves that `publish.yml` in this repository
signed the image while running on `main` or on a `vX.Y.Z[-PRERELEASE]` tag.
That is only as strong as the rules on those refs:

- A tag ruleset on `refs/tags/v*` restricts creating, moving and deleting
  release tags to repository admins.
- `main` is protected: changes land through pull requests that pass CI, with
  no force pushes or deletions.
- The publish workflow refuses tags that are not on `main` or not strict
  SemVer, refuses to republish an existing version, and only moves a tag to a
  digest after signing and verifying it.

A signature does not name the commit. To check what a release contains, compare
the `org.opencontainers.image.revision` label, or the provenance, with the tag.
