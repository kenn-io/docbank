# vector package invariants

`go.kenn.io/kit/vector` owns the backend-neutral parts of an embedding
pipeline. Preserve these invariants when changing it.

## The storage boundary is the point of this package

- The core `vector` package must not import `database/sql`, a driver, or
  any backend client, and must not construct backend SQL. The `Fill` and
  `Search` flows reach storage only through the `Store[K, G]` interface.
- Persistence is a function of the caller's source system. Backends live
  in their own subpackages (e.g. `vector/sqlitevec`) so a caller wiring
  one backend never pulls another backend's driver. New backends
  (pgvector, duckdb) go in sibling subpackages, not into the core.
- Backends own query construction. The differences between sqlite-vec
  `vec0 MATCH`, pgvector `<=>`, and duckdb `array_distance` belong behind
  `QueryGeneration`, never in the core flows.

- `sqlitevec` never imports a SQLite driver or extension binding. Callers
  register the extension for their chosen driver; CGO settings do not choose
  a driver. Keep the modernc consumer test in a separate package so a mattn
  test import cannot conceal a link dependency.

## Encoded vectors must be usable for cosine distance

- Blank text is never sent to an encoder. `Split` omits blank windows so `Fill`
  stamp-saves blank documents without vectors; `EncodeBatched` rejects manually
  supplied blank chunks before any encoder call with an error wrapping
  `ErrEmptyEmbeddingInput`. Blank means every rune is whitespace, invisible
  formatting (category Cf: zero-width space and joiners, byte order mark, soft
  hyphen), or a control character — see `blank` in `blank.go`, the one place
  that rule lives. Runes that only render as blank but belong to visible
  categories, such as U+2800 and U+3164, are content and stay. Widening the
  rule beyond invisible runes needs a concrete failing input, not a hunch: a
  false positive silently drops a document from the index.
- `EncodeBatched` rejects encoder output that has the right vector count
  but cannot participate in cosine distance — a non-finite component or a
  zero-norm vector — with an error wrapping `*InvalidVectorError`. `Fill`
  and `Search` route every encode through it, so faulty endpoint output
  never reaches a `Store`. Do not weaken this to a skip or a warning: a
  stamped invalid vector looks complete forever and silently poisons
  search rankings.

## Fill batches without losing document boundaries

- Fill packs chunks across documents in one scan page. A call holds at most
  `DefaultFillBatchSize` (32) chunks, or a positive `WithBatchSize`, and at
  most `DefaultFillBatchTokens` (16384) estimated tokens. The estimate counts
  an ASCII rune as a quarter token and any other rune as one, so CJK text is
  never undercounted. A chunk over the budget goes alone. The per-document
  path runs only for a nil encoder.
- Without `WithFillBatchErrorIsolation`, a shared-call error is diagnosed per
  document only when `WithFillEncodeError` is set, so one bad document stays
  skippable. An explicit nil classifier diagnoses nothing.
- `WithBatchTokenBudget` replaces Fill's estimate with the caller's
  conservative per-input token upper bound. The vector package does not choose
  a tokenizer or alter input text; the estimate only sizes encode calls.
  Reject a configured upper bound that cannot fit one input before calling the
  encoder.
- Vectors from a shared encode batch must be scattered back to their exact
  document and chunk indexes before `SaveVectors`. Saves and `OnEncodeError`
  remain serialized and per document.
- A failed shared encode batch is isolated only when
  `ShouldIsolateBatchError` permits document-slice diagnosis. A nil or false
  classifier aborts without probes; classification never authorizes a skip.
- A document slice is one document's refs within the failed batch, not
  necessarily its complete chunk list. Probes, classification, hook decisions,
  and saves stay on the serialized collector and use Fill's outer context.
- Errors carrying an in-range `*InvalidVectorError` batch index bypass
  classification. Decide the attributed document before recovering other
  slices, never retry the known-invalid slice, and make an out-of-range index
  fatal.
- Consult `OnEncodeError` exactly once per failed document. Record an accepted
  skip before saving so `saveEncoded` does not consult the hook again; a nil or
  rejected hook aborts immediately.
- Collector diagnosis back-pressures the worker that delivered the failed
  result. Filter concurrently completed results against documents already
  saved or skip-decided, and cancel in-flight workers promptly on abort.
- If every active slice succeeds in isolation and no already-failed document
  explains the shared error, keep the original error fatal. Preserve provider,
  invalid-vector, and context causes through all wrappers with `%w`.
- With one fill worker, finish the current bounded encode window and its saves
  before starting another window. A save failure must not launch later encode
  work.
- With multiple fill workers, save a completed document without waiting for an
  unrelated earlier batch. Saves remain serialized even when encode calls
  complete out of order.
- `WithFillDocumentLimit` bounds how many pending documents one Fill starts.
  A document that returns `ErrStale` still counts. The next Fill continues
  with what is still pending. A limit of zero or less does not bound the run.
- `WithFillProgress` runs only after `SaveVectors` succeeds, including a
  stamp-only save. It does not run when the save returns `ErrStale`.
- `WithFillPrepared` replaces `Split` for that call. The callback receives
  Fill's context. Fill does not call it for a later document in the page
  when that context is already cancelled, and it does not encode or stamp
  that page. Text is the encoder input and may differ from the source.
  `SourceSpan` and `Truncated` come back through progress and are not
  stored. An empty chunk list is a stamp-only save. An error from the
  prepare function aborts the page before any of its documents are encoded
  or stamped. A blank prepared text is still `ErrEmptyEmbeddingInput`.

## Keys and generations are opaque

- Document identity is the caller's type `K` and generation identity its
  type `G`. msgvault uses `int64`; kata uses UUIDs. Compare them for
  equality only; never assume a type, a single id namespace, or an
  ordering. Backends additionally require `K`/`G` to be types
  `database/sql` can bind and scan.

## The stamp is conditional

- `SaveVectors` receives the `Pending.Revision` token read with the
  content. A store that tracks revisions must persist nothing and return
  an error wrapping `ErrStale` when the document's revision has changed
  since the scan — never stamp stale vectors over a concurrent edit. The
  document stays pending and the next fill re-reads it.
- `Fill` treats `ErrStale` as "leave it for the next run", not a failure:
  the document is excluded for the rest of the run so an actively edited
  document cannot starve the loop, and the scan limit is widened past
  excluded documents so fresh work stays visible.
- An empty vectors slice is a stamp-only save: the document is marked
  handled for the generation without storing vectors. Fill relies on this
  both for empty content and for documents `OnEncodeError` elects to
  skip after a permanent encode failure — without it, one poison document
  would wedge every future fill.

## Merge semantics

- `Merge` takes per-generation lists in descending preference and keeps
  the earliest list's hit on overlap (prefer the newer generation during
  a migration). Coverage is a union — never drop a document that only one
  generation covers, and never emit duplicates.
- Cross-generation scores are not comparable. Default to
  `MergeNormalizedScore`; raw-score merging is opt-in.

## Generations during migration

- The mid-migration union exists because new documents land only in the
  building generation while the active generation still serves the bulk.
  `Search` must keep querying every generation `LiveGenerations` returns,
  in the order it returns them.
- sqlitevec `Activate` publishes a generation only when that generation's
  vec0 table still exists. `Reclaim` drops the table and keeps the
  generation row. An empty corpus must not mark the reclaimed generation
  active, and `Activate` must not recreate the table.

## Snapshots export what search would see

- `sqlitevec.Snapshot` is the read surface for replicating a generation
  elsewhere. `CoveredDocs` and `UncoveredCount` are built on the same
  `coveredPredicate` as `PendingForGeneration` and `QueryGeneration`; a
  caller must never re-derive the freshness rule or the managed table
  names from `VectorsPrefix` to export vectors.
- A snapshot holds one read transaction until `Close`, so the documents,
  their chunks, and the readiness count describe one instant. Rows from
  `CoveredDocs` must be closed before the snapshot.
- `Chunks` reads whatever the generation holds for a document. Only export
  documents that `CoveredDocs` returned from the same snapshot.

## Publication checks coverage; reclamation is explicit

- `Coverage` counts embedded, stamp-only, and uncovered documents with
  `coveredPredicate`. A stamp-only document is covered. A stale revision is
  uncovered even when its old vectors are still stored. Callers use this
  count instead of reading the stamps or chunks tables.
- `Activate` publishes one generation only when its backlog is zero, in the
  same transaction as that count. It marks that generation active and retires
  other building and active generations. It does not drop their storage, and
  it does not change which generations `Search` queries.
- `Reclaim` drops one retired generation's vec0 table, chunk rows, and stamps.
  The generation row stays retired. Reclaiming a generation that is not
  retired fails. Calling it again after success is safe.
- Lifecycle errors wrap `ErrGenerationNotFound` for a key never ensured and
  `ErrRetired` for a retired generation used as live, so callers match them
  with `errors.Is`.
- A retired generation never leaves retired. `SetGenerationState` and
  `EnsureGeneration` refuse any other state, and ensuring it as retired does
  not recreate reclaimed storage.
- `ActiveGeneration` returns the newest active generation by ordinal.
  `LiveGenerations` still returns building generations ahead of active ones.
  Callers that serve only the active generation select it themselves.

## Hits come from live, current documents

- Revision-aware backends return the indexed revision in `Hit.Revision` in
  the same query as its score. Hydration must not pair an old score with a
  newer source revision.
- sqlitevec ordinary, composable and probed queries share candidate SQL and
  freshness checks. Raw probes survive source filtering; an empty result
  window can still have more raw neighbors. Candidate limits precede source
  filters; result limits follow them.
- `BuildFilteredQuery` is the exact alternative to raw KNN windows: apply
  source eligibility and the shared freshness predicate before scoring, select
  the best chunk per optional source group, then limit results. Its work scales
  with eligible chunks; a result limit is not a work bound. Keep ties stable by
  document key and chunk index and preserve the winning evidence identity.
- Backends must not return hits whose source row no longer exists; the
  caller may delete documents without telling the store, so
  `QueryGeneration` joins back to the documents table.
- Hits must also be current for the searched generation: stale vectors
  for an edited or invalidated document must never surface between the
  edit and the next fill, or a search can leak removed or redacted text.
  In sqlitevec, searchable and pending are exact complements of one
  shared freshness predicate (`coveredPredicate`) used by
  `PendingForGeneration`, `QueryGeneration`, and invalidation checks —
  never fork a new freshness expression for one read path.
- Freshness is per generation (sqlitevec: the stamps table), never
  `embed_gen = searched generation`. The embed-gen column records just
  the newest generation, so comparing it to the searched generation
  would wrongly hide the active generation's valid hits while
  generations overlap and break the union.
- Filtering hides stale and orphan hits but the vectors still occupy
  KNN slots; replacement happens at the next `SaveVectors`, and deletion
  cleanup (`sqlitevec.DeleteVectors`) is the caller's responsibility
  when removing documents.
