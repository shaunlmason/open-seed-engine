// Package upgrade implements `seed upgrade` (open-seed R8, plan
// os-4a347bd1): move the template's engine pin (.seed/engine.lock) against
// tagged releases — resolve the target, fetch and validate its checksums,
// preflight protocol compatibility, and rewrite the lockfile atomically.
// The command NEVER touches git: engine.lock is control surface, and the
// human gate (a reviewed PR) stays exactly where it is; the success output
// walks the operator through it.
//
// Exit map (upgrade is not a port verb and does not borrow the port's
// reserved codes): 0 ok, 1 refusal (incompatible protocol, downgrade
// without --to, malformed or incomplete checksums, damaged lockfile),
// 7 release host unreachable, 64 usage (mapped by the CLI).
package upgrade

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	ExitOK          = 0
	ExitRefused     = 1
	ExitUnreachable = 7
)

// Err carries the exit code and the envelope error name.
type Err struct {
	Code int
	Name string
	Msg  string
}

func (e *Err) Error() string { return e.Name + ": " + e.Msg }

func refuse(msg string, a ...any) *Err {
	return &Err{Code: ExitRefused, Name: "upgrade_refused", Msg: fmt.Sprintf(msg, a...)}
}
func unreachable(msg string, a ...any) *Err {
	return &Err{Code: ExitUnreachable, Name: "release_host_unreachable", Msg: fmt.Sprintf(msg, a...)}
}

type Options struct {
	Root             string // repo root (contains .seed/)
	To               string // explicit target tag; "" = latest
	Check            bool   // report only, never write
	AssumeProtocolOK bool   // proceed when the release predates protocol.txt
	BaseURL          string // override for tests; non-HTTPS refused unless loopback
}

// The six archives the bootstrap shims download — exact filenames including
// extension (scripts/seed uses .tar.gz, scripts/seed.ps1 uses .zip).
var platforms = []struct {
	Key string // lockfile suffix: sha256_<Key>
	Ext string
}{
	{"linux_amd64", "tar.gz"}, {"linux_arm64", "tar.gz"},
	{"darwin_amd64", "tar.gz"}, {"darwin_arm64", "tar.gz"},
	{"windows_amd64", "zip"}, {"windows_arm64", "zip"},
}

var (
	tagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type semver struct{ maj, min, pat int }

func parseTag(tag string) (semver, *Err) {
	m := tagRe.FindStringSubmatch(tag)
	if m == nil {
		return semver{}, refuse("tag %q is not semver (vX.Y.Z)", tag)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return semver{maj, min, pat}, nil
}

func (a semver) less(b semver) bool {
	if a.maj != b.maj {
		return a.maj < b.maj
	}
	if a.min != b.min {
		return a.min < b.min
	}
	return a.pat < b.pat
}

// lockfile is the parsed .seed/engine.lock, preserving every line for the
// byte-identical rewrite of untouched content.
type lockfile struct {
	lines      []string
	versionIdx int
	repo       string
	hashIdx    map[string]int // platform key -> line index
	hasVendor  bool
	Version    string
}

func parseLock(content string) (*lockfile, *Err) {
	lf := &lockfile{lines: strings.Split(content, "\n"), versionIdx: -1, hashIdx: map[string]int{}}
	for i, line := range lf.lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "version "):
			if lf.versionIdx >= 0 {
				return nil, refuse("engine.lock carries more than one version line — repair it before upgrading")
			}
			lf.versionIdx = i
			lf.Version = strings.TrimSpace(strings.TrimPrefix(t, "version "))
		case strings.HasPrefix(t, "repo "):
			if lf.repo != "" {
				return nil, refuse("engine.lock carries more than one repo line — repair it before upgrading")
			}
			lf.repo = strings.TrimSpace(strings.TrimPrefix(t, "repo "))
		case strings.HasPrefix(t, "vendor "):
			lf.hasVendor = true
		case strings.HasPrefix(t, "sha256_"):
			fields := strings.Fields(t)
			key := strings.TrimPrefix(fields[0], "sha256_")
			if _, dup := lf.hashIdx[key]; dup {
				return nil, refuse("engine.lock carries duplicate sha256_%s lines — repair it before upgrading", key)
			}
			lf.hashIdx[key] = i
		}
	}
	if lf.versionIdx < 0 || lf.repo == "" {
		return nil, refuse("engine.lock is missing its version or repo line — repair it before upgrading")
	}
	for _, p := range platforms {
		if _, ok := lf.hashIdx[p.Key]; !ok {
			return nil, refuse("engine.lock is missing sha256_%s — a partial lockfile is refused, never repaired silently", p.Key)
		}
	}
	return lf, nil
}

func (lf *lockfile) hash(key string) string {
	f := strings.Fields(strings.TrimSpace(lf.lines[lf.hashIdx[key]]))
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// Result feeds the JSON envelope.
type Result struct {
	Current    string   `json:"current"`
	Target     string   `json:"target"`
	UpToDate   bool     `json:"up_to_date"`
	Written    bool     `json:"written"`
	ReleaseURL string   `json:"release_url"`
	Notes      []string `json:"notes,omitempty"`
	NextSteps  []string `json:"next_steps,omitempty"`
}

func baseURL(opts Options) (string, *Err) {
	if opts.BaseURL == "" {
		return "https://github.com", nil
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return "", refuse("bad SEED_UPGRADE_BASE_URL: %v", err)
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			return "", refuse("SEED_UPGRADE_BASE_URL must be https (plaintext endpoints may not supply the hashes that rewrite control surface); non-HTTPS is allowed only for loopback test fakes")
		}
	}
	return strings.TrimRight(opts.BaseURL, "/"), nil
}

// noFollow returns the Location of a redirect without following it — Go's
// default client would follow to a 200 and discard the header.
var client = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
}

func fetch(u string) (string, int, *Err) {
	resp, err := client.Get(u)
	if err != nil {
		return "", 0, unreachable("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", 0, unreachable("reading %s: %v", u, err)
	}
	return string(body), resp.StatusCode, nil
}

func resolveLatest(base, repo string) (string, *Err) {
	u := base + "/" + repo + "/releases/latest"
	resp, err := client.Get(u)
	if err != nil {
		return "", unreachable("GET %s: %v", u, err)
	}
	resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", unreachable("%s did not answer with a release redirect (HTTP %d)", u, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return "", unreachable("release redirect Location %q carries no /tag/", loc)
	}
	// Subsequent URLs are constructed from the lockfile repo, never from
	// the redirect's host.
	return strings.TrimSpace(loc[i+len("/tag/"):]), nil
}

// parseChecksums extracts the six platform sums for version ver (no leading
// v), matching exact asset filenames and normalizing digests to lowercase
// (the POSIX shim compares against lowercase sha256sum output).
func parseChecksums(content, ver string) (map[string]string, *Err) {
	sums := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		digest, name := strings.ToLower(f[0]), f[1]
		for _, p := range platforms {
			if name == fmt.Sprintf("seed_%s_%s.%s", ver, p.Key, p.Ext) {
				if _, dup := sums[p.Key]; dup {
					return nil, refuse("checksums.txt lists %s more than once — refusing an ambiguous release", name)
				}
				if !hexRe.MatchString(digest) {
					return nil, refuse("checksum for %s is not 64 hex chars", name)
				}
				sums[p.Key] = digest
			}
		}
	}
	for _, p := range platforms {
		if _, ok := sums[p.Key]; !ok {
			return nil, refuse("checksums.txt is missing seed_%s_%s.%s — the cross-compile matrix is a release invariant, and a partial lockfile would strand that platform (refused)", ver, p.Key, p.Ext)
		}
	}
	return sums, nil
}

// Run executes the upgrade (or --check) and returns the envelope material.
func Run(opts Options) (*Result, *Err) {
	base, e := baseURL(opts)
	if e != nil {
		return nil, e
	}
	lockPath := filepath.Join(opts.Root, ".seed", "engine.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, refuse("cannot read %s: %v", lockPath, err)
	}
	lf, e := parseLock(string(raw))
	if e != nil {
		return nil, e
	}
	cur, e := parseTag(lf.Version)
	if e != nil {
		return nil, refuse("engine.lock version %q is not semver — repair it before upgrading", lf.Version)
	}

	target := opts.To
	if target == "" {
		target, e = resolveLatest(base, lf.repo)
		if e != nil {
			return nil, e
		}
	}
	tgt, e := parseTag(target)
	if e != nil {
		return nil, e
	}
	res := &Result{Current: lf.Version, Target: target,
		ReleaseURL: "https://github.com/" + lf.repo + "/releases/tag/" + target}

	if tgt.less(cur) && opts.To == "" {
		return nil, refuse("latest release %s is older than the pinned %s — a silent downgrade is refused (name it with --to for a deliberate rollback)", target, lf.Version)
	}
	if tgt.less(cur) {
		res.Notes = append(res.Notes, fmt.Sprintf("explicit rollback: %s -> %s", lf.Version, target))
	}

	ver := strings.TrimPrefix(target, "v")
	dl := base + "/" + lf.repo + "/releases/download/" + target

	// Protocol preflight: an incompatible release must be refused BEFORE
	// writing — the alternative is a pin whose every scripts/seed
	// invocation (upgrade included) exits 10.
	proto, code, e := fetch(dl + "/protocol.txt")
	if e != nil {
		return nil, e
	}
	switch {
	case code == 200:
		repoProto, err := os.ReadFile(filepath.Join(opts.Root, ".seed", "version"))
		if err != nil {
			return nil, refuse("cannot read .seed/version: %v", err)
		}
		want := strings.TrimSpace(string(repoProto))
		ok := false
		for _, line := range strings.Split(proto, "\n") {
			if strings.TrimSpace(line) == want {
				ok = true
			}
		}
		if !ok {
			return nil, refuse("release %s supports protocol(s) %q but this checkout's .seed/version is %s — upgrade the template first, or pick a compatible release with --to (rollback stays possible)", target, strings.TrimSpace(proto), want)
		}
	case code == 404 && opts.AssumeProtocolOK:
		res.Notes = append(res.Notes, "release predates protocol.txt; proceeding on --assume-protocol-ok")
	case code == 404:
		return nil, refuse("release %s ships no protocol.txt (it predates the protocol preflight) — re-run with --assume-protocol-ok after checking the release notes: %s", target, res.ReleaseURL)
	default:
		return nil, unreachable("protocol.txt fetch HTTP %d", code)
	}

	sumsRaw, code, e := fetch(dl + "/checksums.txt")
	if e != nil {
		return nil, e
	}
	if code == 404 {
		return nil, refuse("release %s has no checksums.txt — not a release the shim can verify", target)
	}
	if code != 200 {
		return nil, unreachable("checksums.txt fetch HTTP %d", code)
	}
	sums, e := parseChecksums(sumsRaw, ver)
	if e != nil {
		return nil, e
	}

	// up_to_date = version AND all six hashes match the release.
	res.UpToDate = lf.Version == target
	if res.UpToDate {
		for _, p := range platforms {
			if lf.hash(p.Key) != sums[p.Key] {
				res.UpToDate = false
				res.Notes = append(res.Notes, fmt.Sprintf("pinned sha256_%s does not match release %s — the lock is inconsistent with the release", p.Key, target))
			}
		}
	}
	if opts.Check {
		return res, nil
	}
	if res.UpToDate {
		res.Notes = append(res.Notes, "already up to date; nothing written")
		return res, nil
	}

	// Atomic rewrite: only the version and sha256_* lines change; a write
	// failure leaves the original intact (temp file + rename, same dir).
	out := make([]string, len(lf.lines))
	copy(out, lf.lines)
	out[lf.versionIdx] = "version " + target
	keys := make([]string, 0, len(platforms))
	for _, p := range platforms {
		keys = append(keys, p.Key)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out[lf.hashIdx[k]] = "sha256_" + k + " " + sums[k]
	}
	tmp, err := os.CreateTemp(filepath.Dir(lockPath), ".engine.lock.*")
	if err != nil {
		return nil, refuse("cannot stage lockfile write: %v", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(strings.Join(out, "\n")); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, refuse("lockfile write failed: %v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, refuse("lockfile write failed: %v", err)
	}
	if err := os.Rename(tmpName, lockPath); err != nil {
		os.Remove(tmpName)
		return nil, refuse("lockfile rename failed: %v", err)
	}
	res.Written = true
	if lf.hasVendor {
		res.Notes = append(res.Notes, "engine.lock carries a vendor line: the shim will keep dispatching the vendored binary until that line is removed — the pin rewrite alone does not switch binaries")
	}
	res.NextSteps = []string{
		"git diff .seed/engine.lock  # review the pin move",
		"scripts/seed version        # shim fetches + hash-verifies the new engine",
		"read the release notes: " + res.ReleaseURL,
		"optional: gh attestation verify <archive> --repo " + lf.repo + "  # build provenance (design §7.5 stricter mode)",
		"open a reviewed PR with the change — engine.lock is control surface and never lands without owner review",
	}
	return res, nil
}
