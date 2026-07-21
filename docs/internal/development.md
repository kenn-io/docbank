# Development guide

This page routes changes through docbank's architecture so agents and
developers update the right layer and preserve cross-layer contracts.

## Package ownership

| Area | Owns | Must not own |
|------|------|--------------|
| `internal/store` | schema, transactions, tree identity, reachability queries, typed domain errors | HTTP, CLI formatting, physical pack lifecycle |
| `internal/blob` | docbank adapter around Kit physical storage | virtual paths or node policy |
| `internal/backupapp` | frozen logical enumeration, fidelity stats, restore paths, mixed backup reads, packed restore policy | repository orchestration or CLI policy |
| `internal/ingest` | source traversal and bytes-before-reference pipeline | daemon discovery or UI behavior |
| `internal/api` | wire contract, auth, middleware, gate classification, error mapping | direct CLI output policy |
| `internal/client` | typed HTTP calls and daemon convergence | opening SQLite or blobs |
| `internal/home` | vault layout, privacy, and portable vault/tree locking | data operations |
| `internal/config` | strict config parsing and security validation | runtime discovery |
| `cmd/docbank` | Cobra ergonomics and human output | store business logic |
| `pkg` | lifecycle and bounded public operations for one exclusively owned embedded vault | standalone CLI paths or a second storage implementation |

## Common change paths

### Add a data operation

Start with the transactional store operation and tests. Add the API route and
wire types, classify it against the maintenance gate, add the internal client
call, then expose the CLI command. Generate and inspect OpenAPI, update public
reference and agent guidance, and ensure the CLI still has no direct vault path.

### Change a wire contract

Update API types, route tests, client decoding, OpenAPI assertions, public HTTP
documentation, and agent examples together. Decide whether a running old daemon
could misinterpret the new request. If safety is not exact, bump the daemon
protocol revision so `Ensure` replaces it before any data call.

Never rely on an optional JSON field to make a formerly destructive endpoint
safe against a daemon that ignores unknown fields. Use capability/protocol
separation or a new non-destructive route.

### Change storage or reachability

Read [Storage design](storage-design.md) first. Keep physical mechanics in Kit
and logical liveness in docbank. Exercise Kit catalog conformance plus a real
docbank lifecycle. Update GC reports so “reclaimed” means physical bytes
actually removed; packed logical death is pending repack.

### Change schema

Layouts that never shipped are disposable. v0.9.0 is the first released
compatibility boundary: incompatible SQLite changes use a tested deterministic
JSONL cutover from an exact released-schema fixture, not an in-place migration
ladder. Preserve the source database until the rebuilt current database has
imported and validated logical authority and restored its physical pack catalog.

### Change daemon lifecycle

Exercise foreground, detached, auto-start, restart, mismatch replacement,
concurrent starters, PID reuse, graceful stop, idle timeout, and both Windows
architectures. Keep status/stop discovery permissive and all starter paths
convergent.

## Design and documentation updates

For every material design change:

1. Update the relevant internal living design page with current mechanics and
   rationale.
2. Update public architecture when the user-visible model or boundary changes.
3. Update CLI/API references and examples when a contract changes.
4. Keep planned public behavior inside explicit planned callouts.

Do not add a historical decision ledger. Git records prior versions; the
working tree should let a new contributor understand the current system without
replaying them. Track work, ordering, blockers, and acceptance state in kata;
documentation carries the resulting capability and durable rationale.

## Verification contract

Repository commands are defined in `AGENTS.md` and the Makefile. The important
design-specific checks are:

- every Go build/test/lint uses CGO and the `fts5` tag;
- Linux, macOS, and Windows exercise the real daemon and vault lifecycle, with
  Windows CI covering amd64 and arm64;
- docs build strictly, publish Markdown counterparts, and exclude this internal
  directory;
- API examples are checked against generated OpenAPI when routes change;
- store tests cover transaction rollback and schema invariants, not only happy
  path handlers; and
- cross-filesystem storage behavior is exercised through Kit rather than
  mocked into a second docbank implementation.

## Review posture

Review the actual trust and scale model: local, loopback-only, one user, one
daemon, personal-archive scale. Focus security review on authentication gaps,
non-loopback exposure, data loss, stale compatibility, crash ordering, and
incorrect authority boundaries—not multi-tenant controls the product does not
claim.
