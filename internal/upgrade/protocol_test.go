package upgrade

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The PROTOCOL file becomes the release's protocol.txt asset; it must list
// the protocol version the shipped spec actually declares, or the upgrade
// preflight would lie.
func TestProtocolFileMatchesSpec(t *testing.T) {
	raw, err := os.ReadFile("../../PROTOCOL")
	if err != nil {
		t.Fatal(err)
	}
	port, err := os.ReadFile("../spec/testdata/seed/port-schema/port.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := json.Unmarshal(port, &p); err != nil {
		t.Fatal(err)
	}
	want := strconv.Itoa(p.ProtocolVersion)
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("PROTOCOL (%q) does not list the spec's protocol_version %d", string(raw), p.ProtocolVersion)
	}
}
