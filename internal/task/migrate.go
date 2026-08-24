// Backend migration (fastcards plan step 6): `seed state export` dumps the
// whole store: every card with its full history-bearing fields (they all
// live in the card files), handoff stubs, and the run log, as one JSON
// document; `seed state import` loads that document into the currently
// configured backend, preserving paths (and therefore ids) exactly, and
// refuses a non-empty target. "Switch backends" is export → flip config →
// init → import, never a config line that strands state.
package task

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

func slicesContains(s []string, v string) bool { return slices.Contains(s, v) }

// StateExport is the migration document. Files are path-keyed verbatim, so
// nothing the store holds can be lost in translation: states, blocked_on
// edges, rejected authors, evidence, comments, the run log.
type StateExport struct {
	SchemaVersion string            `json:"schema_version"`
	Backend       string            `json:"backend"`
	Head          string            `json:"head"`
	Files         map[string]string `json:"files"`
}

// Export dumps the active store.
func (sv *Service) Export() *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	paths, err := sv.Store.ListAll(head)
	if err != nil {
		return errResult(err)
	}
	files := map[string]string{}
	for _, p := range paths {
		content, found, err := sv.Store.ReadFile(head, p)
		if err != nil {
			return errResult(err)
		}
		if found {
			files[p] = content
		}
	}
	doc := StateExport{SchemaVersion: "1.0", Backend: sv.Cfg.Coordination.Backend, Head: head, Files: files}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "state-export", "files": len(files), "document": json.RawMessage(b)})
}

// Import loads an export document into the active store. The target must be
// empty (no cards, empty run log): import is for populating a fresh store,
// never for silently merging two histories.
func (sv *Service) Import(raw []byte, actor string) *Result {
	if !sv.Cfg.IsOperator(actor) {
		return failure(spec.ExitInvalid, "operator_required", nil)
	}
	var doc StateExport
	if err := json.Unmarshal(raw, &doc); err != nil {
		return failure(spec.ExitInvalid, "bad_export_document", map[string]any{"detail": err.Error()})
	}
	if doc.SchemaVersion != "1.0" {
		return failure(spec.ExitVersionMismatch, "export_schema_mismatch", map[string]any{"got": doc.SchemaVersion})
	}
	var imported int
	_, err := sv.Store.Mutate(false, func(head string) (*stateref.Mutation, error) {
		existing, err := sv.Store.ListDir(head, "tasks")
		if err != nil {
			return nil, err
		}
		if len(existing) > 0 {
			return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "target_not_empty",
				Data: map[string]any{"detail": "the configured store already holds cards; import only fills a fresh store"}}
		}
		if log, found, err := sv.Store.ReadFile(head, "run-log.jsonl"); err != nil {
			return nil, err
		} else if found && strings.TrimSpace(log) != "" {
			return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "target_not_empty",
				Data: map[string]any{"detail": "the configured store's run log is non-empty; import only fills a fresh store"}}
		}
		paths := make([]string, 0, len(doc.Files))
		for p := range doc.Files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		// The import event is appended onto the IMPORTED run log directly:
		// Mutation.Events would append onto the target's pre-import (empty)
		// log and clobber the migrated history.
		files := make(map[string]string, len(doc.Files))
		for p, c := range doc.Files {
			files[p] = c
		}
		log := files["run-log.jsonl"]
		if log != "" && !strings.HasSuffix(log, "\n") {
			log += "\n"
		}
		files["run-log.jsonl"] = log + sv.event(actor, "state-import", "", map[string]any{
			"files": len(doc.Files), "source_backend": doc.Backend, "source_head": doc.Head}) + "\n"
		if !slicesContains(paths, "run-log.jsonl") {
			paths = append(paths, "run-log.jsonl")
			sort.Strings(paths)
		}
		changes := make([]gitx.Change, 0, len(paths))
		for _, p := range paths {
			changes = append(changes, gitx.Change{Path: p, Content: files[p]})
		}
		imported = len(doc.Files)
		return &stateref.Mutation{
			Message: "state import: " + doc.Backend + " export (" + doc.Head + ")",
			Changes: changes,
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "state-import", "files": imported})
}
