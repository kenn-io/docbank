# Immutable object storage

- Keep shared values and errors provider-neutral. Consumers own store
  interfaces, key naming, catalog state, and retention decisions. The package
  exports only the narrow read interfaces its own helpers accept.
- Only a plain io.EOF ends a stream cleanly. A source or body can return an
  error joined to io.EOF, or together with its final bytes; never drop either
  with errors.Is(err, io.EOF) or io.ReadFull. Use internal/strictio.
- Open and Reader pin every range to the inspected revision and never request
  a zero-length range; cloud stores reject one. Reader's sequential Read keeps
  one stream open until Seek moves the position. ReadAt stays stateless and
  safe for concurrent use.
- Publication returns a verified receipt or a zero receipt with an error.
  Validate the source before attempting publication. A verified identical
  retry succeeds; only a proven stored difference is ErrExists.
- Uncertain publication or verification wraps ErrOutcomeUnknown and its cause.
  Never overwrite or delete a key to resolve uncertainty.
- Local retries verify bytes and repeat file and ancestor directory sync.
  Preserve atomicfile's supported-platform semantics and no-copy behavior.
  Delete retries sync the parent even when the entry is already absent.
- Local roots and their ancestors are caller-owned, stable directories.
  Local revisions are unsupported; do not invent file identity guarantees.
- Local key components beginning with `.objectstore-` are reserved
  case-insensitively for staging. Reject them in all public point operations,
  while preserving their inventory visibility; a staging file cannot receive
  an immutable receipt or be removed through Delete.
  Reject Windows 8.3 aliases with fsname.CheckPath before accessing key paths.
  Remove crash-left staging files only while all publishers are stopped;
  timestamps alone cannot establish that another writer has abandoned a file.
- Shared key validation is host-independent. Local stores apply filesystem
  restrictions; cloud stores do not inherit Windows device-name restrictions.
- Listing preserves foreign entries and grants no deletion authority.
  S3 inventory requests URL-encoded object names and decodes them once in
  the shared listing boundary before namespace or pack-name classification,
  and only when the response declares URL encoding. Decode plus as space and
  escaped plus as a literal plus.
  Continuation tokens remain opaque and unchanged.
  Ordinary tests use temporary files or loopback fixtures, never ambient
  credentials, provider endpoints, or application state.
- S3 credentials and custom endpoints are explicit. Construction performs no
  mutating probes. Conditional PUT and multipart completion never fall back to
  overwrite. Reconcile SDK retries as well as the initial completion attempt.
- S3 conflict reconciliation uses full-object SHA-256 or revision-guarded full
  readback. CRC32C and composite digests cannot establish an existing key's
  identity. `AllowReadback` controls routine upload fallback only.
- S3 inspection, readback, SDK retries, and multipart abort share the caller's
  remaining context. Preserve uncertainty across SDK attempts; an exhausted
  context after final dispatch returns outcome-unknown with its cause.
- S3 revisions use ETags to bind inspection to multipart completion before
  accepting CRC32C, and as conditional operation tokens, never as content
  identity. A single-part routine receipt may rest on the PUT response's
  echoed SHA-256 when inspection finds that PUT's ETag; some endpoints omit
  the checksum type on inspection, and an untyped checksum stays composite. Publication conflicts do not imply a caller revision changed.
  Provider support needs live contract validation; loopback fixtures and RustFS
  conformance do not establish AWS or other provider support. The live tests
  run only when `KIT_S3_LIVE_URL` or `KIT_GCS_LIVE_URL` is set; see
  `docs/development.md`.
- GCS uses the official `cloud.google.com/go/storage` client with JSON reads.
  Config takes explicit OAuth credentials; the store never falls back to
  Application Default Credentials. HMAC login is not supported; add it only as
  an explicit opt-in, never as the default.
- GCS uploads use a does-not-exist precondition and send the validated CRC32C.
  A routine CRC32C receipt comes from the upload response; a 412 or any
  uncertain upload failure reconciles with a full SHA-256 readback bound to the
  observed generation. A retried upload can receive a client error after an
  earlier attempt completed, so report a 4xx as a definite failure only when
  the first inspection finds no object or bucket. Single-request uploads retry
  transient failures until the caller's context ends; resumable chunks use the
  client's 32-second default; other requests make at most three attempts.
- Keep resumable session URLs out of errors; they authorize uploads.
- Never request compressed reads; JSON reads reject them. Reject transcoded
  range responses instead.
- GCS tests run against an in-process fake-gcs-server. It does not establish
  real GCS compatibility. It ignores generation preconditions on reads and
  deletes, so tests that need them enforce them in a transport wrapper.
- Preserve explicit NoSuchBucket failures; they are not missing objects and do
  not make deletion successful. An S3 HEAD 404 lacks this distinction.
  A GCS JSON API 404 does not say which is missing, so object operations
  confirm a 404 with a one-item listing, which reports a missing bucket
  distinctly. A failed confirmation preserves the listing failure without
  reporting object absence or resolving publication uncertainty.
