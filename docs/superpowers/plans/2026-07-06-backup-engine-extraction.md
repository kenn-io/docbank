# Backup Engine Extraction Implementation Plan (kit target)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the pack container and the generalized backup engine into `go.kenn.io/kit` (`kit/pack`, `kit/backup`) so msgvault and future archive tools (docbank) share one implementation — with msgvault behavior and on-disk format unchanged, proven in both compatibility directions.

**Architecture:** Dependency direction is `msgvault → kit ← docbank`; kit never references either app. Order of operations matters: the engine is generalized around a `backup.App` seam **inside msgvault first** (so msgvault-specific SQL never lands in kit, even transiently), then the already-generic engine moves to kit. msgvault keeps `internal/backupapp` (its `App` implementation) plus all app-coupled integration tests; kit gets the engine, its schema-free unit tests, and a generic fake-app round-trip test.

**Repos/branches:** msgvault `backup-engine-extraction` (exists; 3 commits: fixture, golden test, test hardening). kit: create branch `pack-backup-engine` from kit's main. Never commit to main in either repo.

**Tech Stack:** Go, SQLite (mattn/go-sqlite3 — already a kit dependency), testify. No new dependency classes in kit.

**Module wiring during development:** create an untracked `go.work` in `/Users/wesm/code/msgvault` (`go 1.26.3` / `use ., /Users/wesm/code/kit`) and add `go.work*` to `.git/info/exclude` (already excluded: `.superpowers/`). msgvault's `go.mod` keeps its current kit version until the kit branch is pushed; then bump to the pseudo-version of the kit branch head. The msgvault PR merges only after the kit PR.

**Spec:** `~/code/docbank/docs/superpowers/specs/2026-07-06-docbank-design.md` (section "Relationship to msgvault (Phase 0: extraction)"), amended by this plan: the export target is kit, not msgvault `pkg/`.

**Confidentiality:** never mention docbank in msgvault or kit code, comments, commit messages, or PRs. Motivate changes as "reusable by other archive tools."

**Deviation from spec sketch:** The spec sketched `App.Freeze(ctx, dbPath) (FrozenView, error)`. The freeze protocol (WAL checkpoint + pinned read transaction) is generic SQLite and stays in the engine; the app only wraps an already-open `*FrozenSession` with its schema queries via `App.FrozenView(s)`.

## Global Constraints

- msgvault behavior unchanged: every existing test keeps passing (updated only for signatures/paths, never for expectations).
- Manifest wire format frozen: JSON key names, key order, and value encodings byte-identical for msgvault-written manifests. `msgvault_version` stays as the wire key (Go field renames only). Format version constants unchanged; the golden test proves it.
- kit conventions (kit/CLAUDE.md): packages app-neutral — nothing msgvault-specific (schema, table names, layout constants) may appear in kit code or tests; testify with local `require := require.New(t)` helpers for repeated checks; no new `t.Fatal*`/`t.Error*`.
- msgvault test invocations: `make test`, or `go test -tags "n sqlite_vec" ...` (AGENTS.md rule). kit: plain `go test ./...`.
- Every msgvault task ends with `go build ./... && go vet ./...` + tagged tests green + `go fmt ./...` silent; every kit task with `go build ./... && go vet ./... && go test ./...` + fmt silent. Then a commit (msgvault commits must not mention kit-side reasoning that names docbank).
- Known encoding subtlety: manifests are written with `json.MarshalIndent` but snapshot IDs are computed over compact `json.Marshal`. `encoding/json` compacts `json.RawMessage` during `Marshal` and re-indents during `MarshalIndent`; the golden test and the manifest-reload test prove the `RawMessage` transition is transparent.
- Scope decision: kit gains ONLY `pack` and `backup`. Scheduler/config/fileutil stay in msgvault until another tool needs them.

---

### Task 1: Compatibility fixture — DONE (msgvault 15940c1)

Committed pre-extraction repo fixture + `TestRestoreCompatFixture` (pins snapshot IDs). No action.

### Task 2: Golden old-reader test — DONE (msgvault a43b0b3, hardened in 8d41a90)

Vendored frozen reader with strict decode, re-marshal equality, version gate, embedded-ID and recomputed-ID checks. No action.

---

### Task 3: kit/pack + msgvault switches to it

Two commits: kit gains the pack package; msgvault deletes `internal/pack` and imports `go.kenn.io/kit/pack`.

**Files:**
- Create (kit): `pack/` — every file from msgvault `internal/pack/`, package `pack`
- Modify (kit): none else. Check kit `go.mod` — `github.com/klauspost/compress`, `golang.org/x/crypto`, `github.com/oklog/ulid/v2` must become direct requirements if not already (run `go mod tidy`)
- Delete (msgvault): `internal/pack/`
- Modify (msgvault): every file matching `rg -l 'go\.kenn\.io/msgvault/internal/pack' --type go`, comment references to `internal/pack`, plus untracked `go.work`

**Interfaces:**
- Produces: `go.kenn.io/kit/pack` with the identical public API `internal/pack` has today, plus: `MkdirAllSynced(dir string) error` exported (currently unexported `mkdirAllSynced`, `writer.go:240`) with doc comment "MkdirAllSynced creates dir and every missing ancestor, fsyncing each created directory so the entries are durable."; and the `SyncDir` variable's doc comment replaced with the hardened test-seam contract (below).

- [ ] **Step 1 (kit): create branch and copy the package**

```bash
cd /Users/wesm/code/kit && git switch -c pack-backup-engine
mkdir pack && cp /Users/wesm/code/msgvault/internal/pack/* pack/
```

- [ ] **Step 2 (kit): apply the two API changes**

Rename `mkdirAllSynced` → `MkdirAllSynced` everywhere in `pack/` (incl. tests and the recursive self-call) with the doc comment above. Replace `SyncDir`'s doc comment (`writer.go` ~line 225) with:

```go
// SyncDir fsyncs a directory so a rename into it is durable. On Windows it
// is a no-op (see syncdir_windows.go).
//
// SyncDir is a variable ONLY so tests — including this package's consumers'
// tests — can inject fsync failures, which cannot be provoked portably any
// other way. It is a test seam, not a configuration point: production code
// must never reassign it, and it carries no compatibility guarantee beyond
// being callable as a function.
var SyncDir = syncDirPlatform
```

- [ ] **Step 3 (kit): tidy, verify, commit**

Run: `go mod tidy && go build ./... && go vet ./... && go test ./pack/ -count=1 && go fmt ./...`
Expected: green; `go.mod` gains compress/crypto/ulid as direct deps if they weren't.

```bash
git add -A && git commit -m "Add pack: content-addressed encrypted blob container"
```

Body: why kit (shared durability/CAS primitive for archive tools), provenance (extracted from msgvault, format unchanged, `.mvpack` format version 1), the one export (`MkdirAllSynced`) and the SyncDir seam contract. Attribution per dispatch contract.

- [ ] **Step 4 (msgvault): create go.work and switch imports**

```bash
cd /Users/wesm/code/msgvault
printf 'go 1.26.3\n\nuse (\n\t.\n\t/Users/wesm/code/kit\n)\n' > go.work
echo 'go.work*' >> .git/info/exclude
git rm -r internal/pack
rg -l 'go\.kenn\.io/msgvault/internal/pack' --type go \
  | xargs sed -i '' 's|go\.kenn\.io/msgvault/internal/pack|go.kenn.io/kit/pack|g'
```

Also update comment-only references (`internal/config/config.go` mentions "internal/pack" in prose ~lines 161/168; `internal/backup/repo.go:2`) to say `kit/pack`. `internal/backup/packer_test.go:117` says "mkdirAllSynced" in a comment — update to `MkdirAllSynced`.

- [ ] **Step 5 (msgvault): verify and commit**

Run: `go build ./... && go vet ./... && go test -tags "n sqlite_vec" ./internal/backup/ -count=1 && go fmt ./...`
Expected: green (workspace resolves kit locally).

```bash
git add -A && git commit -m "Replace internal/pack with go.kenn.io/kit/pack"
```

Body: the container now lives in kit for reuse; identical API and on-disk format; note go.mod bump to the kit pseudo-version happens before PR (workspace covers local dev). Attribution per dispatch contract.

---

### Task 4: Define the App seam and implement internal/backupapp (msgvault)

Add the interfaces to `internal/backup` and build msgvault's implementation by COPYING the schema-specific code into `internal/backupapp` (deleted from the engine in Task 5). Engine not rewired yet; repo compiles and stays green at the commit boundary.

**Files:**
- Create: `internal/backup/app.go`
- Modify: `internal/backup/frozen.go` (add `Tx()` accessor)
- Create: `internal/backupapp/app.go`
- Create: `internal/backupapp/app_test.go`

**Interfaces:**
- Produces (in `internal/backup`):

```go
// ContentInfo is what the engine needs to know about the application's
// content-addressed files, computed inside the frozen snapshot.
type ContentInfo struct {
	Refs []ContentRef // one per unique hash, first-seen order
	Rows int64        // DB rows referencing content (manifest attachments.rows)
	// NonCanonicalPaths reports any ref recorded at a path other than the
	// canonical "<hash[:2]>/<hash>" layout; such snapshots require a
	// path-aware restore and a higher manifest reader version.
	NonCanonicalPaths bool
}

// FrozenView answers the application-schema questions Create asks, against
// the pinned read transaction of a FrozenSession.
type FrozenView interface {
	ContentInfo(ctx context.Context) (*ContentInfo, error)
	Stats(ctx context.Context) (json.RawMessage, error)
}

// App supplies every application-specific behavior the engine needs. The
// engine treats stats payloads as opaque bytes: it records them at create
// and byte-compares them at restore.
type App interface {
	FrozenView(s *FrozenSession) FrozenView
	DBFileName() string     // e.g. "msgvault.db"
	ContentDirName() string // e.g. "attachments"
	// RestoredContentPaths re-derives hash → relative paths from a restored
	// DB so restore can materialize and verify every referenced file.
	RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error)
	// RestoredStats recomputes stats from a restored DB for the fidelity proof.
	RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error)
	// CheckManifest returns app-level manifest consistency problems (verify).
	CheckManifest(m *Manifest) []string
	ExcludedPaths() []string
	Version() string // recorded as the manifest's app version
}
```

- Produces: `func (s *FrozenSession) Tx() *sql.Tx` in `frozen.go`:

```go
// Tx exposes the pinned read transaction so an App's FrozenView can run
// its schema queries inside the frozen snapshot.
func (s *FrozenSession) Tx() *sql.Tx { return s.tx }
```

- Produces (in `internal/backupapp`): `func New(version string) *App` implementing `backup.App`; `type Stats struct{...}` (moved `ManifestStats`, identical fields/tags/order); `func ParseStats(raw json.RawMessage) (Stats, error)`.

- [ ] **Step 1: Add `internal/backup/app.go` and the `Tx()` accessor** (exact contents above; package `backup`, imports `context`, `database/sql`, `encoding/json`).

- [ ] **Step 2: Write the failing backupapp unit tests**

```go
package backupapp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/backup"
	"go.kenn.io/msgvault/internal/backupapp"
)

// seedDB creates the minimal msgvault-shaped schema (same shape as
// internal/backup's compat fixture) with 2 messages, 2 attachments, 1
// thumbnail, one attachment recorded at a non-canonical namespaced path.
func seedDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	for _, stmt := range []string{
		`CREATE TABLE messages (id INTEGER PRIMARY KEY, sent_at TEXT)`,
		`CREATE TABLE conversations (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE sources (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE account_identities (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE labels (id INTEGER PRIMARY KEY)`,
		`CREATE TABLE attachments (id INTEGER PRIMARY KEY,
			content_hash TEXT, storage_path TEXT,
			thumbnail_hash TEXT, thumbnail_path TEXT, size INTEGER)`,
		`INSERT INTO messages (sent_at) VALUES
			('2024-01-01T00:00:00Z'), ('2024-06-01T00:00:00Z')`,
		`INSERT INTO attachments
			(content_hash, storage_path, thumbnail_hash, thumbnail_path, size) VALUES
			('aabb01', 'aa/aabb01', 'ccdd02', 'cc/ccdd02', 10),
			('eeff03', 'imports/eeff03', NULL, NULL, 20)`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err, "seed: %s", stmt)
	}
	return dbPath
}

func TestFrozenViewContentInfoAndStats(t *testing.T) {
	app := backupapp.New("test")
	session, err := backup.OpenFrozenSession(
		context.Background(), seedDB(t), backup.NoopFreezeCoordinator{})
	require.NoError(t, err)
	defer func() { require.NoError(t, session.Close()) }()
	view := app.FrozenView(session)

	info, err := view.ContentInfo(context.Background())
	require.NoError(t, err)
	assert.Len(t, info.Refs, 3) // 2 content hashes + 1 thumbnail
	assert.Equal(t, int64(2), info.Rows)
	assert.True(t, info.NonCanonicalPaths) // 'imports/eeff03'

	raw, err := view.Stats(context.Background())
	require.NoError(t, err)
	stats, err := backupapp.ParseStats(raw)
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Messages)
	assert.Equal(t, int64(2), stats.AttachmentRows)
	assert.Equal(t, int64(3), stats.AttachmentBlobs)
	assert.Equal(t, "2024-01-01T00:00:00Z", stats.DateRange[0])

	// Stats marshaling must be stable: ParseStats→Marshal reproduces raw.
	again, err := json.Marshal(stats)
	require.NoError(t, err)
	assert.Equal(t, string(raw), string(again))
}

func TestAppConstants(t *testing.T) {
	app := backupapp.New("1.2.3")
	assert.Equal(t, "msgvault.db", app.DBFileName())
	assert.Equal(t, "attachments", app.ContentDirName())
	assert.Equal(t, "1.2.3", app.Version())
	assert.Equal(t,
		[]string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"},
		app.ExcludedPaths())
}
```

- [ ] **Step 3: Run to verify failure** — `go test -tags "n sqlite_vec" ./internal/backupapp/ -v` FAILS (package missing).

- [ ] **Step 4: Implement `internal/backupapp/app.go` by copying engine code**

Package doc: "backupapp implements internal/backup's App interface for msgvault: the schema queries, layout names, and manifest stats that make the generic snapshot engine back up a msgvault archive."

COPY (do not yet delete — the engine still uses its own copies until Task 5) from `internal/backup`:
- `contentBearing`, `thumbBearing`, `attachmentBlobsQuery` consts (`frozen.go:147-166`)
- `computeManifestStats` (`frozen.go:183-209`) returning the moved `Stats` type; keep the `rowQuerier` interface
- `FrozenSession.AttachmentRefs` body (`frozen.go:218-262`) as `frozenView.ContentInfo`, extended with `SELECT COUNT(*) FROM attachments` (→ `Rows`) and the `HasNonCanonicalAttachmentPaths` query (`frozen.go:271-284`), returning `*backup.ContentInfo`
- `ManifestStats` struct (`manifest.go:67-76`) as `Stats`, IDENTICAL field order and json tags
- `loadRestoredAttachmentPaths` body (`restore.go`, the `SELECT content_hash, storage_path FROM attachments ... UNION ...` query + path-safety checks) as `RestoredContentPaths(ctx, db)` — taking the `*sql.DB` the engine opened, not opening its own
- The excluded-paths list (`create.go:43`)

Glue:

```go
type App struct{ version string }

func New(version string) *App { return &App{version: version} }

func (a *App) FrozenView(s *backup.FrozenSession) backup.FrozenView {
	return &frozenView{tx: s.Tx()}
}

type frozenView struct{ tx *sql.Tx }

func (v *frozenView) Stats(ctx context.Context) (json.RawMessage, error) {
	st, err := computeManifestStats(ctx, v.tx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (a *App) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	st, err := computeManifestStats(ctx, db)
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

func (a *App) CheckManifest(m *backup.Manifest) []string {
	// INTERIM SHIM until Task 5 makes m.Stats json.RawMessage: marshal the
	// typed stats to reuse ParseStats. Task 5 removes the shim.
	raw, err := json.Marshal(m.Stats)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	st, err := ParseStats(raw)
	if err != nil {
		return []string{fmt.Sprintf("manifest stats unreadable: %v", err)}
	}
	if st.AttachmentBlobs != m.Attachments.Blobs {
		return []string{fmt.Sprintf(
			"stats.attachment_blobs %d != attachments.blobs %d",
			st.AttachmentBlobs, m.Attachments.Blobs)}
	}
	return nil
}

func ParseStats(raw json.RawMessage) (Stats, error) {
	var st Stats
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("backupapp: parsing manifest stats: %w", err)
	}
	return st, nil
}
```

- [ ] **Step 5: Verify** — `go test -tags "n sqlite_vec" ./internal/backupapp/ ./internal/backup/ -count=1 && go build ./... && go vet ./... && go fmt ./...` all green.

- [ ] **Step 6: Commit** — "Add backup App seam and msgvault backupapp implementation". Body: the seam lets the snapshot engine serve any SQLite-plus-content-files application; msgvault's schema knowledge moves behind it.

---

### Task 5: Cut the engine over to the App seam (msgvault)

Create, Restore, and Verify switch to the hooks in one task (the manifest type change compile-couples them). All engine tests updated to pass `backupapp.New("test")`; the Task 1 fixture test and Task 2 golden test MUST pass with unchanged assertions — they are the format guard.

**Files:**
- Modify: `internal/backup/manifest.go`, `create.go`, `restore.go`, `verify.go`, `frozen.go` (delete moved SQL)
- Modify: every `internal/backup/*_test.go` constructing `CreateOptions`/calling `Create`/`Restore`/`Verify` or reading `m.Stats` fields
- Modify: `internal/backupapp/app.go` (remove the Task 4 `CheckManifest` shim)

**Interfaces (signature changes; everything else unchanged):**

```go
type Manifest struct {
	...
	AppVersion string          `json:"msgvault_version"` // wire key frozen at format v1
	...
	Stats      json.RawMessage `json:"stats"` // app-defined; engine-opaque
	...
}

// CreateOptions: AttachmentsDir renamed to ContentDir; MsgvaultVersion REMOVED.
func Create(ctx context.Context, r *Repo, app App, opts CreateOptions) (*Manifest, error)
func Restore(ctx context.Context, r *Repo, app App, opts RestoreOptions) (*RestoreResult, error)
func Verify(ctx context.Context, r *Repo, app App, opts VerifyOptions) (*VerifyResult, error)
```

- [ ] **Step 1: Manifest types** — `MsgvaultVersion string` → `AppVersion string` keeping tag `json:"msgvault_version"` with comment "// wire key frozen at format v1: renaming it would break every existing repo's snapshot-ID recompute"; `Stats ManifestStats` → `Stats json.RawMessage`; DELETE `ManifestStats` (now `backupapp.Stats`).

- [ ] **Step 2: Rewire Create** (`create.go`) — signature `Create(ctx, r, app, opts)`; `CreateOptions`: `AttachmentsDir`→`ContentDir`, delete `MsgvaultVersion`. Replace `session.Stats/AttachmentRefs/HasNonCanonicalAttachmentPaths` (`create.go:93-104`) with:

```go
view := app.FrozenView(session)
statsRaw, err := view.Stats(ctx)
if err != nil { return nil, err }
info, err := view.ContentInfo(ctx)
if err != nil { return nil, err }
// info.Refs where refs was used; info.NonCanonicalPaths for nonCanonicalPaths
```

`CaptureAttachments(ctx, opts.ContentDir, ...)`. Manifest literal: `AppVersion: app.Version()`, `Rows: info.Rows`, `Stats: statsRaw`, `Excluded: app.ExcludedPaths()`; delete `manifestExcluded` (`create.go:42-43`).

- [ ] **Step 3: Rewire Restore** (`restore.go`) — signature adds `app`. Delete `restoredDBFileName` const; `res.DBPath = filepath.Join(opts.TargetDir, app.DBFileName())`; thread the name into the sidecar-cleanup logic in `prepareRestoreTarget`/overwrite handling. `filepath.Join(opts.TargetDir, "attachments")` → `app.ContentDirName()`. Engine opens the restored DB itself (keep `restoredDBDSN`) and calls `app.RestoredContentPaths(ctx, db)`; delete the engine's `loadRestoredAttachmentPaths` and the `contentBearing`/`thumbBearing` consts from `frozen.go`.

**Reserved-name guard (review finding — do not skip):** `restoreExtrasEntry`'s overlap guard (`restore.go:695-711`) hardcodes `"attachments"`, `"msgvault.db"` and its `-wal`/`-shm` sidecars. Build that reserved list from `app.DBFileName()` (+ `"-wal"`, `"-shm"` suffixes) and `app.ContentDirName()` so a generic app's extras can never overwrite its restored DB or content tree.

Proof (`restore.go:803-810`): replace `computeManifestStats` + struct compare with:

```go
restoredStats, err := app.RestoredStats(ctx, db)
if err != nil { return err }
if !bytes.Equal(compactJSON(restoredStats), compactJSON(m.Stats)) {
	return fmt.Errorf("backup: restored database stats %s do not match manifest stats %s",
		restoredStats, m.Stats)
}
```

```go
// compactJSON normalizes RawMessage formatting: manifests on disk are
// indented, and a RawMessage captured from an indented document keeps that
// indentation, while freshly marshaled stats are compact.
func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
```

Delete `computeManifestStats`, `FrozenSession.Stats`, `FrozenSession.AttachmentRefs`, `FrozenSession.HasNonCanonicalAttachmentPaths`, `attachmentBlobsQuery` from `frozen.go` — the engine keeps only the freeze protocol, `Tx()`, `Close`.

- [ ] **Step 4: Rewire Verify** (`verify.go`) — signature adds `app`; replace the typed stats cross-check (`verify.go:690-693`) by appending each string from `app.CheckManifest(m)` through the same problem-reporting path used at `verify.go:682-688`. Remove the Task 4 shim in backupapp (CheckManifest reads `m.Stats` directly via `ParseStats`).

- [ ] **Step 5: Manifest-reload ID test** — append to `internal/backup/manifest_test.go`:

```go
// TestLoadManifestRecomputesIDWithRawStats guards the json.RawMessage
// transition: manifests are stored indented, RawMessage preserves captured
// formatting, and the snapshot-ID recompute in LoadManifest must still
// match. encoding/json compacts RawMessage during Marshal, which is what
// makes this hold — this test is the proof.
func TestLoadManifestRecomputesIDWithRawStats(t *testing.T) {
	r, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	m := &backup.Manifest{
		FormatVersion:    backup.FormatVersion,
		MinReaderVersion: backup.MinReaderVersion,
		AppVersion:       "raw-stats-test",
		CreatedAt:        "2026-01-02T03:04:05Z",
		DB:               backup.ManifestDB{Engine: "sqlite", PageSize: 4096, PageCount: 1},
		Attachments:      backup.ManifestAttachments{Layout: []string{"loose"}, Recipes: []string{}},
		Excluded:         []string{},
		Stats: json.RawMessage(`{"messages":1,"conversations":0,"sources":0,` +
			`"accounts":0,"attachment_rows":0,"attachment_blobs":0,"labels":0,` +
			`"date_range":["",""]}`),
	}
	id, err := r.WriteManifest(m)
	require.NoError(t, err)
	loaded, err := r.LoadManifest(id) // internal ID recompute IS the assertion
	require.NoError(t, err)
	assert.Equal(t, id, loaded.SnapshotID)
}
```

(Adjust package form to the file's existing package — if it is white-box `package backup`, drop the `backup.` qualifiers. If `WriteManifest`/`LoadManifest` validates fields this literal omits, extend the literal; do not weaken the load-recompute path.)

- [ ] **Step 6: Update all engine tests** — mechanical sweep over `internal/backup/*_test.go`: `backup.Create(ctx, r, opts)` → `backup.Create(ctx, r, backupapp.New("test"), opts)` (importing `internal/backupapp` from `backup_test`-package files is fine; for white-box `package backup` test files this import would cycle — convert those call sites' files to `backup_test` package only if they call Create/Restore/Verify, otherwise leave). `AttachmentsDir:`→`ContentDir:`; delete `MsgvaultVersion:`; `m.Stats.X` reads → `mustParseStats(t, m.Stats).X` helper. Compat/golden tests: update ONLY call signatures/field names; assertions stay byte-for-byte; vendored old-reader structs MUST NOT change.

- [ ] **Step 7: Full gate** — `go build ./... && go vet ./... && go test -tags "n sqlite_vec" ./internal/... -count=1` then `make test && go fmt ./...`. `TestRestoreCompatFixture` and `TestNewManifestReadableByOldReader` passing is the point; if the golden test fails, the wire format broke — fix the encoding, never the vendored structs.

- [ ] **Step 8: Commit** — "Generalize backup engine around the App seam".

---

### Task 6: Move the generic engine to kit/backup

The engine is now app-neutral; move it. Schema-free unit tests move with it; app-coupled tests stay in msgvault.

**Files:**
- Create (kit): `backup/` — the engine sources from msgvault `internal/backup/` (imports rewritten `go.kenn.io/msgvault/internal/pack`→ already `go.kenn.io/kit/pack`; package path only)
- Create (kit): `backup/genericapp_test.go` (the fakeApp round-trip — code below)
- Delete (msgvault): `internal/backup/` engine sources; KEEP in msgvault: `compat_test.go`, `oldreader_compat_test.go`, testdata, and every test file that seeds msgvault schema or uses backupapp — these move to `internal/backupapp/` (adjusting package to `backupapp_test` and imports to `go.kenn.io/kit/backup`)
- Modify (msgvault): all `go.kenn.io/msgvault/internal/backup` imports → `go.kenn.io/kit/backup`

**Test triage rule:** a test file moves to kit iff it references neither msgvault schema (messages/attachments tables) nor backupapp — expected kit set: pagemap, pagehash, index, packer, scan, lock, manifest, repo, extras, fsutil, progress, attachments(unit parts) tests; expected msgvault set: create, restore, verify, e2e, compat, oldreader tests. Where a file mixes both, split it; put the app-coupled parts in `internal/backupapp`.

kit's generic-app test (proves no msgvault residue; includes the extras-overlap case from the review finding):

```go
package backup_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
)

// fakeApp is a minimal non-msgvault application: one table, no content
// files. It proves kit/backup runs without any application schema.
type fakeApp struct{}

func (fakeApp) FrozenView(s *backup.FrozenSession) backup.FrozenView {
	return fakeView{tx: s.Tx()}
}
func (fakeApp) DBFileName() string     { return "fake.db" }
func (fakeApp) ContentDirName() string { return "content" }
func (fakeApp) RestoredContentPaths(context.Context, *sql.DB) (map[string][]string, error) {
	return nil, nil
}
func (fakeApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"notes": n})
}
func (fakeApp) CheckManifest(*backup.Manifest) []string { return nil }
func (fakeApp) ExcludedPaths() []string                 { return nil }
func (fakeApp) Version() string                         { return "fake-1.0" }

type fakeView struct{ tx *sql.Tx }

func (v fakeView) ContentInfo(context.Context) (*backup.ContentInfo, error) {
	return &backup.ContentInfo{}, nil
}
func (v fakeView) Stats(ctx context.Context) (json.RawMessage, error) {
	var n int64
	if err := v.tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&n); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]int64{"notes": n})
}

func TestGenericAppRoundTrip(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fake.db")
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT);
		INSERT INTO notes (body) VALUES ('alpha'), ('beta')`)
	require.NoError(err)
	require.NoError(db.Close())

	r, err := backup.Init(filepath.Join(dir, "repo"))
	require.NoError(err)
	m, err := backup.Create(context.Background(), r, fakeApp{}, backup.CreateOptions{
		DBPath:     dbPath,
		ContentDir: filepath.Join(dir, "content"),
		DataDir:    dir,
	})
	require.NoError(err)
	assert.Equal(t, "fake-1.0", m.AppVersion)
	assert.JSONEq(t, `{"notes":2}`, string(m.Stats))

	res, err := backup.Restore(context.Background(), r, fakeApp{}, backup.RestoreOptions{
		TargetDir: filepath.Join(dir, "restored"),
	})
	require.NoError(err) // Restore's stats proof ran against fakeApp
	assert.Equal(t, "fake.db", filepath.Base(res.DBPath))

	vres, err := backup.Verify(context.Background(), r, fakeApp{}, backup.VerifyOptions{})
	require.NoError(err)
	assert.Empty(t, vres.Problems)
}
```

(Adjust `CreateOptions`/`VerifyOptions`/`VerifyResult` field names to the real ones; `os.MkdirAll` the content dir if capture requires it. Add a second test in the same file: extras restore must refuse an extras entry whose path is `fake.db`, `fake.db-wal`, or `content/x` — construct via `CaptureExtras` on a crafted data dir or a hand-built extras tree, matching how the reserved-name guard is reachable; assert the restore error names the reserved path.)

- [ ] **Step 1 (kit):** copy engine sources + triaged test files into `kit/backup/`, add `genericapp_test.go`; `go mod tidy && go build ./... && go vet ./... && go test ./... && go fmt ./...`; commit "Add backup: incremental snapshot engine for SQLite + content files" (body: provenance, format versioning, App seam summary).
- [ ] **Step 2 (msgvault):** delete engine sources; relocate app-coupled test files to `internal/backupapp/`; rewrite imports to `go.kenn.io/kit/backup`; the compat fixture testdata moves to `internal/backupapp/testdata/compat/` with the path constant updated. Gate: `go build ./... && go vet ./... && go test -tags "n sqlite_vec" ./internal/... -count=1 && make test && go fmt ./...`. Commit "Replace internal/backup with go.kenn.io/kit/backup".

---

### Task 7: Rewire msgvault callers, update docs, close out

**Files:**
- Modify (msgvault): `cmd/msgvault/cmd/backup.go` (Create at `:345`, list stats at `:148`, restore/verify calls), `cmd/msgvault/cmd/backup_progress.go`/`_test.go` (renamed types only), `internal/api/backup_freeze.go` (imports)
- Modify (msgvault): `docs/architecture/backup-format.md`, `CLAUDE.md` if it references internal/backup or internal/pack

- [ ] **Step 1:** CLI rewiring — import `internal/backupapp`; `backup.Create(cmd.Context(), r, backupapp.New(Version), backup.CreateOptions{... ContentDir: cfg.AttachmentsDir() ...})` (drop `MsgvaultVersion:`); pass `backupapp.New(Version)` to Restore/Verify; `printBackupSnapshots`: `m.Stats.Messages` → `backupapp.ParseStats(m.Stats)` with error handling (`fmt.Errorf("snapshot %s: %w", m.SnapshotID, err)`).
- [ ] **Step 2:** Sweep: `rg -n 'MsgvaultVersion|AttachmentsDir' cmd/ internal/ --type go` and `rg -n 'internal/backup|internal/pack' docs/ CLAUDE.md README.md Makefile` — fix every hit. `backup-format.md`: package paths → `kit/pack`/`kit/backup`, plus a paragraph: "Application seam: the engine is application-agnostic; msgvault-specific schema queries, layout names, and manifest stats live in `internal/backupapp`, which implements `backup.App`. The manifest's `msgvault_version` key and `stats` payload are app-defined; their wire encoding is frozen at format v1."
- [ ] **Step 3:** Module wiring for CI: push the kit branch, set msgvault `go.mod` to the kit pseudo-version (`go get go.kenn.io/kit@<branch-head-sha>`), verify a workspace-off build (`GOWORK=off go build ./... && GOWORK=off make test`).
- [ ] **Step 4:** Full gates in both repos; commit msgvault "Rewire backup callers to the App seam"; kit needs no further commit unless sweeps touched it.

---

## Completion checklist (before PRs)

- [ ] msgvault: `TestRestoreCompatFixture` passes (old writer → new reader)
- [ ] msgvault: `TestNewManifestReadableByOldReader` passes (new writer → old reader)
- [ ] kit: `TestGenericAppRoundTrip` + extras-overlap test pass (engine app-free)
- [ ] kit: `rg -n 'msgvault|messages|conversations|attachments? ' kit/backup/*.go` (non-test) shows no application SQL or names — wire-key names like `ManifestAttachments` and the `msgvault_version` JSON tag are the frozen wire format, they stay
- [ ] Both repos: build/vet/tests/lint green; msgvault `GOWORK=off` build green against pushed kit
- [ ] PRs: kit first ("Add pack and backup packages"), then msgvault ("Adopt kit pack/backup engine"); concise changelog-oriented bodies; no docbank mentions anywhere
