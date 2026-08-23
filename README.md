# open-seed-engine

The protocol engine for [open-seed](https://github.com/shaunlmason/open-seed): the
pinned `seed` CLI binary that the template's bootstrap shim downloads, hash-verifies,
and execs.

Per open-seed design decision 7.5, this repo carries the protocol-critical core:
the task port, claim/lease/fence protocol, validators, `receipt verify`, fan-out
sync, `init`, as one static, cross-compiled Go binary. The *contract* (schemas,
transition table, guardrails) stays as files in the open-seed template; this engine
is a replaceable implementation of that spec, and a repo without it installed must
remain workable.

## Status

The full v1 port is shipped, and the v2 surface is in: the task verbs
(create/ready/get/list/claim/transition/release/close + the operator
verbs), the `seed-state` ref store and the builtin `fastcards` SQLite
store, `receipt generate`/`verify`, the validators, `seed sync` fan-out,
`seed upgrade` / `template upgrade`, `init` / `init-github`,
`state lint|anchor|export|import|resume`, `maintain reap|report`,
`mirror plan|record`, mail + handoff packets, the checked-in workflow
engine (`seed workflow validate|run`), the skills manifest/lockfile,
and the MCP stdio transport (`seed mcp serve`). The current release is
the `VERSION` file (releases are cut from it).

The [open-seed build plan](https://github.com/shaunlmason/open-seed/blob/main/docs/build-plan.md)
tracks per-phase acceptance; the [architecture
map](https://github.com/shaunlmason/open-seed/blob/main/docs/architecture.md)
is the cross-repo design map (layering, the port, the evidence chain,
where each gate grounds). Package map: `internal/<pkg>` mirrors those
responsibilities: the pure table-driven decision core is
`internal/port`, and the store boundary is `internal/stateref` +
`internal/fastcards` behind one `Backing` interface.

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

`0` success · `2` claim contention · `3` invalid transition · `4` not found ·
`5` backend unavailable · `6` fenced out (stale claim token) · `10`
protocol-version mismatch: the full registry is reserved by the port
contract (`.seed/port-schema/port.json`). CLI usage errors exit `64`
(EX_USAGE) so they can never be mistaken for a port result.

## Releasing

Releases are driven by the `VERSION` file: bump it (e.g. to `v0.2.0`) and push to
`main`. The `release` workflow mints the tag at HEAD in-runner, so the tag and
the released commit can never disagree, and no contributor needs tag-push
rights, then runs goreleaser across linux/darwin/windows × amd64/arm64,
publishes archives + `checksums.txt`, and attests provenance.
(`workflow_dispatch` with a `tag` input does the same manually.)
