// Package receipt implements the evidence chain (open-seed D4.5), split into
// the two things it was always trying to be at once.
//
// A *claim* is the committed file (receipts/<task>.json): the task, the plan
// blob it was implemented against, and the commands that plan authorizes CI
// to run. Every field of it is stable under rebase, merge and further
// commits, so the file goes stale only when the plan itself changes: the one
// event that must force re-verification (D3).
//
// An *attestation* is what CI produces: the claim plus the snapshot it was
// verified against (merge base, head, the diff and its hash, validation
// results). CI is the author of record (R11), so the attestation is emitted
// per run and never committed. A committed attestation describes a merge base
// and a diff that the next push invalidates, which made the gate a treadmill
// rather than a check: schema 2.0 keeps that snapshot out of the tree.
//
// Verify enforces the D3/D4.5 gate over both: an approved, lint-clean plan at
// the merge base, that plan unchanged at the current base head (the stale-plan
// rule: a superseded plan must be revocable), PR purity, the plan's validation
// commands green, and a committed claim whose durable fields agree.
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// SchemaVersion is the committed claim's schema. 1.0 files are still read
// (their durable fields are a subset of 2.0's); 2.0 is what gets written.
const SchemaVersion = "2.0"

// LegacySchemaVersion is the pre-split shape, which carried the snapshot
// inline and named its commands only through their results.
const LegacySchemaVersion = "1.0"

// diffExclude keeps receipts/** out of the hashed diff, so committing a
// receipt cannot change its own hash. Purity is checked over the full changed
// -file list instead, which is why the two lists are computed separately.
const diffExclude = ":(exclude)receipts"

type Validation struct {
	Cmd  string `json:"cmd"`
	Exit int    `json:"exit"`
}

// Claim is the committed receipt: only what an implementer can honestly
// assert before the branch stops moving.
type Claim struct {
	SchemaVersion      string   `json:"schema_version"`
	Task               string   `json:"task"`
	PlanPath           string   `json:"plan_path"`
	PlanSHA256         string   `json:"plan_sha256"`
	ValidationCommands []string `json:"validation_commands"`
	GeneratedBy        string   `json:"generated_by"`
}

// Receipt is the full attestation. The claim is embedded, so the emitted JSON
// carries the committed file's fields plus the snapshot.
type Receipt struct {
	Claim
	MergeBase string `json:"merge_base"`
	Head      string `json:"head"`
	// DiffFiles are the files the diff hash covers (receipts/** excluded);
	// ChangedFiles is every path the branch touches, which is what purity
	// is checked over.
	DiffFiles    []string     `json:"diff_files"`
	ChangedFiles []string     `json:"changed_files"`
	DiffSHA256   string       `json:"diff_sha256"`
	Validation   []Validation `json:"validation"`
	VerifiedBy   string       `json:"verified_by,omitempty"`
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type Options struct {
	RunValidation bool
	GeneratedBy   string
	// VerifiedBy identifies the CI run in an emitted attestation.
	VerifiedBy string
}

// PlanError is a plan-resolution outcome the gate reports, not an engine
// error: the caller renders it as a FAIL line beside the other gate results.
type PlanError struct{ msg string }

func (e *PlanError) Error() string { return e.msg }

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// resolvePlan returns the approved plan blob: the merge-base copy, never the
// working tree's (D3). The three failures are deliberately distinct, because
// they have three different remedies and only one of them is a violation.
func resolvePlan(repo *gitx.Repo, planPath, baseRef, mergeBase string) (string, error) {
	mbBlob, atMergeBase, err := repo.CatFile(mergeBase, planPath)
	if err != nil {
		return "", err
	}
	if !atMergeBase {
		_, atBase, err := repo.CatFile(baseRef, planPath)
		if err != nil {
			return "", err
		}
		if atBase {
			// Not a violation: the branch was cut before its own plan PR
			// merged. One merge moves the merge base forward onto it.
			return "", &PlanError{fmt.Sprintf(
				"%s merged after this branch was cut (present at %s, absent at the branch point %s) — run \"git merge %s\", then regenerate the receipt; the approved plan is the merge-base blob (D3)",
				planPath, baseRef, short(mergeBase), baseRef)}
		}
		return "", &PlanError{fmt.Sprintf(
			"no approved plan: %s exists neither at the branch point (%s) nor at %s — implementation above L1 requires a merged plan PR first (D3)",
			planPath, short(mergeBase), baseRef)}
	}
	if errs := plan.Lint(mbBlob); len(errs) > 0 {
		return "", &PlanError{fmt.Sprintf("approved plan %s fails lint at the merge base: %v — amend it in a new plan PR, then rebase (D3)", planPath, errs)}
	}
	return mbBlob, nil
}

func lines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	slices.Sort(out)
	return out
}

// Generate builds the attestation for taskID on the current HEAD against
// baseRef. The plan is read from the merge-base blob, never the working tree.
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
	planBlob, err := resolvePlan(repo, planPath, baseRef, mb)
	if err != nil {
		return nil, err
	}

	filesOut, err := repo.Git("diff", "--name-only", mb, head, "--", ".", diffExclude)
	if err != nil {
		return nil, err
	}
	changedOut, err := repo.Git("diff", "--name-only", mb, head)
	if err != nil {
		return nil, err
	}
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

	parsed := plan.Parse(planBlob)
	r := &Receipt{
		Claim: Claim{
			SchemaVersion:      SchemaVersion,
			Task:               taskID,
			PlanPath:           planPath,
			PlanSHA256:         plan.Hash(planBlob),
			ValidationCommands: parsed.ValidationCommands,
			GeneratedBy:        opts.GeneratedBy,
		},
		MergeBase:    mb,
		Head:         head,
		DiffFiles:    lines(filesOut),
		ChangedFiles: lines(changedOut),
		DiffSHA256:   sha(diffContent),
		VerifiedBy:   opts.VerifiedBy,
	}
	for _, cmd := range parsed.ValidationCommands {
		v := Validation{Cmd: cmd, Exit: -1} // -1 = not executed
		if opts.RunValidation {
			c := exec.Command("sh", "-c", cmd)
			c.Dir = repo.Dir
			if err := c.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
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

// ValidTaskID rejects ids that would escape receipts/. The id becomes a file
// name under that directory, so a separator or a parent reference in one lets
// a --write or a migrate rewrite a file the verb has no business touching.
func ValidTaskID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") ||
		strings.ContainsRune(id, 0) || strings.HasPrefix(id, ".") {
		return fmt.Errorf("invalid task id %q: a task id names one file under receipts/, not a path", id)
	}
	return nil
}

func Path(repoRoot, taskID string) string {
	return filepath.Join(repoRoot, "receipts", taskID+".json")
}

// UnderReceipts reports whether path resolves inside repoRoot's receipts
// directory. The attestation is never tree content, so emitting one there
// would leave a file that reads as a claim and whose snapshot is stale from
// the moment it lands.
func UnderReceipts(repoRoot, path string) bool {
	dir, err := filepath.Abs(filepath.Join(repoRoot, "receipts"))
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// WriteClaim writes the committed form to receipts/<task>.json.
func (r *Receipt) WriteClaim(repoRoot string) error {
	return writeJSON(Path(repoRoot, r.Task), r.Claim)
}

// WriteAttestation writes the full record CI emits per run. It is evidence,
// not tree content: nothing in the repository should point at a committed
// copy of it.
func (r *Receipt) WriteAttestation(path string) error {
	return writeJSON(path, r)
}

// ReadClaim reads a committed receipt. Schema 1.0 files predate the split and
// carry the snapshot inline; their durable fields are a subset, so they are
// read rather than rejected and `seed receipt migrate` rewrites them. Any
// other version is refused rather than guessed at: a receipt whose shape this
// engine does not know is not evidence it can weigh.
func ReadClaim(path string) (Claim, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Claim{}, err
	}
	var raw struct {
		Claim
		// The 1.0 snapshot, which is also how an attestation is recognised.
		Validation []Validation `json:"validation"`
		MergeBase  string       `json:"merge_base"`
		Head       string       `json:"head"`
		DiffSHA256 string       `json:"diff_sha256"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Claim{}, err
	}
	c := raw.Claim
	switch c.SchemaVersion {
	case SchemaVersion:
		// A 2.0 claim carries no snapshot. One that does is an attestation
		// that was committed, by --emit pointed here or by hand: it reads
		// as a claim while describing a merge base and a diff that the next
		// push invalidates, which is the failure mode the split removed.
		if raw.MergeBase != "" || raw.Head != "" || raw.DiffSHA256 != "" {
			return Claim{}, fmt.Errorf("this is an attestation, not a claim: a schema %s receipt records the plan it was implemented against, never a merge base, a head or a diff (those are CI's per-run finding and nothing commits them)", SchemaVersion)
		}
	case LegacySchemaVersion:
		if len(c.ValidationCommands) == 0 {
			for _, v := range raw.Validation {
				c.ValidationCommands = append(c.ValidationCommands, v.Cmd)
			}
		}
	default:
		return Claim{}, fmt.Errorf("unsupported receipt schema_version %q: this engine reads %s and %s", c.SchemaVersion, LegacySchemaVersion, SchemaVersion)
	}
	return c, nil
}

// Differs describes the first durable disagreement between a committed claim
// and the regenerated one, or "" when they agree. The snapshot fields are
// deliberately not compared: they are a function of the merge base and the
// head, so any later push would invalidate a comparison that proves nothing
// CI has not already recomputed.
func (c Claim) Differs(want Claim) string {
	switch {
	case c.Task != want.Task:
		return fmt.Sprintf("task %q, not %q", c.Task, want.Task)
	case c.PlanPath != want.PlanPath:
		return fmt.Sprintf("plan path %q, not %q", c.PlanPath, want.PlanPath)
	case c.PlanSHA256 != want.PlanSHA256:
		return fmt.Sprintf("plan pin %s, but the approved plan at the merge base is %s", short(c.PlanSHA256), short(want.PlanSHA256))
	case !slices.Equal(c.ValidationCommands, want.ValidationCommands):
		return fmt.Sprintf("validation commands %v, but the approved plan authorizes %v", c.ValidationCommands, want.ValidationCommands)
	}
	return ""
}

// Report is a verify outcome: OK, or the list of independent failures, plus
// the regenerated (authoritative) attestation when one could be built.
type Report struct {
	OK          bool
	Failures    []string
	Regenerated *Receipt
}

func (rep *Report) fail(format string, args ...any) {
	rep.OK = false
	rep.Failures = append(rep.Failures, fmt.Sprintf(format, args...))
}

// Verify runs the D3/D4.5 gate for one task PR checkout.
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
		// A plan-resolution failure is a gate result with a named remedy,
		// not an engine fault: report it rather than aborting the run.
		var pe *PlanError
		if errors.As(err, &pe) {
			rep.fail("%v", pe)
			return rep, nil
		}
		return nil, err
	}
	rep.Regenerated = regen

	// Stale-plan rule: the merge-base plan must equal the plan at the current
	// base head, or the implementer is executing a revoked plan.
	basePlan, foundBase, err := repo.CatFile(baseRef, regen.PlanPath)
	if err != nil {
		return nil, err
	}
	if !foundBase {
		rep.fail("plan %s no longer exists at %s — plan revoked; re-plan before proceeding", regen.PlanPath, baseRef)
	} else if plan.Hash(basePlan) != regen.PlanSHA256 {
		rep.fail("plan changed since branch base — run \"git merge %s\" and regenerate the receipt (D3 stale-plan rule)", baseRef)
	}

	// Purity runs over every changed path, receipts/** included: the hashed
	// diff excludes them, so without this a task PR could edit another
	// task's receipt and no gate would see it.
	if err := prclass.CheckPurity(kind, taskID, regen.ChangedFiles); err != nil {
		rep.fail("%v", err)
	}

	for _, v := range regen.Validation {
		if opts.RunValidation && v.Exit != 0 {
			rep.fail("validation command failed (exit %d): %s", v.Exit, v.Cmd)
		}
	}

	committed, err := ReadClaim(Path(repo.Dir, taskID))
	if err != nil {
		if os.IsNotExist(err) {
			rep.fail("committed receipt missing at receipts/%s.json — run \"seed receipt generate %s --base %s --write\" and commit it (D4.5)", taskID, taskID, baseRef)
		} else {
			rep.fail("committed receipt rejected: %v", err)
		}
		return rep, nil
	}
	if d := committed.Differs(regen.Claim); d != "" {
		rep.fail("committed receipt claims %s — regenerate it (\"seed receipt generate %s --base %s --write\"); CI is the author of record (R11)", d, taskID, baseRef)
	}
	return rep, nil
}

// Migrate rewrites a committed receipt in the current schema, dropping the
// snapshot a 1.0 file carried. It is a pure file rewrite: every durable field
// already lives in the old form.
func Migrate(path string) (bool, error) {
	c, err := ReadClaim(path)
	if err != nil {
		return false, err
	}
	if c.SchemaVersion == SchemaVersion {
		return false, nil
	}
	c.SchemaVersion = SchemaVersion
	return true, writeJSON(path, c)
}
