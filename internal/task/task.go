// Package task orchestrates port verbs over the filecards backend: it loads
// fresh state from the seed-state ref, asks the table-driven evaluator
// (internal/port) whether the operation is legal, applies the spec-declared
// effects, and commits card mutation + run-log event atomically: one commit
// per verb (§7.2). No transition or class logic lives here; it all comes from
// the spec tables.
package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/card"
	"github.com/shaunlmason/open-seed-engine/internal/config"
	"github.com/shaunlmason/open-seed-engine/internal/fastcards"
	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/port"
	"github.com/shaunlmason/open-seed-engine/internal/spec"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"
	"github.com/shaunlmason/open-seed-engine/internal/validate"
)

var blockedOnPattern = regexp.MustCompile(`^(plan:[0-9]+|dep:os-[0-9a-f]{4,}|manual:.+)$`)

type Service struct {
	Root  string
	Spec  *spec.Spec
	Cfg   *config.Config
	Store stateref.Backing
	Now   func() time.Time
}

// Result is envelope material: exit code plus fields for the JSON envelope.
type Result struct {
	Code   int
	Err    string
	Fields map[string]any
}

func ok(fields map[string]any) *Result { return &Result{Code: 0, Fields: fields} }

func failure(code int, name string, fields map[string]any) *Result {
	return &Result{Code: code, Err: name, Fields: fields}
}

// NewService loads spec + config from root and enforces the protocol-version
// check (exit 10) before any verb runs: the shim is the enforcement point.
func NewService(root string) (*Service, error) {
	seedDir := filepath.Join(root, ".seed")
	s, err := spec.Load(filepath.Join(seedDir, "port-schema"))
	if err != nil {
		return nil, err
	}
	if err := spec.CheckVersion(s, seedDir); err != nil {
		return nil, err
	}
	cfg, err := config.Load(seedDir)
	if err != nil {
		return nil, err
	}
	// Builtin store selection (§7.1 amendment): filecards is the git state
	// ref; fastcards is the machine-local SQLite store. External backends
	// never reach this constructor: the CLI dispatches them to their
	// plugin first.
	var store stateref.Backing
	if cfg.Coordination.Backend == "fastcards" {
		fc, err := fastcards.Open(root)
		if err != nil {
			return nil, err
		}
		store = fc
	} else {
		store = stateref.Open(root, cfg.Coordination.Remote, cfg.Coordination.StateBranch)
	}
	return &Service{
		Root:  root,
		Spec:  s,
		Cfg:   cfg,
		Store: store,
		Now:   time.Now,
	}, nil
}

func (sv *Service) now() string { return sv.Now().UTC().Format(time.RFC3339) }

func (sv *Service) event(actor, verb, id string, data map[string]any) string {
	e := map[string]any{"ts": sv.now(), "actor": actor, "verb": verb, "task": id}
	if len(data) > 0 {
		e["data"] = data
	}
	b, _ := json.Marshal(e)
	return string(b)
}

func (sv *Service) loadCard(head, id string) (*card.Card, error) {
	content, found, err := sv.Store.ReadFile(head, card.Path(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &stateref.Terminal{Code: spec.ExitNotFound, Name: "not_found"}
	}
	return card.Parse(content)
}

func (sv *Service) portCard(c *card.Card) port.Card {
	pc := port.Card{State: c.State, RejectedAuthors: c.RejectedAuthors, BlockedOn: c.BlockedOn}
	if c.Claim != nil {
		pc.ClaimToken = c.Claim.Token
		if exp, err := time.Parse(time.RFC3339, c.Claim.LeaseExpires); err == nil {
			pc.LeaseExpired = sv.Now().UTC().After(exp)
		}
	}
	return pc
}

// credential resolves the class per §7.1/Q5: claim is always worker; a
// tokenless transition (or a named operator verb) by a roster member is
// operator-class; everything else is worker (and the evaluator refuses or
// fences accordingly).
func (sv *Service) credential(verb, actor, token string) port.Credential {
	cred := port.Credential{Class: port.Worker, Actor: actor, Token: token}
	if verb == "claim" {
		return cred
	}
	if token == "" && sv.Cfg.IsOperator(actor) {
		cred.Class = port.Operator
	}
	return cred
}

// Init creates the state ref.
func (sv *Service) Init() *Result {
	head, err := sv.Store.Init()
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "init", "head": head})
}

// Create writes a new card in backlog.
type CreateArgs struct {
	Title, Body, Priority, Squad, Parent, Actor string
	Labels, Blocks, BlockedBy                   []string
}

func (sv *Service) Create(a CreateArgs) *Result {
	if a.Priority == "" {
		a.Priority = "P2"
	}
	id := card.NewID()
	c := &card.Card{
		ID: id, Title: a.Title, State: "backlog", Priority: a.Priority,
		Squad: a.Squad, Parent: a.Parent, Labels: a.Labels,
		CreatedAt: sv.now(), Body: a.Body,
	}
	if len(a.Blocks) > 0 {
		c.Links = &card.Links{Blocks: a.Blocks}
	}
	for _, dep := range a.BlockedBy {
		c.BlockedOn = append(c.BlockedOn, "dep:"+dep)
	}
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		content, serr := c.Serialize()
		if serr != nil {
			return nil, serr
		}
		return &stateref.Mutation{
			Message: "create " + id,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event(a.Actor, "create", id, map[string]any{"title": a.Title})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "create", "task": id, "state": "backlog"})
}

// Ready lists claimable work: state ready, unclaimed, claimant not rejected.
func (sv *Service) Ready(actor, squad string) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return errResult(err)
	}
	teams, _ := validate.LoadTeams(sv.Root)
	var out []map[string]any
	for _, c := range cards {
		if c.State != "ready" || c.Claim != nil {
			continue
		}
		if actor != "" && slices.Contains(c.RejectedAuthors, actor) {
			continue
		}
		resolved := validate.ResolveSquad(c.Squad, c.Labels, teams)
		if squad != "" && resolved != squad {
			continue
		}
		out = append(out, map[string]any{
			"task": c.ID, "title": c.Title, "priority": c.Priority,
			"squad": resolved, "created_at": c.CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := out[i]["priority"].(string), out[j]["priority"].(string)
		if pi != pj {
			return pi < pj // P0 < P1 < ...
		}
		return out[i]["created_at"].(string) < out[j]["created_at"].(string)
	})
	return ok(map[string]any{"verb": "ready", "tasks": out})
}

func (sv *Service) Get(id string) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	c, err := sv.loadCard(head, id)
	if err != nil {
		return errResult(err)
	}
	teams, _ := validate.LoadTeams(sv.Root)
	return ok(map[string]any{"verb": "get", "task": c.ID, "state": c.State,
		"squad": validate.ResolveSquad(c.Squad, c.Labels, teams), "card": c})
}

func (sv *Service) List(state string) *Result {
	head, err := sv.Store.Sync()
	if err != nil {
		return errResult(err)
	}
	cards, err := sv.allCards(head)
	if err != nil {
		return errResult(err)
	}
	teams, _ := validate.LoadTeams(sv.Root)
	var out []map[string]any
	for _, c := range cards {
		if state != "" && c.State != state {
			continue
		}
		out = append(out, map[string]any{"task": c.ID, "title": c.Title, "state": c.State, "priority": c.Priority,
			"squad": validate.ResolveSquad(c.Squad, c.Labels, teams)})
	}
	return ok(map[string]any{"verb": "list", "tasks": out})
}

// Claim is the synchronous, push-wins claim (§7.1): evaluate on fresh state
// each attempt; the loser of the push race re-fetches, sees the claim, and
// exits 2 before any work begins.
func (sv *Service) Claim(id, actor, lease string) *Result {
	leaseDur := sv.Cfg.DefaultLease()
	if lease != "" {
		d, err := time.ParseDuration(lease)
		if err != nil {
			return failure(spec.ExitInvalid, "bad_lease", nil)
		}
		leaseDur = d
	}
	token := card.NewToken()
	var expires string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, id)
		if err != nil {
			return nil, err
		}
		out := port.Evaluate(sv.Spec, port.Request{Verb: "claim"}, sv.portCard(c), sv.credential("claim", actor, ""))
		if out.Code != 0 {
			data := map[string]any{}
			if c.Claim != nil {
				data["holder"] = c.Claim.Actor
				data["lease_expires"] = c.Claim.LeaseExpires
				// §7.1: "task now claimed by another → exit 2", a card that
				// left ready under someone's claim is contention, not an
				// invalid transition.
				if out.Code == spec.ExitInvalid {
					return nil, &stateref.Terminal{Code: spec.ExitClaimNotGranted, Name: "claim_contention", Data: data}
				}
			}
			return nil, &stateref.Terminal{Code: out.Code, Name: out.Err, Data: data}
		}
		expires = sv.Now().UTC().Add(leaseDur).Format(time.RFC3339)
		c.State = out.NewState
		c.Claim = &card.Claim{Actor: actor, Token: token, ClaimedAt: sv.now(), LeaseExpires: expires}
		c.UpdatedAt = sv.now()
		content, err := c.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "claim " + id + " by " + actor,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event(actor, "claim", id, map[string]any{"lease_expires": expires})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "claim", "task": id, "state": "in_progress",
		"actor": actor, "claim_token": token, "lease_expires": expires})
}

// TransitionArgs covers the generic transition verb, the release/close
// composites, and every named operator verb: the evaluator decides.
type TransitionArgs struct {
	Verb, ID, To, Actor, Token string
	BlockedOn                  string // entry for block/parking
	Resolution                 string // evidence for accept/reject/close
	NoPR                       bool   // D7 no-PR close: evidence gets the no-pr: exemption marker
}

func (sv *Service) Transition(a TransitionArgs) *Result {
	var newState string
	var transitioned bool
	var cascaded []string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, a.ID)
		if err != nil {
			return nil, err
		}
		cred := sv.credential(a.Verb, a.Actor, a.Token)
		out := port.Evaluate(sv.Spec, port.Request{Verb: a.Verb, To: a.To}, sv.portCard(c), cred)
		if out.Code != 0 {
			return nil, &stateref.Terminal{Code: out.Code, Name: out.Err}
		}
		newState, transitioned = out.NewState, out.Transitioned
		mut := &stateref.Mutation{Message: a.Verb + " " + a.ID + " → " + out.NewState}
		var err2 error
		cascaded, err2 = sv.applyEffects(head, c, out, a, mut)
		if err2 != nil {
			return nil, err2
		}
		return mut, nil
	})
	if err != nil {
		return errResult(err)
	}
	fields := map[string]any{"verb": a.Verb, "task": a.ID, "state": newState, "transitioned": transitioned}
	if len(cascaded) > 0 {
		fields["cascaded"] = cascaded
	}
	return ok(fields)
}

// applyEffects turns the evaluator's spec-declared effects into concrete card
// mutations, handoff stubs, cascade updates, and run-log events: all inside
// the verb's single commit.
func (sv *Service) applyEffects(head string, c *card.Card, out port.Outcome, a TransitionArgs, mut *stateref.Mutation) ([]string, error) {
	now := sv.now()
	priorClaim := c.Claim
	var cascaded []string

	for _, eff := range out.Effects {
		switch eff {
		case "end_claim":
			if out.NewState == "review" && priorClaim != nil {
				c.Author = priorClaim.Actor // implementer of record
			}
			c.Claim = nil
		case "write_handoff":
			mut.Changes = append(mut.Changes, gitx.Change{
				Path:    "handoff/" + c.ID + ".md",
				Content: sv.handoffStub(c, priorClaim, out, a),
			})
		case "record_review":
			outcome := "accepted"
			if a.Verb == "reject" {
				outcome = "rejected"
			}
			evidence := a.Resolution
			if a.NoPR {
				// D7 exemption marker: every validator recognizes no-PR
				// closes by this prefix; the workflow supplies the
				// server-attributed artifact URL as the resolution.
				evidence = "no-pr:" + evidence
			}
			c.Review = &card.Review{Reviewer: a.Actor, ReviewedAt: now, Outcome: outcome, Evidence: evidence}
		case "append_rejected_author":
			if c.Author != "" && !slices.Contains(c.RejectedAuthors, c.Author) {
				c.RejectedAuthors = append(c.RejectedAuthors, c.Author)
			}
		case "add_blocked_on":
			entry := a.BlockedOn
			if entry == "" {
				entry = "manual:" + a.Actor
			}
			if !blockedOnPattern.MatchString(entry) {
				return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "bad_blocked_on_entry"}
			}
			if !slices.Contains(c.BlockedOn, entry) {
				c.BlockedOn = append(c.BlockedOn, entry)
			}
		case "remove_blocked_on_manual":
			c.BlockedOn = slices.DeleteFunc(slices.Clone(c.BlockedOn), func(e string) bool {
				return e == "manual:"+a.Actor
			})
		case "cascade":
			var err error
			cascaded, err = sv.cascade(head, c.ID, mut)
			if err != nil {
				return nil, err
			}
		case "mint_token", "set_lease":
			// handled by Claim, which owns token/lease material
		}
	}

	if out.Transitioned {
		c.State = out.NewState
	}
	c.UpdatedAt = now
	content, err := c.Serialize()
	if err != nil {
		return nil, err
	}
	mut.Changes = append(mut.Changes, gitx.Change{Path: card.Path(c.ID), Content: content})
	data := map[string]any{"to": out.NewState, "transitioned": out.Transitioned}
	if out.Override != "" {
		data["override"] = out.Override
	}
	mut.Events = append(mut.Events, sv.event(a.Actor, a.Verb, c.ID, data))
	return cascaded, nil
}

// cascade removes dep:<closedID> everywhere; a blocked card whose set empties
// auto-unblocks (the dep_cascade auto-path), all in this same commit.
func (sv *Service) cascade(head, closedID string, mut *stateref.Mutation) ([]string, error) {
	cards, err := sv.allCards(head)
	if err != nil {
		return nil, err
	}
	entry := "dep:" + closedID
	var unblocked []string
	for _, other := range cards {
		if other.ID == closedID || !slices.Contains(other.BlockedOn, entry) {
			continue
		}
		other.BlockedOn = slices.DeleteFunc(slices.Clone(other.BlockedOn), func(e string) bool { return e == entry })
		data := map[string]any{"removed": entry}
		if other.State == "blocked" && len(other.BlockedOn) == 0 {
			other.State = "ready"
			unblocked = append(unblocked, other.ID)
			data["auto_path"] = "dep_cascade"
			data["to"] = "ready"
		}
		other.UpdatedAt = sv.now()
		content, err := other.Serialize()
		if err != nil {
			return nil, err
		}
		mut.Changes = append(mut.Changes, gitx.Change{Path: card.Path(other.ID), Content: content})
		mut.Events = append(mut.Events, sv.event("shim", "blocker_resolved", other.ID, data))
	}
	return unblocked, nil
}

// handoffStub renders the write_handoff effect's packet via the shared
// generator (plan os-499c5978, superseding the §7.1 v1 stub). A reap runs
// in the maintenance checkout, not the worker's, so its packet marks the
// workspace anchors unavailable; worker release/park observes its own
// checkout.
func (sv *Service) handoffStub(c *card.Card, prior *card.Claim, out port.Outcome, a TransitionArgs) string {
	reason := a.Verb
	if out.Override != "" {
		reason = out.Override
	}
	var anchors *wsAnchors
	if out.Override != "reap" {
		anchors = sv.collectAnchors()
	}
	return sv.renderPacket(c, reason, prior, anchors, a.BlockedOn)
}

// Comment and AttachEvidence are fenced while the card holds a claim.
// Both mint a stable record id (verbs.json declares comment_id and
// evidence_id as required outputs: plan os-61967950): the id is stamped
// into the appended card-body section so it is resolvable by reading the
// card, and returned in the envelope.
func (sv *Service) Append(kind, id, actor, token, body, ref string) *Result {
	verb := "comment"
	idField := "comment_id"
	prefix := "cm"
	if kind != "comment" {
		verb = "attach-evidence"
		idField = "evidence_id"
		prefix = "ev"
	}
	var recordID string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, id)
		if err != nil {
			return nil, err
		}
		if c.Claim != nil && token != c.Claim.Token {
			return nil, &stateref.Terminal{Code: spec.ExitFenced, Name: "fenced_out"}
		}
		now := sv.now()
		sum := sha256.Sum256([]byte(id + "\x00" + now + "\x00" + actor + "\x00" + body + ref + "\x00" + fmt.Sprint(len(c.Body))))
		recordID = prefix + "-" + hex.EncodeToString(sum[:4])
		if kind == "comment" {
			c.Body += fmt.Sprintf("\n## Comment %s (%s, %s)\n\n%s\n", recordID, actor, now, body)
		} else {
			c.Body += fmt.Sprintf("\n## Evidence %s (%s, %s, %s)\n\n%s\n", recordID, kind, actor, now, ref)
		}
		c.UpdatedAt = now
		content, err := c.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: verb + " " + id,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event(actor, verb, id, map[string]any{"kind": kind, idField: recordID})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": verb, "task": id, idField: recordID})
}

// LeaseRenew extends a live claim (worker verb outside the table; legal only
// in claim-bearing states, per the spec's worker_verbs_outside_table).
func (sv *Service) LeaseRenew(id, actor, token, lease string) *Result {
	leaseDur := sv.Cfg.DefaultLease()
	if lease != "" {
		d, err := time.ParseDuration(lease)
		if err != nil {
			return failure(spec.ExitInvalid, "bad_lease", nil)
		}
		leaseDur = d
	}
	var expires string
	_, err := sv.Store.Mutate(true, func(head string) (*stateref.Mutation, error) {
		c, err := sv.loadCard(head, id)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(sv.Spec.Port.ClaimBearingStates, c.State) || c.Claim == nil {
			return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "invalid_transition"}
		}
		if token != c.Claim.Token {
			return nil, &stateref.Terminal{Code: spec.ExitFenced, Name: "fenced_out"}
		}
		expires = sv.Now().UTC().Add(leaseDur).Format(time.RFC3339)
		c.Claim.LeaseExpires = expires
		c.UpdatedAt = sv.now()
		content, err := c.Serialize()
		if err != nil {
			return nil, err
		}
		return &stateref.Mutation{
			Message: "lease-renew " + id,
			Changes: []gitx.Change{{Path: card.Path(id), Content: content}},
			Events:  []string{sv.event(actor, "lease-renew", id, map[string]any{"lease_expires": expires})},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "lease-renew", "task": id, "lease_expires": expires})
}

// Resume clears the HALT marker (operator only, §7.2).
func (sv *Service) Resume(actor string) *Result {
	if !sv.Cfg.IsOperator(actor) {
		return failure(spec.ExitInvalid, "operator_required", nil)
	}
	_, err := sv.Store.Mutate(false, func(head string) (*stateref.Mutation, error) {
		if halted, _ := sv.Store.Halted(head); !halted {
			return nil, &stateref.Terminal{Code: spec.ExitInvalid, Name: "not_halted"}
		}
		return &stateref.Mutation{
			Message: "state resume by " + actor,
			Changes: []gitx.Change{{Path: "HALT", Delete: true}},
			Events:  []string{sv.event(actor, "state-resume", "", nil)},
		}, nil
	})
	if err != nil {
		return errResult(err)
	}
	return ok(map[string]any{"verb": "state-resume"})
}

func (sv *Service) allCards(head string) ([]*card.Card, error) {
	names, err := sv.Store.ListDir(head, "tasks")
	if err != nil {
		return nil, err
	}
	var out []*card.Card
	for _, n := range names {
		if !strings.HasSuffix(n, ".md") {
			continue
		}
		content, found, err := sv.Store.ReadFile(head, "tasks/"+n)
		if err != nil || !found {
			continue
		}
		c, err := card.Parse(content)
		if err != nil {
			continue // a malformed card is the conformance lint's concern, not a read failure
		}
		out = append(out, c)
	}
	return out, nil
}

func errResult(err error) *Result {
	switch e := err.(type) {
	case *stateref.Terminal:
		return failure(e.Code, e.Name, e.Data)
	case *stateref.IntegrityError:
		return failure(spec.ExitUnavailable, e.Reason, map[string]any{"message": e.Detail})
	case *spec.VersionMismatch:
		return failure(spec.ExitVersionMismatch, "version_mismatch", map[string]any{"message": e.Error()})
	default:
		return failure(spec.ExitUnavailable, "backend_error", map[string]any{"message": err.Error()})
	}
}
