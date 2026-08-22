// Package prclass classifies PRs by head branch (open-seed D3 purity rule):
// plan PRs (seed/<id>-plan) touch exactly one plan file; task PRs (seed/<id>)
// may not touch plans/** at all — not even another task's plan, which would
// launder plan tampering through an unrelated review.
package prclass

import (
	"fmt"
	"regexp"
	"strings"
)

type Kind string

const (
	PlanPR Kind = "plan"
	TaskPR Kind = "task"
	Other  Kind = "other"
)

var (
	planBranch = regexp.MustCompile(`^seed/(os-[0-9a-f]{4,})-plan$`)
	taskBranch = regexp.MustCompile(`^seed/(os-[0-9a-f]{4,})$`)
)

// Classify returns the PR class and, for seed branches, the task id.
func Classify(headBranch string) (Kind, string) {
	if m := planBranch.FindStringSubmatch(headBranch); m != nil {
		return PlanPR, m[1]
	}
	if m := taskBranch.FindStringSubmatch(headBranch); m != nil {
		return TaskPR, m[1]
	}
	return Other, ""
}

// CheckPurity validates the changed-file set against the PR class. Other PRs
// pass vacuously here — but callers adapting to merge queues must classify by
// the underlying PR's head branch, never by the merge-group ref (D3).
func CheckPurity(kind Kind, taskID string, files []string) error {
	switch kind {
	case PlanPR:
		want := "plans/" + taskID + ".md"
		if len(files) != 1 || files[0] != want {
			return fmt.Errorf("plan PR must touch exactly %s, got %v", want, files)
		}
	case TaskPR:
		for _, f := range files {
			if strings.HasPrefix(f, "plans/") {
				return fmt.Errorf("task PR touches %s — task PRs may not touch plans/** (D3 purity)", f)
			}
		}
	}
	return nil
}
