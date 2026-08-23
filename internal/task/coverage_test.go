package task

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/mirror"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
	"github.com/shaunlmason/open-seed-engine/internal/validate"
)

// Top-up coverage for reporting/lint/nudge edges the lifecycle suites
// don't reach.

func TestMailNudgeBranches(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	if r := sv.MailNudge(""); r.Code == 0 {
		t.Fatal("actorless nudge accepted")
	}
	// With tmux on PATH but no such pane: skipped, still ok (advisory).
	r := mustOK(t, sv.MailNudge("no-such-pane-xyzzy"))
	if note, _ := r.Fields["note"].(string); note == "" {
		t.Fatalf("nudge note missing: %+v", r.Fields)
	}
	// Without tmux at all: the declared no-op branch.
	t.Setenv("PATH", t.TempDir())
	r = mustOK(t, sv.MailNudge("anyone"))
	if note, _ := r.Fields["note"].(string); !strings.Contains(note, "no-op") {
		t.Fatalf("tmux-less nudge note: %q", note)
	}
}

func TestMailPruneAndUnreadCounts(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	for i := 0; i < 3; i++ {
		mustOK(t, sv.MailSend("a", "bob", "info", "m", "", ""))
	}
	r := mustOK(t, sv.MailUnreadCounts())
	counts := r.Fields["unread"].(map[string]int)
	if counts["bob"] != 3 {
		t.Fatalf("unread counts: %v", counts)
	}
	// Ack all three, then prune bob down to 1 acked message.
	msgs := mustOK(t, sv.MailRead("bob", true)).Fields["messages"].([]Message)
	for _, m := range msgs {
		mustOK(t, sv.MailAck("bob", m.ID))
	}
	r = mustOK(t, sv.MailPrune("bob", 1))
	if pruned := r.Fields["pruned"].([]string); len(pruned) != 2 {
		t.Fatalf("pruned %v, want 2 entries", pruned)
	}
	// Nothing left over the bound: the explicit noop path.
	r = mustOK(t, sv.MailPrune("bob", 1))
	if pruned := r.Fields["pruned"].([]string); len(pruned) != 0 {
		t.Fatalf("second prune not a noop: %v", pruned)
	}
	// Unread counts with an empty mailbox root read cleanly too.
	sv2 := fastService(t, "")
	mustOK(t, sv2.Init())
	mustOK(t, sv2.MailUnreadCounts())
}

func TestReportLongParkedAndAncestry(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := createReady(t, sv, "Parked plan card")
	mustOK(t, sv.Transition(TransitionArgs{Verb: "block", ID: id, Actor: "lead", BlockedOn: "plan:9"}))
	rid := createReady(t, sv, "Stalled review card")
	cl := mustOK(t, sv.Claim(rid, "w", ""))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "transition", ID: rid, To: "review", Actor: "w",
		Token: cl.Fields["claim_token"].(string)}))

	later := time.Now().Add(96 * time.Hour)
	sv.Now = func() time.Time { return later }
	r := mustOK(t, sv.Report(48*time.Hour))
	if parked := r.Fields["long_parked_plans"].([]string); len(parked) != 1 || parked[0] != id {
		t.Fatalf("long_parked_plans: %v", r.Fields)
	}
	if stalled := r.Fields["stalled_reviews"].([]string); len(stalled) != 1 || stalled[0] != rid {
		t.Fatalf("stalled_reviews: %v", r.Fields)
	}

	// Service-level ancestry adapter: active teams, unrooted open cards warn.
	warns := sv.AncestryWarnings([]validate.Team{{Name: "core", Mission: "ship"}})
	if len(warns) < 2 {
		t.Fatalf("ancestry warnings: %v", warns)
	}
	if warns = sv.AncestryWarnings([]validate.Team{{Name: "core"}}); warns != nil {
		t.Fatalf("inactive ancestry warned: %v", warns)
	}
}

func TestStateLintFailuresAndHalt(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	createReady(t, sv, "good card")
	// Plant conformance violations directly in the store: an unknown
	// state, a claim outside a claim-bearing state, and unparseable text.
	_, err := sv.Store.Mutate(false, func(head string) (*stateref.Mutation, error) {
		return &stateref.Mutation{
			Message: "plant lint failures",
			Changes: []gitx.Change{
				{Path: "tasks/os-badstate.md", Content: "---\nid: os-badstate\ntitle: X\nstate: limbo\npriority: P2\ncreated_at: 2026-01-01T00:00:00Z\n---\n\nbody\n"},
				{Path: "tasks/os-mangled.md", Content: "not a card at all"},
			},
			Events: []string{`{"ts":"t","actor":"t","verb":"plant"}`},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	r := sv.StateLint(false, "lead")
	if r.Code != 1 {
		t.Fatalf("lint passed over planted violations: %+v", r.Fields)
	}
	// halt-on-fail writes the HALT marker; mutating verbs then refuse.
	r = sv.StateLint(true, "lead")
	if r.Code != 1 {
		t.Fatal("halting lint did not fail")
	}
	if c := sv.Create(CreateArgs{Title: "while halted", Actor: "a"}); c.Code == 0 {
		t.Fatal("create accepted while HALTed")
	}
	mustOK(t, sv.Resume("lead"))
	mustOK(t, sv.Create(CreateArgs{Title: "after resume", Actor: "a"}))
}

func TestMirrorRecordEdges(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	if r := sv.MirrorRecord("os-x", 1, "ready", "not-operator"); r.Err != "operator_required" {
		t.Fatalf("non-operator mirror record: %+v", r)
	}
	if r := sv.MirrorRecord("os-missing", 1, "ready", "lead"); r.Code != 4 {
		t.Fatalf("mirror record on missing card: %+v", r)
	}
	id := createReady(t, sv, "mirrored")
	mustOK(t, sv.MirrorRecord(id, 7, "ready", "lead"))
}

func TestErrResultClasses(t *testing.T) {
	cases := map[error]int{
		&stateref.Terminal{Code: 6, Name: "fenced_out"}:    6,
		&stateref.IntegrityError{Reason: "r", Detail: "d"}: 5,
		&gitx.ErrNoRemoteRef{Ref: "refs/seed/state"}:       5,
	}
	for err, want := range cases {
		if r := errResult(err); r.Code != want {
			t.Fatalf("errResult(%T) code %d, want %d", err, r.Code, want)
		}
	}
}

func TestMailSendRefusals(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	if r := sv.MailSend("", "b", "info", "x", "", ""); r.Code == 0 {
		t.Fatal("senderless send accepted")
	}
	if r := sv.MailSend("a", "b", "gossip", "x", "", ""); r.Code == 0 {
		t.Fatal("unknown mail type accepted")
	}
	if r := sv.MailRead("", false); r.Code == 0 {
		t.Fatal("actorless read accepted")
	}
	if r := sv.MailAck("a", ""); r.Code == 0 {
		t.Fatal("idless ack accepted")
	}
	if r := sv.MailAck("a", "msg-nope"); r.Code != 4 {
		t.Fatalf("phantom ack: %+v", r)
	}
}

func TestImportAndResumeEdges(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	if r := sv.Import([]byte(`{"schema_version":"9.9"}`), "lead"); r.Err != "export_schema_mismatch" {
		t.Fatalf("schema mismatch: %+v", r)
	}
	if r := sv.Import([]byte(`{{{`), "lead"); r.Err != "bad_export_document" {
		t.Fatalf("bad document: %+v", r)
	}
	if r := sv.Resume("nobody"); r.Code == 0 {
		t.Fatal("non-operator resume accepted")
	}
	if r := sv.HandoffGenerate("os-missing", "a", false); r.Code != 4 {
		t.Fatalf("handoff on missing card: %+v", r)
	}
}

// Git-store paths: replay lint, anchors, and the state-lint noRef note,
// none of which the machine-local store can exercise.
func TestGitStoreLintReplayAndAnchor(t *testing.T) {
	h := newHarness(t)
	sv := h.clone("a")
	// Before init: the "no state ref yet" note, not an error.
	r := sv.StateLint(false, "lead")
	if r.Code != 0 || r.Fields["note"] == nil {
		t.Fatalf("pre-init lint: %+v", r)
	}
	mustOK(t, sv.Init())
	id := createReady(t, sv, "replayed")
	mustOK(t, sv.Claim(id, "w", ""))
	if r := sv.StateLint(false, "lead"); r.Code != 0 {
		t.Fatalf("clean history flagged: %+v", r.Fields)
	}

	// Hand-edit a card WITHOUT appending to the run log: replay flags the
	// atomicity violation and the illegal state hop.
	head, _ := sv.Store.Sync()
	c, err := sv.loadCard(head, id)
	if err != nil {
		t.Fatal(err)
	}
	c.State = "done"
	c.Claim = nil
	content, _ := c.Serialize()
	gitStore := sv.Store.(*stateref.Store)
	sha, err := gitStore.Repo.CommitTree(head, []string{head}, "hand edit", []gitx.Change{
		{Path: "tasks/" + id + ".md", Content: content},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gitStore.Repo.Push("origin", sha, "seed-state"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitStore.Repo.Git("update-ref", "refs/seed/state", sha); err != nil {
		t.Fatal(err)
	}
	r = sv.StateLint(false, "lead")
	if r.Code != 1 {
		t.Fatalf("hand edit survived replay: %+v", r.Fields)
	}
	joined := strings.Join(r.Fields["failures"].([]string), " | ")
	if !strings.Contains(joined, "run-log.jsonl") {
		t.Fatalf("atomicity violation not named: %s", joined)
	}

	// Anchor on the git store: tag + push, then Sync still passes.
	sv2 := h.clone("b")
	r = mustOK(t, sv2.Anchor())
	if r.Fields["tag"] == nil {
		t.Fatalf("anchor: %+v", r.Fields)
	}
	if _, err := sv2.Store.Sync(); err != nil {
		t.Fatalf("sync after anchor: %v", err)
	}
}

func TestReapSkipsAndImportRefusesNonEmpty(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := createReady(t, sv, "will expire")
	mustOK(t, sv.Claim(id, "w", "1m"))
	// HALT the store: reap's transition is refused, landing in skipped.
	if _, err := sv.Store.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "halt", Changes: []gitx.Change{{Path: "HALT", Content: "x"}},
			Events: []string{`{"verb":"halt"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	sv.Now = func() time.Time { return later }
	r := mustOK(t, sv.ReapExpired("lead"))
	if skipped := r.Fields["skipped"].([]string); len(skipped) != 1 {
		t.Fatalf("reap skip: %+v", r.Fields)
	}
	mustOK(t, sv.Resume("lead"))

	// Import refuses a store that already holds cards.
	doc := `{"schema_version":"1.0","backend":"fastcards","head":"1","files":{"tasks/os-zz.md":"x","run-log.jsonl":""}}`
	if r := sv.Import([]byte(doc), "lead"); r.Code == 0 {
		t.Fatalf("import into a non-empty store accepted: %+v", r)
	}
}

func TestMailNudgeDeliveredAndPruneAll(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	pane := "seedcov"
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", pane).CombinedOutput(); err != nil {
		t.Skipf("tmux session: %v %s", err, out)
	}
	defer exec.Command("tmux", "kill-session", "-t", pane).Run()
	r := mustOK(t, sv.MailNudge(pane))
	if note := r.Fields["note"].(string); !strings.Contains(note, "nudged via tmux") {
		t.Fatalf("nudge note: %q", note)
	}

	// Prune with actor="" walks every recipient dir; keep<=0 defaults.
	mustOK(t, sv.MailSend("a", "x", "info", "1", "", ""))
	mustOK(t, sv.MailSend("a", "y", "info", "2", "", ""))
	for _, who := range []string{"x", "y"} {
		msgs := mustOK(t, sv.MailRead(who, true)).Fields["messages"].([]Message)
		for _, m := range msgs {
			mustOK(t, sv.MailAck(who, m.ID))
		}
	}
	r = mustOK(t, sv.MailPrune("", 0))
	if r.Fields["pruned"] == nil {
		t.Fatalf("prune-all: %+v", r.Fields)
	}
}

func TestDoneConsistencyLint(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	// A done card with every D7 violation at once: no evidence, a
	// reviewer outside the roster, no plan file, and no no-PR exemption.
	bad := "---\nid: os-fakedone\ntitle: X\nstate: done\npriority: P2\nauthor: w\nreview:\n  reviewer: impostor\n  outcome: accepted\n  evidence: \"\"\ncreated_at: 2026-01-01T00:00:00Z\n---\n\nbody\n"
	if _, err := sv.Store.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "plant done", Changes: []gitx.Change{{Path: "tasks/os-fakedone.md", Content: bad}},
			Events: []string{`{"verb":"plant"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	r := sv.StateLint(false, "lead")
	if r.Code != 1 {
		t.Fatalf("forged done passed lint: %+v", r.Fields)
	}
	joined := strings.Join(r.Fields["failures"].([]string), " | ")
	for _, want := range []string{"without evidence", "operator roster", "resolvable plan"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}

func TestGitStoreExportImportRoundTrip(t *testing.T) {
	h := newHarness(t)
	a := h.clone("a")
	mustOK(t, a.Init())
	id := createReady(t, a, "portable")
	r := mustOK(t, a.Export())
	doc := r.Fields["document"].(json.RawMessage)

	h2 := newHarness(t)
	b := h2.clone("b")
	mustOK(t, b.Init())
	imp := mustOK(t, b.Import([]byte(doc), "lead"))
	if imp.Fields["files"] == nil {
		t.Fatalf("import: %+v", imp.Fields)
	}
	if g := mustOK(t, b.Get(id)); g.Fields["state"] != "ready" {
		t.Fatalf("imported card state: %v", g.Fields["state"])
	}
}

func TestMirrorAndHandoffEdges(t *testing.T) {
	sv := fastService(t, "")
	mustOK(t, sv.Init())
	id := createReady(t, sv, "mirrored")

	// A corrupt mapping poisons both mirror verbs.
	if _, err := sv.Store.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "corrupt map",
			Changes: []gitx.Change{{Path: mirror.MapPath, Content: "{nope"}},
			Events:  []string{`{"verb":"x"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := sv.MirrorPlan(); r.Code == 0 {
		t.Fatal("corrupt mapping planned")
	}
	if r := sv.MirrorRecord(id, 7, "ready", "lead"); r.Code == 0 {
		t.Fatal("corrupt mapping recorded")
	}
	// Restored mapping: recording an unknown card still refuses.
	if _, err := sv.Store.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "fix map",
			Changes: []gitx.Change{{Path: mirror.MapPath, Content: "{\"cards\": {}}"}},
			Events:  []string{`{"verb":"x"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := sv.MirrorRecord("os-none1234", 7, "ready", "lead"); r.Code == 0 {
		t.Fatal("phantom card recorded")
	}

	// Handoff for a card that does not exist.
	if r := sv.HandoffGenerate("os-none1234", "lead", false); r.Code == 0 {
		t.Fatal("phantom handoff generated")
	}
	// A giant body trips the 8KB packet bound; empty actor defaults.
	big := "---\nid: os-bigbig12\ntitle: big\nstate: ready\npriority: P2\nauthor: w\ncreated_at: 2026-01-01T00:00:00Z\n---\n\n" +
		strings.Repeat("## Evidence appended by a worker run, with a long trailing explanation to bulk the packet\n", 200)
	if _, err := sv.Store.Mutate(false, func(string) (*stateref.Mutation, error) {
		return &stateref.Mutation{Message: "plant big",
			Changes: []gitx.Change{{Path: "tasks/os-bigbig12.md", Content: big}},
			Events:  []string{`{"verb":"plant"}`}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	r := mustOK(t, sv.HandoffGenerate("os-bigbig12", "", true))
	if r.Fields["written"] != "handoff/os-bigbig12.md" {
		t.Fatalf("write: %+v", r.Fields)
	}
	r = mustOK(t, sv.HandoffGenerate("os-bigbig12", "", false))
	if !strings.Contains(r.Fields["packet"].(string), "[truncated at 8KB") {
		t.Fatal("giant packet not truncated")
	}
}
