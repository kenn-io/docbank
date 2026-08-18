# Media and Voyage Embedding Contracts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `document/media` (provider-neutral media detection and eligibility) and `document/voyage` (fail-closed Voyage multimodal embedding transport with capability probe, manifest, and authorization).

**Architecture:** Two storage-neutral packages under `document/`, mirroring `document` and `document/mistral`. `document/voyage` imports `document/media`; neither imports anything else in Docbank. Media bytes are handled in memory (≤ 20 MiB); there is no spool. Capability gating follows the Mistral pattern: pinned policy → operator probe → validated manifest → `Policy.Authorize`.

**Tech Stack:** Go 1.26 standard library only (`image/jpeg`, `image/png`, `image/gif`, `net/http`, `encoding/json`); tests use `testify` and `httptest` as the Mistral package does.

**Spec:** `docs/superpowers/specs/2026-08-18-media-and-voyage-embedding-contracts-design.md`

## Global Constraints

- Build and test with `-tags fts5` (`make test`); keep CGO and `CGO_ENABLED=0` modes passing.
- `make lint` clean; `make docs-build` clean.
- No private data in fixtures; every fixture is generated or hand-built synthetic bytes.
- Pinned Voyage values: endpoint `https://api.voyageai.com/v1`, model `voyage-multimodal-3.5`, dimension `1024`.
- Media defaults: 20 MiB, 16 000 000 pixels, no duration cap.
- Client bounds: `MaxBatchItems` ≤ 64, `MaxRequestBytes` ≤ 64 MiB, `MaxResponseBytes` ≤ 8 MiB, timeout default 45 s max 5 min, retries default 3 max 10.
- Commit every task; run `prek run` before each commit.

---

### Task 1: `document/media` detection

**Files:**
- Create: `document/media/doc.go`, `document/media/types.go`, `document/media/detect.go`, `document/media/detect_test.go`, `document/media/testdata_test.go` (fixture builders)

**Interfaces:**
- Produces: `Format` consts (`FormatJPEG`, `FormatPNG`, `FormatWebP`, `FormatGIF`, `FormatMP4`), `Kind` consts (`KindImage`, `KindVideo`), `Metadata{Format, Kind, MediaType, DeclaredMediaType, Size, Width, Height, FrameCount, DurationMS, Animated}`, `Detect(io.ReaderAt, int64, string) (Metadata, error)`, `DetectBytes([]byte, string) (Metadata, error)`, `ErrUnsupportedMedia`, `ErrMalformedMedia`.

- [ ] Write table tests: JPEG/PNG (stdlib-encoded), hand-built WebP VP8X/VP8L/VP8, still + animated GIF (stdlib `gif.EncodeAll`), hand-built MP4 (v0 and v1 `mvhd`), PDF/Ogg/ID3/empty → `ErrUnsupportedMedia`, truncated PNG/WebP/GIF/MP4 → `ErrMalformedMedia`, declared type recorded verbatim.
- [ ] Run, confirm fail; port sniffers from the application (`inspectMediaBytes`, `gifMetadata` returning frame count, `webPDimensions`, `scanMP4Boxes`, `parseMVHD`); make tests pass.
- [ ] Commit `Add provider-neutral media detection`.

### Task 2: `document/media` eligibility

**Files:**
- Create: `document/media/policy.go`, `document/media/policy_test.go`

**Interfaces:**
- Consumes: Task 1 types.
- Produces: `Policy{MaxBytes, MaxPixels, MaxDurationMS, AllowStill, AllowAnimated, AllowVideo}`, `DefaultPolicy()`, `Reason` consts (`ReasonEligible`, `ReasonUnsupportedMediaType`, `ReasonMalformedMedia`, `ReasonTooLarge`, `ReasonTooManyPixels`, `ReasonTooLong`, `ReasonStillNotAllowed`, `ReasonAnimatedNotAllowed`, `ReasonVideoNotAllowed`), `Evaluate(Metadata, Policy) Reason`, `Inspect(io.ReaderAt, int64, string, Policy) (Metadata, Reason, error)`, `InspectBytes([]byte, string, Policy) (Metadata, Reason)`.

- [ ] Table tests: each reason reachable; per-axis and product pixel caps; zero caps use defaults; detection errors map to reasons; oversized input is refused before reading past `MaxBytes+1`.
- [ ] Implement; pass; commit `Add bounded media eligibility policy`.

### Task 3: `document/voyage` policy, capabilities, manifest

**Files:**
- Create: `document/voyage/doc.go`, `document/voyage/policy.go`, `document/voyage/capabilities.go`, `document/voyage/manifest.go`, `document/voyage/policy_test.go`, `document/voyage/manifest_test.go`

**Interfaces:**
- Consumes: `media.Policy`.
- Produces: `PolicyConfig{Media media.Policy, MaxBatchItems, MaxRequestBytes, MaxResponseBytes}`, `PolicyValues`, `Policy` (opaque; `Values()`, `MediaPolicy()`, `CanonicalJSON(manifest)`, `Fingerprint(manifest)`, `Authorize(manifest, capabilityID) (Authorization, error)`), `Authorization` (`Capability()`, `PolicyFingerprint()`), `Capability{ID, Kind, Format, Animated, Description}`, `Capabilities()`, `CapabilityByID`, capability ID consts, `CapabilityManifest`, `CapabilityResult`, `ProbeStatus` consts, `CapabilitySchemaVersion`, `ValidateComplete`, `EncodeCapabilityManifest`, `DecodeCapabilityManifest`, `requestFingerprint(capability, values)` (unexported).

- [ ] Tests: `NewPolicy` bounds; `ValidateComplete` pins and scrub rules; decode rejects unknown/duplicate/oversized; `Authorize` fails closed for missing manifest, unknown ID, non-passed, fingerprint mismatch, target mismatch; `Fingerprint` stable and changes with authorities.
- [ ] Implement; pass; commit `Add Voyage policy and capability manifest`.

### Task 4: `document/voyage` client

**Files:**
- Create: `document/voyage/client.go`, `document/voyage/errors.go`, `document/voyage/retry_after.go`, `document/voyage/client_test.go`

**Interfaces:**
- Consumes: Task 3 `Policy`, `Authorization`; `media.Metadata`.
- Produces: `Input{Parts []Part}`, `Part{Text string; Media *Media}`, `Media{Metadata media.Metadata; Bytes []byte}`, `Usage{TotalTokens int64; Available bool}`, `RequestMetrics{Requests, Retries int; Latency time.Duration}`, `Result{Vectors [][]float32; Usage; Metrics}`, `ClientConfig{APIKey, Timeout, MaxRetries, RetryBaseDelay, HTTPClient}`, `NewClient(Policy, ClientConfig) (*Client, error)`, `(*Client).EmbedDocuments(ctx, []Input, []Authorization) (Result, error)`, `(*Client).EmbedQuery(ctx, Input, Authorization) ([]float32, Usage, error)`, sentinels `ErrUnauthorized`, `ErrBatchTooLarge`, `ErrPermanentResponse`, `ErrTransientResponse`, `ErrMalformedResponse`, `ErrCapabilityContract`, `IsRetryable`, `RetryAfter(err)`, `MetricsFromError(err)`.

- [ ] Port application tests through `httptest`: ordered inputs and index restoration; query text/image/combined; malformed vectors; request/response limits; 401/400-size/400-other/429-Retry-After/5xx classification and retry; timeout and cancellation; authorization enforcement (media without matching capability → `ErrCapabilityContract`).
- [ ] Implement; pass; commit `Add fail-closed Voyage multimodal client`.

### Task 5: probe fixtures and capability probe

**Files:**
- Create: `document/voyage/fixtures.go`, `document/voyage/fixture_writer.go`, `document/voyage/probe.go`, `document/voyage/fixtures_test.go`, `document/voyage/probe_test.go`, `document/voyage/probe_live_test.go` (`//go:build voyage_probe`), `document/voyage/voyagetest/voyagetest.go`

**Interfaces:**
- Consumes: Tasks 3–4.
- Produces: `FixtureOptions{SeedDirectory string}`, `WriteProbeFixtures(ctx, dir, FixtureOptions) error`, `ProbeFixtureConfig{FixtureDirectory string}`, `ValidateProbeFixtures(ctx, Policy, ProbeFixtureConfig) error`, `ProbeConfig{Fixtures ProbeFixtureConfig; ObservedAt time.Time}`, `RunCapabilityProbe(ctx, *Client, ProbeConfig) (CapabilityManifest, error)`, `voyagetest.SyntheticManifest(policy, passed ...string)`, `voyagetest.PNG(label)`, `voyagetest.AnimatedGIF()`.

- [ ] Tests: writer produces deterministic bytes; validation fails without seeds and with wrong-format seeds; probe against an `httptest` server produces a complete manifest with scrubbed failures; `Authorize` succeeds on the produced manifest.
- [ ] Implement; pass; commit `Add Voyage capability probe and fixtures`.

### Task 6: docs and handoff

**Files:**
- Modify: `docs/document-understanding.md`, `docs/capabilities.md`, `docs/llms.txt` if it indexes doc pages.

- [ ] Add "Visual attachments" section (operator flow, per-item flow, package boundary diagram update); adjust the capabilities boundary sentence.
- [ ] `make docs-build`, `make lint`, `make test`, `CGO_ENABLED=0 go test -tags fts5 ./document/...`.
- [ ] Commit `Document media and Voyage embedding contracts`; push branch; open PR (do not merge); update kata `0jnq`.
