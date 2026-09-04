package prclass

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		branch string
		kind   Kind
		task   string
	}{
		{"seed/os-1a2b", TaskPR, "os-1a2b"},
		{"seed/os-1a2b-plan", PlanPR, "os-1a2b"},
		{"main", Other, ""},
		{"feature/os-1a2b", Other, ""},
		{"seed/os-1a2b-plan-extra", Other, ""},
		{"gh-readonly-queue/main/pr-9", Other, ""},
	}
	for _, c := range cases {
		kind, task := Classify(c.branch)
		if kind != c.kind || task != c.task {
			t.Errorf("Classify(%q) = %s,%s want %s,%s", c.branch, kind, task, c.kind, c.task)
		}
	}
}

func TestPurity(t *testing.T) {
	if err := CheckPurity(PlanPR, "os-1a2b", []string{"plans/os-1a2b.md"}); err != nil {
		t.Errorf("exact plan PR rejected: %v", err)
	}
	if err := CheckPurity(PlanPR, "os-1a2b", []string{"plans/os-1a2b.md", "src/x.go"}); err == nil {
		t.Error("plan PR with extra file accepted")
	}
	if err := CheckPurity(PlanPR, "os-1a2b", []string{"plans/os-9999.md"}); err == nil {
		t.Error("plan PR touching another task's plan accepted")
	}
	if err := CheckPurity(TaskPR, "os-1a2b", []string{"src/x.go", "receipts/os-1a2b.json"}); err != nil {
		t.Errorf("clean task PR rejected: %v", err)
	}
	// The laundering cases: a task PR touching ANOTHER task's plan, or
	// another task's receipt. The hashed diff excludes receipts/**, so the
	// second one is invisible to every other gate.
	if err := CheckPurity(TaskPR, "os-1a2b", []string{"src/x.go", "plans/os-9999.md"}); err == nil {
		t.Error("task PR touching plans/** accepted")
	}
	if err := CheckPurity(TaskPR, "os-1a2b", []string{"src/x.go", "receipts/os-9999.json"}); err == nil {
		t.Error("task PR touching another task's receipt accepted")
	}
	if err := CheckPurity(Other, "", []string{"anything"}); err != nil {
		t.Errorf("other PR gated: %v", err)
	}
}
