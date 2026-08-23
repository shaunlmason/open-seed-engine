// Package gitx drives the system git binary. The engine deliberately shells
// out (design §7.5) so all remote operations reuse the user's existing auth;
// state-ref content is read and written entirely through plumbing: the
// seed-state branch is never checked out (§7.2).
package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Repo struct {
	Dir string
	// Env entries appended to the inherited environment for every git call.
	Env []string
}

// RunError carries git's combined output for callers that classify failures.
type RunError struct {
	Args   []string
	Output string
	Err    error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Output))
}

func (r *Repo) git(stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), r.Env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), &RunError{Args: args, Output: string(out), Err: err}
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (r *Repo) Git(args ...string) (string, error) { return r.git("", args...) }

// gitRaw runs git capturing stdout byte-exact (no newline trimming):
// required for cat-file, where trailing newlines are content.
func (r *Repo) gitRaw(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), r.Env...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &RunError{Args: args, Output: stderr.String(), Err: err}
	}
	return stdout.String(), nil
}

// ResolveRef returns the SHA a ref points at, or ok=false if it doesn't exist.
func (r *Repo) ResolveRef(ref string) (string, bool) {
	out, err := r.Git("rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// CatFile returns the content of path at treeish (byte-exact), ok=false if
// absent.
func (r *Repo) CatFile(treeish, path string) (string, bool, error) {
	out, err := r.gitRaw("cat-file", "-p", treeish+":"+path)
	if err != nil {
		if re, ok := err.(*RunError); ok &&
			(strings.Contains(re.Output, "does not exist") ||
				strings.Contains(re.Output, "exists on disk, but not in") ||
				strings.Contains(re.Output, "Not a valid object name") ||
				strings.Contains(re.Output, "path not in the working tree")) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// ListTree lists the immediate entry names under dir at treeish (empty dir =
// tree root). A missing dir returns an empty list.
func (r *Repo) ListTree(treeish, dir string) ([]string, error) {
	spec := treeish
	if dir != "" {
		spec = treeish + ":" + dir
	}
	out, err := r.Git("ls-tree", "--name-only", spec)
	if err != nil {
		if re, ok := err.(*RunError); ok && strings.Contains(re.Output, "Not a valid object name") {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (r *Repo) HashObject(content string) (string, error) {
	return r.git(content, "hash-object", "-w", "--stdin")
}

// IsAncestor reports whether a is an ancestor of b.
func (r *Repo) IsAncestor(a, b string) (bool, error) {
	_, err := r.Git("merge-base", "--is-ancestor", a, b)
	if err != nil {
		if re, ok := err.(*RunError); ok {
			if ee, ok := re.Err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

type Change struct {
	Path    string
	Content string
	Delete  bool
	Mode    string // index mode (100644, 100755, 120000); "" = 100644
}

// CommitTree builds a commit on top of parentTree (a treeish, or "" for an
// empty base) applying changes via a temporary index, and returns the new
// commit SHA. parents lists the commit parents ("" base = root commit).
func (r *Repo) CommitTree(parentTreeish string, parents []string, message string, changes []Change) (string, error) {
	idx, err := os.CreateTemp("", "seed-index-*")
	if err != nil {
		return "", err
	}
	idxPath := idx.Name()
	idx.Close()
	os.Remove(idxPath) // git wants to create it
	defer os.Remove(idxPath)

	tmp := &Repo{Dir: r.Dir, Env: append(append([]string{}, r.Env...), "GIT_INDEX_FILE="+idxPath)}
	if parentTreeish != "" {
		if _, err := tmp.Git("read-tree", parentTreeish); err != nil {
			return "", err
		}
	} else {
		if _, err := tmp.Git("read-tree", "--empty"); err != nil {
			return "", err
		}
	}
	for _, c := range changes {
		if c.Delete {
			if _, err := tmp.Git("update-index", "--force-remove", "--", c.Path); err != nil {
				return "", err
			}
			continue
		}
		blob, err := tmp.HashObject(c.Content)
		if err != nil {
			return "", err
		}
		mode := c.Mode
		if mode == "" {
			mode = "100644"
		}
		if _, err := tmp.Git("update-index", "--add", "--cacheinfo", mode+","+blob+","+filepath.ToSlash(c.Path)); err != nil {
			return "", err
		}
	}
	tree, err := tmp.Git("write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", message}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	commitEnv := append(append([]string{}, r.Env...),
		"GIT_AUTHOR_NAME=seed", "GIT_AUTHOR_EMAIL=seed@open-seed",
		"GIT_COMMITTER_NAME=seed", "GIT_COMMITTER_EMAIL=seed@open-seed")
	cr := &Repo{Dir: r.Dir, Env: commitEnv}
	return cr.Git(args...)
}

// ErrNonFastForward marks a push or fetch rejected because the remote and
// local histories diverge: the claim-contention and integrity signal.
type ErrNonFastForward struct{ Output string }

func (e *ErrNonFastForward) Error() string { return "non-fast-forward: " + strings.TrimSpace(e.Output) }

func classifyFF(err error) error {
	re, ok := err.(*RunError)
	if !ok {
		return err
	}
	o := re.Output
	if strings.Contains(o, "non-fast-forward") || strings.Contains(o, "[rejected]") ||
		strings.Contains(o, "fetch first") || strings.Contains(o, "stale info") {
		return &ErrNonFastForward{Output: o}
	}
	return err
}

// Push pushes sha to the remote branch without force. Rejections surface as
// ErrNonFastForward.
func (r *Repo) Push(remote, sha, branch string) error {
	if _, err := r.Git("push", remote, sha+":refs/heads/"+branch); err != nil {
		return classifyFF(err)
	}
	return nil
}

// ErrNoRemoteRef marks a fetch of a branch that doesn't exist on the remote.
type ErrNoRemoteRef struct{ Ref string }

func (e *ErrNoRemoteRef) Error() string { return "remote ref not found: " + e.Ref }

// FetchNoForce fetches remote branch into localRef WITHOUT force: a remote
// history rewrite is rejected by git itself and surfaces as
// ErrNonFastForward: the integrity incident of §7.2 (never silently adopt).
func (r *Repo) FetchNoForce(remote, branch, localRef string) error {
	_, err := r.Git("fetch", remote, "refs/heads/"+branch+":"+localRef)
	if err != nil {
		if re, ok := err.(*RunError); ok && strings.Contains(re.Output, "couldn't find remote ref") {
			return &ErrNoRemoteRef{Ref: branch}
		}
		return classifyFF(err)
	}
	return nil
}

// FetchTagsNoForce fetches tags matching pattern without force, so a moved
// tag is also an integrity incident.
func (r *Repo) FetchTagsNoForce(remote, pattern string) error {
	_, err := r.Git("fetch", remote, "refs/tags/"+pattern+":refs/tags/"+pattern)
	if err != nil {
		if re, ok := err.(*RunError); ok && strings.Contains(re.Output, "couldn't find remote ref") {
			return nil
		}
		return classifyFF(err)
	}
	return nil
}

// ListTags returns local tag names matching pattern.
func (r *Repo) ListTags(pattern string) ([]string, error) {
	out, err := r.Git("tag", "--list", pattern)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}
