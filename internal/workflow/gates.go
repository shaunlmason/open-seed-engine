package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runGate evaluates the gate guarding a step. Returns the step report so
// far and, for a pending approval, the gate id the run pauses on. Under
// --mock every gate auto-passes and says so.
func (r *runner) runGate(s *Step) (*StepReport, string) {
	g := s.Gate
	if r.opts.Mock {
		return &StepReport{Note: "mock: " + g.Type + " gate auto-passed"}, ""
	}
	switch g.Type {
	case "approval":
		return r.approvalGate(s)
	case "review":
		return r.reviewGate(s)
	case "checks":
		return r.checksGate(s)
	}
	return &StepReport{Status: "failed", Note: "unknown gate type " + g.Type}, ""
}

// approvalGate pauses the run until a human writes the response file; the
// captured response joins the run's gate records.
func (r *runner) approvalGate(s *Step) (*StepReport, string) {
	resp := filepath.Join(r.dir, "gates", s.ID+".response.json")
	if _, err := os.Stat(resp); err != nil {
		msg := s.Gate.Message
		if msg == "" {
			msg = "approval required"
		}
		return &StepReport{Status: "paused", Note: msg}, s.ID
	}
	note := "approved"
	if s.Gate.CaptureResponse {
		note = "approved (response captured at gates/" + s.ID + ".response.json)"
	}
	return &StepReport{Note: note}, ""
}

// reviewGate is a review-and-fix loop, never a bare re-poll: on REVISE the
// named remediation step (a coding-role step) runs BEFORE the reviewer is
// re-run, up to max_revisions.
func (r *runner) reviewGate(s *Step) (*StepReport, string) {
	g := s.Gate
	maxRev := g.MaxRevisions
	if maxRev <= 0 {
		maxRev = 2
	}
	remediation := r.findStep(g.Remediation)
	revisions := 0
	for {
		verdict, err := r.reviewerVerdict(s)
		if err != nil {
			return &StepReport{Status: "failed", Note: "review gate: " + err.Error(), Revisions: revisions}, ""
		}
		switch verdict {
		case "APPROVE", "APPROVE_WITH_NOTES":
			return &StepReport{Note: "review verdict " + verdict, Revisions: revisions}, ""
		case "REVISE":
			if revisions >= maxRev {
				return &StepReport{Status: "failed", Note: fmt.Sprintf("review gate: still REVISE after %d revision(s)", revisions), Revisions: revisions}, ""
			}
			revisions++
			r.execAction(remediation)
			r.mu.Lock()
			failed := r.cp.Steps[remediation.ID] != nil && r.cp.Steps[remediation.ID].Status == "failed"
			r.mu.Unlock()
			if failed {
				return &StepReport{Status: "failed", Note: "review gate: remediation step " + remediation.ID + " failed", Revisions: revisions}, ""
			}
		default:
			return &StepReport{Status: "failed", Note: "review gate: reviewer returned no APPROVE|APPROVE_WITH_NOTES|REVISE verdict (" + verdict + ")", Revisions: revisions}, ""
		}
	}
}

// reviewerVerdict runs the reviewer role through the harness contract and
// reads the verdict from gates/<step>.review.json (the reviewer's declared
// output file, passed via SEED_PRODUCES).
func (r *runner) reviewerVerdict(s *Step) (string, error) {
	out := filepath.Join(r.dir, "gates", s.ID+".review.json")
	_ = os.Remove(out)
	produces, _ := json.Marshal([]map[string]any{{
		"name": s.ID + "-review", "file": out,
		"schema": map[string]any{"type": "object", "required": []any{"verdict"},
			"properties": map[string]any{"verdict": map[string]any{"enum": []any{"APPROVE", "APPROVE_WITH_NOTES", "REVISE"}}}},
	}})
	prompt := fmt.Sprintf("Review the work upstream of step %q in run %s. Write your verdict JSON {\"verdict\": \"APPROVE|APPROVE_WITH_NOTES|REVISE\", \"notes\": \"...\"} to %s.", s.ID, r.cp.RunID, out)
	cmd := exec.CommandContext(r.ctx, filepath.Join(r.opts.Root, "scripts", "seed-harness"), firstNonEmpty(s.Harness, r.wf.Defaults.Harness))
	cmd.Dir = r.opts.Root
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"SEED_ROLE="+s.Gate.ReviewerRole,
		"SEED_PERMISSION=read-only",
		"SEED_MODEL="+firstNonEmpty(s.Model, r.wf.Defaults.Model),
		"SEED_STEP="+s.ID,
		"SEED_RUN_DIR="+r.dir,
		"SEED_PRODUCES="+string(produces),
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("reviewer harness failed: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return "", fmt.Errorf("reviewer wrote no verdict file: %v", err)
	}
	var v struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("verdict file unparseable: %v", err)
	}
	return v.Verdict, nil
}

func (r *runner) findStep(id string) *Step {
	for _, s := range allSteps(r.wf.Steps) {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// checksGate requires BOTH decided conditions: check runs green on the
// ref (REST) AND unresolved_threads == 0 on the PR (GraphQL review-thread
// query). GITHUB_TOKEN is required; SEED_GITHUB_API / SEED_GITHUB_GRAPHQL
// override the endpoints for tests.
func (r *runner) checksGate(s *Step) (*StepReport, string) {
	g := s.Gate
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return &StepReport{Status: "failed", Note: "checks gate needs GITHUB_TOKEN (or run with --mock)"}, ""
	}
	repo := r.subst(g.Repo)
	ref := r.subst(g.Ref)
	pr := r.subst(g.PR)
	if g.RequiredCI {
		api := firstNonEmpty(os.Getenv("SEED_GITHUB_API"), "https://api.github.com")
		body, err := ghGET(api+"/repos/"+repo+"/commits/"+ref+"/check-runs", token)
		if err != nil {
			return &StepReport{Status: "failed", Note: "checks gate: " + err.Error()}, ""
		}
		var cr struct {
			CheckRuns []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(body, &cr); err != nil {
			return &StepReport{Status: "failed", Note: "checks gate: check-runs unparseable: " + err.Error()}, ""
		}
		for _, c := range cr.CheckRuns {
			if c.Status != "completed" || (c.Conclusion != "success" && c.Conclusion != "neutral" && c.Conclusion != "skipped") {
				return &StepReport{Status: "failed", Note: "checks gate closed: check " + c.Name + " is " + c.Status + "/" + c.Conclusion}, ""
			}
		}
	}
	if g.UnresolvedThreads != nil {
		gql := firstNonEmpty(os.Getenv("SEED_GITHUB_GRAPHQL"), "https://api.github.com/graphql")
		owner, name, ok := strings.Cut(repo, "/")
		if !ok {
			return &StepReport{Status: "failed", Note: "checks gate: repo must be owner/name"}, ""
		}
		q := `query($o:String!,$n:String!,$pr:Int!){repository(owner:$o,name:$n){pullRequest(number:$pr){reviewThreads(first:100){nodes{isResolved}}}}}`
		payload, _ := json.Marshal(map[string]any{"query": q, "variables": map[string]any{"o": owner, "n": name, "pr": atoiSafe(pr)}})
		body, err := ghPOST(gql, token, payload)
		if err != nil {
			return &StepReport{Status: "failed", Note: "checks gate: " + err.Error()}, ""
		}
		var resp struct {
			Data struct {
				Repository struct {
					PullRequest struct {
						ReviewThreads struct {
							Nodes []struct {
								IsResolved bool `json:"isResolved"`
							} `json:"nodes"`
						} `json:"reviewThreads"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return &StepReport{Status: "failed", Note: "checks gate: review threads unparseable: " + err.Error()}, ""
		}
		unresolved := 0
		for _, n := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
			if !n.IsResolved {
				unresolved++
			}
		}
		if unresolved > *g.UnresolvedThreads {
			return &StepReport{Status: "failed", Note: fmt.Sprintf("checks gate closed: %d unresolved review thread(s), allowed %d", unresolved, *g.UnresolvedThreads)}, ""
		}
	}
	return &StepReport{Note: "checks gate passed"}, ""
}

func ghGET(url, token string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return ghDo(req)
}

func ghPOST(url, token string, body []byte) ([]byte, error) {
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return ghDo(req)
}

func ghDo(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", req.URL, resp.StatusCode)
	}
	return body, nil
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
