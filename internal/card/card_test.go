package card

import (
	"strings"
	"testing"
)

func TestIDsAndTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id := NewID()
		if !strings.HasPrefix(id, "os-") || len(id) != 11 || seen[id] {
			t.Fatalf("id shape/uniqueness: %q", id)
		}
		seen[id] = true
	}
	tok := NewToken()
	if !strings.HasPrefix(tok, "c-") || len(tok) != 18 {
		t.Fatalf("token shape: %q", tok)
	}
}

func TestParseSerializeRoundTrip(t *testing.T) {
	c := &Card{
		ID: "os-1234abcd", Title: "T", State: "in_progress", Priority: "P1",
		Squad: "core", Parent: "os-p", Labels: []string{"x"},
		Links:     &Links{Blocks: []string{"os-b"}},
		BlockedOn: []string{"plan:7"},
		Claim:     &Claim{Actor: "a", Token: "c-t", ClaimedAt: "2026-01-01T00:00:00Z", LeaseExpires: "2026-01-01T01:00:00Z"},
		CreatedAt: "2026-01-01T00:00:00Z",
		Body:      "the work order",
	}
	s, err := c.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != c.ID || back.State != c.State || back.Claim == nil ||
		back.Claim.Token != "c-t" || back.Body != "the work order\n" && strings.TrimSpace(back.Body) != "the work order" {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if Path(c.ID) != "tasks/os-1234abcd.md" {
		t.Fatalf("Path: %q", Path(c.ID))
	}
}

func TestParseRefusals(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":      "just text",
		"unterminated":        "---\nid: os-1\n",
		"invalid yaml":        "---\n{{nope\n---\n\nbody\n",
		"missing id or state": "---\ntitle: x\n---\n\nbody\n",
	}
	for name, content := range cases {
		if _, err := Parse(content); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
