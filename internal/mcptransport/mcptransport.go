// Package mcptransport implements `seed mcp serve` (plan os-67a1bf14,
// research/10 §5.4): MCP as an ADDITIONAL transport, never a
// replacement — an MCP stdio server exposing one tool per port verb,
// dispatching through the identical task-service path the CLI uses.
// Same fencing, same transition table, same run-log events, same
// envelopes; the wrapper holds no coordination logic and adds no
// authority (--actor stays an asserted tool argument, operator verbs
// still check the roster, a HALT marker refuses mutating tools exactly
// as it refuses CLI verbs). The surface is four JSON-RPC 2.0 methods:
// initialize, notifications/initialized, tools/list, tools/call — no
// SDK dependency.
package mcptransport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/shaunlmason/open-seed-engine/internal/task"
)

const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	handler     func(sv *task.Service, a args) *task.Result
}

type args map[string]string

func (a args) get(k string) string { return a[k] }

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func schema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

// Tools is the port verb surface, one tool per verb — the arg shapes
// mirror verbs.json.
func Tools() []Tool {
	return []Tool{
		{Name: "task_create", Description: "Create a card in backlog",
			InputSchema: schema([]string{"title", "actor"}, map[string]any{
				"title": str("card title"), "body": str("markdown work order"), "priority": str("P0|P1|P2|P3"),
				"squad": str("squad name"), "actor": str("acting identity")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Create(task.CreateArgs{Title: a.get("title"), Body: a.get("body"), Priority: a.get("priority"), Squad: a.get("squad"), Actor: a.get("actor")})
			}},
		{Name: "task_ready", Description: "List claimable cards (no open blockers, unclaimed, caller not locked out)",
			InputSchema: schema([]string{"actor"}, map[string]any{"actor": str("acting identity")}),
			handler:     func(sv *task.Service, a args) *task.Result { return sv.Ready(a.get("actor"), "") }},
		{Name: "task_get", Description: "Fetch one card",
			InputSchema: schema([]string{"task"}, map[string]any{"task": str("task id")}),
			handler:     func(sv *task.Service, a args) *task.Result { return sv.Get(a.get("task")) }},
		{Name: "task_list", Description: "List card summaries",
			InputSchema: schema(nil, map[string]any{"state": str("state filter")}),
			handler:     func(sv *task.Service, a args) *task.Result { return sv.List(a.get("state")) }},
		{Name: "task_claim", Description: "Atomically claim a ready card (exclusive; contention is a structured error)",
			InputSchema: schema([]string{"task", "actor"}, map[string]any{
				"task": str("task id"), "actor": str("acting identity"), "lease": str("lease duration, e.g. 60m")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Claim(a.get("task"), a.get("actor"), a.get("lease"))
			}},
		{Name: "task_release", Description: "Give up a claim without closing (token-fenced)",
			InputSchema: schema([]string{"task", "actor", "token"}, map[string]any{
				"task": str("task id"), "actor": str("acting identity"), "token": str("claim token")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Transition(task.TransitionArgs{Verb: "release", ID: a.get("task"), Actor: a.get("actor"), Token: a.get("token")})
			}},
		{Name: "task_transition", Description: "Move a card between states (worker edges are token-fenced; invalid transitions are refused)",
			InputSchema: schema([]string{"task", "to", "actor"}, map[string]any{
				"task": str("task id"), "to": str("target state"), "actor": str("acting identity"),
				"token": str("claim token (worker edges)"), "blocked_on": str("entry when to=blocked (plan:<pr>|dep:<id>|manual:<op>)")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Transition(task.TransitionArgs{Verb: "transition", ID: a.get("task"), To: a.get("to"),
					Actor: a.get("actor"), Token: a.get("token"), BlockedOn: a.get("blocked_on")})
			}},
		{Name: "task_close", Description: "Accept + cascade: review to done only (operator)",
			InputSchema: schema([]string{"task", "actor"}, map[string]any{
				"task": str("task id"), "actor": str("operator identity"), "resolution": str("resolution message")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Transition(task.TransitionArgs{Verb: "close", ID: a.get("task"), Actor: a.get("actor"), Resolution: a.get("resolution")})
			}},
		{Name: "task_comment", Description: "Append a comment (fenced while the card holds a claim); returns comment_id",
			InputSchema: schema([]string{"task", "actor", "body"}, map[string]any{
				"task": str("task id"), "actor": str("acting identity"), "body": str("comment text"), "token": str("claim token if the card is claimed")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Append("comment", a.get("task"), a.get("actor"), a.get("token"), a.get("body"), "")
			}},
		{Name: "task_attach_evidence", Description: "Append evidence (append-only); returns evidence_id",
			InputSchema: schema([]string{"task", "actor", "ref"}, map[string]any{
				"task": str("task id"), "actor": str("acting identity"), "kind": str("log|commit|pr|file"),
				"ref": str("SHA, URL, path, or excerpt ref"), "token": str("claim token if the card is claimed")}),
			handler: func(sv *task.Service, a args) *task.Result {
				kind := a.get("kind")
				if kind == "" || kind == "comment" {
					kind = "commit"
				}
				return sv.Append(kind, a.get("task"), a.get("actor"), a.get("token"), "", a.get("ref"))
			}},
		{Name: "task_plan_unblock",
			Description: "Remove a plan:<pr> entry from a blocked card (operator; the caller must have established the PR merged)",
			InputSchema: schema([]string{"task", "pr", "actor"}, map[string]any{
				"task": str("task id"), "pr": str("plan PR number"), "actor": str("operator identity")}),
			handler: func(sv *task.Service, a args) *task.Result {
				n := 0
				fmt.Sscanf(a.get("pr"), "%d", &n)
				return sv.PlanUnblock(a.get("task"), n, a.get("actor"))
			}},
	}
}

// operator-named transition verbs (the transition table keys edges by
// verb): one tool each, same handler shape.
func operatorTools() []Tool {
	type ov struct{ name, verb, desc string }
	verbs := []ov{
		{"task_promote", "promote", "backlog → ready (operator)"},
		{"task_deprioritize", "deprioritize", "ready → backlog (operator)"},
		{"task_reject", "reject", "review → ready, locking the implementer out (operator)"},
		{"task_cancel", "cancel", "terminal cancel from any non-terminal state, with cascade (operator)"},
		{"task_reinstate", "reinstate", "cancelled → backlog (operator)"},
		{"task_block", "block", "park an unclaimed card with a blocked_on entry (operator)"},
		{"task_unblock", "unblock", "remove a blocked_on entry; releases when the set empties (operator)"},
	}
	var out []Tool
	for _, v := range verbs {
		v := v
		out = append(out, Tool{Name: v.name, Description: v.desc,
			InputSchema: schema([]string{"task", "actor"}, map[string]any{
				"task": str("task id"), "actor": str("operator identity"),
				"resolution": str("message (reject/cancel)"), "blocked_on": str("entry (block/unblock)")}),
			handler: func(sv *task.Service, a args) *task.Result {
				return sv.Transition(task.TransitionArgs{Verb: v.verb, ID: a.get("task"), Actor: a.get("actor"),
					Resolution: a.get("resolution"), BlockedOn: a.get("blocked_on")})
			}})
	}
	return out
}

// Serve speaks MCP over in/out until EOF. newService builds the task
// service per call (the CLI's own construction path), keeping the
// wrapper stateless.
func Serve(in io.Reader, out io.Writer, newService func() (*task.Service, error)) error {
	tools := append(Tools(), operatorTools()...)
	byName := map[string]Tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	reply := func(id json.RawMessage, result any, rerr *rpcError) error {
		if id == nil { // notification: no response
			return nil
		}
		return enc.Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rerr})
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		switch req.Method {
		case "initialize":
			if err := reply(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "seed", "version": "1"},
			}, nil); err != nil {
				return err
			}
		case "notifications/initialized":
			// no response
		case "tools/list":
			if err := reply(req.ID, map[string]any{"tools": tools}, nil); err != nil {
				return err
			}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments map[string]any  `json:"arguments"`
				Raw       json.RawMessage `json:"-"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				if err := reply(req.ID, nil, &rpcError{Code: -32602, Message: "bad params"}); err != nil {
					return err
				}
				continue
			}
			tool, okT := byName[p.Name]
			if !okT {
				if err := reply(req.ID, nil, &rpcError{Code: -32602, Message: "unknown tool " + p.Name}); err != nil {
					return err
				}
				continue
			}
			a := args{}
			for k, v := range p.Arguments {
				a[k] = fmt.Sprint(v)
			}
			sv, err := newService()
			var content string
			isErr := false
			if err != nil {
				content = fmt.Sprintf(`{"ok":false,"schema_version":"1.0","error":"backend_unavailable","message":%q}`, err.Error())
				isErr = true
			} else {
				res := tool.handler(sv, a)
				envelope := map[string]any{"ok": res.Code == 0, "schema_version": "1.0"}
				for k, v := range res.Fields {
					envelope[k] = v
				}
				if res.Code != 0 {
					envelope["error"] = res.Err
					envelope["exit"] = res.Code
					isErr = true
				}
				b, _ := json.Marshal(envelope)
				content = string(b)
			}
			if err := reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": content}},
				"isError": isErr,
			}, nil); err != nil {
				return err
			}
		default:
			if err := reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
