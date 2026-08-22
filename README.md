# open-seed-engine

The protocol engine for [open-seed](https://github.com/shaunlmason/open-seed): the
pinned `seed` CLI binary that the template's bootstrap shim downloads, hash-verifies,
and execs.

Per open-seed design decision 7.5, this repo carries the protocol-critical core —
the task port, claim/lease/fence protocol, validators, `receipt verify`, fan-out
sync, `init` — as one static, cross-compiled Go binary. The *contract* (schemas,
transition table, guardrails) stays as files in the open-seed template; this engine
is a replaceable implementation of that spec, and a repo without it installed must
remain workable.

## Status

**Phase 0** of the [build plan](https://github.com/shaunlmason/open-seed/blob/main/docs/build-plan.md):
the release pipeline is proven end-to-end with a stub binary (`seed version` only).
Port verbs arrive in Phase 1.

## Install

Pinned binaries (what the shim uses) come from
[GitHub Releases](https://github.com/shaunlmason/open-seed-engine/releases): each
release carries a `checksums.txt` and GitHub build-provenance attestations
(verify with `gh attestation verify <artifact> -R shaunlmason/open-seed-engine`).

From source:

```sh
go install github.com/shaunlmason/open-seed-engine/cmd/seed@latest
```

## Exit codes

`0` success · `2` claim contention · `3` invalid transition · `6` fenced out
(stale claim token) · `10` protocol-version mismatch — reserved by the port
contract (Phase 1+). CLI usage errors exit `64` (EX_USAGE) so they can never be
mistaken for a port result.

## Releasing

Releases are driven by the `VERSION` file: bump it (e.g. to `v0.2.0`) and push to
`main`. The `release` workflow mints the tag at HEAD in-runner — so the tag and
the released commit can never disagree, and no contributor needs tag-push
rights — then runs goreleaser across linux/darwin/windows × amd64/arm64,
publishes archives + `checksums.txt`, and attests provenance.
(`workflow_dispatch` with a `tag` input does the same manually.)
