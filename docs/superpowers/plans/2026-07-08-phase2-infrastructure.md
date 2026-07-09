# Phase 2 Infrastructure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A daemon-centric docbank: `docbank serve` owns the vault behind a huma/OpenAPI HTTP API, every CLI command is an auto-starting HTTP client, `docbank update` self-updates from GitHub releases, and an embedded placeholder page reserves the web frontend's route/config surface.

**Architecture:** One daemon process holds the vault flock exclusively and is the only SQLite/blob accessor. `internal/api` (huma v2 + humago on a stdlib mux) serves resource-shaped routes under `/api/v1`; `internal/client` is a hand-written typed client sharing the api package's request/response types; `kit/daemon` supplies runtime records, probing, listen, and detached spawn; `kit/selfupdate` supplies updates. Spec: `docs/superpowers/specs/2026-07-08-phase2-infrastructure-design.md`.

**Tech Stack:** Go 1.26.3, cobra, huma v2 (`humago` adapter), `go.kenn.io/kit` (`daemon`, `logging`, `selfupdate`), `BurntSushi/toml`, `shirou/gopsutil/v4` (process create-time only), mattn/go-sqlite3 (`fts5` tag, CGO).

## Global Constraints

- Module `go.kenn.io/docbank`; go 1.26.3; all builds/tests use `-tags fts5` with CGO enabled. Run tests as `go test -tags fts5 ./...` (or `make test`).
- New dependencies (look up the current stable release with `go get <mod>@latest` at execution time — do not trust versions from memory; siblings pin huma v2.38.0, toml v1.6.0, gopsutil v4.26.6): `github.com/danielgtaylor/huma/v2`, `github.com/BurntSushi/toml`, `github.com/shirou/gopsutil/v4`.
- Absolute imports only. ≤100-line functions. Lint must pass: `make lint` (golangci-lint with build tag fts5). Zero warnings.
- Service name is exactly `docbank` (runtime records, ping handler, probes). API base path `/api/v1`. Ping path is `kit/daemon.DefaultPingPath` (`/api/ping`).
- Error envelope: `{"title","status","detail","code","errors"}` (RFC 7807 + `code` extension member). Codes are the single wire contract between server and client — defined once in Task 4, mapped back in Task 8.
- Vault stays Unix-only. Non-Unix stubs must keep `internal/...` compiling (Windows CI builds/vets/tests `./internal/...`).
- Commit after every task (repo convention: auto-commit every code-producing turn; never `--amend`). Run `prek run` before committing.
- The daemon-side store/blob/ingest packages are Phase 1 code: do not restructure them beyond what a task explicitly says.
- TDD discipline: for every regression-style test, verify it fails before the code change that makes it pass (git stash the fix if needed).

## File Structure

```
internal/version/version.go            build identity (new)
internal/config/config.go              config.toml (new)
internal/api/errors.go                 error envelope + store-error mapping (new)
internal/api/types.go                  wire DTOs shared with client (new)
internal/api/server.go                 huma wiring, mux, middleware chain (new)
internal/api/middleware.go             auth, timeout, loopback, logging (new)
internal/api/gate.go                   maintenance gate (new)
internal/api/tracker.go                idle/activity tracker (new)
internal/api/web.go + index.html       web placeholder (new)
internal/api/routes_read.go            stat/children/content/search (new)
internal/api/routes_mutate.go          create/move/trash/restore (new)
internal/api/routes_ops.go             ingest/trash-empty/gc/verify (new)
internal/api/age.go                    ParseAge (moved from cmd/trash.go)
internal/api/openapi.go                offline OpenAPI document (new)
internal/client/client.go              typed HTTP client (new)
internal/client/ensure.go              discovery, auto-start, stop (new)
internal/update/update.go              selfupdate wrapper (new)
internal/store/{write,trash,errors}.go revision preconditions (modified)
internal/home/lock.go + lock_stub.go   TryLockExclusive; shared API removed (modified)
cmd/docbank/cmd/serve.go               foreground daemon (new)
cmd/docbank/cmd/serve_lifecycle.go     start/stop/status (new)
cmd/docbank/cmd/openapi.go             OpenAPI dump (new)
cmd/docbank/cmd/update.go              self-update (new)
cmd/docbank/cmd/*.go                   all Phase 1 commands rewritten over client
cmd/docbank/cmd/vault.go               DELETED
.github/workflows/release.yml          release pipeline (new)
docs/...                               zensical updates
```

---

### Task 1: `internal/version` package

**Files:**
- Create: `internal/version/version.go`
- Modify: `cmd/docbank/cmd/root.go`, `Makefile:14-15`

**Interfaces:**
- Produces: `version.Version string` (default `"dev"`), `version.Commit string` (default `"unknown"`) — consumed by root.go now; by api/server.go, client/ensure.go, update, and the runtime record in later tasks.

- [ ] **Step 1: Create the package**

```go
// internal/version/version.go
// Package version carries the build-stamped identity shared by the CLI,
// the daemon runtime record, the OpenAPI document, and self-update.
package version

// Set via -ldflags at build time.
var (
	Version = "dev"
	Commit  = "unknown"
)
```

- [ ] **Step 2: Rewire root.go**

In `cmd/docbank/cmd/root.go`: delete the `var (Version = "dev"; Commit = "unknown")` block, import `go.kenn.io/docbank/internal/version`, and set `Version: fmt.Sprintf("%s (%s)", version.Version, version.Commit)` on `rootCmd`.

- [ ] **Step 3: Update Makefile ldflags**

```makefile
LDFLAGS := -X go.kenn.io/docbank/internal/version.Version=$(VERSION) \
           -X go.kenn.io/docbank/internal/version.Commit=$(COMMIT)
```

- [ ] **Step 4: Verify**

Run: `make build && ./docbank --version && go test -tags fts5 ./... && make lint`
Expected: version prints the git-describe value (not `dev`), tests pass.

- [ ] **Step 5: Commit** — `git add -A && git commit` (subject: `Move build identity into internal/version`).

---

### Task 2: Store revision preconditions

The API's `If-Match` must be checked inside the mutation transaction or it is a TOCTOU no-op. Add an `ifRev` parameter (`-1` = unconditional) to the three single-node mutations, and make `Trash` return the trashed node.

**Files:**
- Modify: `internal/store/errors.go`, `internal/store/write.go` (`Move`, `moveTx`, `MovePath` call site), `internal/store/trash.go` (`Trash`, `Restore`)
- Modify: every caller — `cmd/docbank/cmd/restore.go` (`Restore(ctx, id)` → `Restore(ctx, id, -1)`), and all `internal/store/*_test.go` call sites of `Move`/`Trash`/`Restore` (pass `-1`; `Trash` now returns `(Node, error)` — use `_, err`).
- Test: `internal/store/revision_test.go`

**Interfaces:**
- Produces: `store.ErrStaleRevision`; `Move(ctx, id, newParentID int64, newName string, ifRev int64) (Node, error)`; `Trash(ctx, id, ifRev int64) (Node, error)`; `Restore(ctx, id, ifRev int64) (Node, error)`. Semantics: `ifRev >= 0` and node's current `revision != ifRev` → error wrapping `ErrStaleRevision`, no mutation. `ifRev == -1` skips the check.

- [ ] **Step 1: Write the failing tests**

```go
// internal/store/revision_test.go
package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaleRevisionRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	d, err := s.Mkdir(ctx, s.RootID(), "d")
	require.NoError(t, err)
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)

	_, err = s.Move(ctx, f.ID, d.ID, "f.txt", f.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.Trash(ctx, f.ID, f.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)

	// Nothing mutated: still live at the root under its original name.
	got, err := s.NodeByPath(ctx, "/f.txt")
	require.NoError(t, err)
	assert.Equal(t, f.Revision, got.Revision)

	trashed, err := s.Trash(ctx, f.ID, f.Revision)
	require.NoError(t, err)
	assert.NotNil(t, trashed.TrashedAt)
	_, err = s.Restore(ctx, f.ID, trashed.Revision+1)
	require.ErrorIs(t, err, ErrStaleRevision)
	_, err = s.Restore(ctx, f.ID, trashed.Revision)
	require.NoError(t, err)
}

func TestNegativeRevisionSkipsCheck(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	f, err := s.CreateFile(ctx, s.RootID(), "f.txt", fakeHash("a1"), 1, "text/plain")
	require.NoError(t, err)
	_, err = s.Move(ctx, f.ID, s.RootID(), "g.txt", -1)
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run to verify failure** — `go test -tags fts5 ./internal/store/ -run 'Revision' -v`. Expected: compile error (wrong arity) — that is the failure.

- [ ] **Step 3: Implement**

`internal/store/errors.go` — add:

```go
	// ErrStaleRevision means a mutation's expected revision no longer
	// matches the node (lost-update guard for If-Match).
	ErrStaleRevision = errors.New("revision mismatch")
```

`internal/store/write.go` — thread `ifRev` through `moveTx` and check right after the node fetch:

```go
func (s *Store) Move(ctx context.Context, id, newParentID int64, newName string, ifRev int64) (Node, error) {
	// body unchanged except moveTx call gains ifRev
}

func (s *Store) moveTx(tx *sql.Tx, id, newParentID int64, newName string, ifRev int64) (Node, error) {
	// ... existing fetch of n via nodeByIDTx, then:
	if ifRev >= 0 && n.Revision != ifRev {
		return Node{}, fmt.Errorf("node %d at revision %d, expected %d: %w",
			id, n.Revision, ifRev, ErrStaleRevision)
	}
	// ... rest unchanged
}
```

`MovePath` passes `-1`. `internal/store/trash.go`:

```go
func (s *Store) Trash(ctx context.Context, id, ifRev int64) (Node, error) {
	// after the existing nodeByIDTx fetch and already-trashed check:
	//   if ifRev >= 0 && n.Revision != ifRev { return stale error as above }
	// after trashNodeTx succeeds, re-fetch: trashed, err = nodeByIDTx(tx, id)
	// and return it.
}

func (s *Store) Restore(ctx context.Context, id, ifRev int64) (Node, error) {
	// same check right after the trashed-state validation, before any UPDATE.
}
```

Update all call sites (grep: `rg -n '\.Move\(|\.Trash\(|\.Restore\(' --type go`). `TrashPath` keeps calling `trashNodeTx` directly (no precondition — it resolves in-tx).

- [ ] **Step 4: Run tests** — `go test -tags fts5 ./... && make lint`. Expected: PASS.

- [ ] **Step 5: Commit** — subject: `Add revision preconditions to Move, Trash, and Restore`.

---

### Task 3: `internal/config` — config.toml

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
type Duration time.Duration                       // TOML string, e.g. "30m"; "0" = disabled
func (d Duration) Std() time.Duration
type ServerConfig struct {
	BindAddr    string   `toml:"bind_addr"`       // default "127.0.0.1"
	APIPort     int      `toml:"api_port"`        // default 0 (ephemeral)
	APIKey      string   `toml:"api_key"`         // default "" (keyless local)
	IdleTimeout Duration `toml:"idle_timeout"`    // default 30m; background daemons only
}
type WebConfig struct{ Enabled bool `toml:"enabled"` } // default true
type Config struct {
	Server ServerConfig `toml:"server"`
	Web    WebConfig    `toml:"web"`
}
func Default() Config
func Load(root string) (Config, error)   // reads <root>/config.toml; missing file → Default()
func (c Config) Validate() error         // bind/key security policy
```

- [ ] **Step 1: Add the dependency** — `go get github.com/BurntSushi/toml@latest && go mod tidy`

- [ ] **Step 2: Write the failing tests**

```go
// internal/config/config_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", c.Server.BindAddr)
	assert.Equal(t, 0, c.Server.APIPort)
	assert.Empty(t, c.Server.APIKey)
	assert.Equal(t, 30*time.Minute, c.Server.IdleTimeout.Std())
	assert.True(t, c.Web.Enabled)
}

func TestLoadParsesFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(
		"[server]\nbind_addr = \"127.0.0.1\"\napi_port = 8080\napi_key = \"k\"\n"+
			"idle_timeout = \"0\"\n[web]\nenabled = false\n"), 0o600))
	c, err := Load(dir)
	require.NoError(t, err)
	assert.Equal(t, 8080, c.Server.APIPort)
	assert.Equal(t, "k", c.Server.APIKey)
	assert.Equal(t, time.Duration(0), c.Server.IdleTimeout.Std())
	assert.False(t, c.Web.Enabled)
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[server]\nbindaddr = \"x\"\n"), 0o600))
	_, err := Load(dir)
	require.ErrorContains(t, err, "bindaddr")
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name, bind, key string
		wantErr         bool
	}{
		{"loopback keyless", "127.0.0.1", "", false},
		{"localhost keyless", "localhost", "", false},
		{"private keyless", "192.168.1.5", "", true},
		{"private with key", "192.168.1.5", "k", false},
		{"public with key", "203.0.113.9", "k", true},
		{"wildcard keyless", "0.0.0.0", "", true},
		{"garbage host", "not an ip", "k", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.Server.BindAddr, c.Server.APIKey = tc.bind, tc.key
			err := c.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
```

- [ ] **Step 3: Verify failure** — `go test -tags fts5 ./internal/config/ -v`. Expected: FAIL (package does not exist).

- [ ] **Step 4: Implement**

```go
// internal/config/config.go
// Package config loads the optional $DOCBANK_HOME/config.toml. Every value
// has a default; the file's absence is not an error. There are no per-field
// env or flag overrides — the only environment knob is DOCBANK_HOME.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	kitdaemon "go.kenn.io/kit/daemon"
)

type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	if v < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", string(b))
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// ServerConfig, WebConfig, Config exactly as in the Interfaces block above.

func Default() Config {
	return Config{
		Server: ServerConfig{
			BindAddr:    "127.0.0.1",
			IdleTimeout: Duration(30 * time.Minute),
		},
		Web: WebConfig{Enabled: true},
	}
}

func Load(root string) (Config, error) {
	c := Default()
	path := filepath.Join(root, "config.toml")
	md, err := toml.DecodeFile(path, &c)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("loading %s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		return Config{}, fmt.Errorf("loading %s: unknown key %q (typo?)", path, undec[0].String())
	}
	return c, nil
}

// Validate enforces the bind/key policy: keyless is valid only on loopback,
// and public addresses are never accepted (kit RequireNonPublic).
func (c Config) Validate() error {
	host := c.Server.BindAddr
	if isLoopbackHost(host) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("[server] bind_addr %q: not an IP address or localhost", host)
	}
	if c.Server.APIKey == "" {
		return fmt.Errorf("[server] bind_addr %q requires a non-empty api_key "+
			"(keyless mode is loopback-only)", host)
	}
	if err := kitdaemon.RequireNonPublic(net.JoinHostPort(host, "0")); err != nil {
		return fmt.Errorf("[server] bind_addr %q: %w", host, err)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
```

Note: check `kit/daemon.RequireNonPublic`'s exact input shape first (`rg -n "func RequireNonPublic" $(go env GOMODCACHE)/go.kenn.io/kit@*/daemon/endpoint.go` — it takes an address string; if it rejects a bare-host form, pass `net.JoinHostPort(host, "0")` as shown). Wildcard `0.0.0.0`/`::` must fail Validate keyless AND with a key it binds every interface including public — treat unspecified IPs (`ip.IsUnspecified()`) as an error outright, with a message telling the user to bind a concrete address. Add that branch and a `{"wildcard with key", "0.0.0.0", "k", true}` test case.

- [ ] **Step 5: Run tests** — `go test -tags fts5 ./internal/config/ -v && make lint`. Expected: PASS.

- [ ] **Step 6: Commit** — subject: `Add config.toml loading and bind/key validation`.

---

### Task 4: `internal/api` skeleton — server, errors, auth, health/ping, web placeholder

**Files:**
- Create: `internal/api/errors.go`, `internal/api/types.go`, `internal/api/server.go`, `internal/api/middleware.go`, `internal/api/gate.go`, `internal/api/tracker.go`, `internal/api/web.go`, `internal/api/index.html`
- Test: `internal/api/server_test.go`

**Interfaces:**
- Consumes: `config.Config` (Task 3), `version.Version` (Task 1), `store.Err*` incl. `ErrStaleRevision` (Task 2).
- Produces (later tasks and the client depend on these exact names):

```go
// errors.go
type Error struct {
	Title  string   `json:"title"`
	Status int      `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Code   string   `json:"code,omitempty"`
	Errors []string `json:"errors,omitempty"`
}
func (e *Error) Error() string   // returns Detail
func (e *Error) GetStatus() int
func NewError(status int, code, detail string) *Error
func FromStoreError(err error) error   // typed store error → *Error; unknown → 500 "internal"
// Codes: "not_found","exists","cycle","not_dir","not_file","invalid_name",
//        "not_trashed","is_root","stale_revision","precondition_required",
//        "loopback_only","validation","internal"

// types.go
type Node struct {
	ID         int64  `json:"id"`
	ParentID   *int64 `json:"parent_id,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind" enum:"dir,file"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type,omitempty"`
	Revision   int64  `json:"revision"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
	TrashedAt  string `json:"trashed_at,omitempty"`
	Path       string `json:"path,omitempty"` // set on single-node responses only
}
func fromStoreNode(n store.Node) Node

// server.go
type Deps struct {
	Store         *store.Store
	Blobs         *blob.Store
	Cfg           config.Config
	Logger        *slog.Logger      // nil → slog.Default()
	StartedAt     time.Time
	ShutdownToken string            // "" disables the shutdown route
	Shutdown      func()            // called (async) by the shutdown route
	Tracker       *ActivityTracker  // nil → no idle tracking
}
type Server struct{ /* unexported */ }
func NewServer(d Deps) *Server
func (s *Server) Handler() http.Handler
func (s *Server) API() huma.API

// tracker.go
type ActivityTracker struct{ /* unexported */ }
func NewActivityTracker() *ActivityTracker
func (t *ActivityTracker) Begin()
func (t *ActivityTracker) End()
func (t *ActivityTracker) IdleFor() time.Duration // 0 while requests are in flight

// gate.go — used by Tasks 6/7
type gate struct{ mu sync.RWMutex }
```

- [ ] **Step 1: Add huma** — `go get github.com/danielgtaylor/huma/v2@latest && go mod tidy`

- [ ] **Step 2: Write the failing tests**

```go
// internal/api/server_test.go
package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// newTestServer builds a real store in a temp dir and serves the API over
// httptest (loopback client addr). Later route tasks reuse this helper.
func newTestServer(t *testing.T, mutate func(*api.Deps)) (*httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	d := api.Deps{Store: s, Blobs: blob.New(filepath.Join(dir, "blobs")), Cfg: config.Default()}
	if mutate != nil {
		mutate(&d)
	}
	ts := httptest.NewServer(api.NewServer(d).Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func get(t *testing.T, ts *httptest.Server, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, string(body)
}

func TestHealthAndPing(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/health", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `"status":"ok"`)
	resp, body = get(t, ts, "/api/ping", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, `"docbank"`)
}

func TestAuthRequiredWhenKeySet(t *testing.T) {
	mutate := func(d *api.Deps) { d.Cfg.Server.APIKey = "sekrit" }
	ts, _ := newTestServer(t, mutate)

	resp, body := get(t, ts, "/api/v1/nodes/1", nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, `"code":"unauthorized"`)

	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"X-Api-Key": "sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
	resp, _ = get(t, ts, "/api/v1/nodes/1", map[string]string{"Authorization": "Bearer sekrit"})
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)

	// Exempt surfaces work keyless.
	for _, p := range []string{"/health", "/api/ping", "/docs", "/openapi.json", "/"} {
		resp, _ := get(t, ts, p, nil)
		assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode, p)
	}
}

func TestKeylessAllowsAll(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, _ := get(t, ts, "/api/v1/nodes/1", nil)
	assert.NotEqual(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestWebPlaceholder(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	resp, body := get(t, ts, "/", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, body, "/docs")

	off := func(d *api.Deps) { d.Cfg.Web.Enabled = false }
	ts2, _ := newTestServer(t, off)
	resp, _ = get(t, ts2, "/", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestShutdownRoute(t *testing.T) {
	called := make(chan struct{}, 1)
	mutate := func(d *api.Deps) {
		d.ShutdownToken = "tok"
		d.Shutdown = func() { called <- struct{}{} }
	}
	ts, _ := newTestServer(t, mutate)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode) // no token

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/daemon/shutdown", nil)
	req.Header.Set("X-Docbank-Daemon-Token", "tok")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	<-called
}

func TestValidationErrorEnvelope(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	// Bad path param type → huma validation error → our envelope.
	resp, body := get(t, ts, "/api/v1/nodes/not-a-number", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.True(t, strings.Contains(body, `"code":"validation"`), body)
}
```

Note: `TestValidationErrorEnvelope` and the auth probes hit `/api/v1/nodes/{id}`, which Task 5 implements. For THIS task, create `routes_read.go` containing `nodeOutput`, `nodeWithPath`, and the `getNode` operation exactly as written in Task 5's Step 3 (they are small), and let Task 5 add the rest of the read surface to the same file. This keeps every test green at task end.

- [ ] **Step 3: Verify failure** — `go test -tags fts5 ./internal/api/ -v`. Expected: FAIL (package missing).

- [ ] **Step 4: Implement errors.go**

```go
// internal/api/errors.go
package api

import (
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

// Error is the wire error envelope: RFC 7807 fields plus a machine-readable
// "code" extension member. Code is the contract clients branch on; Detail is
// for humans and may change freely.
type Error struct {
	Title  string   `json:"title"`
	Status int      `json:"status"`
	Detail string   `json:"detail,omitempty"`
	Code   string   `json:"code,omitempty"`
	Errors []string `json:"errors,omitempty"`
}

func (e *Error) Error() string  { return e.Detail }
func (e *Error) GetStatus() int { return e.Status }

// ContentType keeps huma emitting problem+json for our envelope.
func (e *Error) ContentType(ct string) string {
	if ct == "application/json" {
		return "application/problem+json"
	}
	return ct
}

func NewError(status int, code, detail string) *Error {
	return &Error{Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

// installErrorFormatter routes huma's own errors (request validation,
// parsing) through the same envelope. Called once from NewServer.
func installErrorFormatter() {
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		code := "validation"
		if status >= http.StatusInternalServerError {
			code = "internal"
		}
		e := NewError(status, code, msg)
		for _, err := range errs {
			if err != nil {
				e.Errors = append(e.Errors, err.Error())
			}
		}
		return e
	}
}

var storeErrCodes = []struct {
	target error
	status int
	code   string
}{
	{store.ErrNotFound, http.StatusNotFound, "not_found"},
	{store.ErrExists, http.StatusConflict, "exists"},
	{store.ErrCycle, http.StatusConflict, "cycle"},
	{store.ErrStaleRevision, http.StatusPreconditionFailed, "stale_revision"},
	{store.ErrNotDir, http.StatusUnprocessableEntity, "not_dir"},
	{store.ErrNotFile, http.StatusUnprocessableEntity, "not_file"},
	{store.ErrInvalidName, http.StatusUnprocessableEntity, "invalid_name"},
	{store.ErrNotTrashed, http.StatusUnprocessableEntity, "not_trashed"},
	{store.ErrIsRoot, http.StatusUnprocessableEntity, "is_root"},
}

// FromStoreError maps the store's typed errors onto the wire envelope; an
// unrecognized error becomes an opaque 500 (message still surfaced — this
// is a single-user local daemon, not a hardened multi-tenant service).
func FromStoreError(err error) error {
	if err == nil {
		return nil
	}
	for _, m := range storeErrCodes {
		if errors.Is(err, m.target) {
			return NewError(m.status, m.code, err.Error())
		}
	}
	return NewError(http.StatusInternalServerError, "internal", err.Error())
}
```

- [ ] **Step 5: Implement types.go**

```go
// internal/api/types.go
package api

import "go.kenn.io/docbank/internal/store"

// Node as in the Interfaces block, verbatim.

func fromStoreNode(n store.Node) Node {
	out := Node{
		ID: n.ID, ParentID: n.ParentID, Name: n.Name, Kind: n.Kind,
		Size: n.Size, MimeType: n.MimeType, Revision: n.Revision,
		CreatedAt: n.CreatedAt, ModifiedAt: n.ModifiedAt,
	}
	if n.TrashedAt != nil {
		out.TrashedAt = *n.TrashedAt
	}
	return out
}
```

- [ ] **Step 6: Implement gate.go and tracker.go**

```go
// internal/api/gate.go
package api

import "sync"

// gate serializes maintenance against regular mutations. Regular mutating
// handlers hold the read side (they may run concurrently with each other);
// gc --run, trash empty, and verify hold the write side so they observe a
// quiescent vault. Requests queue rather than fail.
type gate struct{ mu sync.RWMutex }

func (g *gate) mutate(fn func() error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return fn()
}

func (g *gate) maintain(fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}
```

```go
// internal/api/tracker.go
package api

import (
	"sync"
	"time"
)

// ActivityTracker feeds the background daemon's idle-shutdown timer.
type ActivityTracker struct {
	mu       sync.Mutex
	inflight int
	lastDone time.Time
}

func NewActivityTracker() *ActivityTracker {
	return &ActivityTracker{lastDone: time.Now()}
}

func (t *ActivityTracker) Begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight++
}

func (t *ActivityTracker) End() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflight--
	t.lastDone = time.Now()
}

// IdleFor returns how long the server has been fully quiet; zero while any
// request is in flight.
func (t *ActivityTracker) IdleFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight > 0 {
		return 0
	}
	return time.Since(t.lastDone)
}
```

- [ ] **Step 7: Implement middleware.go**

```go
// internal/api/middleware.go
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const requestTimeout = 60 * time.Second

// timeout-exempt: long-running maintenance and bulk ingest.
func timeoutExempt(path string) bool {
	switch path {
	case "/api/v1/ingest", "/api/v1/gc", "/api/v1/verify", "/api/v1/trash/empty":
		return true
	}
	return false
}

// auth-exempt: discovery, docs, and the static placeholder carry no vault
// data. Everything else under /api requires the key when one is set.
func authExempt(path string) bool {
	switch path {
	case "/", "/health", kitPingPath:
		return true
	}
	return strings.HasPrefix(path, "/docs") ||
		strings.HasPrefix(path, "/openapi") ||
		strings.HasPrefix(path, "/schemas")
}

func writeError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(e)
}

func authMiddleware(next http.Handler, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			writeError(w, NewError(http.StatusUnauthorized, "unauthorized", "missing or invalid API key"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// loopbackMiddleware fences endpoints that grant local-filesystem
// capability (POST /api/v1/ingest) to loopback peers, regardless of bind
// address or key. See the spec's ingest addendum.
func loopbackMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ingest" && !isLoopbackRemote(r.RemoteAddr) {
			writeError(w, NewError(http.StatusForbidden, "loopback_only",
				"ingest by server-side path is loopback-only; remote clients need multipart upload (planned)"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func timeoutMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if timeoutExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func trackMiddleware(next http.Handler, t *ActivityTracker) http.Handler {
	if t == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Begin()
		defer t.End()
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "duration", time.Since(start))
	})
}

func recoverMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeError(w, NewError(http.StatusInternalServerError, "internal", "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

- [ ] **Step 8: Implement server.go, web.go, index.html**

```go
// internal/api/server.go
package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/version"
)

const kitPingPath = kitdaemon.DefaultPingPath

// Deps exactly as in the Interfaces block.

type Server struct {
	deps    Deps
	handler http.Handler
	api     huma.API
}

// NewServer wires all routes and middleware onto a fresh mux. The handler
// is safe to mount under httptest; nothing here binds a socket.
func NewServer(d Deps) *Server {
	installErrorFormatter()
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.StartedAt.IsZero() {
		d.StartedAt = time.Now()
	}

	mux := http.NewServeMux()
	humaAPI := humago.New(mux, huma.DefaultConfig("docbank", version.Version))
	s := &Server{deps: d, api: humaAPI}
	g := &gate{}

	registerReadRoutes(humaAPI, d)          // Task 5 (stat-by-id lands in this task)
	registerMutateRoutes(humaAPI, d, g)     // Task 6
	registerOpsRoutes(humaAPI, d, g)        // Task 7
	s.registerHealth(mux)
	mux.Handle("GET "+kitPingPath, kitdaemon.NewPingHandler(kitdaemon.PingHandlerOptions{
		Service: "docbank", Version: version.Version, PID: os.Getpid(),
	}))
	s.registerShutdown(mux)
	registerWeb(mux, d.Cfg.Web.Enabled)

	h := http.Handler(mux)
	h = authMiddleware(h, d.Cfg.Server.APIKey)
	h = loopbackMiddleware(h)
	h = timeoutMiddleware(h)
	h = recoverMiddleware(h, d.Logger)
	h = logMiddleware(h, d.Logger)
	h = trackMiddleware(h, d.Tracker)
	s.handler = h
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }
func (s *Server) API() huma.API         { return s.api }

func (s *Server) registerHealth(mux *http.ServeMux) {
	type health struct {
		Status        string `json:"status"`
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, health{
			Status: "ok", Version: version.Version,
			UptimeSeconds: int64(time.Since(s.deps.StartedAt).Seconds()),
		})
	})
}

// registerShutdown adds the hidden token-gated endpoint serve stop uses.
// Not in the OpenAPI document: it is lifecycle plumbing, not agent surface.
func (s *Server) registerShutdown(mux *http.ServeMux) {
	if s.deps.ShutdownToken == "" {
		return
	}
	mux.HandleFunc("POST /api/daemon/shutdown", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Docbank-Daemon-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.deps.ShutdownToken)) != 1 {
			writeError(w, NewError(http.StatusUnauthorized, "unauthorized", "bad shutdown token"))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if s.deps.Shutdown != nil {
			go s.deps.Shutdown()
		}
	})
}
```

Add a tiny `writeJSON(w http.ResponseWriter, status int, v any)` helper in server.go (set Content-Type application/json, WriteHeader, json.NewEncoder(w).Encode). For this task, create `routes_read.go` containing ONLY `registerReadRoutes` with the `GET /api/v1/nodes/{id}` operation (code shown in Task 5, `getNode` block — implement it now exactly as written there), and create `routes_mutate.go` / `routes_ops.go` with empty registration functions plus a comment saying which task fills them:

```go
// internal/api/routes_mutate.go
package api

import "github.com/danielgtaylor/huma/v2"

// Filled in by the mutation endpoints task.
func registerMutateRoutes(_ huma.API, _ Deps, _ *gate) {}
```

```go
// internal/api/web.go
package api

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// registerWeb serves the placeholder frontend at the root. The real
// frontend later replaces the embedded assets; the route layout ("/" = UI,
// /api/v1 = API, /docs = API reference) is fixed now.
func registerWeb(mux *http.ServeMux, enabled bool) {
	if !enabled {
		return
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
}
```

```html
<!-- internal/api/index.html -->
<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>docbank</title></head>
<body>
<h1>docbank</h1>
<p>This vault's web interface is not built yet.</p>
<ul>
  <li><a href="/docs">API reference</a></li>
  <li><a href="/openapi.json">OpenAPI document</a></li>
</ul>
</body>
</html>
```

- [ ] **Step 9: Run tests** — `go test -tags fts5 ./internal/api/ -v && go test -tags fts5 ./... && make lint`. Expected: PASS. If huma's default OpenAPI paths differ from `/openapi.json` (check with a quick `curl` against a test server or huma's `DefaultConfig` source in the module cache), adjust the exempt-prefix test/middleware — the exemption is prefix `/openapi`, so both `.json` and `.yaml` are covered.

- [ ] **Step 10: Commit** — subject: `Add HTTP API skeleton: server, error envelope, auth, web placeholder`.

---

### Task 5: Read endpoints — stat, path resolve, children, content, search

**Files:**
- Create/Modify: `internal/api/routes_read.go`
- Test: `internal/api/routes_read_test.go`

**Interfaces:**
- Produces routes: `GET /api/v1/nodes/{id}`, `GET /api/v1/path?path=`, `GET /api/v1/nodes/{id}/children?limit=&offset=`, `GET /api/v1/nodes/{id}/content`, `GET /api/v1/search?q=&limit=`.
- Produces types (client consumes): `SearchHit{Node Node `json:"node"`; Path string `json:"path"`}`, `ChildrenPage{Items []Node `json:"items"`; Total int `json:"total"`; Limit int `json:"limit"`; Offset int `json:"offset"`}`.
- Single-node responses set `Path` in the body and an `ETag: "<revision>"` header.

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/routes_read_test.go
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestStatByIDAndPath(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	d, err := s.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)

	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", d.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)
	assert.Equal(t, "/docs", n.Path)
	assert.Equal(t, fmt.Sprintf("%q", fmt.Sprint(d.Revision)), resp.Header.Get("ETag"))

	resp, body = get(t, ts, "/api/v1/path?path=%2Fdocs", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, d.ID, n.ID)

	// Root stats fine; relative and missing paths are rejected.
	resp, _ = get(t, ts, "/api/v1/path?path=%2F", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp, body = get(t, ts, "/api/v1/path?path=docs", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)
	resp, body = get(t, ts, "/api/v1/path?path=%2Fnope", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_found"`)
}

func TestChildrenPagination(t *testing.T) {
	ts, s := newTestServer(t, nil)
	ctx := t.Context()
	for i := range 5 {
		_, err := s.Mkdir(ctx, s.RootID(), fmt.Sprintf("d%d", i))
		require.NoError(t, err)
	}
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/children?limit=2&offset=4", s.RootID()), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var page struct {
		Items []api.Node `json:"items"`
		Total int        `json:"total"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &page))
	assert.Equal(t, 5, page.Total)
	assert.Len(t, page.Items, 1) // offset 4 of 5
}

func TestContentStreamsBlob(t *testing.T) {
	ts, s := newTestServer(t, nil)
	// Write a real blob through the test server's blob dir, then link it.
	n := createFileWithContent(t, ts, s, "/hello.txt", "hello world")
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", n.ID), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "hello world", body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
}

func TestContentOnDirIs422(t *testing.T) {
	ts, s := newTestServer(t, nil)
	d, err := s.Mkdir(t.Context(), s.RootID(), "d")
	require.NoError(t, err)
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d/content", d.ID), nil)
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"not_file"`)
}

func TestSearch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	_, err := s.CreateFile(t.Context(), s.RootID(), "insurance-2024.pdf", testHash("x"), 3, "application/pdf")
	require.NoError(t, err)
	resp, body := get(t, ts, "/api/v1/search?q=insurance", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, strings.Contains(body, "insurance-2024.pdf"), body)
}
```

Add to `server_test.go` the shared fixtures: `testHash(seed string) string` (sha256 hex of the seed — the store only validates shape) and `createFileWithContent(t, ts, s, path, content)` which writes `content` through the test's `blob.Store` (`blobs.Write(strings.NewReader(content))` returns the real hash) and then `s.CreateFile(ctx, s.RootID(), name, hash, size, "text/plain")`. The blob store instance must be shared between fixture and server — restructure `newTestServer` to also return its `*blob.Store`.

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/api/ -run 'Stat|Children|Content|Search' -v`. Expected: FAIL (404s — routes unregistered).

- [ ] **Step 3: Implement routes_read.go**

```go
// internal/api/routes_read.go
package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

type nodeOutput struct {
	ETag string `header:"ETag"`
	Body Node
}

// nodeWithPath loads the node's display path and builds the single-node
// response. Every single-node endpoint returns this shape.
func nodeWithPath(ctx context.Context, d Deps, id int64) (*nodeOutput, error) {
	n, err := d.Store.NodeByID(ctx, id)
	if err != nil {
		return nil, FromStoreError(err)
	}
	p, err := d.Store.Path(ctx, id)
	if err != nil {
		return nil, FromStoreError(err)
	}
	body := fromStoreNode(n)
	body.Path = p
	return &nodeOutput{ETag: fmt.Sprintf("%q", fmt.Sprint(n.Revision)), Body: body}, nil
}

func registerReadRoutes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "getNode", Method: http.MethodGet, Path: "/api/v1/nodes/{id}",
		Summary: "Stat a node by id (live or trashed)",
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*nodeOutput, error) {
		return nodeWithPath(ctx, d, in.ID)
	})

	huma.Register(api, huma.Operation{
		OperationID: "resolvePath", Method: http.MethodGet, Path: "/api/v1/path",
		Summary: "Resolve an absolute virtual path to its node",
		Description: "path is a query parameter (one well-defined encoding; " +
			"catch-all URL segments are ambiguous for encoded slashes). " +
			"Must start with '/'; '/' resolves the root.",
	}, func(ctx context.Context, in *struct {
		Path string `query:"path" required:"true" example:"/inbox/doc.pdf"`
	}) (*nodeOutput, error) {
		if !strings.HasPrefix(in.Path, "/") {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				fmt.Sprintf("path %q must be absolute (start with /)", in.Path))
		}
		n, err := d.Store.NodeByPath(ctx, in.Path)
		if err != nil {
			return nil, FromStoreError(err)
		}
		return nodeWithPath(ctx, d, n.ID)
	})

	type childrenPage struct {
		Body struct {
			Items  []Node `json:"items"`
			Total  int    `json:"total"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "listChildren", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/children",
		Summary: "List a directory's live children (dirs first, name-sorted), paginated",
	}, func(ctx context.Context, in *struct {
		ID     int64 `path:"id"`
		Limit  int   `query:"limit" default:"500" minimum:"1" maximum:"5000"`
		Offset int   `query:"offset" default:"0" minimum:"0"`
	}) (*childrenPage, error) {
		kids, err := d.Store.Children(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &childrenPage{}
		out.Body.Total, out.Body.Limit, out.Body.Offset = len(kids), in.Limit, in.Offset
		out.Body.Items = []Node{}
		for i := in.Offset; i < len(kids) && i < in.Offset+in.Limit; i++ {
			out.Body.Items = append(out.Body.Items, fromStoreNode(kids[i]))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getNodeContent", Method: http.MethodGet, Path: "/api/v1/nodes/{id}/content",
		Summary: "Stream a file's bytes",
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*huma.StreamResponse, error) {
		n, err := d.Store.NodeByID(ctx, in.ID)
		if err != nil {
			return nil, FromStoreError(err)
		}
		if n.IsDir() {
			return nil, NewError(http.StatusUnprocessableEntity, "not_file",
				fmt.Sprintf("node %d is a directory", n.ID))
		}
		f, err := d.Blobs.Open(n.BlobHash)
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal",
				fmt.Sprintf("opening blob %s: %v (run docbank verify)", n.BlobHash, err))
		}
		ct := n.MimeType
		if ct == "" {
			ct = "application/octet-stream"
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = f.Close() }()
			hctx.SetHeader("Content-Type", ct)
			hctx.SetHeader("Content-Length", fmt.Sprint(n.Size))
			_, _ = io.Copy(hctx.BodyWriter(), f)
		}}, nil
	})

	type searchOutput struct {
		Body struct {
			Hits []SearchHit `json:"hits"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "search", Method: http.MethodGet, Path: "/api/v1/search",
		Summary: "Full-text search over node names, best rank first",
	}, func(ctx context.Context, in *struct {
		Q     string `query:"q" required:"true"`
		Limit int    `query:"limit" default:"50" minimum:"1" maximum:"1000"`
	}) (*searchOutput, error) {
		hits, err := d.Store.Search(ctx, in.Q, in.Limit)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &searchOutput{}
		out.Body.Hits = []SearchHit{}
		for _, h := range hits {
			out.Body.Hits = append(out.Body.Hits, SearchHit{Node: fromStoreNode(h.Node), Path: h.Path})
		}
		return out, nil
	})
}
```

Add `SearchHit` to `types.go`:

```go
// SearchHit pairs a matched node with its display path.
type SearchHit struct {
	Node Node   `json:"node"`
	Path string `json:"path"`
}
```

- [ ] **Step 4: Run tests** — `go test -tags fts5 ./internal/api/ -v && make lint`. Expected: PASS.

- [ ] **Step 5: Commit** — subject: `Add read endpoints: stat, path resolve, children, content, search`.

---

### Task 6: Mutation endpoints — create dir, move, trash, restore

**Files:**
- Modify: `internal/api/routes_mutate.go`
- Test: `internal/api/routes_mutate_test.go`

**Interfaces:**
- Produces routes: `POST /api/v1/nodes` (dirs only), `PATCH /api/v1/nodes/{id}`, `POST /api/v1/nodes/{id}/trash`, `POST /api/v1/nodes/{id}/restore`.
- Precondition contract (spec table): PATCH/trash/restore REQUIRE `If-Match` (missing → 428 `precondition_required`, stale → 412 `stale_revision`, unparsable → 400 `validation`); `POST /nodes` takes none. All return the single-node shape (`nodeOutput` from Task 5) with the new revision in `ETag`; trash returns the node without `Path` computation on its former location (its live path is gone — return `Path` as computed post-trash, which the store yields naturally).
- Produces helper (consumed within package): `parseIfMatch(v string) (int64, error)`.

- [ ] **Step 1: Write the failing tests**

Add a `do(t, ts, method, path string, hdr map[string]string, body any) (*http.Response, string)` helper in `server_test.go` (marshal body to JSON when non-nil, set Content-Type). Then:

```go
// internal/api/routes_mutate_test.go
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func etagOf(t *testing.T, ts *httptest.Server, id int64) (api.Node, string) {
	t.Helper()
	resp, body := get(t, ts, fmt.Sprintf("/api/v1/nodes/%d", id), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	return n, resp.Header.Get("ETag")
}

func TestCreateDirectory(t *testing.T) {
	ts, s := newTestServer(t, nil)
	resp, body := do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "taxes", "kind": "dir"})
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, "/taxes", n.Path)

	// Name collision → 409 exists; kind=file → 422 (multipart is planned).
	resp, body = do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "taxes", "kind": "dir"})
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, body, `"code":"exists"`)
	resp, body = do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": s.RootID(), "name": "f.txt", "kind": "file"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestMoveRequiresIfMatch(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/a.txt", "x")

	resp, body := do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		nil, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusPreconditionRequired, resp.StatusCode)
	assert.Contains(t, body, `"code":"precondition_required"`)

	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": `"999"`}, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
	assert.Contains(t, body, `"code":"stale_revision"`)

	// "-1" is the store's unconditional sentinel; via HTTP it must be a 400,
	// never a precondition bypass.
	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": `"-1"`}, map[string]any{"new_name": "b.txt"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	_, etag := etagOf(t, ts, f.ID)
	resp, body = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": etag}, map[string]any{"new_name": "b.txt"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Equal(t, "/b.txt", n.Path)

	// Empty patch body → 422.
	_, etag = etagOf(t, ts, f.ID)
	resp, _ = do(t, ts, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", f.ID),
		map[string]string{"If-Match": etag}, map[string]any{})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestTrashAndRestoreRoundTripHTTP(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/doc.txt", "x")

	_, etag := etagOf(t, ts, f.ID)
	resp, body := do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var n api.Node
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.NotEmpty(t, n.TrashedAt)

	_, etag = etagOf(t, ts, f.ID)
	resp, body = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/restore", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NoError(t, json.Unmarshal([]byte(body), &n))
	assert.Empty(t, n.TrashedAt)
	assert.Equal(t, "/doc.txt", n.Path)
}
```

(`httptest` import in the helper snippet: place `etagOf` in `server_test.go` next to the other helpers so imports stay tidy.)

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/api/ -run 'Create|Move|TrashAndRestore' -v`. Expected: FAIL (routes 404 — `registerMutateRoutes` is empty).

- [ ] **Step 3: Implement routes_mutate.go**

```go
// internal/api/routes_mutate.go
package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// parseIfMatch parses the required If-Match revision. ETag-style quoting is
// accepted ("3" or 3). Empty → 428; garbage or negative → 400. Negatives are
// rejected here because the store reserves -1 as its unconditional sentinel:
// an If-Match of "-1" reaching the store would silently skip the precondition
// this header exists to enforce.
func parseIfMatch(v string) (int64, error) {
	if v == "" {
		return 0, NewError(http.StatusPreconditionRequired, "precondition_required",
			"this endpoint requires If-Match: <revision> (stat the node to get it)")
	}
	rev, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
	if err != nil || rev < 0 {
		return 0, NewError(http.StatusBadRequest, "validation",
			fmt.Sprintf("invalid If-Match %q: want a non-negative node revision", v))
	}
	return rev, nil
}

func registerMutateRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "createNode", Method: http.MethodPost, Path: "/api/v1/nodes",
		Summary: "Create a directory", DefaultStatus: http.StatusCreated,
		Description: "kind must be \"dir\"; file creation is POST /api/v1/ingest " +
			"(server-side paths) today, multipart upload later.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			ParentID int64  `json:"parent_id"`
			Name     string `json:"name" minLength:"1"`
			Kind     string `json:"kind" enum:"dir,file"`
		}
	}) (*nodeOutput, error) {
		if in.Body.Kind != "dir" {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"kind \"file\" is not supported here: use POST /api/v1/ingest (multipart upload is planned)")
		}
		var out *nodeOutput
		err := g.mutate(func() error {
			n, err := d.Store.Mkdir(ctx, in.Body.ParentID, in.Body.Name)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "moveNode", Method: http.MethodPatch, Path: "/api/v1/nodes/{id}",
		Summary: "Move and/or rename a node (metadata only; bytes never move)",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			NewParentID *int64  `json:"new_parent_id,omitempty"`
			NewName     *string `json:"new_name,omitempty"`
		}
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if in.Body.NewParentID == nil && in.Body.NewName == nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"nothing to do: set new_parent_id and/or new_name")
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			// Defaults for the omitted half come from the current node; the
			// revision precondition inside Move catches racing changes.
			cur, err := d.Store.NodeByID(ctx, in.ID)
			if err != nil {
				return FromStoreError(err)
			}
			parent, name := cur.ParentID, cur.Name
			if in.Body.NewParentID != nil {
				parent = in.Body.NewParentID
			}
			if in.Body.NewName != nil {
				name = *in.Body.NewName
			}
			if parent == nil {
				return FromStoreError(fmt.Errorf("node %d: %w", in.ID, storeErrIsRoot()))
			}
			n, err := d.Store.Move(ctx, in.ID, *parent, name, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "trashNode", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/trash",
		Summary: "Move a node and its subtree to the trash",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			n, err := d.Store.Trash(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "restoreNode", Method: http.MethodPost, Path: "/api/v1/nodes/{id}/restore",
		Summary: "Restore a trash root to its original location (root fallback, suffix on collision)",
	}, func(ctx context.Context, in *struct {
		ID      int64  `path:"id"`
		IfMatch string `header:"If-Match"`
	}) (*nodeOutput, error) {
		rev, err := parseIfMatch(in.IfMatch)
		if err != nil {
			return nil, err
		}
		var out *nodeOutput
		err = g.mutate(func() error {
			n, err := d.Store.Restore(ctx, in.ID, rev)
			if err != nil {
				return FromStoreError(err)
			}
			out, err = nodeWithPath(ctx, d, n.ID)
			return err
		})
		return out, err
	})
}
```

`storeErrIsRoot()` is just `store.ErrIsRoot` — import the store package and use it directly (`return FromStoreError(fmt.Errorf("node %d: %w", in.ID, store.ErrIsRoot))`); do not write a wrapper function.

- [ ] **Step 4: Run tests** — `go test -tags fts5 ./internal/api/ -v && make lint`. Expected: PASS.

- [ ] **Step 5: Commit** — subject: `Add mutation endpoints with If-Match preconditions`.

---

### Task 7: Operations endpoints — ingest, trash list/empty, gc, verify

**Files:**
- Modify: `internal/api/routes_ops.go`
- Create: `internal/api/age.go` (move `parseAge` from `cmd/docbank/cmd/trash.go`, exported)
- Modify: `cmd/docbank/cmd/trash.go` (delete `parseAge`, call `api.ParseAge`)
- Test: `internal/api/routes_ops_test.go`, move `parseAge` tests from cmd if any exist (`rg -n parseAge cmd/`)

**Interfaces:**
- Produces routes: `POST /api/v1/ingest`, `GET /api/v1/trash`, `POST /api/v1/trash/empty`, `POST /api/v1/gc`, `POST /api/v1/verify`.
- Produces types (client + CLI consume):

```go
type IngestFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}
type IngestReport struct {
	Added   int             `json:"added"`
	Skipped int             `json:"skipped"`
	Failed  []IngestFailure `json:"failed,omitempty"`
}
type GCReport struct {
	CandidateBlobs   int   `json:"candidate_blobs"`
	UntrackedFiles   int   `json:"untracked_files"`
	ReclaimableBytes int64 `json:"reclaimable_bytes"`
	Removed          int   `json:"removed"`
	Run              bool  `json:"run"`
}
type VerifyProblem struct {
	Hash    string `json:"hash"`
	Problem string `json:"problem" enum:"missing,corrupt,unreadable"`
}
type VerifyReport struct {
	OK       int             `json:"ok"`
	Problems []VerifyProblem `json:"problems,omitempty"`
}
func ParseAge(s string) (time.Duration, error)  // "" → 0; "30d" → 720h; rejects negatives
```

- [ ] **Step 1: Write the failing tests**

```go
// internal/api/routes_ops_test.go
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestIngestEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))

	resp, body := do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "a.txt")}, "dest": "/inbox"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var rep api.IngestReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Added)

	// Relative paths are rejected: the daemon's cwd is meaningless.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{"relative/x.txt"}, "dest": "/inbox"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, `"code":"validation"`)

	// A missing source is a per-path failure, not a request failure.
	resp, body = do(t, ts, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": []string{filepath.Join(src, "gone.txt")}, "dest": "/inbox"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Len(t, rep.Failed, 1)
}

func TestIngestRejectsNonLoopback(t *testing.T) {
	// httptest.NewRequest-style direct handler invocation with a non-loopback
	// RemoteAddr proves the middleware fence without real remote networking.
	dir := t.TempDir()
	s := openStoreAt(t, dir)
	srv := api.NewServer(api.Deps{Store: s, Blobs: blobStoreAt(dir), Cfg: config.Default()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"paths":["/x"],"dest":"/inbox"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:4444"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"loopback_only"`)
}

func TestTrashListAndEmpty(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/old.txt", "x")
	_, etag := etagOf(t, ts, f.ID)
	resp, _ := do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := get(t, ts, "/api/v1/trash", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "old.txt")

	resp, body = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Deleted int64 `json:"deleted"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	assert.Equal(t, int64(1), out.Deleted)

	// Bad age string → 422.
	resp, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": "-3d"})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestGCDryRunAndRun(t *testing.T) {
	ts, s := newTestServer(t, nil)
	f := createFileWithContent(t, ts, s, "/g.txt", "gc-me")
	_, etag := etagOf(t, ts, f.ID)
	_, _ = do(t, ts, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", f.ID),
		map[string]string{"If-Match": etag}, nil)
	_, _ = do(t, ts, http.MethodPost, "/api/v1/trash/empty", nil, map[string]any{})

	resp, body := do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": false})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.GCReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.CandidateBlobs)
	assert.False(t, rep.Run)

	resp, body = do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.Removed)
}

func TestVerifyEndpoint(t *testing.T) {
	ts, s := newTestServer(t, nil)
	createFileWithContent(t, ts, s, "/ok.txt", "fine")
	resp, body := do(t, ts, http.MethodPost, "/api/v1/verify", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rep api.VerifyReport
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, 1, rep.OK)
	assert.Empty(t, rep.Problems)
}
```

Adjust helper names (`openStoreAt`, `blobStoreAt`) to whatever `newTestServer` already factors out — reuse, don't duplicate.

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/api/ -run 'Ingest|TrashList|GC|Verify' -v`. Expected: FAIL.

- [ ] **Step 3: Implement age.go**

Move `parseAge` from `cmd/docbank/cmd/trash.go` verbatim into `internal/api/age.go` as `ParseAge` (exported, same doc comment), update `trash.go` to call `api.ParseAge`, delete the cmd copy.

- [ ] **Step 4: Implement routes_ops.go**

```go
// internal/api/routes_ops.go
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/ingest"
)

func registerOpsRoutes(api huma.API, d Deps, g *gate) {
	type ingestOutput struct{ Body IngestReport }
	huma.Register(api, huma.Operation{
		OperationID: "ingest", Method: http.MethodPost, Path: "/api/v1/ingest",
		Summary: "Import server-side files or directory trees (loopback callers only)",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Paths []string `json:"paths" minItems:"1"`
			Dest  string   `json:"dest" default:"/inbox"`
		}
	}) (*ingestOutput, error) {
		for _, p := range in.Body.Paths {
			if !filepath.IsAbs(p) {
				return nil, NewError(http.StatusUnprocessableEntity, "validation",
					fmt.Sprintf("path %q must be absolute: the daemon has no meaningful working directory", p))
			}
		}
		out := &ingestOutput{}
		err := g.mutate(func() error {
			ing := &ingest.Ingester{Store: d.Store, Blobs: d.Blobs}
			rep, err := ing.AddPaths(ctx, in.Body.Paths, in.Body.Dest)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body = IngestReport{Added: rep.Added, Skipped: rep.Skipped}
			for _, f := range rep.Failed {
				out.Body.Failed = append(out.Body.Failed, IngestFailure{Path: f.Path, Error: f.Err.Error()})
			}
			return nil
		})
		return out, err
	})

	type trashListOutput struct {
		Body struct {
			Items []Node `json:"items"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "listTrash", Method: http.MethodGet, Path: "/api/v1/trash",
		Summary: "List restorable trash roots, newest first",
	}, func(ctx context.Context, _ *struct{}) (*trashListOutput, error) {
		roots, err := d.Store.TrashedRoots(ctx)
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := &trashListOutput{}
		out.Body.Items = []Node{}
		for _, n := range roots {
			out.Body.Items = append(out.Body.Items, fromStoreNode(n))
		}
		return out, nil
	})

	type emptyOutput struct {
		Body struct {
			Deleted int64 `json:"deleted"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "emptyTrash", Method: http.MethodPost, Path: "/api/v1/trash/empty",
		Summary: "Hard-delete trash roots (their blobs become gc candidates)",
	}, func(ctx context.Context, in *struct {
		Body struct {
			OlderThan string `json:"older_than,omitempty" example:"30d"`
		}
	}) (*emptyOutput, error) {
		age, err := ParseAge(in.Body.OlderThan)
		if err != nil {
			return nil, NewError(http.StatusUnprocessableEntity, "validation", err.Error())
		}
		out := &emptyOutput{}
		err = g.maintain(func() error {
			n, err := d.Store.EmptyTrash(ctx, age)
			if err != nil {
				return FromStoreError(err)
			}
			out.Body.Deleted = n
			return nil
		})
		return out, err
	})

	type gcOutput struct{ Body GCReport }
	huma.Register(api, huma.Operation{
		OperationID: "gc", Method: http.MethodPost, Path: "/api/v1/gc",
		Summary: "Report (run=false) or reclaim (run=true) unreachable blobs",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Run bool `json:"run"`
		}
	}) (*gcOutput, error) {
		out := &gcOutput{}
		err := g.maintain(func() error {
			rep, err := runGC(ctx, d, in.Body.Run)
			if err != nil {
				return err
			}
			out.Body = rep
			return nil
		})
		return out, err
	})

	type verifyOutput struct{ Body VerifyReport }
	huma.Register(api, huma.Operation{
		OperationID: "verify", Method: http.MethodPost, Path: "/api/v1/verify",
		Summary: "Re-hash every stored blob and report corruption",
	}, func(ctx context.Context, _ *struct{}) (*verifyOutput, error) {
		out := &verifyOutput{}
		err := g.maintain(func() error {
			blobs, err := d.Store.AllBlobs(ctx)
			if err != nil {
				return FromStoreError(err)
			}
			for _, b := range blobs {
				if err := ctx.Err(); err != nil {
					return NewError(http.StatusInternalServerError, "internal",
						fmt.Sprintf("verify interrupted: %v", err))
				}
				if problem := checkBlob(d, b.Hash); problem == "" {
					out.Body.OK++
				} else {
					out.Body.Problems = append(out.Body.Problems, VerifyProblem{Hash: b.Hash, Problem: problem})
				}
			}
			return nil
		})
		return out, err
	})
}

// runGC ports cmd/gc.go's semantics: candidates from row reachability,
// untracked files from a shard scan (safe under the maintenance gate — no
// concurrent ingest can be mid-write), files removed before rows so a crash
// leaves reconcilable row-without-file state, never the reverse.
func runGC(ctx context.Context, d Deps, run bool) (GCReport, error) {
	candidates, err := d.Store.UnreachableBlobs(ctx)
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	tracked, err := d.Store.AllBlobs(ctx)
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	trackedSet := make(map[string]bool, len(tracked))
	for _, b := range tracked {
		trackedSet[b.Hash] = true
	}
	files, err := d.Blobs.List()
	if err != nil {
		return GCReport{}, FromStoreError(err)
	}
	var untracked []string
	rep := GCReport{CandidateBlobs: len(candidates), Run: run}
	for hash, size := range files {
		if !trackedSet[hash] {
			untracked = append(untracked, hash)
			rep.ReclaimableBytes += size
		}
	}
	sort.Strings(untracked)
	rep.UntrackedFiles = len(untracked)
	for _, c := range candidates {
		rep.ReclaimableBytes += c.Size
	}
	if !run {
		return rep, nil
	}
	for _, h := range untracked {
		if err := d.Blobs.Remove(h); err != nil {
			return GCReport{}, FromStoreError(err)
		}
	}
	hashes := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if err := d.Blobs.Remove(c.Hash); err != nil {
			return GCReport{}, FromStoreError(err)
		}
		hashes = append(hashes, c.Hash)
	}
	if err := d.Store.DeleteBlobRows(ctx, hashes); err != nil {
		return GCReport{}, FromStoreError(err)
	}
	rep.Removed = len(hashes) + len(untracked)
	return rep, nil
}

// checkBlob returns "", "missing", "corrupt", or "unreadable".
func checkBlob(d Deps, hash string) string {
	f, err := d.Blobs.Open(hash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unreadable"
	}
	if hex.EncodeToString(h.Sum(nil)) != hash {
		return "corrupt"
	}
	return ""
}
```

Add the report types from the Interfaces block to `types.go`. Do NOT delete `cmd/docbank/cmd/gc.go`/`verify.go` logic yet — the CLI rewrite task replaces them wholesale.

- [ ] **Step 5: Add the maintenance-gate ordering test** (spec requirement: concurrent mutation vs `gc` must serialize, not fail)

```go
// append to internal/api/routes_ops_test.go
func TestMaintenanceGateQueuesMutations(t *testing.T) {
	ts, s := newTestServer(t, nil)
	// Saturate: fire a gc run and a burst of mkdirs concurrently. Every
	// request must succeed — the gate queues, it never rejects — and the
	// store must end up consistent (all dirs present).
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 10 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resp, body := do(t, ts, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": true})
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("gc: %d %s", resp.StatusCode, body)
			}
		}()
		go func() {
			defer wg.Done()
			resp, body := do(t, ts, http.MethodPost, "/api/v1/nodes", nil,
				map[string]any{"parent_id": s.RootID(), "name": fmt.Sprintf("dir-%d", i), "kind": "dir"})
			if resp.StatusCode != http.StatusCreated {
				errs <- fmt.Errorf("mkdir: %d %s", resp.StatusCode, body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	kids, err := s.Children(t.Context(), s.RootID())
	require.NoError(t, err)
	assert.Len(t, kids, 10)
}
```

Run it with the race detector: `go test -tags fts5 -race ./internal/api/ -run MaintenanceGate -v`. Note: `do` uses `t.Helper` + `require` — inside goroutines `require` must not be used (it calls `t.FailNow` off the test goroutine), which is why the goroutines above only send on `errs`. If `do` itself calls `require.NoError` on transport errors, add a non-failing variant `try(...)` returning an error for this test.

- [ ] **Step 6: Run tests** — `go test -tags fts5 -race ./internal/api/ && go test -tags fts5 ./... && make lint`. Expected: PASS (including existing cmd tests — `trash.go` now imports `api.ParseAge`).

- [ ] **Step 7: Commit** — subject: `Add operations endpoints: ingest, trash, gc, verify`.

---

### Task 8: `internal/client` — typed HTTP client

**Files:**
- Create: `internal/client/client.go`
- Test: `internal/client/client_test.go`

**Interfaces:**
- Consumes: every `api` type and route from Tasks 4-7.
- Produces (the CLI rewrite consumes these exact signatures):

```go
type Client struct{ /* unexported */ }
func New(baseURL, apiKey string) *Client
func (c *Client) Health(ctx context.Context) error
func (c *Client) Node(ctx context.Context, id int64) (api.Node, error)
func (c *Client) Stat(ctx context.Context, path string) (api.Node, error)
func (c *Client) Children(ctx context.Context, id int64) ([]api.Node, error) // auto-pages
func (c *Client) Content(ctx context.Context, id int64) (io.ReadCloser, error)
func (c *Client) Search(ctx context.Context, query string, limit int) ([]api.SearchHit, error)
func (c *Client) Mkdir(ctx context.Context, parentID int64, name string) (api.Node, error)
func (c *Client) Ingest(ctx context.Context, paths []string, dest string) (api.IngestReport, error)
func (c *Client) Move(ctx context.Context, id, rev int64, newParentID *int64, newName *string) (api.Node, error)
func (c *Client) Trash(ctx context.Context, id, rev int64) (api.Node, error)
func (c *Client) Restore(ctx context.Context, id, rev int64) (api.Node, error)
func (c *Client) TrashList(ctx context.Context) ([]api.Node, error)
func (c *Client) TrashEmpty(ctx context.Context, olderThan string) (int64, error)
func (c *Client) GC(ctx context.Context, run bool) (api.GCReport, error)
func (c *Client) Verify(ctx context.Context) (api.VerifyReport, error)
func (c *Client) Shutdown(ctx context.Context, token string) error
```

- Error contract: a non-2xx response decodes the `api.Error` envelope; the returned error wraps the corresponding `store.Err*` (via the code) so `errors.Is(err, store.ErrNotFound)` works in command code, and its message is the envelope `detail`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/client/client_test.go
package client_test

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func newClient(t *testing.T, key string) (*client.Client, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	cfg := config.Default()
	cfg.Server.APIKey = key
	srv := api.NewServer(api.Deps{Store: s, Blobs: blob.New(filepath.Join(dir, "blobs")), Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return client.New(ts.URL, key), s
}

func TestRoundTrip(t *testing.T) {
	c, s := newClient(t, "")
	ctx := t.Context()

	dir, err := c.Mkdir(ctx, s.RootID(), "docs")
	require.NoError(t, err)
	assert.Equal(t, "/docs", dir.Path)

	got, err := c.Stat(ctx, "/docs")
	require.NoError(t, err)
	assert.Equal(t, dir.ID, got.ID)

	renamed, err := c.Move(ctx, dir.ID, got.Revision, nil, ptr("papers"))
	require.NoError(t, err)
	assert.Equal(t, "/papers", renamed.Path)

	kids, err := c.Children(ctx, s.RootID())
	require.NoError(t, err)
	assert.Len(t, kids, 1)

	trashed, err := c.Trash(ctx, dir.ID, renamed.Revision)
	require.NoError(t, err)
	restored, err := c.Restore(ctx, dir.ID, trashed.Revision)
	require.NoError(t, err)
	assert.Equal(t, "/papers", restored.Path)
}

func TestErrorMapping(t *testing.T) {
	c, s := newClient(t, "")
	ctx := t.Context()

	_, err := c.Stat(ctx, "/missing")
	require.ErrorIs(t, err, store.ErrNotFound)

	d, err := c.Mkdir(ctx, s.RootID(), "dup")
	require.NoError(t, err)
	_, err = c.Mkdir(ctx, s.RootID(), "dup")
	require.ErrorIs(t, err, store.ErrExists)

	_, err = c.Move(ctx, d.ID, d.Revision+99, nil, ptr("x"))
	require.ErrorIs(t, err, store.ErrStaleRevision)
}

func TestAPIKeySent(t *testing.T) {
	c, _ := newClient(t, "k")
	require.NoError(t, c.Health(t.Context()))
	_, err := c.TrashList(t.Context()) // authed route succeeds only with the key
	require.NoError(t, err)
}

func ptr[T any](v T) *T { return &v }
```

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/client/ -v`. Expected: FAIL (package missing).

- [ ] **Step 3: Implement client.go**

```go
// internal/client/client.go
// Package client is the typed HTTP client for the docbank daemon. It shares
// wire types with internal/api (same module), so the contract is checked at
// compile time; agents use the OpenAPI document instead.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

type Client struct {
	base string
	key  string
	hc   *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{base: baseURL, key: apiKey, hc: &http.Client{Timeout: 0}}
}

// codeToStoreErr is the inverse of the server's FromStoreError mapping.
var codeToStoreErr = map[string]error{
	"not_found":      store.ErrNotFound,
	"exists":         store.ErrExists,
	"cycle":          store.ErrCycle,
	"stale_revision": store.ErrStaleRevision,
	"not_dir":        store.ErrNotDir,
	"not_file":       store.ErrNotFile,
	"invalid_name":   store.ErrInvalidName,
	"not_trashed":    store.ErrNotTrashed,
	"is_root":        store.ErrIsRoot,
}

func decodeError(resp *http.Response) error {
	var e api.Error
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &e); err != nil || e.Status == 0 {
		return fmt.Errorf("daemon returned %s: %s", resp.Status, string(body))
	}
	if target, ok := codeToStoreErr[e.Code]; ok {
		return fmt.Errorf("%s: %w", e.Detail, target)
	}
	return fmt.Errorf("daemon error (%d %s): %s", e.Status, e.Code, e.Detail)
}

// do issues one JSON round-trip. Non-nil out must be a pointer; a non-2xx
// status decodes the error envelope instead.
func (c *Client) do(ctx context.Context, method, path string, hdr map[string]string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding %s %s request: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("calling daemon (%s %s): %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return nil
}

func ifMatch(rev int64) map[string]string {
	return map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(rev))}
}

func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil, nil)
}

func (c *Client) Node(ctx context.Context, id int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/nodes/%d", id), nil, nil, &n)
	return n, err
}

func (c *Client) Stat(ctx context.Context, path string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodGet, "/api/v1/path?path="+url.QueryEscape(path), nil, nil, &n)
	return n, err
}

// Children fetches every page. Callers that need streaming can add it when
// a real consumer appears (YAGNI).
func (c *Client) Children(ctx context.Context, id int64) ([]api.Node, error) {
	const pageSize = 1000
	var all []api.Node
	for offset := 0; ; offset += pageSize {
		var page struct {
			Items []api.Node `json:"items"`
			Total int        `json:"total"`
		}
		p := fmt.Sprintf("/api/v1/nodes/%d/children?limit=%d&offset=%d", id, pageSize, offset)
		if err := c.do(ctx, http.MethodGet, p, nil, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if offset+pageSize >= page.Total {
			return all, nil
		}
	}
}

func (c *Client) Content(ctx context.Context, id int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/nodes/%d/content", c.base, id), nil)
	if err != nil {
		return nil, fmt.Errorf("building content request: %w", err)
	}
	if c.key != "" {
		req.Header.Set("X-Api-Key", c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching content of node %d: %w", id, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}

func (c *Client) Search(ctx context.Context, query string, limit int) ([]api.SearchHit, error) {
	var out struct {
		Hits []api.SearchHit `json:"hits"`
	}
	p := fmt.Sprintf("/api/v1/search?q=%s&limit=%d", url.QueryEscape(query), limit)
	err := c.do(ctx, http.MethodGet, p, nil, nil, &out)
	return out.Hits, err
}

func (c *Client) Mkdir(ctx context.Context, parentID int64, name string) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, "/api/v1/nodes", nil,
		map[string]any{"parent_id": parentID, "name": name, "kind": "dir"}, &n)
	return n, err
}

func (c *Client) Ingest(ctx context.Context, paths []string, dest string) (api.IngestReport, error) {
	var rep api.IngestReport
	err := c.do(ctx, http.MethodPost, "/api/v1/ingest", nil,
		map[string]any{"paths": paths, "dest": dest}, &rep)
	return rep, err
}

func (c *Client) Move(ctx context.Context, id, rev int64, newParentID *int64, newName *string) (api.Node, error) {
	var n api.Node
	body := map[string]any{}
	if newParentID != nil {
		body["new_parent_id"] = *newParentID
	}
	if newName != nil {
		body["new_name"] = *newName
	}
	err := c.do(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/nodes/%d", id), ifMatch(rev), body, &n)
	return n, err
}

func (c *Client) Trash(ctx context.Context, id, rev int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/trash", id), ifMatch(rev), nil, &n)
	return n, err
}

func (c *Client) Restore(ctx context.Context, id, rev int64) (api.Node, error) {
	var n api.Node
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/nodes/%d/restore", id), ifMatch(rev), nil, &n)
	return n, err
}

func (c *Client) TrashList(ctx context.Context) ([]api.Node, error) {
	var out struct {
		Items []api.Node `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/trash", nil, nil, &out)
	return out.Items, err
}

func (c *Client) TrashEmpty(ctx context.Context, olderThan string) (int64, error) {
	var out struct {
		Deleted int64 `json:"deleted"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/trash/empty", nil,
		map[string]any{"older_than": olderThan}, &out)
	return out.Deleted, err
}

func (c *Client) GC(ctx context.Context, run bool) (api.GCReport, error) {
	var rep api.GCReport
	err := c.do(ctx, http.MethodPost, "/api/v1/gc", nil, map[string]any{"run": run}, &rep)
	return rep, err
}

func (c *Client) Verify(ctx context.Context) (api.VerifyReport, error) {
	var rep api.VerifyReport
	err := c.do(ctx, http.MethodPost, "/api/v1/verify", nil, nil, &rep)
	return rep, err
}

func (c *Client) Shutdown(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.do(ctx, http.MethodPost, "/api/daemon/shutdown",
		map[string]string{"X-Docbank-Daemon-Token": token}, nil, nil)
}

var _ = errors.Is // keep errors import if otherwise unused after edits
```

Delete the trailing `var _ = errors.Is` line if the import is genuinely unused — that line is a reminder, not code to keep.

- [ ] **Step 4: Run tests** — `go test -tags fts5 ./internal/client/ -v && make lint`. Expected: PASS.

- [ ] **Step 5: Commit** — subject: `Add typed HTTP client sharing wire types with the API`.

---

### Task 9: `docbank serve` — foreground daemon

**Files:**
- Modify: `internal/home/lock.go`, `internal/home/lock_stub.go` (add `TryLockExclusive`; keep the old API until Task 11 removes it)
- Create: `cmd/docbank/cmd/serve.go`
- Test: `internal/home/lock_test.go` additions, `cmd/docbank/cmd/serve_test.go`

**Interfaces:**
- Consumes: `config.Load/Validate`, `api.NewServer/Deps/ActivityTracker`, kit `daemon.Listen/NewRuntimeRecord/RuntimeStore/Endpoint/NetworkTCP`, `kit/logging.NewLogger`.
- Produces:

```go
// internal/home
var ErrVaultLocked = errors.New("vault is locked by another process")
func (l Layout) TryLockExclusive() (*Lock, error) // non-blocking; ErrVaultLocked when held

// internal/client (this task adds the record plumbing; discovery is Task 10)
const Service = "docbank"
const EnvBackgroundDaemon = "DOCBANK_BACKGROUND_DAEMON"
const metaCreateTime = "create_time"     // exported as needed by tests
const metaShutdownToken = "shutdown_token"
func RuntimeStore(root string) kitdaemon.RuntimeStore // {Dir: root, Prefix: "daemon"}
func NewRecord(addr, token string) kitdaemon.RuntimeRecord // service/version/metadata filled
```

- Behavior: `docbank serve` = foreground; refuses a second instance (`ErrVaultLocked`); writes the runtime record after binding; removes it on shutdown; SIGINT/SIGTERM and the token route both trigger graceful shutdown; `DOCBANK_BACKGROUND_DAEMON=1` + `idle_timeout > 0` exits after quiet period; background mode logs JSON to `$DOCBANK_HOME/logs/` via kit/logging.

- [ ] **Step 1: Add gopsutil** — `go get github.com/shirou/gopsutil/v4@latest && go mod tidy`

- [ ] **Step 2: Write the failing lock test**

```go
// append to internal/home/lock_test.go (create if the file has no exclusive tests)
func TestTryLockExclusive(t *testing.T) {
	l := Layout{Root: t.TempDir()}
	require.NoError(t, l.Ensure())
	lk, err := l.TryLockExclusive()
	require.NoError(t, err)
	_, err = l.TryLockExclusive()
	require.ErrorIs(t, err, ErrVaultLocked)
	require.NoError(t, lk.Release())
	lk2, err := l.TryLockExclusive()
	require.NoError(t, err)
	require.NoError(t, lk2.Release())
}
```

Caveat: flock is per-(file,process) on some paths — same-process re-flock of the SAME fd would succeed, so `TryLockExclusive` must open its own fd each call (it does, mirroring `AcquireLock`). If the same-process second acquire does NOT fail on macOS (flock re-entrancy applies per open file description, and two opens are two descriptions — it should fail), keep the test; if CI proves otherwise, split the second acquire into a subprocess helper the way msgvault tests do.

- [ ] **Step 3: Implement TryLockExclusive**

In `internal/home/lock.go` (unix), following the existing `AcquireLock` open pattern (same lock file, `O_NOFOLLOW` open flags if used there):

```go
// ErrVaultLocked — see Interfaces.

// TryLockExclusive takes the vault lock without blocking. The daemon is the
// single lock holder for the vault's lifetime; a second daemon (or a stale
// holder) surfaces immediately instead of hanging.
func (l Layout) TryLockExclusive() (*Lock, error) {
	f, err := os.OpenFile(l.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening vault lock %s: %w", l.LockPath(), err)
	}
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w (is a docbank daemon already running?)", l.LockPath(), ErrVaultLocked)
		}
		return nil, err
	}
	return &Lock{f: f, exclusive: true}, nil
}
```

Match the actual `Lock` struct fields (read `internal/home/lock.go` first — the constructor above must mirror how `AcquireLock` builds `Lock`). Add the same function to `lock_stub.go` returning `errUnsupported`. Run: `go test -tags fts5 ./internal/home/ -v` → PASS.

- [ ] **Step 4: Create the runtime-record plumbing in internal/client**

```go
// internal/client/runtime.go
package client

import (
	"fmt"
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/version"
)

const (
	Service            = "docbank"
	EnvBackgroundDaemon = "DOCBANK_BACKGROUND_DAEMON"
	metaCreateTime      = "create_time"
	metaShutdownToken   = "shutdown_token"
)

func RuntimeStore(root string) kitdaemon.RuntimeStore {
	return kitdaemon.RuntimeStore{Dir: root, Prefix: "daemon"}
}

// NewRecord builds this process's runtime record. create_time guards the
// recorded PID against reuse: kit's record has no such field, so docbank
// carries it in Metadata (msgvault's pattern) and checks it before trusting
// or signaling a PID.
func NewRecord(addr, token string) kitdaemon.RuntimeRecord {
	rec := kitdaemon.NewRuntimeRecord(Service, version.Version,
		kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: addr})
	if rec.Metadata == nil {
		rec.Metadata = map[string]string{}
	}
	rec.Metadata[metaShutdownToken] = token
	if ct, ok := processCreateTimeMillis(rec.PID); ok {
		rec.Metadata[metaCreateTime] = strconv.FormatInt(ct, 10)
	}
	return rec
}

func processCreateTimeMillis(pid int) (int64, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	created, err := p.CreateTime()
	if err != nil {
		return 0, false
	}
	return created, true
}

// createTimeMatches reports whether rec's recorded create_time still
// describes the live process at rec.PID. A record without the key matches
// trivially (older daemons); a mismatch means PID reuse — treat as dead.
func createTimeMatches(rec kitdaemon.RuntimeRecord) bool {
	recorded := rec.Metadata[metaCreateTime]
	if recorded == "" {
		return true
	}
	live, ok := processCreateTimeMillis(rec.PID)
	if !ok {
		return false
	}
	return recorded == strconv.FormatInt(live, 10)
}

var _ = fmt.Sprintf // remove if fmt ends up unused
```

(Drop the trailing `var _` if unused; check `kitdaemon.NewRuntimeRecord` — if it already initializes Metadata, drop the nil guard.)

- [ ] **Step 5: Implement serve.go**

```go
// cmd/docbank/cmd/serve.go
package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	kitdaemon "go.kenn.io/kit/daemon"
	kitlogging "go.kenn.io/kit/logging"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the docbank daemon in the foreground",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return runServe(cmd.Context()) },
}

func runServe(ctx context.Context) error {
	layout, err := home.Resolve()
	if err != nil {
		return err
	}
	if err := layout.Ensure(); err != nil {
		return err
	}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	background := os.Getenv(client.EnvBackgroundDaemon) == "1"
	logger, err := buildServeLogger(layout, background)
	if err != nil {
		return err
	}

	lock, err := layout.TryLockExclusive()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	s, err := store.Open(layout.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	blobs := blob.New(layout.BlobsDir())
	// Exclusive lock holder: any stale tmp file is provably abandoned.
	if err := blobs.CleanTmp(); err != nil {
		return err
	}

	listener, err := kitdaemon.Listen(ctx, kitdaemon.Endpoint{
		Network: kitdaemon.NetworkTCP,
		Address: net.JoinHostPort(cfg.Server.BindAddr, strconv.Itoa(cfg.Server.APIPort)),
	})
	if err != nil {
		return fmt.Errorf("binding API listener: %w", err)
	}
	addr := listener.Addr().String()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generating shutdown token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	rtStore := client.RuntimeStore(layout.Root)
	recPath, err := rtStore.Write(client.NewRecord(addr, token))
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("writing daemon runtime record: %w", err)
	}
	defer func() { _ = os.Remove(recPath) }()

	var stopOnce sync.Once
	stopCh := make(chan struct{})
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }

	tracker := api.NewActivityTracker()
	srv := api.NewServer(api.Deps{
		Store: s, Blobs: blobs, Cfg: cfg, Logger: logger,
		StartedAt: time.Now(), ShutdownToken: token, Shutdown: stop, Tracker: tracker,
	})
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	sigCtx, sigCancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer sigCancel()

	if background && cfg.Server.IdleTimeout.Std() > 0 {
		go idleWatch(sigCtx, tracker, cfg.Server.IdleTimeout.Std(), logger, stop)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(listener) }()
	logger.Info("docbank daemon listening", "addr", addr, "pid", os.Getpid(), "background", background)

	select {
	case err := <-errCh:
		return fmt.Errorf("daemon API server: %w", err)
	case <-sigCtx.Done():
	case <-stopCh:
	}
	logger.Info("docbank daemon shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("draining daemon requests: %w", err)
	}
	return nil
}

// idleWatch exits an auto-started daemon after a fully quiet window so
// spawned daemons don't accumulate. Foreground serves never idle out.
func idleWatch(ctx context.Context, t *api.ActivityTracker, timeout time.Duration,
	logger *slog.Logger, stop func()) {
	// Clamp the poll interval: NewTicker panics on a non-positive duration,
	// and a pathologically small configured timeout must not spin.
	interval := timeout / 10
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.IdleFor() >= timeout {
				logger.Info("idle timeout reached, shutting down", "idle", timeout.String())
				stop()
				return
			}
		}
	}
}

func buildServeLogger(layout home.Layout, background bool) (*slog.Logger, error) {
	logger, _, err := kitlogging.NewLogger(kitlogging.Options{
		Stderr:      os.Stderr,
		EnvLevelVar: "DOCBANK_LOG_LEVEL",
		File: kitlogging.FileOptions{
			Enabled:         background,
			Dir:             layout.LogsDir(),
			DailyFilePrefix: "docbank",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("building daemon logger: %w", err)
	}
	return logger, nil
}

func init() { rootCmd.AddCommand(serveCmd) }
```

Check `kit/logging.Options`/`FileOptions` field names against the module source before compiling (`rg -n "type Options|type FileOptions" $(go env GOMODCACHE)/go.kenn.io/kit@*/logging/logging.go`); adjust to reality. Same for `kitdaemon.Listen`'s signature.

- [ ] **Step 6: Write the serve smoke test**

```go
// cmd/docbank/cmd/serve_test.go
//go:build unix

package cmd

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/client"
)

func TestServeServesAndShutsDownGracefully(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()

	// Discover via the runtime record like a real client would.
	var rec kitdaemon.RuntimeRecord
	require.Eventually(t, func() bool {
		recs, err := client.RuntimeStore(dir).List()
		if err != nil || len(recs) == 0 {
			return false
		}
		rec = recs[0]
		resp, err := http.Get("http://" + rec.Address + "/health")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond)

	// Second daemon on the same vault must refuse.
	err := runServe(context.Background())
	assert.Error(t, err)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
	// Record removed on shutdown.
	recs, err := client.RuntimeStore(dir).List()
	require.NoError(t, err)
	assert.Empty(t, recs)
}
```

Note the second `runServe` call returns quickly only because `TryLockExclusive` is non-blocking — if this test hangs, the lock is blocking and Step 3 is wrong.

- [ ] **Step 7: Run tests** — `go test -tags fts5 ./internal/home/ ./cmd/... -v -run 'TryLock|Serve' && go test -tags fts5 ./... && make lint`. Expected: PASS.

- [ ] **Step 8: Commit** — subject: `Add docbank serve: exclusive-lock daemon with runtime record`.

---

### Task 10: Discovery, auto-start, and `serve start|stop|status`

**Files:**
- Create: `internal/client/ensure.go`, `cmd/docbank/cmd/serve_lifecycle.go`
- Test: `internal/client/ensure_test.go`, `cmd/docbank/cmd/serve_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 9's `RuntimeStore`, `NewRecord`, `createTimeMatches`, `EnvBackgroundDaemon`; kit `daemon.Discover/DiscoverOptions/ProbeOptions/StartDetached/ProcessAlive`.
- Produces (CLI rewrite consumes `Ensure`; lifecycle commands consume the rest):

```go
// Find reports the live, responding docbank daemon (any version): daemon
// discovery for status/stop. NEVER auto-starts.
func Find(ctx context.Context, root string) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error)
// Ensure returns a client for a version-matched daemon, starting (and if
// needed, replacing a version-mismatched) one. CLI commands call this.
func Ensure(ctx context.Context) (*Client, error)
// Start spawns a detached daemon and waits for a compatible ping.
func Start(ctx context.Context, root string) (kitdaemon.RuntimeRecord, error)
// Stop gracefully stops the discovered daemon: token endpoint first,
// SIGTERM only when create_time still matches the recorded PID. Returns
// false when no daemon was running.
func Stop(ctx context.Context, root string) (bool, error)
```

- [ ] **Step 1: Implement ensure.go** (discovery logic is hard to unit-test without a daemon; the serve smoke test pattern from Task 9 carries the load — write the tests in Step 2 against a real in-process `runServe`)

```go
// internal/client/ensure.go
//go:build unix

package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

const ensureTimeout = 30 * time.Second

func probeOptions() kitdaemon.ProbeOptions {
	return kitdaemon.ProbeOptions{ExpectedService: Service, Timeout: 2 * time.Second}
}

func discoverOptions(requireVersion bool) kitdaemon.DiscoverOptions {
	return kitdaemon.DiscoverOptions{
		Probe:           probeOptions(),
		RequirePIDAlive: true,
		Accept: func(rec kitdaemon.RuntimeRecord, info kitdaemon.PingInfo) bool {
			if !createTimeMatches(rec) {
				return false
			}
			return !requireVersion || info.Version == version.Version
		},
	}
}

// Find — see Interfaces block. Any-version discovery for status/stop.
func Find(ctx context.Context, root string) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
	return kitdaemon.Discover(ctx, RuntimeStore(root), discoverOptions(false))
}

func newClientFor(rec kitdaemon.RuntimeRecord, cfg config.Config) *Client {
	return New("http://"+rec.Address, cfg.Server.APIKey)
}

// Ensure — see Interfaces block.
func Ensure(ctx context.Context) (*Client, error) {
	layout, err := home.Resolve()
	if err != nil {
		return nil, err
	}
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return nil, err
	}

	rec, _, ok, err := kitdaemon.Discover(ctx, RuntimeStore(layout.Root), discoverOptions(true))
	if err != nil {
		return nil, fmt.Errorf("discovering daemon: %w", err)
	}
	if ok {
		return newClientFor(rec, cfg), nil
	}

	// Serialize racing starters; re-check under the lock.
	launch := flock.New(filepath.Join(layout.Root, "launch.lock"))
	lockCtx, cancel := context.WithTimeout(ctx, ensureTimeout)
	defer cancel()
	if _, err := launch.TryLockContext(lockCtx, 100*time.Millisecond); err != nil {
		return nil, fmt.Errorf("acquiring daemon launch lock: %w", err)
	}
	defer func() { _ = launch.Unlock() }()

	rec, _, ok, err = kitdaemon.Discover(ctx, RuntimeStore(layout.Root), discoverOptions(true))
	if err == nil && ok {
		return newClientFor(rec, cfg), nil
	}

	// A live daemon with the wrong version blocks the vault lock: replace it.
	if old, _, found, _ := Find(ctx, layout.Root); found {
		if err := stopRecord(ctx, old, cfg); err != nil {
			return nil, fmt.Errorf("stopping version-mismatched daemon (pid %d, %s): %w",
				old.PID, old.Version, err)
		}
	}

	rec, err = Start(ctx, layout.Root)
	if err != nil {
		return nil, err
	}
	return newClientFor(rec, cfg), nil
}

// Start — see Interfaces block.
func Start(ctx context.Context, root string) (kitdaemon.RuntimeRecord, error) {
	exe, err := os.Executable()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("resolving executable for daemon spawn: %w", err)
	}
	logPath := filepath.Join(root, "logs", "serve.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("opening %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()
	// DOCBANK_HOME is forced to root so a caller-supplied root (update's
	// restart path, tests) can never spawn a daemon on a different vault
	// than the one being discovered.
	err = kitdaemon.StartDetached(ctx, kitdaemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"serve"},
		Env:        append(os.Environ(), EnvBackgroundDaemon+"=1", "DOCBANK_HOME="+root),
		Stdout:     logFile,
		Stderr:     logFile,
	})
	if err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("spawning daemon: %w", err)
	}

	deadline := time.Now().Add(ensureTimeout)
	for time.Now().Before(deadline) {
		rec, _, ok, err := kitdaemon.Discover(ctx, RuntimeStore(root), discoverOptions(true))
		if err == nil && ok {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return kitdaemon.RuntimeRecord{}, fmt.Errorf("waiting for daemon: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return kitdaemon.RuntimeRecord{}, fmt.Errorf(
		"daemon did not become ready within %s; check %s", ensureTimeout, logPath)
}

// Stop — see Interfaces block.
func Stop(ctx context.Context, root string) (bool, error) {
	layout := home.Layout{Root: root}
	cfg, err := config.Load(layout.Root)
	if err != nil {
		return false, err
	}
	rec, _, ok, err := Find(ctx, root)
	if err != nil || !ok {
		return false, err
	}
	return true, stopRecord(ctx, rec, cfg)
}

func stopRecord(ctx context.Context, rec kitdaemon.RuntimeRecord, cfg config.Config) error {
	c := newClientFor(rec, cfg)
	if token := rec.Metadata[metaShutdownToken]; token != "" {
		if err := c.Shutdown(ctx, token); err == nil {
			if waitDead(ctx, rec, 10*time.Second) {
				return nil
			}
		}
	}
	// Signal fallback only when the PID is provably still our daemon.
	if !createTimeMatches(rec) {
		return errors.New("daemon PID no longer matches its recorded create time; not signaling")
	}
	if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signaling daemon pid %d: %w", rec.PID, err)
	}
	if !waitDead(ctx, rec, 10*time.Second) {
		return fmt.Errorf("daemon pid %d did not exit after SIGTERM", rec.PID)
	}
	return nil
}

func waitDead(ctx context.Context, rec kitdaemon.RuntimeRecord, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !kitdaemon.ProcessAlive(rec.PID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
	return !kitdaemon.ProcessAlive(rec.PID)
}
```

`github.com/gofrs/flock` is already in kit's dependency tree; `go get github.com/gofrs/flock@latest && go mod tidy`. A `!unix` stub (`ensure_stub.go`) mirrors the Phase 1 pattern: all four functions return the unsupported error, keeping `./internal/...` compiling on Windows CI.

- [ ] **Step 2: Write lifecycle tests** — the integration test builds the real binary once and drives the full spawn path:

```go
// cmd/docbank/cmd/serve_lifecycle_test.go
//go:build unix

package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/client"
)

func buildDocbank(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "docbank")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, "go.kenn.io/docbank/cmd/docbank")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

func TestLifecycleStartStatusStop(t *testing.T) {
	bin := buildDocbank(t)
	dir := t.TempDir()
	env := append(os.Environ(), "DOCBANK_HOME="+dir)

	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// status/stop before any daemon: report, don't spawn.
	out, err := run("serve", "status")
	assert.Error(t, err) // exit 1 when not running
	assert.Contains(t, out, "not running")
	recs, lerr := client.RuntimeStore(dir).List()
	require.NoError(t, lerr)
	assert.Empty(t, recs, "status must not autostart")

	out, err = run("serve", "start")
	require.NoError(t, err, out)
	out, err = run("serve", "status")
	require.NoError(t, err, out)
	assert.Contains(t, out, "running")

	// A CLI command goes through the running daemon.
	out, err = run("ls", "/")
	require.NoError(t, err, out)

	out, err = run("serve", "stop")
	require.NoError(t, err, out)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, found, err := client.Find(ctx, dir)
	require.NoError(t, err)
	assert.False(t, found)
}
```

(The `ls` line only passes after Task 11 — add that single assertion in Task 11, and keep this test free of the `ls` block for now.)

Also write the create-time unit test (spec requirement: a reused-PID record is treated as dead):

```go
// internal/client/ensure_test.go
//go:build unix

package client

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateTimeMatches(t *testing.T) {
	rec := NewRecord("127.0.0.1:1", "tok")
	require.Equal(t, os.Getpid(), rec.PID)
	assert.True(t, createTimeMatches(rec), "own record must match")

	// Simulate PID reuse: same live PID, different recorded create time.
	rec.Metadata[metaCreateTime] = strconv.FormatInt(1, 10)
	assert.False(t, createTimeMatches(rec), "mismatched create_time must read as dead")

	// Records without the key (older daemons) match trivially.
	delete(rec.Metadata, metaCreateTime)
	assert.True(t, createTimeMatches(rec))
}
```

- [ ] **Step 3: Implement serve_lifecycle.go**

```go
// cmd/docbank/cmd/serve_lifecycle.go
package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
)

var serveStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon in the background",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		if err := layout.Ensure(); err != nil {
			return err
		}
		if rec, _, ok, _ := client.Find(cmd.Context(), layout.Root); ok {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "already running: pid %d at %s (%s)\n",
				rec.PID, rec.Address, rec.Version)
			return nil
		}
		rec, err := client.Start(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "started: pid %d at %s (%s)\n",
			rec.PID, rec.Address, rec.Version)
		return nil
	},
}

var serveStatusJSON bool

var serveStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report the daemon's status (never starts one)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		rec, info, ok, err := client.Find(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if serveStatusJSON {
			out := map[string]any{"running": ok}
			if ok {
				out["pid"] = rec.PID
				out["address"] = rec.Address
				out["version"] = info.Version
				out["started_at"] = rec.StartedAt.Format(time.RFC3339)
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(out) //nolint:wrapcheck // direct CLI output
		}
		if !ok {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "daemon not running")
			return fmt.Errorf("daemon not running")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "running: pid %d at %s (%s), up %s\n",
			rec.PID, rec.Address, info.Version, time.Since(rec.StartedAt).Round(time.Second))
		return nil
	},
}

var serveStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon (never starts one)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		stopped, err := client.Stop(cmd.Context(), layout.Root)
		if err != nil {
			return err
		}
		if !stopped {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no daemon running")
			return nil
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "stopped")
		return nil
	},
}

func init() {
	serveStatusCmd.Flags().BoolVar(&serveStatusJSON, "json", false, "machine-readable output")
	serveCmd.AddCommand(serveStartCmd, serveStatusCmd, serveStopCmd)
}
```

The "not running" text and non-zero exit: the test greps `not running` — the human line says `daemon not running`, which contains it. The JSON path exits 0 either way (agents branch on `running`).

- [ ] **Step 4: Run** — `go test -tags fts5 ./cmd/... -run Lifecycle -v` (slow: builds the binary). Then full suite + lint. Expected: PASS.

- [ ] **Step 5: Commit** — subject: `Add daemon discovery, auto-start, and serve lifecycle commands`.

---

### Task 11: Rewrite the CLI over the client; remove direct-store plumbing

Every Phase 1 command keeps its exact UX (names, flags, output bytes) but talks to the daemon. This task also deletes the code the daemon made dead.

**Files:**
- Modify: `cmd/docbank/cmd/{add,ls,tree,cat,mv,rm,restore,search,trash,gc,verify}.go`, `cmd/docbank/cmd/cli_test.go`, `cmd/docbank/cmd/serve_lifecycle_test.go` (add the `ls` assertion from Task 10)
- Delete: `cmd/docbank/cmd/vault.go`
- Modify: `internal/store/write.go` (delete `MovePath`, `resolveMoveTargetTx`), `internal/store/trash.go` (delete `TrashPath`), their tests; `internal/home/lock.go` + `lock_stub.go` (delete `AcquireLock`, `TryUpgrade`, `Downgrade` and their tests — `TryLockExclusive` + `Release` is the whole lock API now)

**Interfaces:**
- Consumes: `client.Ensure` (Task 10) and every client method (Task 8).
- Pattern for every command:

```go
c, err := client.Ensure(cmd.Context())
if err != nil {
	return err
}
```

- [ ] **Step 1: Convert the test harness first**

Replace `setupVaultHome` in `cli_test.go` so every existing CLI test runs against a real in-process daemon (full client path incl. discovery):

```go
func setupVaultHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	startTestDaemon(t, dir)
	return dir
}

// startTestDaemon runs runServe in-process against dir and tears it down
// with the test. CLI commands discover it through the runtime record like
// production; no test-only transport exists.
func startTestDaemon(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("test daemon did not shut down")
		}
	})
	require.Eventually(t, func() bool {
		_, _, ok, err := client.Find(ctx, dir)
		return err == nil && ok
	}, 10*time.Second, 25*time.Millisecond, "test daemon never became ready")
}
```

Tests that opened the store directly to assert on state keep working: the daemon and the test can both open the SQLite file (WAL, multi-process safe); only the flock is exclusive, and `store.Open` doesn't take it.

- [ ] **Step 2: Run the existing CLI tests to see the failure mode** — `go test -tags fts5 ./cmd/... -v 2>&1 | head -50`. Expected: commands still work (they still call openVault... which now fails: the daemon holds the exclusive lock and `openVault` uses the OLD shared-lock API — this task's whole point). Every CLI test should FAIL with a lock error, proving the harness is now daemon-backed.

- [ ] **Step 3: Rewrite the commands**

Each one preserved verbatim in output. The complete new bodies:

`add.go` — sources become absolute client-side (the daemon has no meaningful cwd):

```go
RunE: func(cmd *cobra.Command, args []string) error {
	abs := make([]string, len(args))
	for i, a := range args {
		p, err := filepath.Abs(a)
		if err != nil {
			return fmt.Errorf("resolving %q: %w", a, err)
		}
		abs[i] = p
	}
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	rep, err := c.Ingest(cmd.Context(), abs, addDest)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "added: %d  skipped: %d  failed: %d\n",
		rep.Added, rep.Skipped, len(rep.Failed))
	for _, f := range rep.Failed {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s: %s\n", f.Path, f.Error)
	}
	if len(rep.Failed) > 0 {
		return fmt.Errorf("%d file(s) failed to import", len(rep.Failed))
	}
	return nil
},
```

(Output-compat check: Phase 1 printed `failed: %s: %v\n` with the error value — `%s` of the string field renders identically.)

`ls.go`:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	path := "/"
	if len(args) == 1 {
		path = args[0]
	}
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	dir, err := c.Stat(cmd.Context(), path)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", path, err)
	}
	kids, err := c.Children(cmd.Context(), dir.ID)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tKIND\tSIZE\tMODIFIED\tNAME")
	for _, k := range kids {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%d\t%s\t%s\n", k.ID, k.Kind, k.Size, k.ModifiedAt, k.Name)
	}
	return w.Flush() //nolint:wrapcheck // direct CLI output
},
```

`tree.go` — same shape: `Stat` the root path (error `%s: %w` with `store.ErrNotDir` when `Kind != "dir"`), then a recursive `printTree(ctx, w, c, dirID, depth)` over `c.Children`, byte-identical output format (`%s%s  [%d]\n`).

`cat.go`:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	n, err := c.Stat(cmd.Context(), args[0])
	if err != nil {
		return fmt.Errorf("resolving %q: %w", args[0], err)
	}
	if n.Kind == "dir" {
		return fmt.Errorf("%q: %w", args[0], store.ErrNotFile)
	}
	rc, err := c.Content(cmd.Context(), n.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.Copy(cmd.OutOrStdout(), rc); err != nil {
		return fmt.Errorf("streaming %q: %w", args[0], err)
	}
	return nil
},
```

`mv.go` — replicates the deleted `MovePath` destination semantics client-side (existing live dir → move into, keep name; existing file → `ErrExists`; else parent must exist and basename is the new name; every segment validated so dot-segments are rejected, not Cleaned). The If-Match revision from the source stat makes the two-step resolve-then-move safe against concurrent renames of the source; a concurrently swapped destination directory fails in the store with a clean typed error:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	src, err := c.Stat(ctx, args[0])
	if err != nil {
		return fmt.Errorf("resolving %q: %w", args[0], err)
	}
	newParentID, newName, err := resolveMoveDest(ctx, c, args[1], src.Name)
	if err != nil {
		return err
	}
	moved, err := c.Move(ctx, src.ID, src.Revision, &newParentID, &newName)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%d] %s\n", moved.ID, moved.Path)
	return nil
},
```

```go
// resolveMoveDest mirrors the store-side MovePath rules this CLI used
// before the daemon: an existing live directory means "move into, keep
// name"; an existing file is a conflict; otherwise the parent must exist
// and the basename becomes the new name. Segments are validated literally
// (no dot-segment Cleaning) via the same NormalizeName the server applies.
func resolveMoveDest(ctx context.Context, c *client.Client, destPath, keepName string) (int64, string, error) {
	segs := splitVirtualPath(destPath)
	for i, seg := range segs {
		norm, err := store.NormalizeName(seg)
		if err != nil {
			return 0, "", fmt.Errorf("destination %q: %w", destPath, err)
		}
		segs[i] = norm
	}
	if dest, err := c.Stat(ctx, destPath); err == nil {
		if dest.Kind == "dir" {
			return dest.ID, keepName, nil
		}
		return 0, "", fmt.Errorf("destination %q: %w", destPath, store.ErrExists)
	} else if !errors.Is(err, store.ErrNotFound) {
		return 0, "", fmt.Errorf("resolving destination %q: %w", destPath, err)
	}
	if len(segs) == 0 {
		return 0, "", fmt.Errorf("destination %q: %w", destPath, store.ErrExists)
	}
	parentPath := "/" + strings.Join(segs[:len(segs)-1], "/")
	parent, err := c.Stat(ctx, parentPath)
	if err != nil {
		return 0, "", fmt.Errorf("resolving destination parent %q: %w", parentPath, err)
	}
	return parent.ID, segs[len(segs)-1], nil
}

// splitVirtualPath is store.splitPath's behavior for the CLI side: "/a/b/"
// → ["a","b"]; "" and "/" → nil.
func splitVirtualPath(path string) []string {
	var segs []string
	for seg := range strings.SplitSeq(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	return segs
}
```

`rm.go`:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	n, err := c.Stat(cmd.Context(), args[0])
	if err != nil {
		return fmt.Errorf("resolving %q: %w", args[0], err)
	}
	if _, err := c.Trash(cmd.Context(), n.ID, n.Revision); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"trashed [%d] %s (restore with: docbank restore %d)\n", n.ID, args[0], n.ID)
	return nil
},
```

`restore.go`: parse id as before → `n, err := c.Node(ctx, id)` → `restored, err := c.Restore(ctx, id, n.Revision)` → print `"restored [%d] %s\n"` with `restored.Path`.

`search.go`: `hits, err := c.Search(ctx, strings.Join(args, " "), 50)` → identical no-matches/tabwriter output using `h.Node.ID` and `h.Path`.

`trash.go`: list → `c.TrashList(ctx)`, printing `n.TrashedAt` (already a plain string in the DTO — empty when absent) and `n.Name`; empty → validate `trashOlderThan` client-side with `api.ParseAge` for a fast local error, then `c.TrashEmpty(ctx, trashOlderThan)` → same `"deleted %d trashed node(s)\n"`.

`gc.go` — full replacement (the daemon runs the logic; `untrackedBlobFiles` dies with vault.go):

```go
RunE: func(cmd *cobra.Command, args []string) error {
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	rep, err := c.GC(cmd.Context(), gcRun)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"%d candidate blob(s), %d untracked file(s), %d byte(s) reclaimable\n",
		rep.CandidateBlobs, rep.UntrackedFiles, rep.ReclaimableBytes)
	if !gcRun {
		if rep.CandidateBlobs+rep.UntrackedFiles > 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "dry run — pass --run to delete")
		}
		return nil
	}
	if rep.Removed > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "reclaimed %d blob(s), %d byte(s)\n",
			rep.Removed, rep.ReclaimableBytes)
	}
	return nil
},
```

(Behavior check against the old gc: old code skipped the "reclaimed" line when there was nothing to delete — `rep.Removed > 0` preserves that.)

`verify.go`:

```go
RunE: func(cmd *cobra.Command, args []string) error {
	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	rep, err := c.Verify(cmd.Context())
	if err != nil {
		return err
	}
	for _, p := range rep.Problems {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", p.Problem, p.Hash)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d blob(s) ok, %d problem(s)\n",
		rep.OK, len(rep.Problems))
	if len(rep.Problems) > 0 {
		return fmt.Errorf("verify found %d problem(s)", len(rep.Problems))
	}
	return nil
},
```

- [ ] **Step 4: Delete the dead code**

- `trash rm -f` is wrong — use `git rm cmd/docbank/cmd/vault.go`.
- `internal/store/write.go`: delete `MovePath` and `resolveMoveTargetTx`; drop the now-unused `ifRev`-less path if any. Delete their tests (`rg -ln 'MovePath|TrashPath' internal/store` → prune matching test funcs).
- `internal/store/trash.go`: delete `TrashPath`.
- `internal/home`: delete `AcquireLock`, `TryUpgrade`, `Downgrade` from `lock.go` and `lock_stub.go`; prune their tests. `Lock` keeps only what `TryLockExclusive`/`Release` need — if the struct carried shared-lock state, simplify it.
- `rg -n 'openVault|AcquireLock|MovePath|TrashPath|untrackedBlobFiles' --type go` must return only historical docs/plans afterward.

- [ ] **Step 5: Add the `ls`-through-daemon assertion to the lifecycle test** (from Task 10's note): after `serve start` succeeds, `run("ls", "/")` must succeed.

- [ ] **Step 6: Run everything** — `go test -tags fts5 ./... && make lint`. Every pre-existing CLI test must pass with unchanged expectations (byte-identical output). Also run `GOOS=windows go vet -tags fts5 ./internal/...` to keep the Windows CI leg honest.

- [ ] **Step 7: Commit** — subject: `Route all CLI commands through the daemon; remove direct-store plumbing`.

---

### Task 12: `docbank openapi` — offline OpenAPI document

**Files:**
- Create: `internal/api/openapi.go`, `cmd/docbank/cmd/openapi.go`
- Test: `internal/api/openapi_test.go`

**Interfaces:**
- Produces: `api.OpenAPIYAML() ([]byte, error)`; command `docbank openapi [--json]` printing the document to stdout with no daemon, no DB, no socket.

- [ ] **Step 1: Write the failing test**

```go
// internal/api/openapi_test.go
package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestOpenAPIDocumentOffline(t *testing.T) {
	// No store, no blobs, no listener: registration must not touch deps.
	out, err := api.OpenAPIYAML()
	require.NoError(t, err)
	doc := string(out)
	for _, op := range []string{"getNode", "resolvePath", "listChildren", "getNodeContent",
		"search", "createNode", "moveNode", "trashNode", "restoreNode",
		"ingest", "listTrash", "emptyTrash", "gc", "verify"} {
		assert.Contains(t, doc, op, "operation missing from OpenAPI doc")
	}
	assert.NotContains(t, doc, "/api/daemon/shutdown", "lifecycle plumbing must stay hidden")
}
```

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/api/ -run OpenAPI -v`. Expected: FAIL (undefined `OpenAPIYAML`).

- [ ] **Step 3: Implement**

```go
// internal/api/openapi.go
package api

import (
	"fmt"

	"go.kenn.io/docbank/internal/config"
)

// OpenAPIYAML renders the API contract without binding a socket or opening
// a vault: handlers are registered but never invoked. `docbank openapi`
// (and doc tooling) call this offline.
func OpenAPIYAML() ([]byte, error) {
	s := NewServer(Deps{Cfg: config.Default()})
	out, err := s.API().OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("rendering OpenAPI document: %w", err)
	}
	return out, nil
}
```

```go
// cmd/docbank/cmd/openapi.go
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
)

var openapiJSON bool

var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the HTTP API's OpenAPI document (no daemon needed)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if openapiJSON {
			s := api.NewServer(api.Deps{})
			out, err := s.API().OpenAPI().MarshalJSON()
			if err != nil {
				return fmt.Errorf("rendering OpenAPI document: %w", err)
			}
			_, _ = cmd.OutOrStdout().Write(out)
			return nil
		}
		out, err := api.OpenAPIYAML()
		if err != nil {
			return err
		}
		_, _ = cmd.OutOrStdout().Write(out)
		return nil
	},
}

func init() {
	openapiCmd.Flags().BoolVar(&openapiJSON, "json", false, "emit JSON instead of YAML")
	rootCmd.AddCommand(openapiCmd)
}
```

If `NewServer(Deps{})` panics on the zero-value config (web enabled reads `Cfg.Web.Enabled` — false for zero value, fine), pass `config.Default()` in both places for symmetry. Check huma's OpenAPI type for the exact JSON method (`YAML()` exists; JSON may be `MarshalJSON` via `json.Marshal(oapi)`) — adjust to what compiles.

- [ ] **Step 4: Run tests + a manual check** — `go test -tags fts5 ./internal/api/ -run OpenAPI -v && go run -tags fts5 ./cmd/docbank openapi | head -20`. Expected: PASS; YAML on stdout.

- [ ] **Step 5: Commit** — subject: `Add docbank openapi for offline contract dumps`.

---

### Task 13: `docbank update` — self-update with daemon coordination

**Files:**
- Create: `internal/update/update.go`, `cmd/docbank/cmd/update.go`
- Test: `internal/update/update_test.go`

**Interfaces:**
- Consumes: `kit/selfupdate` (`Client`, `CheckOptions`, `InstallOptions`, `Info`, `EnvironmentGitHubToken`, `IsNewer`, `IsDevBuildVersion`, `FormatSize`), `client.Find/Start/Stop` (Task 10), `version.Version`.
- Produces:

```go
package update
func NewClient(cacheDir string) selfupdate.Client   // kenn-io/docbank wiring
func Run(ctx context.Context, out io.Writer, opts Options) error
type Options struct {
	CheckOnly bool
	Yes       bool
	Force     bool
	Confirm   func(prompt string) (bool, error) // nil with Yes=false → error (non-interactive)
	// test seams:
	Client       *selfupdate.Client // nil → NewClient(default cache dir)
	Root         string             // vault root for daemon coordination; "" → home.Resolve()
	Destination  string             // binary path; "" → os.Executable()
}
```

- [ ] **Step 1: Write the failing test** — fake release server end-to-end (kit's client accepts base-URL overrides):

```go
// internal/update/update_test.go
package update_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/docbank/internal/update"
)

// buildRelease returns a tar.gz containing a fake "docbank" binary and its
// sha256, using kit's DefaultAssetName so naming matches production.
func buildRelease(t *testing.T, version string) (assetName string, archive []byte, sum string) {
	t.Helper()
	content := []byte("#!/bin/sh\necho docbank " + version + "\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "docbank", Mode: 0o755, Size: int64(len(content))}))
	_, err := tw.Write(content)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	name := selfupdate.DefaultAssetName(selfupdate.AssetRequest{
		BinaryName: "docbank", Version: version, GOOS: "linux", GOARCH: "amd64", Extension: ".tar.gz",
	})
	h := sha256.Sum256(buf.Bytes())
	return name, buf.Bytes(), hex.EncodeToString(h[:])
}

func TestUpdateInstallsFromFakeRelease(t *testing.T) {
	asset, archive, sum := buildRelease(t, "9.9.9")
	mux := http.NewServeMux()
	mux.HandleFunc("/kenn-io/docbank/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/kenn-io/docbank/releases/tag/v9.9.9", http.StatusFound)
	})
	mux.HandleFunc("/kenn-io/docbank/releases/download/v9.9.9/"+asset,
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/kenn-io/docbank/releases/download/v9.9.9/SHA256SUMS",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, "%s  %s\n", sum, asset)
		})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	c := update.NewClient(t.TempDir())
	c.GitHubWebBaseURL = ts.URL
	c.GitHubAPIBaseURL = ts.URL // fallback never used; keep it off the real network
	c.CurrentVersion = "0.0.1"

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		Yes: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err, out.String())
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Contains(t, string(got), "9.9.9")
	assert.Contains(t, out.String(), "9.9.9")
}

func TestUpdateCheckOnly(t *testing.T) {
	ts, _ := fakeReleaseServer(t, "9.9.9")
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "docbank")
	require.NoError(t, os.WriteFile(dest, []byte("old"), 0o755))

	c := update.NewClient(t.TempDir())
	c.GitHubWebBaseURL = ts.URL
	c.GitHubAPIBaseURL = ts.URL
	c.CurrentVersion = "0.0.1"

	var out strings.Builder
	err := update.Run(t.Context(), &out, update.Options{
		CheckOnly: true, Client: &c, Root: t.TempDir(), Destination: dest,
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "9.9.9")
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "old", string(got), "check-only must not install")
}
```

Factor the mux/handler setup from `TestUpdateInstallsFromFakeRelease` into `fakeReleaseServer(t *testing.T, version string) (*httptest.Server, string /* checksum */)` so both tests share it (build it that way from the start in Step 1). Before writing the fake handlers, read kit's discovery flow (`$(go env GOMODCACHE)/go.kenn.io/kit@*/selfupdate/selfupdate.go`) and mirror the exact URL shapes it requests (releases/latest redirect → tag → download URLs). Adjust handler paths to what the client actually fetches; the test must pass against real kit behavior, not assumed behavior.

- [ ] **Step 2: Verify failure** — `go test -tags fts5 ./internal/update/ -v`. Expected: FAIL.

- [ ] **Step 3: Implement internal/update/update.go**

```go
// Package update wraps kit/selfupdate with docbank's release identity and
// daemon-aware install: stop the daemon, swap the binary, restart it.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

func NewClient(cacheDir string) selfupdate.Client {
	return selfupdate.Client{
		Owner: "kenn-io", Repo: "docbank", BinaryName: "docbank",
		CurrentVersion:         version.Version,
		CacheDir:               cacheDir,
		GitHubToken:            selfupdate.EnvironmentGitHubToken(),
		AllowUnsignedChecksums: true,
	}
}

type Options struct {
	CheckOnly   bool
	Yes         bool
	Force       bool
	Confirm     func(prompt string) (bool, error)
	Client      *selfupdate.Client
	Root        string
	Destination string
}

func Run(ctx context.Context, out io.Writer, opts Options) error {
	c := opts.Client
	if c == nil {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		built := NewClient(filepath.Join(layout.Root, "cache", "update"))
		c = &built
	}
	root := opts.Root
	if root == "" {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		root = layout.Root
	}

	info, err := c.Check(ctx, selfupdate.CheckOptions{Force: opts.Force})
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	_, _ = fmt.Fprintf(out, "current: %s\nlatest:  %s\n", c.CurrentVersion, info.LatestVersion)
	if !selfupdate.IsNewer(c.CurrentVersion, info.LatestVersion) && !opts.Force {
		_, _ = fmt.Fprintln(out, "already up to date")
		return nil
	}
	if selfupdate.IsDevBuildVersion(c.CurrentVersion) && !opts.Force {
		_, _ = fmt.Fprintln(out, "dev build: pass --force to replace it")
		return nil
	}
	if opts.CheckOnly {
		return nil
	}
	if info.NeedsRefetch() {
		if info, err = c.Check(ctx, selfupdate.CheckOptions{Force: true}); err != nil {
			return fmt.Errorf("refreshing release info: %w", err)
		}
	}
	if info.Checksum == "" {
		return errors.New("release has no SHA256 checksum; refusing to install")
	}
	_, _ = fmt.Fprintf(out, "download: %s (%s, sha256 %s)\n",
		info.AssetName, selfupdate.FormatSize(info.Size), info.Checksum)
	if !opts.Yes {
		if opts.Confirm == nil {
			return errors.New("confirmation required: pass --yes in non-interactive use")
		}
		ok, err := opts.Confirm(fmt.Sprintf("install %s?", info.LatestVersion))
		if err != nil || !ok {
			return err
		}
	}

	dest := opts.Destination
	if dest == "" {
		if dest, err = os.Executable(); err != nil {
			return fmt.Errorf("resolving current executable: %w", err)
		}
	}

	// Daemon coordination: a running daemon serves from the old binary and
	// would version-mismatch every CLI call after the swap. Stop, install,
	// restart with the new executable.
	wasRunning, err := client.Stop(ctx, root)
	if err != nil {
		return fmt.Errorf("stopping daemon before update: %w", err)
	}
	if wasRunning {
		_, _ = fmt.Fprintln(out, "stopped running daemon")
	}
	if err := c.Install(ctx, info, selfupdate.InstallOptions{DestinationPath: dest}); err != nil {
		if wasRunning {
			if _, rerr := client.Start(ctx, root); rerr != nil {
				return fmt.Errorf("install failed (%w) and daemon restart failed: %w", err, rerr)
			}
			_, _ = fmt.Fprintln(out, "install failed; restarted previous daemon")
		}
		return fmt.Errorf("installing %s: %w", info.LatestVersion, err)
	}
	_, _ = fmt.Fprintf(out, "installed %s → %s\n", info.LatestVersion, dest)
	if wasRunning {
		if _, err := client.Start(ctx, root); err != nil {
			return fmt.Errorf("daemon restart after update failed (start it with docbank serve start): %w", err)
		}
		_, _ = fmt.Fprintln(out, "daemon restarted")
	}
	return nil
}
```

Restart nuance: `client.Start` re-execs `os.Executable()` of the CURRENT process — after the swap that path holds the new binary, which is exactly what we want. Verify `InstallOptions`/`Info` field names against kit source; if `Install` needs `ArchiveBinaryName`, pass `"docbank"`.

- [ ] **Step 4: Implement cmd/docbank/cmd/update.go**

```go
package cmd

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/update"
)

var (
	updateCheck bool
	updateYes   bool
	updateForce bool
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update docbank to the latest GitHub release",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return update.Run(cmd.Context(), cmd.OutOrStdout(), update.Options{
			CheckOnly: updateCheck,
			Yes:       updateYes,
			Force:     updateForce,
			Confirm: func(prompt string) (bool, error) {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil {
					return false, fmt.Errorf("reading confirmation: %w", err)
				}
				return strings.EqualFold(strings.TrimSpace(line), "y"), nil
			},
		})
	},
}

func init() {
	updateCmd.Flags().BoolVar(&updateCheck, "check", false, "check only; do not install")
	updateCmd.Flags().BoolVar(&updateYes, "yes", false, "install without confirmation")
	updateCmd.Flags().BoolVar(&updateForce, "force", false, "install even if up to date or a dev build")
	rootCmd.AddCommand(updateCmd)
}
```

- [ ] **Step 5: Run tests** — `go test -tags fts5 ./internal/update/ -v && go test -tags fts5 ./... && make lint`. Expected: PASS.

- [ ] **Step 6: Commit** — subject: `Add docbank update via kit selfupdate with daemon coordination`.

---

### Task 14: Release workflow

**Files:**
- Create: `.github/workflows/release.yml`

**Interfaces:**
- Consumes: Makefile-compatible build (CGO, `fts5` tag, `internal/version` ldflags from Task 1).
- Produces: on tag `v*`, GitHub release with `docbank_<version>_<os>_<arch>.tar.gz` for linux/darwin × amd64/arm64 plus `SHA256SUMS` — the exact assets `kit/selfupdate` discovers (Task 13's naming test already pins `DefaultAssetName` agreement).

- [ ] **Step 1: Confirm asset naming against kit** — `rg -n "func DefaultAssetName" -A 10 $(go env GOMODCACHE)/go.kenn.io/kit@*/selfupdate/selfupdate.go`. The archive names in the workflow below must match its output for `BinaryName=docbank` (expected `docbank_<version>_<goos>_<goarch>.tar.gz`; adjust if kit says otherwise). Also confirm the checksum filename kit reads (`ChecksumAssetNames` default — expected `SHA256SUMS`).

- [ ] **Step 2: Resolve action SHAs** — reuse the already-pinned SHAs from `.github/workflows/ci.yml` for `actions/checkout` and `actions/setup-go`. For upload/download-artifact, resolve current SHAs (never trust memory):

```bash
gh api repos/actions/upload-artifact/releases/latest --jq .tag_name
gh api repos/actions/upload-artifact/git/ref/tags/<tag> --jq .object.sha
gh api repos/actions/download-artifact/releases/latest --jq .tag_name
gh api repos/actions/download-artifact/git/ref/tags/<tag> --jq .object.sha
```

(If the tag object is annotated, dereference once more: `gh api repos/actions/upload-artifact/git/tags/<sha> --jq .object.sha`.)

- [ ] **Step 3: Write the workflow**

```yaml
# .github/workflows/release.yml
name: release

on:
  push:
    tags: ["v*"]

permissions:
  contents: read

jobs:
  build:
    strategy:
      matrix:
        include:
          - { runner: ubuntu-latest,     goos: linux,  goarch: amd64 }
          - { runner: ubuntu-24.04-arm,  goos: linux,  goarch: arm64 }
          - { runner: macos-13,          goos: darwin, goarch: amd64 }
          - { runner: macos-latest,      goos: darwin, goarch: arm64 }
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0
        with:
          persist-credentials: false
      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0
        with:
          go-version-file: go.mod
      - name: Build
        env:
          CGO_ENABLED: "1"
        run: |
          set -euo pipefail
          VERSION="${GITHUB_REF#refs/tags/v}"
          COMMIT="$(git rev-parse --short HEAD)"
          LDFLAGS="-X go.kenn.io/docbank/internal/version.Version=${VERSION} \
                   -X go.kenn.io/docbank/internal/version.Commit=${COMMIT}"
          go build -tags fts5 -trimpath -ldflags "${LDFLAGS}" -o dist/docbank ./cmd/docbank
          ./dist/docbank --version
      - name: Package
        run: |
          set -euo pipefail
          VERSION="${GITHUB_REF#refs/tags/v}"
          ARCHIVE="docbank_${VERSION}_${{ matrix.goos }}_${{ matrix.goarch }}.tar.gz"
          tar -C dist -czf "${ARCHIVE}" docbank
          mkdir -p out && mv "${ARCHIVE}" out/
      - uses: actions/upload-artifact@<resolved-sha>  # vX.Y.Z
        with:
          name: docbank-${{ matrix.goos }}-${{ matrix.goarch }}
          path: out/*.tar.gz
          if-no-files-found: error

  publish:
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: write
    steps:
      - uses: actions/download-artifact@<resolved-sha>  # vX.Y.Z
        with:
          path: assets
          merge-multiple: true
      - name: Checksums
        run: |
          set -euo pipefail
          cd assets
          sha256sum ./*.tar.gz > SHA256SUMS
          cat SHA256SUMS
      - name: Create release
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail
          TAG="${GITHUB_REF#refs/tags/}"
          gh release create "${TAG}" assets/* \
            --repo "${GITHUB_REPOSITORY}" \
            --title "${TAG}" \
            --generate-notes
```

The checkout/setup-go pins above are ci.yml's existing pins (verify they still match ci.yml). Replace the two `<resolved-sha>` placeholders with the 40-char SHAs resolved in Step 2 and put the real version in each `# vX.Y.Z` comment. Native runners per arch mean no cross-compilation — matching msgvault, required by CGO. `macos-13` is the last x86_64 macOS runner; if GitHub has retired it by execution time, drop the darwin/amd64 leg and note it in the commit message rather than shipping an untested cross-compile.

- [ ] **Step 4: Lint the workflow** — `actionlint .github/workflows/release.yml && zizmor .github/workflows/release.yml`. Fix everything (zizmor will insist on the `persist-credentials: false` and minimal permissions already present).

- [ ] **Step 5: Commit** — subject: `Add tag-driven release workflow producing selfupdate assets`.

---

### Task 15: Documentation

**Files:**
- Create: `docs/architecture/daemon.md`
- Modify: `docs/configuration.md`, `docs/architecture/http-api.md`, `docs/architecture/locking.md`, `docs/cli-reference.md`, `docs/quickstart.md`, `docs/roadmap.md`, `docs/changelog.md`, `docs/zensical.toml`

Zensical rules from Phase 1 apply: user-facing pages describe only what exists; anything designed-but-absent sits under an explicit `!!! info "Planned"` admonition; `make docs-build` runs strict and must pass.

- [ ] **Step 1: Write docs/architecture/daemon.md**

Front matter + sections (write full prose; the content contract is):

- **Why a daemon** — one process owns SQLite and the blob store; the CLI, agents, and the future web UI are all HTTP clients of the same API; design test: the CLI proves the agent surface is sufficient because it has no other path.
- **Lifecycle** — `docbank serve` (foreground), `serve start/stop/status`; exclusive vault flock; blob tmp cleanup at startup; graceful shutdown (drain, close, release, remove record).
- **Discovery** — runtime record in `$DOCBANK_HOME` (service, version, address, PID, `create_time` + shutdown token in metadata); probe of `/api/ping`; PID-reuse guard via create-time; exact version match with automatic stop-and-replace on mismatch; launch lock against racing starters.
- **Auto-start and idle shutdown** — every CLI command ensures a daemon; background daemons exit after `idle_timeout` of quiet; foreground never idles out; `serve status`/`stop` never start one.
- **Logs** — `$DOCBANK_HOME/logs/` JSON daily files for background daemons; stderr foreground.

- [ ] **Step 2: Rewrite docs/configuration.md** — replace the "Planned configuration" admonition with real `config.toml` documentation: the annotated TOML block from the spec (all defaults shown), the bind/key validation rules (keyless = loopback-only; public binds always refused), `logs/` now real, and the launch/runtime-record files added to the directory-layout listing.

- [ ] **Step 3: Update docs/architecture/http-api.md** — flip the page banner from "Planned — Phase 2" to implemented-with-exceptions; update the endpoint table: `GET /path?path=` query form (with the encoding rationale), implemented rows marked, editing/tags/batch/multipart rows kept under a Planned admonition; add the If-Match-per-endpoint table from the spec (412/428 semantics); add the `POST /ingest` addendum section (server-side paths, loopback-only fence, multipart planned); error table gains the `code` extension member documentation and the `stale_revision`/`precondition_required`/`loopback_only` rows.

- [ ] **Step 4: Update docs/architecture/locking.md** — single-lock-holder model: the daemon holds the exclusive flock for its lifetime; the shared/exclusive split and gc's exclusive acquisition are replaced by the in-daemon maintenance gate; `TryLockExclusive` semantics (second daemon refused, not blocked).

- [ ] **Step 5: Update docs/cli-reference.md** — add `serve` (+ `start/stop/status --json`), `update` (`--check/--yes/--force`), `openapi` (`--json`); note under every data command that it talks to the auto-started daemon; document `DOCBANK_LOG_LEVEL`.

- [ ] **Step 6: Update docs/quickstart.md, docs/roadmap.md, docs/changelog.md** — quickstart: unchanged flow plus a short "the daemon" paragraph (first command starts it; `docbank serve status` to see it). Roadmap: split Phase 2 into "2a Infrastructure — Implemented" (this plan's bullet list) and "2b Features — Designed" (editing, tags, watched inboxes, extraction). Changelog: one entry summarizing daemon/API/client/update/release.

- [ ] **Step 7: Wire navigation** — `docs/zensical.toml` nav: add `{"Daemon" = "architecture/daemon.md"}` to the Design section (after Concurrency & Locking).

- [ ] **Step 8: Build strict** — `make docs-build`. Expected: clean. Fix every warning.

- [ ] **Step 9: Commit** — subject: `Document the daemon, config.toml, and implemented API surface`.

---

## Final Verification (after all tasks)

- [ ] `go test -tags fts5 ./...` — full suite green, including the slow lifecycle integration test (not under `-short`).
- [ ] `make lint` — zero findings.
- [ ] `GOOS=windows go vet -tags fts5 ./internal/...` — Windows CI leg stays green (stubs compile).
- [ ] `make docs-build` — strict docs build green.
- [ ] Manual smoke: `make build && DOCBANK_HOME=$(mktemp -d) ./docbank add README.md && DOCBANK_HOME=... ./docbank ls /inbox && ./docbank serve status && ./docbank serve stop` (same DOCBANK_HOME throughout) — first command auto-starts the daemon; stop leaves no records; `curl <addr>/docs` renders while running.
- [ ] `rg -n 'openVault|AcquireLock\(|MovePath|TrashPath' --type go` — no live-code matches.
- [ ] Open a PR from `phase2-infrastructure` to `main`.

## Deviations Log

Executors: when reality contradicts this plan (kit signature differs, huma behavior differs, a runner image is gone), fix forward, note the deviation in the task's commit message, and update this plan file in the same commit.






- Task 13: the brief's `Run` pseudocode dereferenced `info.LatestVersion` where kit's `Check` returns `nil, nil` when up to date, and its Step-1 test needed a TLS test server, a `/releases/tag/` handler, and a runtime-GOOS/GOARCH asset to match real kit behavior. Implemented against the actual kit contract.
- Task 14: darwin/amd64 leg dropped per this plan's own contingency — GitHub retired macos-13, the last free x86_64 macOS runner.
- Final Verification: the smoke test imports `docs/index.md` — the repo has no root `README.md`. The `MovePath|TrashPath` grep is intentionally non-empty since commit 4900f7a restored them as transactional store methods backing `POST /api/v1/path/move` and `/path/trash`; the grep still proves `openVault`/`AcquireLock` are gone. `GOOS=windows go vet` fails on `internal/store` locally on any branch including main (cgo cross-compile without a Windows toolchain); the native Windows CI leg is the authoritative check.
