// Package receipt implements the evidence chain (open-seed D4.5): a receipt
// records the merge-base plan pin and the diff it authorized. Above L1 the CI
// verify check regenerates the receipt and is the author of record: the
// committed copy is a claim, the regenerated one is the truth, and a mismatch
// fails verification (R11). Verify also enforces the stale-plan rule (D3): a
// superseded plan must be revocable, so the merge-base plan blob must equal
// the plan blob at the current base head.
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/plan"
	"github.com/shaunlmason/open-seed-engine/internal/prclass"
)

const diffExclude = ":(exclude)receipts"

type Validation struct {
	Cmd  string `json:"cmd"`
	Exit int    `json:"exit"`
}

type Receipt struct {
	SchemaVersion string       `json:"schema_version"`
	Task          string       `json:"task"`
	MergeBase     string       `json:"merge_base"`
	Head          string       `json:"head"`
	PlanPath      string       `json:"plan_path"`
	PlanSHA256    string       `json:"plan_sha256"`
	DiffFiles     []string     `json:"diff_files"`
	DiffSHA256    string       `json:"diff_sha256"`
	Validation    []Validation `json:"validation"`
	GeneratedBy   string       `json:"generated_by"`
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type Options struct {
	RunValidation bool
	GeneratedBy   string
}

// Generate builds the receipt for taskID on the current HEAD against baseRef.
// The plan is read from the merge-base blob, never the working tree (D3).
func Generate(repo *gitx.Repo, taskID, baseRef string, opts Options) (*Receipt, error) {
	mb, err := repo.Git("merge-base", baseRef, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("merge-base: %w", err)
	}
	head, err := repo.Git("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	planPath := "plans/" + taskID + ".md"
	planBlob, found, err := repo.CatFile(mb, planPath)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("no approved plan at merge-base (%s missing at %.12s) — implementation requires a merged plan (D3)", planPath, mb)
	}
	if errs := plan.Lint(planBlob); len(errs) > 0 {
		return nil, fmt.Errorf("merge-base plan fails lint: %v", errs)
	}

	filesOut, err := repo.Git("diff", "--name-only", mb, head, "--", ".", diffExclude)
	if err != nil {
		return nil, err
	}
	var files []string
	if strings.TrimSpace(filesOut) != "" {
		files = strings.Split(strings.TrimSpace(filesOut), "\n")
	}
	slices.Sort(files)
	// --full-index: the diff's index lines carry the full blob ids rather
	// than git's auto-abbreviated ones, whose length grows with the
	// clone's object count (seven hex digits below 16384 objects, eight
	// above), so a receipt generated in a partial clone and regenerated
	// in CI's full clone hash the same bytes (R11: CI is the author of
	// record, and the claim it checks must not depend on the clone).
	diffContent, err := repo.Git("diff", "--full-index", mb, head, "--", ".", diffExclude)
	if err != nil {
		return nil, err
	}

	r := &Receipt{
		SchemaVersion: "1.0",
		Task:          taskID,
		MergeBase:     mb,
		Head:          head,
		PlanPath:      planPath,
		PlanSHA256:    plan.Hash(planBlob),
		DiffFiles:     files,
		DiffSHA256:    sha(diffContent),
		GeneratedBy:   opts.GeneratedBy,
	}
	for _, cmd := range plan.Parse(planBlob).ValidationCommands {
		v := Validation{Cmd: cmd, Exit: -1} // -1 = not executed
		if opts.RunValidation {
			c := exec.Command("sh", "-c", cmd)
			c.Dir = repo.Dir
			if err := c.Run(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					v.Exit = ee.ExitCode()
				} else {
					return nil, fmt.Errorf("validation command %q: %w", cmd, err)
				}
			} else {
				v.Exit = 0
			}
		}
		r.Validation = append(r.Validation, v)
	}
	return r, nil
}

func Path(repoRoot, taskID string) string {
	return filepath.Join(repoRoot, "receipts", taskID+".json")
}

func (r *Receipt) WriteFile(repoRoot string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := Path(repoRoot, r.Task)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// Report is a verify outcome: OK, or the list of independent failures, plus
// the regenerated (authoritative) receipt.
type Report struct {
	OK          bool
	Failures    []string
	Regenerated *Receipt
}

func (rep *Report) fail(format string, args ...any) {
	rep.OK = false
	rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
}

// Verify runs the D3/D4.5 gate for one task PR checkout: stale-plan check,
// purity check, receipt regeneration (author of record), and comparison with
// the committed receipt's stable claims.
func Verify(repo *gitx.Repo, taskID, baseRef, headBranch string, opts Options) (*Report, error) {
	rep := &Report{OK: true}

	kind, branchTask := prclass.Classify(headBranch)
	if kind != prclass.TaskPR {
		return nil, fmt.Errorf("verify runs on task PRs; branch %q classifies as %s — merge-queue callers must classify by the underlying PR's head branch (D3)", headBranch, kind)
	}
	if branchTask != taskID {
		return nil, fmt.Errorf("branch %q is task %s, not %s", headBranch, branchTask, taskID)
	}

	regen, err := Generate(repo, taskID, baseRef, opts)
	if err != nil {
		return nil, err
	}
	rep.Regenerated = regen

	// Stale-plan rule: the merge-base plan must equal the plan at the current
	// base head, or the implementer is executing a revoked plan.
	basePlan, foundBase, err := repo.CatFile(baseRef, regen.PlanPath)
	if err != nil {
		return nil, err
	}
	mbPlan, _, err := repo.CatFile(regen.MergeBase, regen.PlanPath)
	if err != nil {
		return nil, err
	}
	if !foundBase {
		rep.fail("plan %s no longer exists at %s — plan revoked; re-plan before proceeding", regen.PlanPath, baseRef)
	} else if plan.Hash(basePlan) != plan.Hash(mbPlan) {
		rep.fail("plan changed since branch base — rebase and re-verify (D3 stale-plan rule)")
	}

	if err := prclass.CheckPurity(kind, taskID, regen.DiffFiles); err != nil {
		rep.fail("%v", err)
	}

	for _, v := range regen.Validation {
		if opts.RunValidation && v.Exit != 0 {
			rep.fail("validation command failed (exit %d): %s", v.Exit, v.Cmd)
		}
	}

	committedPath := Path(repo.Dir, taskID)
	b, err := os.ReadFile(committedPath)
	if err != nil {
		rep.fail("committed receipt missing at receipts/%s.json — every closed task references its receipt (D4.5)", taskID)
		return rep, nil
	}
	var committed Receipt
	if err := json.Unmarshal(b, &committed); err != nil {
		rep.fail("committed receipt unparseable: %v", err)
		return rep, nil
	}
	switch {
	case committed.Task != regen.Task,
		committed.PlanSHA256 != regen.PlanSHA256,
		committed.DiffSHA256 != regen.DiffSHA256,
		!slices.Equal(committed.DiffFiles, regen.DiffFiles),
		!sameCommands(committed.Validation, regen.Validation):
		rep.fail("receipt mismatch — the regenerated receipt is authoritative (CI is the author of record, R11); the committed copy is forged or stale")
	}
	return rep, nil
}

func sameCommands(a, b []Validation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Cmd != b[i].Cmd {
			return false
		}
	}
	return true
}
