package task

import (
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/card"
)

// verbs.json declares comment_id/evidence_id as required outputs; both
// builtin stores must mint them (plan os-61967950).
func TestBuiltinAppendIDs(t *testing.T) {
	services := map[string]func(t *testing.T) *Service{
		"filecards": func(t *testing.T) *Service { return newHarness(t).clone("a") },
		"fastcards": func(t *testing.T) *Service { return fastService(t, "") },
	}
	for backend, mk := range services {
		t.Run(backend, func(t *testing.T) {
			sv := mk(t)
			if r := sv.Init(); r.Code != 0 {
				t.Fatalf("init: %+v", r)
			}
			cr := sv.Create(CreateArgs{Title: "Card", Body: "body", Actor: "a"})
			if cr.Code != 0 {
				t.Fatalf("create: %+v", cr)
			}
			id := cr.Fields["task"].(string)
			res := sv.Append("comment", id, "a", "", "hello", "")
			cid, _ := res.Fields["comment_id"].(string)
			if res.Code != 0 || !strings.HasPrefix(cid, "cm-") {
				t.Fatalf("comment_id missing: %+v", res)
			}
			res = sv.Append("commit", id, "a", "", "", "abc123")
			eid, _ := res.Fields["evidence_id"].(string)
			if res.Code != 0 || !strings.HasPrefix(eid, "ev-") {
				t.Fatalf("evidence_id missing: %+v", res)
			}
			got := sv.Get(id)
			c, ok := got.Fields["card"].(*card.Card)
			if !ok {
				t.Fatalf("card shape: %T", got.Fields["card"])
			}
			if !strings.Contains(c.Body, cid) || !strings.Contains(c.Body, eid) {
				t.Fatalf("ids not resolvable in card body: %q %q\n%s", cid, eid, c.Body)
			}
		})
	}
}
