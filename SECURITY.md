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
- **`oom` can take a host down.** It refuses to run without a cgroup memory
  limit unless `BOP_OOM_UNSAFE=1` is set. Do not set it on a machine you care about.

## Reporting a vulnerability

For anything beyond the behaviour above, such as an issue in the image build,
the release signing or the workflows, please use GitHub's private reporting:
<https://github.com/jackspiering/bop/security/advisories/new>. Please do not
open a public issue for it.

## Verifying images

Release and `:edge` images are signed with keyless cosign and carry an SBOM and
provenance. See "Image" in the [README](README.md) for the `cosign verify` command.
