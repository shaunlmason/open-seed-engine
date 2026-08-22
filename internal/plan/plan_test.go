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
