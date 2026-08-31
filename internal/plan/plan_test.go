package plan

import (
	"strings"
	"testing"
)

const good = `# Plan

## Steps

1. Step one.

## File Scope

- src/

## Acceptance Criteria

- Works.

## Validation Commands

- ` + "`make check`" + `
- go test ./...
`

func TestParseCommands(t *testing.T) {
	p := Parse(good)
	if len(p.ValidationCommands) != 2 || p.ValidationCommands[0] != "make check" || p.ValidationCommands[1] != "go test ./..." {
		t.Fatalf("commands = %v", p.ValidationCommands)
	}
}

func TestParseLabeledCommands(t *testing.T) {
	labeled := strings.Replace(good,
		"- `make check`\n- go test ./...",
		"- Boundary: `go test ./internal/thing/...`\n- Retention: `go test ./...` and `make check`", 1)
	p := Parse(labeled)
	want := []string{"go test ./internal/thing/...", "go test ./...", "make check"}
	if len(p.ValidationCommands) != len(want) {
		t.Fatalf("commands = %v", p.ValidationCommands)
	}
	for i, w := range want {
		if p.ValidationCommands[i] != w {
			t.Fatalf("command %d = %q, want %q (all: %v)", i, p.ValidationCommands[i], w, p.ValidationCommands)
		}
	}
	// An unclosed backtick yields no span; the bullet then has spans
	// from its closed pair only.
	odd := Parse(strings.Replace(good, "- go test ./...", "- Odd: `go vet ./...` and `broken", 1))
	if len(odd.ValidationCommands) != 2 || odd.ValidationCommands[1] != "go vet ./..." {
		t.Fatalf("odd backticks: %v", odd.ValidationCommands)
	}
	// A bullet with ONLY an unclosed backtick produces no commands at
	// all: malformed inline-code syntax must never run wholesale as a
	// legacy bullet.
	lone := Parse(strings.Replace(good, "- go test ./...", "- Boundary: `go test ./...", 1))
	if len(lone.ValidationCommands) != 1 || lone.ValidationCommands[0] != "make check" {
		t.Fatalf("a lone-backtick bullet must yield nothing: %v", lone.ValidationCommands)
	}
}

func TestParseFencedCommands(t *testing.T) {
	fenced := strings.Replace(good,
		"- `make check`\n- go test ./...",
		"```sh\nmake check\ngo test ./...\n```", 1)
	p := Parse(fenced)
	if len(p.ValidationCommands) != 2 {
		t.Fatalf("commands = %v", p.ValidationCommands)
	}
}

func TestLint(t *testing.T) {
	if errs := Lint(good); len(errs) != 0 {
		t.Fatalf("good plan: %v", errs)
	}
	for _, section := range RequiredSections {
		broken := strings.Replace(good, "## "+section, "## Nope-"+section, 1)
		if errs := Lint(broken); len(errs) == 0 {
			t.Errorf("missing %s accepted", section)
		}
	}
	noCmds := strings.Replace(good, "- `make check`\n- go test ./...\n", "", 1)
	if errs := Lint(noCmds); len(errs) == 0 {
		t.Error("plan without commands accepted")
	}
}

func TestHashStable(t *testing.T) {
	if Hash(good) != Hash(good) || Hash(good) == Hash(good+" ") {
		t.Fatal("hash not a content pin")
	}
}
