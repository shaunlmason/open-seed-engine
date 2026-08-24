// Package template implements `seed template upgrade` (open-seed R8, plan
// os-23494e11): pull-based template updates for instantiated repos. The
// recorded provenance in .seed/template.lock names where this repo came
// from; the command fetches the upstream base and target with system git,
// three-way merges what changed upstream against what changed locally, and
// stages the result as ONE commit on a new local branch: conflicts as
// standard markers, working tree untouched, no push, no PR. The trust
// direction is fixed by design (§7.1 amendment): consumer-initiated,
// review-gated, never a push from upstream into downstream repos.
//
// Exit map (same shape as `seed upgrade`): 0 ok (including "completed with
// conflicts staged"), 1 refusal (unparsable lock, downgrade without --to,
// dirty working tree, existing result branch), 7 template host
// unreachable, 64 usage (mapped by the CLI).
package template

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/upgrade"
)

type Options struct {
	Root    string // repo root (contains .seed/)
	To      string // explicit target tag; "" = latest release
	Check   bool   // report only; never fetch bases, branch, or commit
	BaseURL string // release-resolution override (HTTPS-or-loopback rule)
	GitURL  string // fetch-URL override (tests, vendored mirrors); "" = https://github.com/<repo>.git
}

type Result struct {
	Current   string   `json:"current"`
	Target    string   `json:"target"`
	UpToDate  bool     `json:"up_to_date"`
	Branch    string   `json:"branch,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	Merged    []string `json:"merged"`
	Conflicts []string `json:"conflicts"`
	Deletions []string `json:"deletions"`
	Notes     []string `json:"notes"`
	NextSteps []string `json:"next_steps"`
}

func refuse(msg string, a ...any) *upgrade.Err {
	return &upgrade.Err{Code: upgrade.ExitRefused, Name: "upgrade_refused", Msg: fmt.Sprintf(msg, a...)}
}
func unreachable(msg string, a ...any) *upgrade.Err {
	return &upgrade.Err{Code: upgrade.ExitUnreachable, Name: "template_host_unreachable", Msg: fmt.Sprintf(msg, a...)}
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// lock is the parsed .seed/template.lock: `repo <owner/name>` and
// `version <tag>` are required exactly once; `commit <sha>` is optional:
// stamped by this command with the upstream commit it merged from, never
// authored at release time (a release cannot record its own SHA).
type lock struct {
	repo    string
	version string
	commit  string
}

func parseLock(content string) (*lock, *upgrade.Err) {
	l := &lock{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, refuse("template.lock line %q is not `key value`", line)
		}
		switch f[0] {
		case "repo":
			if l.repo != "" {
				return nil, refuse("template.lock declares repo twice")
			}
			l.repo = f[1]
		case "version":
			if l.version != "" {
				return nil, refuse("template.lock declares version twice")
			}
			l.version = f[1]
		case "commit":
			if l.commit != "" {
				return nil, refuse("template.lock declares commit twice")
			}
			if !shaRe.MatchString(f[1]) {
				return nil, refuse("template.lock commit %q is not 40 hex chars", f[1])
			}
			l.commit = f[1]
		default:
			return nil, refuse("template.lock has unknown key %q", f[0])
		}
	}
	if l.repo == "" || l.version == "" {
		return nil, refuse("template.lock must declare repo and version — repair it before upgrading (see docs/handbook.md §6)")
	}
	return l, nil
}

// renderLock writes the advanced lock: version moved to the target tag and
// commit stamped with the resolved upstream SHA (the non-self-referential
// base for the NEXT upgrade).
func renderLock(l *lock, target, sha string) string {
	return "# open-seed template provenance — maintained by `seed template upgrade`.\n" +
		"# `version` is set by the maintainer's release checklist (version-then-tag);\n" +
		"# `commit` is the upstream commit the last upgrade merged from, stamped by\n" +
		"# the command itself — never authored at release time.\n" +
		"repo " + l.repo + "\n" +
		"version " + target + "\n" +
		"commit " + sha + "\n"
}

const lockPath = ".seed/template.lock"

// excluded reports paths the merge never touches: the lock itself (the
// command advances it) and the consumer's own work products: upstream
// template history has no business in them. Cards live on the state ref,
// which the merge never reaches.
func excluded(path string) bool {
	if path == lockPath {
		return true
	}
	for _, p := range []string{"plans/", "receipts/", "memory/", "decisions/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// treePaths returns path -> mode for every blob under treeish.
func treePaths(r *gitx.Repo, treeish string) (map[string]string, error) {
	out, err := r.Git("ls-tree", "-r", treeish)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// <mode> SP <type> SP <sha> TAB <path>
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		f := strings.Fields(line[:tab])
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		m[line[tab+1:]] = f[0]
	}
	return m, nil
}

// fetchRef fetches one ref (or raw SHA) from url and returns the commit it
// resolves to. A missing ref is a refusal (the recorded base or named
// target does not exist upstream); a transport failure is exit 7.
func fetchRef(r *gitx.Repo, url, ref string) (string, *upgrade.Err) {
	if _, err := r.Git("fetch", "--no-tags", url, ref); err != nil {
		out := ""
		if re, ok := err.(*gitx.RunError); ok {
			out = re.Output
		}
		if strings.Contains(out, "couldn't find remote ref") {
			return "", refuse("upstream %s has no ref %q — the recorded base or named target does not exist there", url, ref)
		}
		return "", unreachable("git fetch %s %s: %s", url, ref, strings.TrimSpace(out))
	}
	// The fetch stores no local ref (no destination in the refspec) but
	// records what arrived in FETCH_HEAD; ^{commit} peels annotated tags.
	sha, err := r.Git("rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", refuse("fetched %s from %s but cannot resolve it to a commit: %v", ref, url, err)
	}
	return strings.TrimSpace(sha), nil
}

// mergeFile three-way merges with git merge-file -p; returns the merged
// content and whether it conflicted.
func mergeFile(r *gitx.Repo, ours, base, theirs string) (string, bool, error) {
	dir, err := os.MkdirTemp("", "seed-merge-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	paths := map[string]string{"ours": ours, "base": base, "theirs": theirs}
	for name, content := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", false, err
		}
	}
	out, err := r.Git("merge-file", "-p", "-L", "ours (local)", "-L", "base (recorded template)", "-L", "theirs (target template)",
		filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs"))
	if err != nil {
		re, ok := err.(*gitx.RunError)
		if !ok {
			return "", false, err
		}
		// merge-file exits with the number of conflicts (positive):
		// stdout still carries the merged content with markers.
		if ee, isExit := re.Err.(interface{ ExitCode() int }); isExit && ee.ExitCode() > 0 {
			return re.Output, true, nil
		}
		return "", false, err
	}
	return out, false, nil
}

// Run executes `seed template upgrade` (or --check).
func Run(opts Options) (*Result, *upgrade.Err) {
	base, e := upgrade.ResolveBaseURL(opts.BaseURL)
	if e != nil {
		return nil, e
	}
	raw, err := os.ReadFile(filepath.Join(opts.Root, lockPath))
	if err != nil {
		return nil, refuse("cannot read %s: %v", lockPath, err)
	}
	lk, e := parseLock(string(raw))
	if e != nil {
		return nil, e
	}

	target := opts.To
	if target == "" {
		target, e = upgrade.ResolveLatest(base, lk.repo)
		if e != nil {
			return nil, &upgrade.Err{Code: e.Code, Name: strings.Replace(e.Name, "release_host", "template_host", 1), Msg: e.Msg}
		}
	}
	cmp, e := upgrade.CompareTags(target, lk.version)
	if e != nil {
		return nil, e
	}
	res := &Result{Current: lk.version, Target: target,
		Merged: []string{}, Conflicts: []string{}, Deletions: []string{}, Notes: []string{}, NextSteps: []string{}}
	if cmp < 0 && opts.To == "" {
		return nil, refuse("latest template release %s is older than the recorded %s — a silent downgrade is refused (name it with --to for a deliberate rollback)", target, lk.version)
	}
	if cmp < 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("explicit rollback: %s -> %s", lk.version, target))
	}
	if cmp == 0 && lk.commit != "" {
		// Same tag, already stamped: nothing upstream to take.
		res.UpToDate = true
		res.Notes = append(res.Notes, "template.lock already records "+target)
		return res, nil
	}
	if opts.Check {
		res.NextSteps = append(res.NextSteps, "run `scripts/seed template upgrade` to stage the merge on a branch")
		return res, nil
	}

	r := &gitx.Repo{Dir: opts.Root}
	// Refuse a dirty working tree (tracked changes): ours is HEAD's tree,
	// and staging a merge over unrecorded local edits would silently drop
	// them from the comparison.
	status, err := r.Git("status", "--porcelain")
	if err != nil {
		return nil, refuse("git status: %v", err)
	}
	for _, line := range strings.Split(status, "\n") {
		if line != "" && !strings.HasPrefix(line, "??") {
			return nil, refuse("working tree has uncommitted tracked changes — commit or stash them first (the merge compares against HEAD)")
		}
	}
	branch := "template-upgrade/" + target
	if _, exists := r.ResolveRef("refs/heads/" + branch); exists {
		return nil, refuse("branch %s already exists — a previous run staged it; delete or use that branch", branch)
	}

	gitURL := opts.GitURL
	if gitURL == "" {
		gitURL = "https://github.com/" + lk.repo + ".git"
	}
	targetSHA, e := fetchRef(r, gitURL, "refs/tags/"+target)
	if e != nil {
		return nil, e
	}
	var baseSHA string
	if lk.commit != "" {
		baseSHA, e = fetchRef(r, gitURL, lk.commit)
	} else {
		baseSHA, e = fetchRef(r, gitURL, "refs/tags/"+lk.version)
	}
	if e != nil {
		return nil, e
	}

	basePaths, err := treePaths(r, baseSHA)
	if err != nil {
		return nil, refuse("listing base tree: %v", err)
	}
	theirPaths, err := treePaths(r, targetSHA)
	if err != nil {
		return nil, refuse("listing target tree: %v", err)
	}
	union := map[string]bool{}
	for p := range basePaths {
		union[p] = true
	}
	for p := range theirPaths {
		union[p] = true
	}
	paths := make([]string, 0, len(union))
	for p := range union {
		if !excluded(p) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	read := func(treeish, path string) (string, bool, *upgrade.Err) {
		content, found, err := r.CatFile(treeish, path)
		if err != nil {
			return "", false, refuse("reading %s at %s: %v", path, treeish, err)
		}
		return content, found, nil
	}

	var changes []gitx.Change
	for _, p := range paths {
		baseC, baseOK, e := read(baseSHA, p)
		if e != nil {
			return nil, e
		}
		theirC, theirOK, e := read(targetSHA, p)
		if e != nil {
			return nil, e
		}
		ourC, ourOK, e := read("HEAD", p)
		if e != nil {
			return nil, e
		}
		mode := theirPaths[p]
		if mode == "" {
			mode = basePaths[p]
		}
		switch {
		case theirOK == baseOK && theirC == baseC:
			// Upstream unchanged: keep ours, whatever it is.
		case ourOK == theirOK && ourC == theirC:
			// Already identical to the target: nothing to stage.
		case !ourOK && baseOK:
			// Locally deleted, upstream changed it: git's three-way
			// semantics flag this; we keep the local deletion and surface
			// it for the review.
			res.Deletions = append(res.Deletions, p+" (locally deleted; upstream changed it — kept deleted, review the target's version)")
		case ourOK == baseOK && ourC == baseC:
			// Local unchanged: take theirs (including an upstream delete).
			if !theirOK {
				changes = append(changes, gitx.Change{Path: p, Delete: true})
				res.Deletions = append(res.Deletions, p+" (upstream deleted)")
			} else {
				changes = append(changes, gitx.Change{Path: p, Content: theirC, Mode: mode})
				res.Merged = append(res.Merged, p)
			}
		case !theirOK:
			// Upstream deleted, we changed it: keep ours, surface it.
			res.Deletions = append(res.Deletions, p+" (upstream deleted; kept local version — delete it yourself to follow upstream)")
		default:
			merged, conflicted, err := mergeFile(r, ourC, baseC, theirC)
			if err != nil {
				return nil, refuse("merging %s: %v", p, err)
			}
			changes = append(changes, gitx.Change{Path: p, Content: merged, Mode: mode})
			if conflicted {
				res.Conflicts = append(res.Conflicts, p)
			} else {
				res.Merged = append(res.Merged, p)
			}
		}
	}

	// .seed/version bump: the protocol seam moved, the engine pin must
	// move next, via `scripts/seed upgrade` (its own reviewed step).
	baseVer, _, _ := r.CatFile(baseSHA, ".seed/version")
	theirVer, _, _ := r.CatFile(targetSHA, ".seed/version")
	if strings.TrimSpace(baseVer) != strings.TrimSpace(theirVer) {
		res.Notes = append(res.Notes, fmt.Sprintf(".seed/version changes %s -> %s in this target: run `scripts/seed upgrade` after merging (engine upgrade required)",
			strings.TrimSpace(baseVer), strings.TrimSpace(theirVer)))
	}

	changes = append(changes, gitx.Change{Path: lockPath, Content: renderLock(lk, target, targetSHA)})
	msg := fmt.Sprintf("template upgrade: %s -> %s", lk.version, target)
	commit, err := r.CommitTree("HEAD", []string{mustHead(r)}, msg, changes)
	if err != nil {
		return nil, refuse("staging the merge commit: %v", err)
	}
	if _, err := r.Git("update-ref", "refs/heads/"+branch, commit); err != nil {
		return nil, refuse("creating branch %s: %v", branch, err)
	}
	res.Branch = branch
	res.Commit = commit
	if len(res.Conflicts) > 0 {
		res.NextSteps = append(res.NextSteps, fmt.Sprintf("resolve the %d conflict marker file(s) on %s", len(res.Conflicts), branch))
	}
	res.NextSteps = append(res.NextSteps,
		"git checkout "+branch+" && make check",
		"push the branch and open the reviewed PR (control surface: owner review)")
	return res, nil
}

func mustHead(r *gitx.Repo) string {
	sha, _ := r.Git("rev-parse", "HEAD")
	return strings.TrimSpace(sha)
}

// Provenance exposes the shared release coordinates in .seed/template.lock
// (repo and version) to other packages, so the plugin channel names the
// same release as the template channel instead of parsing the lock twice.
// A missing lock surfaces as an os.IsNotExist error for the caller to
// treat as "this repo has no template provenance".
func Provenance(root string) (repo, version string, err error) {
	b, err := os.ReadFile(filepath.Join(root, lockPath))
	if err != nil {
		return "", "", err
	}
	l, rerr := parseLock(string(b))
	if rerr != nil {
		return "", "", rerr
	}
	return l.repo, l.version, nil
}
