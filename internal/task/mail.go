package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path"
	"sort"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
	"gopkg.in/yaml.v3"
)

// Mail (plan os-499c5978, inspirations/08 as amended by its erratum):
// one file per MESSAGE at mail/<recipient>/<msg-id>.yaml on the
// seed-state ref — never rewritten (rewritten structured files under
// merge=union interleave into invalid YAML); ack is a file MOVE to
// mail/<recipient>/acked/<msg-id>.yaml. Broadcast recipient is "_all";
// acking a broadcast copies it into the actor's acked dir (the shared
// file stays for other readers, bounded by maintenance pruning). No
// daemon: mailboxes are read at natural checkpoints; the optional tmux
// nudge carries no content — "you have mail" only.

type Message struct {
	ID     string `yaml:"id" json:"id"`
	From   string `yaml:"from" json:"from"`
	At     string `yaml:"at" json:"at"`
	Type   string `yaml:"type" json:"type"`
	Task   string `yaml:"task,omitempty" json:"task,omitempty"`
	Thread string `yaml:"thread,omitempty" json:"thread,omitempty"`
	Text   string `yaml:"text" json:"text"`
	Acked  bool   `yaml:"-" json:"acked"`
}

var mailTypes = map[string]bool{"directive": true, "status": true, "request": true, "info": true, "alert": true, "handoff": true}

const mailRoot = "mail"

func mailDir(actor string) string  { return path.Join(mailRoot, actor) }
func ackedDir(actor string) string { return path.Join(mailDir(actor), "acked") }

// MailSend appends one message file for the recipient.
func (sv *Service) MailSend(from, to, mtype, text, taskID, thread string) *Result {
	if from == "" || to == "" || text == "" {
		return failure(spec.ExitInvalid, "usage", map[string]any{"message": "mail send needs --actor, --to, --text"})
	}
	if from == to {
		return failure(spec.ExitInvalid, "self_send_refused", map[string]any{"message": "sending mail to yourself is refused"})
	}
	if mtype == "" {
		mtype = "info"
	}
	if !mailTypes[mtype] {
		return failure(spec.ExitInvalid, "usage", map[string]any{"message": "mail type must be directive|status|request|info|alert|handoff"})
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	var id string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		now := sv.now()
		id = "msg-" + strings.NewReplacer(":", "", "-", "", "T", "", "Z", "").Replace(now) + "-" + hex.EncodeToString(b)
		m := Message{ID: id, From: from, At: now, Type: mtype, Task: taskID, Thread: thread, Text: text}
		content, err := yaml.Marshal(&m)
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "mail send " + id,
			Changes: []gitx.Change{{Path: path.Join(mailDir(to), id+".yaml"), Content: string(content)}},
			Events:  []string{sv.event(from, "mail-send", taskID, map[string]any{"to": to, "type": mtype, "id": id})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "mail-send", "id": id, "to": to})
}

// MailRead lists the actor's messages plus _all broadcasts, oldest
// first. unreadOnly hides acked history; broadcasts already acked by
// THIS actor (a copy in their acked dir) count as acked.
func (sv *Service) MailRead(actor string, unreadOnly bool) *Result {
	if actor == "" {
		return failure(spec.ExitInvalid, "usage", map[string]any{"message": "mail read needs --actor"})
	}
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	ackedIDs := map[string]bool{}
	var msgs []Message
	collect := func(dir string, acked bool) error {
		names, err := sv.Store.ListDir(head, dir)
		if err != nil {
			return err
		}
		for _, n := range names {
			if !strings.HasSuffix(n, ".yaml") {
				continue
			}
			content, found, err := sv.Store.ReadFile(head, path.Join(dir, n))
			if err != nil || !found {
				continue
			}
			var m Message
			if yaml.Unmarshal([]byte(content), &m) != nil {
				continue
			}
			m.Acked = acked
			if acked {
				ackedIDs[m.ID] = true
			}
			msgs = append(msgs, m)
		}
		return nil
	}
	if err := collect(ackedDir(actor), true); err != nil {
		return errResult(err)
	}
	if err := collect(mailDir(actor), false); err != nil {
		return errResult(err)
	}
	if err := collect(mailDir("_all"), false); err != nil {
		return errResult(err)
	}
	// Broadcasts this actor already acked appear once, as acked; the
	// inbox pass may have re-added the shared copy — dedupe by id.
	seen := map[string]bool{}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		if ackedIDs[m.ID] {
			m.Acked = true
		}
		if unreadOnly && m.Acked {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return ok(map[string]any{"verb": "mail-read", "actor": actor, "messages": out})
}

// MailAck moves a direct message into the actor's acked dir; a
// broadcast is copied there (the shared file remains for others).
func (sv *Service) MailAck(actor, id string) *Result {
	if actor == "" || id == "" {
		return failure(spec.ExitInvalid, "usage", map[string]any{"message": "mail ack needs --actor and --id"})
	}
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		direct := path.Join(mailDir(actor), id+".yaml")
		if content, found, err := sv.Store.ReadFile(head, direct); err == nil && found {
			return &stateref.Mutation{
				Message: "mail ack " + id,
				Changes: []gitx.Change{
					{Path: direct, Delete: true},
					{Path: path.Join(ackedDir(actor), id+".yaml"), Content: content},
				},
				Events: []string{sv.event(actor, "mail-ack", "", map[string]any{"id": id})},
			}, nil
		}
		bcast := path.Join(mailDir("_all"), id+".yaml")
		if content, found, err := sv.Store.ReadFile(head, bcast); err == nil && found {
			return &stateref.Mutation{
				Message: "mail ack " + id,
				Changes: []gitx.Change{{Path: path.Join(ackedDir(actor), id+".yaml"), Content: content}},
				Events:  []string{sv.event(actor, "mail-ack", "", map[string]any{"id": id, "broadcast": true})},
			}, nil
		}
		return nil, &stateref.Terminal{Code: spec.ExitNotFound, Name: "not_found"}
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "mail-ack", "id": id})
}

// MailNudge is advisory: with tmux present it pokes the actor's pane
// with a content-free "you have mail" line (shogun's convention —
// message content never travels through tmux); otherwise a declared
// no-op.
func (sv *Service) MailNudge(actor string) *Result {
	if actor == "" {
		return failure(spec.ExitInvalid, "usage", map[string]any{"message": "mail nudge needs an actor"})
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return ok(map[string]any{"verb": "mail-nudge", "actor": actor, "note": "tmux not present — nudge is a declared no-op; mail is read at checkpoints"})
	}
	cmd := exec.Command("tmux", "send-keys", "-t", actor, fmt.Sprintf("# you have mail: seed mail read --actor %s --unread", actor), "Enter")
	if err := cmd.Run(); err != nil {
		return ok(map[string]any{"verb": "mail-nudge", "actor": actor, "note": "no tmux target for actor — nudge skipped (advisory)"})
	}
	return ok(map[string]any{"verb": "mail-nudge", "actor": actor, "note": "nudged via tmux"})
}

// MailPrune bounds acked history per recipient to the newest keep
// entries (maintenance). Unacked mail is never pruned.
func (sv *Service) MailPrune(actor string, keep int) *Result {
	if keep <= 0 {
		keep = 30
	}
	var pruned []string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		pruned = nil
		recipients := []string{}
		if actor != "" {
			recipients = append(recipients, actor)
		} else {
			names, err := sv.Store.ListDir(head, mailRoot)
			if err != nil {
				return nil, err
			}
			recipients = names
		}
		var changes []gitx.Change
		for _, r := range recipients {
			names, err := sv.Store.ListDir(head, ackedDir(r))
			if err != nil {
				continue
			}
			var files []string
			for _, n := range names {
				if strings.HasSuffix(n, ".yaml") {
					files = append(files, n)
				}
			}
			sort.Strings(files) // ids embed the timestamp: lexical = chronological
			if len(files) <= keep {
				continue
			}
			for _, n := range files[:len(files)-keep] {
				changes = append(changes, gitx.Change{Path: path.Join(ackedDir(r), n), Delete: true})
				pruned = append(pruned, r+"/"+n)
			}
		}
		if len(changes) == 0 {
			return nil, &stateref.Terminal{Code: 0, Name: "noop"}
		}
		return &stateref.Mutation{
			Message: "mail prune",
			Changes: changes,
			Events:  []string{sv.event("seed-maintenance", "mail-prune", "", map[string]any{"pruned": len(changes)})},
		}, nil
	})
	if err != nil {
		if t, okT := err.(*stateref.Terminal); okT && t.Name == "noop" {
			return ok(map[string]any{"verb": "mail-prune", "pruned": []string{}})
		}
		return errResult(err)
	}
	return ok(map[string]any{"verb": "mail-prune", "pruned": pruned})
}

// MailUnreadCounts reports unread (inbox) counts per actor for
// maintenance reporting.
func (sv *Service) MailUnreadCounts() *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	counts := map[string]int{}
	recipients, err := sv.Store.ListDir(head, mailRoot)
	if err == nil {
		for _, r := range recipients {
			names, err := sv.Store.ListDir(head, mailDir(r))
			if err != nil {
				continue
			}
			n := 0
			for _, f := range names {
				if strings.HasSuffix(f, ".yaml") {
					n++
				}
			}
			if n > 0 {
				counts[r] = n
			}
		}
	}
	return ok(map[string]any{"verb": "mail-report", "unread": counts})
}
