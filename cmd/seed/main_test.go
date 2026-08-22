package main

import (
	"os"
	"strings"
	"testing"
)

func TestVersionExitsZero(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if got := run(args, devnull, devnull); got != 0 {
			t.Errorf("run(%v) = %d, want 0", args, got)
		}
	}
}

func TestUnknownAndMissingCommandExitUsage(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	if got := run(nil, devnull, devnull); got != exitUsage {
		t.Errorf("run(nil) = %d, want %d", got, exitUsage)
	}
	if got := run([]string{"bogus"}, devnull, devnull); got != exitUsage {
		t.Errorf("run(bogus) = %d, want %d", got, exitUsage)
	}
	// The reserved port exit codes must not be reachable from usage errors.
	for _, reserved := range []int{2, 3, 6, 10} {
		if exitUsage == reserved {
			t.Errorf("exitUsage collides with reserved port exit code %d", reserved)
		}
	}
}

func TestVersionOutputContainsDefaults(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if got := run([]string{"version"}, f, f); got != 0 {
		t.Fatalf("run(version) = %d", got)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"seed", version, commit} {
		if !strings.Contains(string(b), want) {
			t.Errorf("version output %q missing %q", b, want)
		}
	}
}
