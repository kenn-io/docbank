# Daemon and API design

The daemon owns a standalone vault. Every CLI data command and standalone
agent integration uses its HTTP API. Go applications can instead own a
separately rooted [embedded vault](../embedding.md).

This page owns contributor guidance for daemon and API changes. The public
[daemon guide](../architecture/daemon.md) owns lifecycle and discovery behavior;
the [HTTP API reference](../architecture/http-api.md) owns routes, wire fields,
preconditions, and errors.

## Sole vault ownership

`docbank daemon run` takes the portable vault lock exclusively and without
waiting for another owner. It holds that lock while opening SQLite and the Kit
blob store, cleaning staging files, and serving requests. Follow the exact
[startup and shutdown order](../architecture/daemon.md#lifecycle) when changing
this path.

The lifetime lock proves startup cleanup cannot race another writer. A second
daemon fails immediately because waiting on a lock held for another daemon's
entire lifetime would only hang.

Data commands call `daemonconn.Ensure` and never import store-opening code. Status
and stop are discovery-only so they can find an incompatible daemon without
starting a replacement. Start, restart, and auto-start share one convergence
path under an external per-user launch lock; they replace a daemon whose
version or protocol revision is incompatible with the invoking CLI. The
launcher must not initialize the vault or open vault-local logs before the
child daemon acquires the target-tree lock.

When a change makes old clients unsafe against a new daemon, or vice versa,
bump the daemon protocol revision even when both development binaries still
report the same version string.

## Runtime record and trust boundary

Docbank is a single-user local service. `$DOCBANK_HOME` is private to the
current user (0700 on Unix, a restricted DACL on Windows), and every process
runs with that user's privileges. The real integrity threats
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

`POST /api/v1/nodes/{id}/provenance` is an ID-addressed metadata mutation. It
requires the node revision in `If-Match`, runs on the fast HTTP mutation side of
the operation gate, and appends an immutable fact plus a generic ingest in one
store transaction. `original_path` is opaque evidence, so the daemon never
opens it or treats it as retained content. An optional `supersedes` value must
identify an active caller-supplied fact on the same node; the earlier fact
remains immutable. Operational ingest facts cannot be superseded because
re-ingest uses them for idempotency. Store writes and audit replay enforce
the same restriction.

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

Once maintenance is running or queued, a new HTTP mutation returns
`503 maintenance_busy` instead of waiting indefinitely. Daemon-owned
background jobs keep the blocking shared-side behavior so a transient
maintenance pass does not permanently fail durable work. Maintenance is exempt
from the ordinary request timeout because a personal archive scan may
legitimately be long. The gate is not the vault lock; the daemon already owns
that lock for its lifetime.

Any new endpoint that changes reachability or physical content must be placed
on the correct side of the gate. Read-only metadata and content streams do not
need it unless their contract requires a globally quiescent snapshot.

## API shape and errors

### Photo graph routes

Photo endpoints are daemon-only typed routes over the store's graph authority.
The browser session surface does not expose them in this slice. Asset reads
accept an asset UUID or a member node ID and return the resolved display
pointer, source, revision, ETag, and at most 256 members. Create and promote
accept live file nodes; attach, detach, exclude, display, and settings use
`If-Match` and `g.mutate` so the revision check and response are one mutation
boundary.

The route layer carries node IDs, UUIDs, roles, and ETags. It does not classify
media, choose displays, validate sidecar locality, or repair purge state.
Those decisions belong to the store policy. Generated clients validate
identity, ETags, and response bounds without reproducing the policy.

MCP always exposes photo inspection. Photo mutations are construction-time
opt-in through `docbank mcp --allow-photo-edits`; each write makes one daemon
request and treats ambiguous transport failure as an unknown outcome. Display
and settings writes remain HTTP and CLI operations.

Similar-document reads use the processing service and store authority through
`POST /api/v1/search/similar`. Keep query encoding and provider authorization
outside that call path. The store owns source validation, fenced membership,
content grouping, and the final manifest check. Daemon and browser clients
validate the receipt before rendering it. See the
[wire contract](../architecture/http-api.md#similar-documents).

Huma route definitions generate the OpenAPI contract used by agents and client
generation. Request/response wire types live in `internal/api`; the internal
CLI client shares them so contract drift fails at compile or test time.

Store sentinel errors map to RFC 7807 responses with a stable `code`. Clients
branch on the code, not human detail. Adding a store error normally requires:

1. defining or preserving a typed sentinel;
2. mapping it in `internal/api/errors.go`;
3. mapping it in `internal/daemonconn` when the CLI needs typed behavior;
4. documenting the public code; and
5. testing the non-2xx response envelope.

Document search accepts `rerank: true` as an explicit opt-in. The daemon checks
the separate provider grant for `query_text_and_excerpt` before it sends the
query and bounded excerpts to ZeroEntropy or Cohere. The searcher revalidates
the source-fenced candidates first. Semantic and hybrid search keep their
query egress fence while checking the reranking grant, so the check never
reenters the same revocation lock.

The response may include one `reranking` receipt. Its outcome is `applied`,
`degraded`, or `skipped`; degraded receipts carry a bounded cause and every
receipt carries the candidate count. The receipt is returned independently of
the optional retrieval trace. The client rejects a receipt when the request
did not opt in, and it rejects a missing or malformed receipt for an opted-in
request. Errors use stable `reranking_unavailable` and `reranking_failed`
codes and never include provider bodies, credentials, query text, or excerpts.

The browser and TUI expose Names and text, Auto, Lexical, Semantic, and Hybrid
as processing choices. UI Auto maps to Hybrid only when an embedding binding
is present; API Auto remains lexical. Semantic and Hybrid requests require a
binding. Every processing search resolves one complete live source fence,
validates its sorted UUIDv4 IDs, observed count, and vault-bound fingerprint,
then hydrates only live nodes whose current content version matches the result.
Clients may send `rerank: true` after base results are available; the daemon
returns a bounded reranking receipt for that opt-in request.

Unmapped internal failures may expose useful detail because this is a local
single-user tool, but secrets, API keys, shutdown tokens, and document content
must never enter logs or error strings.

## Ingest boundary

`POST /ingest` names absolute paths on the daemon host. Relative paths are
meaningless to a long-lived process, and non-loopback callers are rejected even
with a valid key because the capability reads daemon-host files. This is not a
remote upload endpoint. Its optional `include` and `exclude` arrays select
source files with one compiled `path.Match`-based policy; exclusion wins and
include rules never prune directories. The streamed and preflight routes carry
the same fields and policy.

The CLI resolves user arguments to absolute paths before sending the request,
preserving shell-relative ergonomics. Partial source failures are returned in
the report while other sources continue.

The ingest request may carry `replace: true` for an opt-in exact destination
policy. The daemon records the destination node revision before reading the
source. Changed bytes create a content version on the same node, equal hash
and size skip without changing stored MIME or history, a live directory fails
before source I/O, and stale or exact-name create races fail the file without
suffixing. The omitted and false values retain ordinary suffixing.

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

The upload handler follows this order:

1. Hold the application mutation gate, then Kit's mutation lease.
2. Ask Kit to durably publish and hash the bytes. This prepared upload grants
   no application authority.
3. Validate the multipart closing boundary and reject extra parts.
4. Insert the blob and node in one metadata transaction.

A digest or size mismatch, or malformed trailing multipart data, can leave an
untracked physical object. It cannot leave a readable blob row. GC reclaims
that residue. A successful retry returns the existing node, so the receipt
continues to identify the same document.

## Manual remote recording publication

`POST /api/v1/media/sources` accepts a JSON reference before any recording
bytes exist. `canonical_url` is the sanitized identity URL. It accepts only an
absolute HTTP(S) URL, stores its normalized identity as bounded digests, and
is write-only. `reference_url` remains the required protected input. Its raw
value and any credential binding stay out of receipts, logs, errors, and
portable metadata.

The service recognizes Cap Cloud locally from the canonical URL. An `https`
URL on `cap.so` or `www.cap.so`, with the default port and an unescaped
`/s/<id>`, `/embed/<id>`, or documented SDK `/dev/<id>` path, uses provider
`cap`. Its origin scope is the
digest of the fixed `https://cap.so` scope, and its source key is the digest of
the video ID, so both hosts, both routes, and every query select one source.
Cap's sharing documentation establishes that share and embed URLs name the
same video. Treating `www.cap.so` as the same service follows the #240 design;
no Cap document states it.
Recognition performs no DNS lookup, HTTP request, or credential resolution.

The service recognizes Loom locally from canonical `https://loom.com` and
`https://www.loom.com` URLs with an exact `/share/<id>` or `/embed/<id>` path.
It keys each route separately because Loom does not document that share and
embed IDs are interchangeable. Loom references use provider `loom`; an
`acquire: true` request is retained as `unsupported` and uses no network
access.

For a recognized Cap URL, `acquire: true` is admitted and retained as an
`unsupported` outcome. It creates no acquisition queue row, so the caller
continues through the manual artifact path below. Cap's documented Developer
API lists videos, status, deletion, and usage, but has no download or caption
route, and it covers only videos created through the calling developer app. A
received share link therefore has no supported acquisition owner. Every other
canonical URL keeps the generic `url` identity, and `acquire: true` still
returns `503 capability_unavailable`. Recognition makes no claim about a
video's visibility or password state.

Daemon configuration can register a self-hosted Cap origin with
`[media_origins.<name>]`. The processing service canonicalizes the exact root
origin, accepts Cap's `/s/<id>`, `/embed/<id>`, and SDK `/dev/<id>` paths there,
and binds one named credential to that origin. The daemon starts one
`probe:media-origins` job for the documented usage endpoint. `providerhttp` enforces the configured
scheme, host, port, DNS allowlist, SPKI pins, and redirect refusal. Probe
evidence is exposed through the existing media-origins listing. Cap references
remain `access_required` because this slice has no acquisition worker.

The manual path publishes an original through the existing artifact route.
The caller sends one complete multipart request to
`POST /api/v1/media/sources/{source_id}/artifacts` with `kind: "media"` and
the exact original bytes. WAV and MP3 keep their existing rules. A remote
recording may also use an MP4 named `.mp4` with `video/mp4`. MP4 admission is
limited to 20 MiB, 2,088,960 coded pixels, 300,000 milliseconds, and 18,000
frames. The inspector's MP4 byte ceiling, a 1080p-class coded frame, five
minutes, and five minutes at 60 frames per second set these limits. The
handler checks the envelope, declared size, and digest. The processing service
checks the filename, MIME type, and declared size before staging the file. It
then inspects the staged bytes for the container, sample authority, and media
bounds.

The service holds the application mutation gate before the Kit mutation lease.
The store transaction then checks the caller, visible occurrence, remote
source kind, and observed source version again. It seals the core content,
appends or reuses the exact source version, binds only the selected occurrence,
records the `media` input, and writes the operation receipt. A rejected
transaction can leave physical bytes for GC, but it cannot leave catalog
authority.

The original must exist before a caption or transcript artifact can be
retained. A caption uses `application/x-subrip` and the built-in
`supplied-captions` profile. The local `document/mediatranscript` parser keeps
cue timing and styling tags, checks every cue against the measured recording
duration, and records supplied provenance. The existing
`supplied-transcript` profile remains unchanged. Captions retained before this
profile exists need an explicit retry. A transcript reaches a rendition only
after the caller reviews a processing plan, grants consent, and requests an
explicit retry. Status and list reads use the source version bound to the
selected visible occurrence. They filter processing receipts to that same
immutable version, so a transcript for an older recording revision cannot
cover newer bytes.

### Exact media transcript reads

`GET /api/v1/media/sources/{source_id}/versions/{source_version_id}/transcript`
requires `content_version_id` as a query parameter. The handler resolves the
caller-visible source tuple and the persisted active rendition, then reads only
the retained transcript artifact referenced by normalized evidence. It checks
the source occurrence, content revision, active build, bound supplied input,
and visibility fence again after blob I/O. A changed authority returns
`evidence_state: "stale"` without transcript text.

`evidence_state` is independent of `coverage_state` and `operation_state`.
`ready` is the only state that carries a transcript; `pending` means admitted
work is still running, `unavailable` means no readable retained artifact is
available, and `stale` means the requested tuple or selected authority changed.
The embedded API, daemon connection, generated clients, and
`docbank media transcript SOURCE_ID --source-version-id ID
--content-version-id ID` use this same read owner. The response contains
retained origin, provider, language, completeness, omission/truncation flags,
and optional paired timing and speaker facts; it exposes no rendition or blob
identifiers.

## Change constraints

- New data commands must be HTTP clients, never direct store callers.
- New mutating routes must choose ID/revision or transactional path semantics
  explicitly.
- New destructive operations need dry-run intent where preview is meaningful.
- New maintenance must be classified against the gate and cancellation model.
- Compatibility changes require a protocol revision bump and old-runtime tests.
- Non-loopback service, multi-user auth, or app-owned TLS would be a product
  boundary change, not a local middleware tweak.
