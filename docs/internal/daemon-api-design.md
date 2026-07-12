# Daemon and API design

The daemon is the sole process that opens a vault. The HTTP API is not an
optional integration layer around a direct-access CLI; it is the only data
contract for the CLI and agents.

## Sole vault ownership

`docbank daemon run` resolves the home, validates configuration, takes the
vault flock exclusively and non-blocking, opens SQLite and the Kit blob store,
cleans staging files, publishes a runtime record, and serves requests until
graceful shutdown.

The lifetime lock proves startup cleanup cannot race another writer. A second
daemon fails immediately because waiting on a lock held for another daemon's
entire lifetime would only hang.

Data commands call `client.Ensure` and never import store-opening code. Status
and stop are discovery-only so they can find an incompatible daemon without
starting a replacement. Start, restart, and auto-start share one convergence
path under `launch.lock`; they replace a daemon whose version or protocol
revision is incompatible with the invoking CLI.

When a change makes old clients unsafe against a new daemon, or vice versa,
bump the daemon protocol revision even when both development binaries still
report the same version string.

## Runtime record and trust boundary

Docbank is a single-user local service. `$DOCBANK_HOME` is tightened to 0700,
and every process runs with that user's privileges. The real integrity threats
are crashes, stale process state, PID reuse, accidental damage, and serving an
object whose identity is false—not an adversary already able to rewrite the
user's vault.

The runtime record contains PID, process create-time, endpoint, build version,
protocol revision, shutdown token, and effective API key. Create-time prevents
a stale record from targeting a reused PID. The record is runtime state, not
archive state.

The daemon always has an API key. An empty configured key means generate a new
per-run key and publish it in the same-user runtime record; it never means
unauthenticated. Binds are loopback-only because plain HTTP on a LAN would
expose both key and content. Remote access terminates an external secure tunnel
at loopback rather than expanding the daemon's trust model.

Auth-exempt health, ping, docs, and OpenAPI routes establish discovery and
contract access only. Every data route and the hidden shutdown route requires
the effective key; shutdown additionally requires its token.

## Node identity, paths, and revisions

Node IDs are stable. Paths are mutable names that can be reused. Single-node
responses expose paths for display, but a trash response intentionally carries
the pre-trash location as recovery context; it is no longer an address for the
trashed node.

ID-addressed move, trash, and restore require `If-Match` with the revision the
client evaluated. The precondition is checked inside the store transaction.
Stale state returns 412, missing preconditions return 428, and malformed or
negative values return a validation error. Revisions are per node rather than
global so unrelated tree changes do not invalidate an agent's work.

Path move and trash are a different contract. They resolve the source and
mutate within one transaction, eliminating a resolve-then-act race. They mean
“operate on whatever this path names when the transaction begins” and therefore
do not accept a client revision.

Do not add a path mutation that resolves through a separate preflight query.
Use an ID plus revision for read-modify-write or add a transactional store
operation for one-shot path intent.

File-node wire representations expose the catalog's lowercase SHA-256
`blob_hash`; stable node identity and immutable content identity are separate
on purpose. A content response sends that expected identity before the body
and computes an RFC 9530 `Content-Digest` trailer while streaming actual bytes.
Do not substitute the catalog value directly into `Content-Digest`: corruption
would turn an integrity field into a false assertion.

The body is read through Kit's verified-on-EOF stream. Only a successful
terminal read earns the digest trailer; cancellation, corruption, or an early
consumer close releases the stream without implicitly draining it. Code that
publishes or archives these bytes must not treat a successfully opened stream
or a readable prefix as evidence of content identity.

Single-node verification requires `If-Match`, reads through the mixed store,
and checks the node revision again afterward. The second check is essential:
ordinary mutations may run concurrently, and evidence must never silently
change meaning if the node is renamed, trashed, or eventually pointed at a new
content version during a long read. Physical pack maintenance remains safe
through Kit's reader lifecycle and does not change blob identity.
Because one blob may still be very large, this route is timeout-exempt like the
vault-wide verifier; cancellation still propagates from the client connection.

## Request concurrency and maintenance

SQLite serializes metadata writes and schema/store invariants choose the winner
of name or cycle races. Ordinary mutations may run concurrently.

Maintenance needs a stronger boundary because GC and verify span database and
filesystem observations. The in-process gate has shared mutation and exclusive
maintenance sides:

- create, ingest, move, trash, and restore take the shared side;
- trash empty, GC, and verify take the exclusive side.

Requests queue rather than returning “busy.” Maintenance is exempt from the
ordinary request timeout because a personal archive scan may legitimately be
long. The gate is not the vault flock; the daemon already owns that flock for
its lifetime.

Any new endpoint that changes reachability or physical content must be placed
on the correct side of the gate. Read-only metadata and content streams do not
need it unless their contract requires a globally quiescent snapshot.

## API shape and errors

Huma route definitions generate the OpenAPI contract used by agents and client
generation. Request/response wire types live in `internal/api`; the internal
CLI client shares them so contract drift fails at compile or test time.

Store sentinel errors map to RFC 7807 responses with a stable `code`. Clients
branch on the code, not human detail. Adding a store error normally requires:

1. defining or preserving a typed sentinel;
2. mapping it in `internal/api/errors.go`;
3. mapping it in `internal/client` when the CLI needs typed behavior;
4. documenting the public code; and
5. testing the non-2xx response envelope.

Unmapped internal failures may expose useful detail because this is a local
single-user tool, but secrets, API keys, shutdown tokens, and document content
must never enter logs or error strings.

## Ingest boundary

`POST /ingest` names absolute paths on the daemon host. Relative paths are
meaningless to a long-lived process, and non-loopback callers are rejected even
with a valid key because the capability reads daemon-host files. This is not a
remote upload endpoint.

The CLI resolves user arguments to absolute paths before sending the request,
preserving shell-relative ergonomics. Partial source failures are returned in
the report while other sources continue.

`POST /uploads` is the remote counterpart, but it is deliberately one file per
request. `parent_id` and normalized `name` identify the destination; required
hash/size headers describe the sole multipart `file` part. File granularity
makes success, failure, and retry atomic rather than embedding partially
successful application work inside one transport result.

The raw handler streams directly from `multipart.Reader` instead of using
Huma's decoded multipart input, which pre-parses the complete request into
memory or temporary files before invoking application code. Its OpenAPI
operation is registered manually against the same Huma document. Keep the raw
handler and schema synchronized in one registration function.

The operation holds the application mutation gate before Kit's mutation lease.
Kit durably publishes and hashes bytes first. A prepared upload exposes no
application authority, allowing the handler to validate the closing boundary
and absence of extra parts before its metadata transaction inserts the blob and
node. Digest/size mismatch or malformed trailing multipart data can leave an
untracked physical object, never a readable blob row; GC owns that ordinary
crash/rejection residue. Successful retries return the existing node, so the
receipt always carries stable identity.

## Change constraints

- New data commands must be HTTP clients, never direct store callers.
- New mutating routes must choose ID/revision or transactional path semantics
  explicitly.
- New destructive operations need dry-run intent where preview is meaningful.
- New maintenance must be classified against the gate and cancellation model.
- Compatibility changes require a protocol revision bump and old-runtime tests.
- Non-loopback service, multi-user auth, or app-owned TLS would be a product
  boundary change, not a local middleware tweak.
