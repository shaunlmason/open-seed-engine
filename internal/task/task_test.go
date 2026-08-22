package task

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

func TestMain(m *testing.M) {
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "test"}, {"GIT_AUTHOR_EMAIL", "test@test"},
		{"GIT_COMMITTER_NAME", "test"}, {"GIT_COMMITTER_EMAIL", "test@test"},
	} {
		os.Setenv(kv[0], kv[1])
	}
	os.Exit(m.Run())
}

// harness: a bare origin plus N working clones, each with a .seed dir copied
// from the canonical spec testdata and a roster naming "lead" as operator.
type harness struct {
	t      *testing.T
	origin string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	return &harness{t: t, origin: origin}
}

func (h *harness) clone(name string) *Service {
	h.t.Helper()
	dir := filepath.Join(h.t.TempDir(), name)
	mustGit(h.t, "", "init", "--initial-branch=main", dir)
	mustGit(h.t, dir, "remote", "add", "origin", h.origin)

	seed := filepath.Join(dir, ".seed")
	if err := os.MkdirAll(filepath.Join(seed, "port-schema"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	src := filepath.Join("..", "spec", "testdata", "seed")
	for _, f := range []string{"port.json", "transitions.json"} {
		b, err := os.ReadFile(filepath.Join(src, "port-schema", f))
		if err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, "port-schema", f), b, 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "version"), []byte("1\n"), 0o644); err != nil {
		h.t.Fatal(err)
	}
	cfgContent := "[operators]\nactors = [\"lead\"]\n[claim]\ndefault_lease = \"60m\"\n"
	if err := os.WriteFile(filepath.Join(seed, "config.toml"), []byte(cfgContent), 0o644); err != nil {
		h.t.Fatal(err)
	}

	sv, err := NewService(dir)
	if err != nil {
		h.t.Fatal(err)
	}
	sv.Store.Sleep = func(time.Duration) {}
	return sv
}

// forgeRemoteRef force-rewrites seed-state on the origin to an unrelated
// root commit, simulating the R10 attacker.
func (h *harness) forgeRemoteRef() {
	h.t.Helper()
	tree := mustGit(h.t, "", "--git-dir="+h.origin, "mktree")
	forged := mustGit(h.t, "", "--git-dir="+h.origin, "commit-tree", tree, "-m", "forged")
	mustGit(h.t, "", "--git-dir="+h.origin, "update-ref", "refs/heads/seed-state", forged)
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustOK(t *testing.T, r *Result) *Result {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("expected ok, got code=%d err=%s fields=%v", r.Code, r.Err, r.Fields)
	}
	return r
}

func createReady(t *testing.T, sv *Service, title string) string {
	t.Helper()
	r := mustOK(t, sv.Create(CreateArgs{Title: title, Actor: "lead"}))
	id := r.Fields["task"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "lead"}))
	return id
}

func TestInitIsIdempotentAndRaceSafe(t *testing.T) {
	h := newHarness(t)
	a, b := h.clone("a"), h.clone("b")
	mustOK(t, a.Init())
	mustOK(t, b.Init()) // ref exists; resolves trivially
}

// Done-when #1: two claimants on one card deterministically produce one
// winner (exit 0) and one loser (exit 2).
func TestConcurrentClaimOneWinner(t *testing.T) {
	h := newHarness(t)
	a, b := h.clone("a"), h.clone("b")
	mustOK(t, a.Init())
	id := createReady(t, a, "contended card")

	ra := a.Claim(id, "agent-a", "")
	rb := b.Claim(id, "agent-b", "")
	if !((ra.Code == 0 && rb.Code == 2) || (ra.Code == 2 && rb.Code == 0)) {
		t.Fatalf("want exactly one winner: a=(%d,%s) b=(%d,%s)", ra.Code, ra.Err, rb.Code, rb.Err)
	}
	loser := rb
	if rb.Code == 0 {
		loser = ra
	}
	if loser.Err != "claim_contention" {
		t.Errorf("loser error = %q", loser.Err)
	}
	if loser.Fields["holder"] == "" {
		t.Errorf("loser envelope missing holder")
	}
}

// Done-when #2: a reaped predecessor's late operations are fenced out (exit 6).
func TestStaleClaimantIsFenced(t *testing.T) {
	h := newHarness(t)
	a, b := h.clone("a"), h.clone("b")
	mustOK(t, a.Init())
	id := createReady(t, a, "fence card")

	ra := mustOK(t, a.Claim(id, "agent-a", "1m"))
	staleToken := ra.Fields["claim_token"].(string)

	// Lease expires; the maintenance operator reaps (in_progress→ready, reap
	// override), then a new claimant takes the card with a fresh fence.
	later := time.Now().Add(2 * time.Hour)
	b.Now = func() time.Time { return later }
	reap := mustOK(t, b.Transition(TransitionArgs{Verb: "transition", ID: id, To: "ready", Actor: "lead"}))
	if reap.Fields["state"] != "ready" {
		t.Fatalf("reap: %v", reap.Fields)
	}
	mustOK(t, b.Claim(id, "agent-b", ""))

	// The reaped predecessor comes back from the dead.
	late := a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: staleToken})
	if late.Code != spec.ExitFenced || late.Err != "fenced_out" {
		t.Fatalf("stale claimant: code=%d err=%s", late.Code, late.Err)
	}
	// The reap wrote a handoff stub.
	head, err := a.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	stub, found, _ := a.Store.ReadFile(head, "handoff/"+id+".md")
	if !found || !strings.Contains(stub, "reap") {
		t.Fatalf("handoff stub missing or wrong: found=%v %q", found, stub)
	}
}

// Done-when #3: a simulated history rewrite halts the shim.
func TestRefRewriteHalts(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "victim")
	_ = id

	// Attacker force-rewrites seed-state on the remote.
	h.forgeRemoteRef()

	r := a.Ready("agent-a", "")
	if r.Code != spec.ExitUnavailable || r.Err != "non_fast_forward" {
		t.Fatalf("rewrite not detected: code=%d err=%s fields=%v", r.Code, r.Err, r.Fields)
	}
}

// Anchor ancestry protects fresh clones, which have no local baseline (§7.2).
func TestAnchorAncestryProtectsFreshClones(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	createReady(t, a, "anchored work")
	head, err := a.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	// Maintenance anchors the head, tag pushed to the remote.
	mustGit(t, a.Root, "tag", "seed-anchor/20260822T050000Z", head)
	mustGit(t, a.Root, "push", "origin", "refs/tags/seed-anchor/20260822T050000Z")

	// Rewrite the remote branch to a history that does not contain the anchor.
	h.forgeRemoteRef()

	fresh := h.clone("fresh") // no local baseline: non-FF fetch cannot catch it
	r := fresh.Ready("agent-x", "")
	if r.Code != spec.ExitUnavailable || r.Err != "anchor_ancestry_failed" {
		t.Fatalf("anchor check missed rewrite: code=%d err=%s fields=%v", r.Code, r.Err, r.Fields)
	}
}

func TestHaltMarkerBlocksMutationsUntilResume(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "halted card")

	// The maintenance conformance lint writes HALT (simulated directly).
	_, err := a.Store.Mutate(false, func(head string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "conformance failure",
			Changes: []gitx.Change{{Path: "HALT", Content: "lint: transition-table conformance failed\n"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if r := a.Claim(id, "agent-a", ""); r.Code != spec.ExitUnavailable || r.Err != "halted" {
		t.Fatalf("mutation during HALT: code=%d err=%s", r.Code, r.Err)
	}
	if r := a.Resume("agent-a"); r.Code == 0 {
		t.Fatal("non-operator cleared HALT")
	}
	mustOK(t, a.Resume("lead"))
	mustOK(t, a.Claim(id, "agent-a", ""))
}

func TestRejectLockoutAndAuthorOfRecord(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "review card")

	r := mustOK(t, a.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok}))
	mustOK(t, a.Transition(TransitionArgs{Verb: "reject", ID: id, Actor: "lead", Resolution: "not good enough"}))

	if r := a.Claim(id, "agent-a", ""); r.Code != spec.ExitClaimNotGranted || r.Err != "rejected_author" {
		t.Fatalf("rejected author re-claimed: code=%d err=%s", r.Code, r.Err)
	}
	mustOK(t, a.Claim(id, "agent-b", ""))
}

func TestCloseCascadeAutoUnblocks(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	blocker := createReady(t, a, "blocker")
	dependent := createReady(t, a, "dependent")
	mustOK(t, a.Transition(TransitionArgs{Verb: "block", ID: dependent, Actor: "lead", BlockedOn: "dep:" + blocker}))

	r := mustOK(t, a.Claim(blocker, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: blocker, To: "review", Actor: "agent-a", Token: tok}))
	closed := mustOK(t, a.Transition(TransitionArgs{Verb: "close", ID: blocker, Actor: "lead", Resolution: "merged PR #1"}))

	cascaded, _ := closed.Fields["cascaded"].([]string)
	if len(cascaded) != 1 || cascaded[0] != dependent {
		t.Fatalf("cascade = %v", closed.Fields["cascaded"])
	}
	g := mustOK(t, a.Get(dependent))
	if g.Fields["state"] != "ready" {
		t.Fatalf("dependent state = %v", g.Fields["state"])
	}
}

func TestOneCommitPerVerbWithRunLog(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "audited card") // create + promote = 2 verbs
	mustOK(t, a.Claim(id, "agent-a", ""))   // 3

	head, err := a.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	count := mustGit(t, a.Root, "rev-list", "--count", head)
	if count != "4" { // init + 3 verbs
		t.Errorf("commit count = %s, want 4 (one per verb)", count)
	}
	log, found, _ := a.Store.ReadFile(head, "run-log.jsonl")
	if !found {
		t.Fatal("run log missing")
	}
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 3 {
		t.Errorf("run log lines = %d, want 3: %q", len(lines), log)
	}
	for _, verb := range []string{"create", "promote", "claim"} {
		if !strings.Contains(log, `"verb":"`+verb+`"`) {
			t.Errorf("run log missing %s event", verb)
		}
	}
}

func TestLeaseRenewFencing(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "leased card")
	r := mustOK(t, a.Claim(id, "agent-a", "5m"))
	tok := r.Fields["claim_token"].(string)

	if rr := a.LeaseRenew(id, "agent-a", "wrong", "10m"); rr.Code != spec.ExitFenced {
		t.Fatalf("wrong-token renew: %d", rr.Code)
	}
	renewed := mustOK(t, a.LeaseRenew(id, "agent-a", tok, "45m"))
	if renewed.Fields["lease_expires"] == "" {
		t.Fatal("no new lease")
	}
}

func TestOperatorVerbsRequireRoster(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	r := mustOK(t, a.Create(CreateArgs{Title: "x", Actor: "lead"}))
	id := r.Fields["task"].(string)
	if rr := a.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "not-a-lead"}); rr.Code != spec.ExitInvalid {
		t.Fatalf("non-roster promote: code=%d err=%s", rr.Code, rr.Err)
	}
	mustOK(t, a.Transition(TransitionArgs{Verb: "promote", ID: id, Actor: "lead"}))
}
