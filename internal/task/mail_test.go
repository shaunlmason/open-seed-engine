package task

import (
	"strings"
	"testing"
	"time"
)

// Mail contract (plan os-499c5978): one never-rewritten file per
// message on the state ref, ack as a move, broadcast copy-ack,
// self-send refusal, prune bounds acked history only.
func TestMailRoundTrip(t *testing.T) {
	sv := newHarness(t).clone("a")
	mustOK(t, sv.Init())

	if r := sv.MailSend("alice", "alice", "info", "hi me", "", ""); r.Code == 0 {
		t.Fatalf("self-send accepted: %+v", r)
	}
	mustOK(t, sv.MailSend("alice", "bob", "directive", "rename verifyToken", "", ""))
	mustOK(t, sv.MailSend("alice", "_all", "alert", "main is frozen", "", ""))

	r := sv.MailRead("bob", true)
	mustOK(t, r)
	msgs := r.Fields["messages"].([]Message)
	if len(msgs) != 2 {
		t.Fatalf("bob unread: %+v", msgs)
	}
	directID, bcastID := "", ""
	for _, m := range msgs {
		if m.Type == "directive" {
			directID = m.ID
		} else {
			bcastID = m.ID
		}
	}

	// Direct ack MOVES; broadcast ack COPIES (carol still sees it).
	mustOK(t, sv.MailAck("bob", directID))
	mustOK(t, sv.MailAck("bob", bcastID))
	r = sv.MailRead("bob", true)
	if got := len(r.Fields["messages"].([]Message)); got != 0 {
		t.Fatalf("bob still has %d unread after acks", got)
	}
	r = sv.MailRead("bob", false)
	if got := len(r.Fields["messages"].([]Message)); got != 2 {
		t.Fatalf("acked history lost: %d", got)
	}
	r = sv.MailRead("carol", true)
	if got := len(r.Fields["messages"].([]Message)); got != 1 {
		t.Fatalf("carol lost the broadcast: %d", got)
	}

	if r := sv.MailAck("bob", "msg-ghost"); r.Code == 0 {
		t.Fatalf("ghost ack accepted")
	}

	// Prune keeps the newest N acked; unread untouched.
	res := sv.MailPrune("bob", 1)
	mustOK(t, res)
	if got := len(res.Fields["pruned"].([]string)); got != 1 {
		t.Fatalf("prune: %+v", res.Fields)
	}
	r = sv.MailRead("carol", true)
	if got := len(r.Fields["messages"].([]Message)); got != 1 {
		t.Fatalf("prune touched unread broadcast: %d", got)
	}
}

func TestHandoffGenerate(t *testing.T) {
	h := newHarness(t)
	sv := h.clone("a")
	mustOK(t, sv.Init())
	id := createReady(t, sv, "Handoff card")
	mustOK(t, sv.Claim(id, "agent-1", ""))
	mustOK(t, sv.Append("comment", id, "agent-1", tokenOf(t, sv, id), "progress note", ""))

	r := sv.HandoffGenerate(id, "agent-1", false)
	mustOK(t, r)
	packet := r.Fields["packet"].(string)
	for _, want := range []string{"Continuation packet — " + id, "Handoff card", "agent-1", "Workspace anchor"} {
		if !strings.Contains(packet, want) {
			t.Fatalf("packet missing %q:\n%s", want, packet)
		}
	}
	if len(packet) > handoffBound {
		t.Fatalf("packet unbounded: %d", len(packet))
	}
	mustOK(t, sv.HandoffGenerate(id, "agent-1", true))
	head, _ := sv.Store.Sync()
	if _, found, _ := sv.Store.ReadFile(head, "handoff/"+id+".md"); !found {
		t.Fatal("packet not written to the state ref")
	}
}

func tokenOf(t *testing.T, sv *Service, id string) string {
	t.Helper()
	head, err := sv.Store.Sync()
	if err != nil {
		t.Fatal(err)
	}
	c, err := sv.loadCard(head, id)
	if err != nil || c.Claim == nil {
		t.Fatalf("no claim on %s", id)
	}
	return c.Claim.Token
}

// Reap runs in the maintenance checkout, so its packet must mark the
// workspace anchors unavailable; a worker release observes its own
// checkout (plan os-499c5978 as amended).
func TestHandoffAnchorsByPath(t *testing.T) {
	h := newHarness(t)
	sv := h.clone("a")
	mustOK(t, sv.Init())

	reapID := createReady(t, sv, "reaped work")
	mustOK(t, sv.Claim(reapID, "agent-a", "1m"))
	later := time.Now().Add(2 * time.Hour)
	sv.Now = func() time.Time { return later }
	mustOK(t, sv.ReapExpired("lead"))
	head, _ := sv.Store.Sync()
	packet, found, _ := sv.Store.ReadFile(head, "handoff/"+reapID+".md")
	if !found {
		t.Fatal("reap wrote no packet")
	}
	if !strings.Contains(packet, "unavailable — claim reaped by maintenance") {
		t.Fatalf("reap packet does not mark anchors unavailable:\n%s", packet)
	}

	relID := createReady(t, sv, "released work")
	mustOK(t, sv.Claim(relID, "agent-b", ""))
	mustOK(t, sv.Transition(TransitionArgs{Verb: "release", ID: relID, Actor: "agent-b", Token: tokenOf(t, sv, relID)}))
	head, _ = sv.Store.Sync()
	packet, found, _ = sv.Store.ReadFile(head, "handoff/"+relID+".md")
	if !found {
		t.Fatal("release wrote no packet")
	}
	if !strings.Contains(packet, "branch ") || strings.Contains(packet, "unavailable — claim reaped") {
		t.Fatalf("release packet should carry real anchors:\n%s", packet)
	}
}
