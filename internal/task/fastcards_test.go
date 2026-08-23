package task

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/fastcards"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
)

// fastService builds a Service over the fastcards SQLite store: a git repo
// (the DB lives under its common git dir) with backend = "fastcards".
func fastService(t *testing.T, dir string) *Service {
	t.Helper()
	if dir == "" {
		dir = filepath.Join(t.TempDir(), "repo")
		mustGit(t, "", "init", "--initial-branch=main", dir)
	}
	seed := filepath.Join(dir, ".seed")
	if err := os.MkdirAll(filepath.Join(seed, "port-schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "spec", "testdata", "seed")
	for _, f := range []string{"port.json", "transitions.json", "verbs.json"} {
		b, err := os.ReadFile(filepath.Join(src, "port-schema", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, "port-schema", f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(seed, "version"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "[coordination]\nbackend = \"fastcards\"\n[operators]\nactors = [\"lead\"]\n[claim]\ndefault_lease = \"60m\"\n"
	if err := os.WriteFile(filepath.Join(seed, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	sv, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sv.Store.(*fastcards.Store); !ok {
		t.Fatalf("backend=fastcards did not select the SQLite store: %T", sv.Store)
	}
	return sv
}

// The suite's core lifecycle: create, promote, claim, fence, park, review,
// reject-lockout, cascade: runs against the SQLite store through exactly
// the same Service code paths as filecards.
func TestFastcardsLifecycle(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	mustOK(t, sv.Init()) // idempotent

	id := createReady(t, sv, "fast card")
	r := mustOK(t, sv.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)

	// Rival claim refused on fresh state read inside the transaction.
	if rb := sv.Claim(id, "agent-b", ""); rb.Code != 2 || rb.Err != "claim_contention" {
		t.Fatalf("rival claim = (%d,%s)", rb.Code, rb.Err)
	}
	// Fencing: wrong token is fenced out.
	if rf := sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: "c-bogus"}); rf.Code != 6 {
		t.Fatalf("wrong token = (%d,%s)", rf.Code, rf.Err)
	}
	// Park on a plan, operator plan-unblock releases.
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "blocked", Actor: "agent-a", Token: tok, BlockedOn: "plan:7"}))
	mustOK(t, sv.PlanUnblock(id, 7, "lead"))

	r = mustOK(t, sv.Claim(id, "agent-a", ""))
	tok2 := r.Fields["claim_token"].(string)
	if tok2 == tok {
		t.Fatal("claim token did not rotate on reclaim")
	}
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok2}))

	// Reject → lockout at claim.
	mustOK(t, sv.Transition(TransitionArgs{Verb: "reject", ID: id, Actor: "lead"}))
	if rl := sv.Claim(id, "agent-a", ""); rl.Code != 2 {
		t.Fatalf("rejected author claim = (%d,%s)", rl.Code, rl.Err)
	}
	r = mustOK(t, sv.Claim(id, "agent-b", ""))
	tok3 := r.Fields["claim_token"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-b", Token: tok3}))

	// Cascade: dependent parks on dep:<id>, blocker close releases it.
	rd := mustOK(t, sv.Create(CreateArgs{Title: "dependent", Actor: "lead", BlockedBy: []string{id}}))
	dep := rd.Fields["task"].(string)
	mustOK(t, sv.Transition(TransitionArgs{Verb: "promote", ID: dep, Actor: "lead"}))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "block", ID: dep, Actor: "lead", BlockedOn: "dep:" + id}))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead", NoPR: true, Resolution: "accepted locally (fastcards operator close lane)"}))
	rg := mustOK(t, sv.Get(dep))
	if state := rg.Fields["state"].(string); state != "ready" {
		t.Fatalf("cascade did not release dependent: %s", state)
	}
	// Anchors are a state-ref mechanism: refused with a clear name here.
	if ra := sv.Anchor(); ra.Code != 5 || ra.Err != "anchors_not_applicable" {
		t.Fatalf("anchor on fastcards = (%d,%s)", ra.Code, ra.Err)
	}
	// The card lints still run; the replay lint is skipped (no history).
	mustOK(t, sv.StateLint(false, "lead"))
}

// The plan's contention proof: the loser blocks on the winner's LIVE
// transaction (busy wait, not error), then refuses on the state the winner
// committed: exactly one claim granted.
func TestFastcardsClaimContentionUnderHeldTransaction(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := createReady(t, sv, "contended")

	fc := sv.Store.(*fastcards.Store)
	inTx := make(chan struct{})
	release := make(chan struct{})
	winnerDone := make(chan string, 1)

	go func() {
		_, err := fc.Mutate(true, func(head string) (*stateref.Mutation, error) {
			close(inTx)
			<-release // hold the write lock with the claim decision pending
			c, err := sv.loadCard(head, id)
			if err != nil {
				return nil, err
			}
			c.State = "in_progress"
			c.Claim = &card.Claim{Actor: "agent-w", Token: "c-winner", ClaimedAt: sv.now(),
				LeaseExpires: sv.Now().UTC().Add(time.Hour).Format(time.RFC3339)}
			content, err := c.Serialize()
			if err != nil {
				return nil, err
			}
			return &stateref.Mutation{Message: "claim (held)", Changes: []gitx.Change{{Path: card.Path(id), Content: content}}}, nil
		})
		if err != nil {
			winnerDone <- err.Error()
			return
		}
		winnerDone <- ""
	}()

	<-inTx
	// Rival Service on the same DB (a second process in real life).
	sv2 := fastService(t, sv.Root)
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(release)
	}()
	rb := sv2.Claim(id, "agent-l", "")
	if msg := <-winnerDone; msg != "" {
		t.Fatalf("winner mutate failed: %s", msg)
	}
	if rb.Code != 2 || rb.Err != "claim_contention" {
		t.Fatalf("loser under held lock = (%d,%s), want (2,claim_contention)", rb.Code, rb.Err)
	}
}

// Linked worktrees resolve the SAME database through the common git dir:
// a claim from the main checkout is contention inside the worktree.
func TestFastcardsWorktreeSharesOneDB(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	// The loop's worktrees need one commit to branch from.
	mustGit(t, sv.Root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "root")
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, sv.Root, "worktree", "add", wt)

	mainDB, err := fastcards.DBPath(sv.Root)
	if err != nil {
		t.Fatal(err)
	}
	wtDB, err := fastcards.DBPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if mainDB != wtDB {
		t.Fatalf("worktree resolved a different DB: %s vs %s", mainDB, wtDB)
	}

	id := createReady(t, sv, "shared card")
	mustOK(t, sv.Claim(id, "agent-a", ""))

	svWT := fastService(t, wt)
	if r := svWT.Claim(id, "agent-b", ""); r.Code != 2 {
		t.Fatalf("worktree rival claim = (%d,%s), want contention", r.Code, r.Err)
	}
	rg := mustOK(t, svWT.Get(id))
	if state := rg.Fields["state"].(string); state != "in_progress" {
		t.Fatalf("worktree does not see the claim: %s", state)
	}
}

// Round-trip: filecards → export → fastcards → export → filecards, ids,
// states, dependency edges, rejections, and the run log preserved.
func TestStateExportImportRoundTrip(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "travelling card")
	r := mustOK(t, a.Claim(id, "agent-a", ""))
	tok := r.Fields["claim_token"].(string)
	mustOK(t, a.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok}))
	mustOK(t, a.Transition(TransitionArgs{Verb: "reject", ID: id, Actor: "lead"}))
	rd := mustOK(t, a.Create(CreateArgs{Title: "dep card", Actor: "lead", BlockedBy: []string{id}}))
	dep := rd.Fields["task"].(string)

	exp := mustOK(t, a.Export())
	doc1 := exp.Fields["document"].(json.RawMessage)

	// Import into a fresh fastcards store.
	fast := fastService(t, "")
	mustOK(t, fast.Init())
	mustOK(t, fast.Import(doc1, "lead"))
	if r := fast.Import(doc1, "lead"); r.Code == 0 {
		t.Fatal("import into a non-empty store must refuse")
	}
	rg := mustOK(t, fast.Get(id))
	if state := rg.Fields["state"].(string); state != "ready" {
		t.Fatalf("state after import = %s", state)
	}
	var parsed1 StateExport
	if err := json.Unmarshal(doc1, &parsed1); err != nil {
		t.Fatal(err)
	}

	// Export from fastcards and land it back in a fresh filecards repo.
	exp2 := mustOK(t, fast.Export())
	doc2 := exp2.Fields["document"].(json.RawMessage)
	var parsed2 StateExport
	if err := json.Unmarshal(doc2, &parsed2); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{card.Path(id), card.Path(dep)} {
		if parsed1.Files[p] != parsed2.Files[p] {
			t.Fatalf("card %s not preserved across the round trip", p)
		}
	}

	h2 := newHarness(t)
	b := h2.clone("b")
	mustOK(t, b.Init())
	mustOK(t, b.Import(doc2, "lead"))
	rg = mustOK(t, b.Get(dep))
	if state := rg.Fields["state"].(string); state != "backlog" {
		t.Fatalf("dependent state after second import = %s", state)
	}
	rg = mustOK(t, b.Get(id))
	c := rg.Fields["card"].(*card.Card)
	if len(c.RejectedAuthors) != 1 || c.RejectedAuthors[0] != "agent-a" {
		t.Fatalf("rejection history lost: %v", c.RejectedAuthors)
	}
}

// Throughput smoke: a burst of sequential verbs completes without any
// network (there is no remote to contact at all).
func TestFastcardsThroughputSmoke(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	start := time.Now()
	for i := 0; i < 25; i++ {
		id := createReady(t, sv, "burst card")
		r := mustOK(t, sv.Claim(id, "agent-a", ""))
		tok := r.Fields["claim_token"].(string)
		mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: id, To: "review", Actor: "agent-a", Token: tok}))
		mustOK(t, sv.Transition(TransitionArgs{Verb: "close", ID: id, Actor: "lead"}))
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("100 verbs took %v", elapsed)
	}
}
