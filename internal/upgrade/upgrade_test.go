package upgrade

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lockTemplate = `# Engine pin (decision 7.5). Comments survive rewrites byte-for-byte.
# Optional vendor line for air-gapped use.
version %s
repo shaunlmason/open-seed-engine
sha256_darwin_amd64 %s
sha256_darwin_arm64 %s
sha256_linux_amd64 %s
sha256_linux_arm64 %s
sha256_windows_amd64 %s
sha256_windows_arm64 %s
`

func digest(seed string) string { return strings.Repeat(seed, 64)[:64] }

// fakeRelease serves the release surface: /releases/latest redirect,
// per-tag checksums.txt and protocol.txt.
type fakeRelease struct {
	latest    string
	checksums map[string]string // tag -> body
	protocols map[string]string // tag -> body ("" = 404)
}

func (f *fakeRelease) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/shaunlmason/open-seed-engine/releases/tag/"+f.latest)
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/shaunlmason/open-seed-engine/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/shaunlmason/open-seed-engine/releases/download/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		tag, file := parts[0], parts[1]
		var body string
		var ok bool
		switch file {
		case "checksums.txt":
			body, ok = f.checksums[tag]
		case "protocol.txt":
			body, ok = f.protocols[tag]
			ok = ok && body != ""
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	})
	return mux
}

func checksumsFor(ver, seed string) string {
	var b strings.Builder
	for _, p := range platforms {
		fmt.Fprintf(&b, "%s  seed_%s_%s.%s\n", digest(seed), ver, p.Key, p.Ext)
	}
	return b.String()
}

func setup(t *testing.T, f *fakeRelease, lockVersion, hashSeed string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := digest(hashSeed)
	lock := fmt.Sprintf(lockTemplate, lockVersion, d, d, d, d, d, d)
	if err := os.WriteFile(filepath.Join(root, ".seed", "engine.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".seed", "version"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, srv.URL
}

func TestUpgradeRewritesOnlyPinLines(t *testing.T) {
	f := &fakeRelease{latest: "v0.10.0",
		checksums: map[string]string{"v0.10.0": checksumsFor("0.10.0", "b")},
		protocols: map[string]string{"v0.10.0": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	before, _ := os.ReadFile(filepath.Join(root, ".seed", "engine.lock"))

	res, e := Run(Options{Root: root, BaseURL: url})
	if e != nil {
		t.Fatalf("upgrade failed: %v", e)
	}
	if !res.Written || res.Target != "v0.10.0" {
		t.Fatalf("result = %+v", res)
	}
	after, _ := os.ReadFile(filepath.Join(root, ".seed", "engine.lock"))
	for i, line := range strings.Split(string(after), "\n") {
		orig := strings.Split(string(before), "\n")[i]
		switch {
		case strings.HasPrefix(line, "version "):
			if line != "version v0.10.0" {
				t.Fatalf("version line = %q", line)
			}
		case strings.HasPrefix(line, "sha256_"):
			if !strings.HasSuffix(line, digest("b")) {
				t.Fatalf("hash line not rewritten: %q", line)
			}
		default:
			if line != orig {
				t.Fatalf("untouched line changed: %q -> %q", orig, line)
			}
		}
	}
	if len(res.NextSteps) == 0 {
		t.Fatal("success output must carry the reviewed-PR next steps")
	}
}

// Semver-aware ordering: v0.10.0 is NEWER than v0.9.0 (a raw string
// comparison would misorder them and refuse a real upgrade).
func TestSemverMultiDigit(t *testing.T) {
	f := &fakeRelease{latest: "v0.10.0",
		checksums: map[string]string{"v0.10.0": checksumsFor("0.10.0", "b")},
		protocols: map[string]string{"v0.10.0": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	if _, e := Run(Options{Root: root, BaseURL: url, Check: true}); e != nil {
		t.Fatalf("v0.9.0 -> v0.10.0 must be an upgrade, got %v", e)
	}
}

func TestDowngradeRefusedUnlessExplicit(t *testing.T) {
	f := &fakeRelease{latest: "v0.8.0",
		checksums: map[string]string{"v0.8.0": checksumsFor("0.8.0", "c")},
		protocols: map[string]string{"v0.8.0": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused {
		t.Fatalf("silent downgrade must refuse, got %v", e)
	}
	res, e := Run(Options{Root: root, BaseURL: url, To: "v0.8.0"})
	if e != nil || !res.Written {
		t.Fatalf("explicit rollback must proceed: res=%+v err=%v", res, e)
	}
}

func TestProtocolPreflight(t *testing.T) {
	f := &fakeRelease{latest: "v1.0.0",
		checksums: map[string]string{"v1.0.0": checksumsFor("1.0.0", "d")},
		protocols: map[string]string{"v1.0.0": "2\n"}} // supports protocol 2 only
	root, url := setup(t, f, "v0.9.0", "a")
	_, e := Run(Options{Root: root, BaseURL: url})
	if e == nil || e.Code != ExitRefused || !strings.Contains(e.Msg, "protocol") {
		t.Fatalf("incompatible protocol must refuse before writing, got %v", e)
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".seed", "engine.lock"))
	if !strings.Contains(string(raw), "version v0.9.0") {
		t.Fatal("refused upgrade must not have written the lockfile")
	}
}

func TestMissingProtocolAssetNeedsExplicitOK(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.5",
		checksums: map[string]string{"v0.9.5": checksumsFor("0.9.5", "e")},
		protocols: map[string]string{}} // predates protocol.txt
	root, url := setup(t, f, "v0.9.0", "a")
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused {
		t.Fatalf("missing protocol.txt must require --assume-protocol-ok, got %v", e)
	}
	res, e := Run(Options{Root: root, BaseURL: url, AssumeProtocolOK: true})
	if e != nil || !res.Written {
		t.Fatalf("--assume-protocol-ok must proceed: res=%+v err=%v", res, e)
	}
}

func TestChecksumRefusals(t *testing.T) {
	// Missing platform.
	partial := checksumsFor("0.9.5", "e")
	partial = strings.Replace(partial, "seed_0.9.5_windows_arm64.zip", "seed_0.9.5_windows_arm64.tar.gz", 1)
	f := &fakeRelease{latest: "v0.9.5",
		checksums: map[string]string{"v0.9.5": partial},
		protocols: map[string]string{"v0.9.5": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused || !strings.Contains(e.Msg, "windows_arm64") {
		t.Fatalf("missing platform must refuse, got %v", e)
	}
	// Malformed digest.
	bad := strings.Replace(checksumsFor("0.9.5", "e"), digest("e"), "nothex", 1)
	f.checksums["v0.9.5"] = bad
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused {
		t.Fatalf("malformed digest must refuse, got %v", e)
	}
	// Duplicate asset.
	dup := checksumsFor("0.9.5", "e") + checksumsFor("0.9.5", "f")
	f.checksums["v0.9.5"] = dup
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused || !strings.Contains(e.Msg, "more than once") {
		t.Fatalf("duplicate asset must refuse, got %v", e)
	}
}

func TestUppercaseDigestsNormalized(t *testing.T) {
	// Uppercase only the digest column; filenames stay verbatim.
	upper := strings.ReplaceAll(checksumsFor("0.9.5", "e"), digest("e"), strings.ToUpper(digest("e")))
	f := &fakeRelease{latest: "v0.9.5",
		checksums: map[string]string{"v0.9.5": upper},
		protocols: map[string]string{"v0.9.5": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	res, e := Run(Options{Root: root, BaseURL: url})
	if e != nil || !res.Written {
		t.Fatalf("uppercase digests must normalize: %v", e)
	}
	raw, _ := os.ReadFile(filepath.Join(root, ".seed", "engine.lock"))
	if strings.Contains(string(raw), strings.ToUpper(digest("e"))) || !strings.Contains(string(raw), digest("e")) {
		t.Fatal("written digests must be lowercase")
	}
}

func TestCheckComparesHashesToo(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.0",
		checksums: map[string]string{"v0.9.0": checksumsFor("0.9.0", "a")},
		protocols: map[string]string{"v0.9.0": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	res, e := Run(Options{Root: root, BaseURL: url, Check: true})
	if e != nil || !res.UpToDate {
		t.Fatalf("same version + hashes must be up_to_date: %+v %v", res, e)
	}
	// Same version, edited hash: not up to date.
	f.checksums["v0.9.0"] = checksumsFor("0.9.0", "b")
	res, e = Run(Options{Root: root, BaseURL: url, Check: true})
	if e != nil || res.UpToDate {
		t.Fatalf("stale hash at same version must not be up_to_date: %+v %v", res, e)
	}
}

func TestDamagedLockfileRefused(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.5"}
	root, url := setup(t, f, "v0.9.0", "a")
	lock := filepath.Join(root, ".seed", "engine.lock")
	raw, _ := os.ReadFile(lock)
	os.WriteFile(lock, []byte(strings.Replace(string(raw), "sha256_linux_amd64", "sha256_missing", 1)), 0o644)
	if _, e := Run(Options{Root: root, BaseURL: url}); e == nil || e.Code != ExitRefused {
		t.Fatalf("partial lockfile must refuse, got %v", e)
	}
}

func TestNonHTTPSOverrideRefusedExceptLoopback(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.5"}
	root, _ := setup(t, f, "v0.9.0", "a")
	if _, e := Run(Options{Root: root, BaseURL: "http://example.com"}); e == nil || e.Code != ExitRefused || !strings.Contains(e.Msg, "https") {
		t.Fatalf("plaintext non-loopback override must refuse, got %v", e)
	}
}

func TestUnreachableHostExits7(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.5"}
	root, _ := setup(t, f, "v0.9.0", "a")
	// Port 9 is the discard port; nothing listens.
	if _, e := Run(Options{Root: root, BaseURL: "http://127.0.0.1:9", Check: true}); e == nil || e.Code != ExitUnreachable {
		t.Fatalf("unreachable host must exit 7, got %v", e)
	}
}

func TestVendorLineWarns(t *testing.T) {
	f := &fakeRelease{latest: "v0.9.5",
		checksums: map[string]string{"v0.9.5": checksumsFor("0.9.5", "e")},
		protocols: map[string]string{"v0.9.5": "1\n"}}
	root, url := setup(t, f, "v0.9.0", "a")
	lock := filepath.Join(root, ".seed", "engine.lock")
	raw, _ := os.ReadFile(lock)
	os.WriteFile(lock, append(raw, []byte("vendor /opt/seed/seed\n")...), 0o644)
	res, e := Run(Options{Root: root, BaseURL: url})
	if e != nil || !res.Written {
		t.Fatalf("vendor-line upgrade should proceed with a warning: %v", e)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "vendor") {
			found = true
		}
	}
	if !found {
		t.Fatalf("vendor warning missing from notes: %+v", res.Notes)
	}
}
