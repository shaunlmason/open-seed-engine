package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/shaunlmason/open-seed-engine/internal/task"
)

// runMail: `seed mail send|read|ack|nudge|prune` (plan os-499c5978).
// Mail rides the state ref through the same service path as cards.
func runMail(args []string, stdout, stderr *os.File) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "seed: usage: seed mail send|read|ack|nudge|prune ...")
		return exitUsage
	}
	fs := flag.NewFlagSet("mail "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	actor := fs.String("actor", "", "acting identity")
	to := fs.String("to", "", "recipient actor, or _all for broadcast")
	mtype := fs.String("type", "info", "directive|status|request|info|alert|handoff")
	text := fs.String("text", "", "message body (data, not instructions)")
	taskID := fs.String("task", "", "card this concerns")
	thread := fs.String("thread", "", "message id this replies to")
	id := fs.String("id", "", "message id")
	unread := fs.Bool("unread", false, "inbox only (hide acked history)")
	keep := fs.Int("keep", 30, "acked messages to keep per recipient (prune)")
	rest := args[1:]
	// nudge takes the actor positionally: seed mail nudge <actor>
	if args[0] == "nudge" && len(rest) > 0 && rest[0] != "" && rest[0][0] != '-' {
		*actor = rest[0]
		rest = rest[1:]
	}
	if fs.Parse(rest) != nil {
		return exitUsage
	}
	return withService(stdout, stderr, func(sv *task.Service) *task.Result {
		switch args[0] {
		case "send":
			return sv.MailSend(*actor, *to, *mtype, *text, *taskID, *thread)
		case "read":
			return sv.MailRead(*actor, *unread)
		case "ack":
			return sv.MailAck(*actor, *id)
		case "nudge":
			return sv.MailNudge(*actor)
		case "prune":
			return sv.MailPrune(*actor, *keep)
		default:
			return sv.MailRead(*actor, *unread)
		}
	})
}

// runHandoff: `seed handoff generate <task> [--write] [--actor A]`.
func runHandoff(args []string, stdout, stderr *os.File) int {
	if len(args) < 2 || args[0] != "generate" {
		fmt.Fprintln(stderr, "seed: usage: seed handoff generate <task> [--write] [--actor A]")
		return exitUsage
	}
	id := args[1]
	fs := flag.NewFlagSet("handoff generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	write := fs.Bool("write", false, "write handoff/<task>.md to the state ref")
	actor := fs.String("actor", "", "acting identity for the run-log event")
	if fs.Parse(args[2:]) != nil {
		return exitUsage
	}
	return withService(stdout, stderr, func(sv *task.Service) *task.Result {
		return sv.HandoffGenerate(id, *actor, *write)
	})
}
