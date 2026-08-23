// Package fastcards is the builtin single-machine store (§7.1 builtin-store
// amendment, §8 R4): coordination state in a local SQLite database instead of
// the seed-state ref. Same path-keyed layout (tasks/<id>.md, run-log.jsonl,
// handoff/…) so card parsing, effects, and lint code are reused unchanged;
// "head" is a monotonic transaction id. Every verb runs in one BEGIN
// IMMEDIATE transaction: claims are genuinely atomic, no push-wins
// emulation. The declared variance: state is machine-local (it does not
// travel with clones, forks, or CI), and the state-ref integrity story
// (anchors, push-access trust) is replaced by local-filesystem trust.
package fastcards

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shaunlmason/open-seed-engine/internal/gitx"
	"github.com/shaunlmason/open-seed-engine/internal/stateref"

	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

const (
	// DBName lives under the repository's COMMON git dir, never a literal
	// .git/ path: linked worktrees (the loop creates one per task) have a
	// .git *file*, and resolving per-worktree would fragment coordination.
	DBName   = "seed-fastcards.db"
	haltPath = "HALT"
	runLog   = "run-log.jsonl"
)

type Store struct {
	Path string
	db   *sql.DB
	// tx is the transaction of the in-flight Mutate; reads route through it
	// so build() sees the locked snapshot. The engine is a short-lived
	// single-goroutine CLI, so one slot is the whole story.
	tx *sql.Tx

	MaxAttempts int
	Sleep       func(time.Duration)
}

// DBPath resolves the shared database location through
// `git rev-parse --git-common-dir` so every linked worktree sees one store.
func DBPath(repoDir string) (string, error) {
	r := &gitx.Repo{Dir: repoDir}
	out, err := r.Git("rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("fastcards: not a git repository (%w)", err)
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(repoDir, common)
	}
	return filepath.Join(common, DBName), nil
}

func Open(repoDir string) (*Store, error) {
	path, err := DBPath(repoDir)
	if err != nil {
		return nil, err
	}
	return OpenPath(path)
}

// OpenPath opens the store at an explicit database path (tests, tooling).
func OpenPath(path string) (*Store, error) {
	// busy_timeout makes a lock-holding rival a wait, not an error; the
	// bounded retry in Mutate covers the timeout's expiry.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{Path: path, db: db, MaxAttempts: 6, Sleep: time.Sleep}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) ensureSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS files (path TEXT PRIMARY KEY, content TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS meta  (key  TEXT PRIMARY KEY, value   TEXT NOT NULL);
	`)
	return err
}

// Init creates the schema and the empty run log (idempotent), mirroring the
// state-ref bootstrap. `seed init` calls this.
func (s *Store) Init() (string, error) {
	if err := s.ensureSchema(); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('head', '1')`); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO files(path, content) VALUES (?, '')`, runLog); err != nil {
		return "", err
	}
	return s.headOn(s.db)
}

// querier is *sql.DB or *sql.Tx: reads route through the open transaction
// during a Mutate so build() sees the locked state.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func (s *Store) q() querier {
	if s.tx != nil {
		return s.tx
	}
	return s.db
}

func (s *Store) headOn(q querier) (string, error) {
	var v string
	err := q.QueryRow(`SELECT value FROM meta WHERE key = 'head'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("fastcards store not initialized — run `seed init`")
	}
	return v, err
}

// Sync returns the current head (the monotonic transaction id). There is no
// remote and nothing to fetch: offline is native here.
func (s *Store) Sync() (string, error) {
	if err := s.ensureSchema(); err != nil {
		return "", err
	}
	return s.headOn(s.q())
}

// Halted reports the HALT marker, honoring the same operator resume protocol
// as the state ref (the conformance lint can still write it locally).
func (s *Store) Halted(head string) (bool, string) {
	content, ok, err := s.ReadFile(head, haltPath)
	if err != nil || !ok {
		return false, ""
	}
	return true, strings.TrimSpace(content)
}

// ReadFile reads the current content of path. The head argument is accepted
// for interface parity; SQLite state has exactly one version, and during a
// Mutate reads see the transaction's locked snapshot: strictly fresher than
// any head the caller could name.
func (s *Store) ReadFile(head, path string) (string, bool, error) {
	var content string
	err := s.q().QueryRow(`SELECT content FROM files WHERE path = ?`, path).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// ListDir lists immediate entry names under dir (matching gitx.ListTree).
func (s *Store) ListDir(head, dir string) ([]string, error) {
	prefix := dir
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	rows, err := s.q().Query(`SELECT path FROM files WHERE path LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var names []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(p, prefix)
		name := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name = rest[:i]
		}
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, rows.Err()
}

// ListAll returns every stored path (export surface; head is parity-only).
func (s *Store) ListAll(head string) ([]string, error) {
	rows, err := s.q().Query(`SELECT path FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func isBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		code := se.Code()
		return code == sqlitelib.SQLITE_BUSY || code == sqlitelib.SQLITE_LOCKED
	}
	return false
}

// Mutate runs one verb in one BEGIN IMMEDIATE transaction: take the write
// lock first, then read fresh state, decide, and apply: the contention
// loser blocks on the lock (busy_timeout), re-reads the winner's claim, and
// refuses on its own terms (exit 2 via *stateref.Terminal). SQLITE_BUSY from
// a lock that outlives the timeout is retried with jittered backoff, bounded
// by MaxAttempts, never surfaced raw.
func (s *Store) Mutate(checkHalt bool, build func(head string) (*stateref.Mutation, error)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < s.MaxAttempts; attempt++ {
		newHead, err := s.mutateOnce(checkHalt, build)
		if err == nil {
			return newHead, nil
		}
		if !isBusy(err) {
			return "", err
		}
		lastErr = err
		s.Sleep(backoff(attempt))
	}
	return "", fmt.Errorf("fastcards: database busy after %d attempts: %w", s.MaxAttempts, lastErr)
}

func (s *Store) mutateOnce(checkHalt bool, build func(head string) (*stateref.Mutation, error)) (string, error) {
	if err := s.ensureSchema(); err != nil {
		return "", err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	// database/sql's Begin issues a deferred BEGIN; force the write lock now
	// so the whole verb is serialized (the plan's BEGIN IMMEDIATE semantics).
	if _, err := tx.Exec(`UPDATE meta SET value = value WHERE key = 'head'`); err != nil {
		tx.Rollback()
		return "", err
	}
	s.tx = tx
	defer func() { s.tx = nil }()

	head, err := s.headOn(tx)
	if err != nil {
		tx.Rollback()
		return "", err
	}
	if checkHalt {
		if halted, reason := s.Halted(head); halted {
			tx.Rollback()
			return "", &stateref.IntegrityError{Reason: "halted", Detail: "state carries a HALT marker (" + reason + "); mutating verbs refused until `seed state resume` (§7.2)"}
		}
	}
	mut, err := build(head)
	if err != nil {
		tx.Rollback()
		return "", err
	}
	for _, c := range mut.Changes {
		if c.Delete {
			if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, c.Path); err != nil {
				tx.Rollback()
				return "", err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT INTO files(path, content) VALUES (?, ?)
			ON CONFLICT(path) DO UPDATE SET content = excluded.content`, c.Path, c.Content); err != nil {
			tx.Rollback()
			return "", err
		}
	}
	if len(mut.Events) > 0 {
		log, _, err := s.ReadFile(head, runLog)
		if err != nil {
			tx.Rollback()
			return "", err
		}
		if log != "" && !strings.HasSuffix(log, "\n") {
			log += "\n"
		}
		log += strings.Join(mut.Events, "\n") + "\n"
		if _, err := tx.Exec(`INSERT INTO files(path, content) VALUES (?, ?)
			ON CONFLICT(path) DO UPDATE SET content = excluded.content`, runLog, log); err != nil {
			tx.Rollback()
			return "", err
		}
	}
	n, _ := strconv.ParseInt(head, 10, 64)
	next := strconv.FormatInt(n+1, 10)
	if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = 'head'`, next); err != nil {
		tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return next, nil
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 50 * time.Millisecond
	return base + time.Duration(rand.Int63n(int64(base)))
}
