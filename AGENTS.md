# AGENTS.md

Working notes for agents and humans making changes to open-seed-engine. See
`README.md` for what the engine is and how it is installed; this file covers
how to change it.

## Build and test

```sh
gofmt -l .        # must print nothing
go vet ./...
go build ./...
go test ./...
```

CI (`.github/workflows/ci.yml`) runs exactly those four on every pull request
and on pushes to `main`, in that order. `gofmt` is a hard gate: the step is
`test -z "$(gofmt -l .)"`, so a single unformatted file fails the build. Go
1.25 or newer (`go.mod` pins `go 1.25.0`); CI resolves `stable`.

## Layout

`cmd/seed` is the CLI seam: argument parsing, envelope emission, and exit
codes, with the CLI-level tests that drive every subcommand through `run()`.
Everything else is `internal/<pkg>`, mirroring the responsibilities in the
cross-repo architecture map. Two orientation points: the pure decision core
is `internal/port`, which deliberately contains no policy of its own and
reads the contract from the port-schema tables, and the store boundary is
`internal/stateref` plus `internal/fastcards` behind one `Backing` interface.

The contract is data, not branching. Transition legality, verb classes, and
effects come from `.seed/port-schema/` (mirrored for tests under
`internal/spec/testdata/seed/port-schema/`). When behavior needs to change,
change the tables; hand-written conditionals that re-derive what the tables
already say are a design violation.

## Prose style: no em-dashes in comments or doc metadata

Do not use `—` (U+2014) in code comments, docstrings, YAML step labels, or
JSON schema `description` / `$comment` metadata. Use a colon when the second
half explains the first, a comma when it is an aside, or a full stop.

Runtime string literals are the exception and keep their em-dashes: error
messages, refusal text, help output, and usage strings are written for the
operator reading them in a terminal, and are left alone. About seventy of
them exist today, mostly in `internal/validate`, `internal/receipt`, and
`internal/skills`.

**Paired dashes need rewording, not substitution.** A single dash maps
cleanly onto a colon or comma. A matched *pair* bracketing an aside does
not: replacing both delimiters with the same character collapses the
bracketing and produces a double colon or a comma splice. Rewrite those.
Worked examples, all now in the tree:

| Was | Became |
| --- | --- |
| `lifecycle — create, … cascade — runs against` | `lifecycle: create, … cascade. It runs against` |
| `nothing the store holds — states, … the run log — can be lost` | `nothing the store holds can be lost in translation: states, …` |
| `exempt from pairwise overlap — it necessarily intersects every scope — but` | `exempt from pairwise overlap (it necessarily intersects every scope), but` |
| `(contention — and, with a distinct error string, …)` | `(contention; and, with a distinct error string, …)` |

The last one is the tell for the general case: reach for the punctuation
that still outranks whatever punctuation the sentence already contains.

To check a change before pushing:

```sh
git grep -nE '(//|/\*).*—' -- '*.go'
git grep -l '—' -- '*.yaml' '*.yml' '*.json' '*.md' ':!AGENTS.md'
```

Both should come back empty. The first pattern is a heuristic, not a Go
parser: it catches trailing and block comments as well as whole-line ones,
but a string literal containing `//` can false-positive, and a dash inside a
multi-line raw string can hide from it. When a hit is ambiguous, widen to
`git grep -n '—' -- '*.go'`, which lists every dash in the package; each one
should sit inside a runtime string literal.

This file is excluded from the second sweep because the table above quotes
the dashes it is teaching you to remove; it is the only file in the tree
allowed to contain them outside a runtime string.

## Commits and pull requests

Subject lines are `<area>: <lowercase summary>`, where the area is the
package or surface touched (`task:`, `skills:`, `mcp:`, `sync:`,
`upgrade:`, `workflow:`, `validate:`) or one of `docs:`, `test:`,
`release:`. Bodies explain why, and cite the design section (`§7.1`,
`D3`, `R11`) or plan id when the change is spec-driven.

Branch off `main`, open a pull request, and let CI decide. If `main` has
moved, rebase onto it rather than opening a pull request that re-displays
already-merged commits: the three-dot diff GitHub renders will show the
whole merged range even when the net change is a few lines.

## Releasing

Releases are driven by the `VERSION` file: bump it and push to `main`, and
the `release` workflow mints the tag at HEAD in-runner. Do not bump it as a
side effect of unrelated work, and do not cut a release for changes that
alter no behavior. `PROTOCOL` is a separate seam and moves only when the
protocol version itself does.
