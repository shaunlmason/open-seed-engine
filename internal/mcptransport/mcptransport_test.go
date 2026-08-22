package mcptransport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shaunlmason/open-seed-engine/internal/task"
)

// Full card lifecycle driven purely through tools/call over the wire —
// identical envelopes, fencing, and refusals to the CLI path.

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	cmd := exec.Command("git", "init", "-q", "--initial-branch=main", root)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	seed := filepath.Join(root, ".seed")
	if err := os.MkdirAll(filepath.Join(seed, "port-schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "spec", "testdata", "seed", "port-schema")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(seed, "port-schema", e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "[coordination]\nbackend = \"fastcards\"\n[operators]\nactors = [\"lead\"]\n"
	if err := os.WriteFile(filepath.Join(seed, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	verFile := filepath.Join(src, "..", "version")
	if b, err := os.ReadFile(verFile); err == nil {
		_ = os.WriteFile(filepath.Join(seed, "version"), b, 0o644)
	} else {
		_ = os.WriteFile(filepath.Join(seed, "version"), []byte("1\n"), 0o644)
	}
	return root
}

type wire struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
	n   int
}

func startServer(t *testing.T, root string) *wire {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer outW.Close()
		_ = Serve(inR, outW, func() (*task.Service, error) { return task.NewService(root) })
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	return &wire{t: t, in: inW, out: sc}
}

func (w *wire) call(method string, params any) map[string]any {
	w.t.Helper()
	w.n++
	req := map[string]any{"jsonrpc": "2.0", "id": w.n, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		w.t.Fatal(err)
	}
	if !w.out.Scan() {
		w.t.Fatalf("no response to %s: %v", method, w.out.Err())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.out.Bytes(), &resp); err != nil {
		w.t.Fatalf("bad response: %v: %s", err, w.out.Text())
	}
	return resp
}

func (w *wire) tool(name string, args map[string]any) (map[string]any, bool) {
	w.t.Helper()
	resp := w.call("tools/call", map[string]any{"name": name, "arguments": args})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		w.t.Fatalf("tool %s: no result: %v", name, resp)
	}
	content := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var envelope map[string]any
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		w.t.Fatalf("tool %s: envelope unparseable: %s", name, content)
	}
	isErr, _ := res["isError"].(bool)
	return envelope, isErr
}

func TestLifecycleOverMCP(t *testing.T) {
	root := fixtureRoot(t)
	w := startServer(t, root)
	defer w.in.Close()

	initResp := w.call("initialize", map[string]any{"protocolVersion": protocolVersion, "capabilities": map[string]any{}})
	if initResp["result"].(map[string]any)["protocolVersion"] != protocolVersion {
		t.Fatalf("handshake: %v", initResp)
	}

	list := w.call("tools/list", nil)
	tools := list["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"task_create", "task_ready", "task_get", "task_list", "task_claim",
		"task_lease_renew", "task_release", "task_transition", "task_close", "task_comment",
		"task_attach_evidence", "task_plan_unblock", "task_promote", "task_deprioritize",
		"task_reject", "task_cancel", "task_reinstate", "task_block", "task_unblock"} {
		if !names[want] {
			t.Fatalf("tools/list missing %s: %v", want, names)
		}
	}

	// The store needs init once (not a port verb): do it service-side.
	sv, err := task.NewService(root)
	if err != nil {
		t.Fatal(err)
	}
	if r := sv.Init(); r.Code != 0 {
		t.Fatalf("init: %+v", r)
	}

	env, isErr := w.tool("task_create", map[string]any{"title": "MCP card", "body": "the work", "actor": "a"})
	if isErr || env["ok"] != true {
		t.Fatalf("create: %v", env)
	}
	id := env["task"].(string)

	if env, isErr = w.tool("task_promote", map[string]any{"task": id, "actor": "lead"}); isErr {
		t.Fatalf("promote: %v", env)
	}
	env, isErr = w.tool("task_claim", map[string]any{"task": id, "actor": "agent-1"})
	if isErr || env["claim_token"] == nil {
		t.Fatalf("claim: %v", env)
	}
	tok := env["claim_token"].(string)

	// Contention surfaces as a structured tool error, not a transport one.
	env, isErr = w.tool("task_claim", map[string]any{"task": id, "actor": "agent-2"})
	if !isErr || fmt.Sprint(env["exit"]) != "2" {
		t.Fatalf("contention: isErr=%v %v", isErr, env)
	}
	// Fencing holds through the transport.
	env, isErr = w.tool("task_comment", map[string]any{"task": id, "actor": "agent-1", "body": "hi", "token": "c-bogus"})
	if !isErr || fmt.Sprint(env["exit"]) != "6" {
		t.Fatalf("fence: isErr=%v %v", isErr, env)
	}
	env, isErr = w.tool("task_comment", map[string]any{"task": id, "actor": "agent-1", "body": "hi", "token": tok})
	if isErr || !strings.HasPrefix(fmt.Sprint(env["comment_id"]), "cm-") {
		t.Fatalf("comment: %v", env)
	}
	if env, isErr = w.tool("task_lease_renew", map[string]any{"task": id, "actor": "agent-1", "token": tok, "lease": "45m"}); isErr || env["lease_expires"] == nil {
		t.Fatalf("lease renew: %v", env)
	}
	if env, isErr = w.tool("task_transition", map[string]any{"task": id, "to": "review", "actor": "agent-1", "token": tok}); isErr {
		t.Fatalf("review: %v", env)
	}
	if env, isErr = w.tool("task_close", map[string]any{"task": id, "actor": "lead", "resolution": "done via MCP"}); isErr {
		t.Fatalf("close: %v", env)
	}
	env, _ = w.tool("task_get", map[string]any{"task": id})
	if env["state"] != "done" {
		t.Fatalf("final state: %v", env["state"])
	}

	// Unknown tool is a JSON-RPC error.
	resp := w.call("tools/call", map[string]any{"name": "task_ghost", "arguments": map[string]any{}})
	if resp["error"] == nil {
		t.Fatalf("unknown tool accepted: %v", resp)
	}
}
