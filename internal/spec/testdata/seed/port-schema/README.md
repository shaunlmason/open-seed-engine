# `.seed/port-schema/` — the port contract as data

These files are the **normative, machine-readable form** of the task-port contract
defined in [`docs/design-options.md`](../../docs/design-options.md) D1 (state
machine) and §7.1 (claim protocol, verb classes). The engine implements the port
**table-driven from these files** — never from hand-written branching — so an edit
here changes engine behavior with no engine code change (design §7.5; build plan
Phase 1). On any conflict, the design doc governs and these files are fixed to
match; diverging here without a reviewed design-doc edit is a design violation.

| File | Contents |
|---|---|
| `port.json` | Protocol version, states, terminal states, exit-code registry |
| `transitions.json` | The D1 edge table: every legal transition with its verb, class (worker/operator), effects, overrides, and auto-paths |
| `verbs.json` | The nine required port verbs + optional capabilities, with input/output field specs |
| `envelope.schema.json` | JSON Schema for the `--json` response envelope every verb emits |
| `card.schema.json` | JSON Schema for task-card frontmatter (the filecards representation of port state) |
| `backend.schema.json` | JSON Schema for `backend.toml` plugin manifests (validated after TOML→JSON) |

Decisions recorded here (consistent with the design doc, made concrete):

- **Exit codes** follow research/10 §5.4 as amended by its erratum: `0` ok, `2`
  claim not granted (contention — and, with a distinct `error` string,
  `rejected_author` lockout), `3` invalid transition, `4` not found, `5` backend
  unavailable, `6` fenced out (stale/missing claim token), `10` schema/protocol
  version mismatch. CLI usage errors are `64` and are not port results.
- **`claim` is the token-minting bootstrap exception** (§7.1): it presents no
  token and mints one; every other worker verb presents `--token`.
- **Reap** is modeled as an operator override on the `in_progress → ready` edge,
  legal only when the lease is expired.
- **`close`** is `accept` plus the blocker-cascade, valid only from `review`.
- **Auto-paths** on `blocked → ready` (dep-cascade, plan-unblock) each remove only
  their own `blocked_on` entry; the transition fires only when the set empties.
