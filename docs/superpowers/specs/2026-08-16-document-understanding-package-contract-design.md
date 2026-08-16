# Public document-understanding package contract

**Status:** Approved design

**Date:** 2026-08-16

**Kata:** `w30d`

**Parent epic:** `rz07`

**Implementation stage:** `e9jz`

**Compatibility source:** `kenn-io/msgvault#616` at
`73d6c0b33f74c1fd072a7c0258f1cf1e80054698`

## Summary

Docbank will expose reusable document understanding through two top-level
public packages:

- `go.kenn.io/docbank/document` for deterministic, provider-neutral document
  normalization; and
- `go.kenn.io/docbank/document/mistral` for bounded Mistral transport, format
  evidence, policy identity, private staging, and authenticated probes.

Msgvault will import these packages as an ordinary Go library. It will not
start a Docbank daemon, open a Docbank vault, or connect to an installed
Docbank instance. The public packages will not import Docbank vault, daemon,
database, queue, or application internals.

The package split follows a concrete existing consumer boundary. Docbank's
`internal/extract` reads blobs directly from packstore and can later use
`document` normalization without acquiring Mistral, HTTP, spool, or capability
manifest concepts. Provider-specific behavior begins in
`document/<provider>`. Common provider behavior moves upward only after a
second concrete provider implementation proves the common contract; a second
provider initially copies the small behavior it needs.

This design freezes the public contract only. Work on `e9jz` requires a new
`superpowers:brainstorming` gate before implementation.

## Goals

The contract must:

- provide one authoritative implementation of deterministic normalization,
  headings, spans, baseline chunks, truncation, and checksums;
- preserve normalization policy version 2 and all baseline normalization,
  request-fingerprint, capability-fingerprint, and Msgvault legacy profile
  fingerprint bytes;
- expose validated provider output only as provider-neutral source units;
- prevent production uploads for formats without passing probe evidence and
  an enforceable unit bound;
- make staging, retry-safe byte identity, and cleanup safe by construction;
- bind production request authority to policy and committed capability
  evidence without claiming that a person gave consent;
- keep credentials, machine paths, disk budgets, run budgets, and application
  configuration outside reusable semantic identity; and
- let both repositories verify compatibility offline without cross-repository
  imports or public test-only APIs.

## Non-goals

This stage does not move or define Msgvault workers, claims, job retries,
reconciliation, schedules, SQLite or PostgreSQL schemas, vector backends,
backups, rebuilds, TOML configuration, message scope, run-cost budgets, spool
budgets, free-space budgets, consent records, CLI, HTTP, OpenAPI, MCP, or search
serving.

It does not define a generic `document.Provider`, `document.Extractor`,
provider-neutral staging layer, public retry package, or generic capability
framework. It does not define `EmbeddingPlan`, distillation, or evaluation.
Those receive their own designs when they have concrete consumers.

## Public package layout

The module will use top-level public subpackages. Before document implementation,
a focused change will move:

- `go.kenn.io/docbank/pkg/sqlite` to `go.kenn.io/docbank/sqlite`;
- `go.kenn.io/docbank/pkg/sqlite/mattn` to
  `go.kenn.io/docbank/sqlite/mattn`; and
- `go.kenn.io/docbank/pkg/sqlite/modernc` to
  `go.kenn.io/docbank/sqlite/modernc`.

That change removes `pkg/` without forwarding packages or aliases. It is an
intentional pre-1.0 public import-path break. The same change updates the
embedding documentation, including its published `modernc` recipe, and adds a
changelog entry. Downstream repositories may remain on their pinned Docbank
release and update imports when they next upgrade.

The document dependency direction is:

```text
document/mistral  --->  document
       |                  |
       +--------X---------+
                no imports from Docbank application internals
```

## Package `document`

### Source evidence

`document` accepts transient provider-neutral evidence:

```go
type SourceDocument struct {
	Family   string
	UnitKind string
	Units    []SourceUnit
}

type SourceUnit struct {
	Index      int
	Markdown   string
	Header     string
	Footer     string
	Dimensions UnitDimensions
}

type UnitDimensions struct {
	DPI    int
	Height int
	Width  int
}
```

`Family` and `UnitKind` remain strings. `document` validates their presence
but does not claim their capability provenance. A provider package is
responsible for stamping authenticated values. `SourceUnit.Index` remains
explicit because normalization rejects noncontiguous units.

### Normalization policy

The version-2 policy is opaque:

```go
func NewNormalizePolicy(maxDocumentChars int) (NormalizePolicy, error)
func (p NormalizePolicy) Identity() NormalizePolicyIdentity
```

`MaxDocumentChars` is the only public input. The other structural values are
private constants with these version-2 effective values:

- `MaxUnitChars`: 1,000,000;
- `MaxSourceUnitBytes`: 4,000,000;
- `MaxMetadataSourceBytes`: 65,536;
- `MaxLinkChars`: 2,048;
- `MaxChunkRunes`: 4,000;
- `ChunkOverlap`: 200; and
- `MaxChunks`: 20,000.

`NormalizePolicyIdentity` is a plain value with exported copies of version,
`MaxDocumentChars`, and every structural value above. `Identity` is the sole
source for canonical Mistral policy serialization and Msgvault's legacy
wrapper. Callers cannot mutate an identity value to change its originating
policy.

Changing the algorithm or any structural value requires normalization version
3. A caller cannot create a nonstandard version-2 identity by tuning an
individual bound. `NewNormalizePolicy` rejects an invalid document limit, and
`NormalizeDocument` independently rejects a zero or invalid policy, including
the compile-legal `NormalizePolicy{}` zero value.

### Normalized output

```go
func NormalizeDocument(
	source SourceDocument,
	policy NormalizePolicy,
) (NormalizedDocument, error)
```

The output types are:

```go
type NormalizedDocument struct {
	PolicyVersion int
	Family        string
	UnitKind      string
	Units         []NormalizedUnit
	Chunks        []Chunk
	Checksum      string
	Truncated     bool
}

type NormalizedUnit struct {
	Index        int
	SourceKey    string
	Kind         string
	Text         string
	Header       string
	Footer       string
	Dimensions   UnitDimensions
	CharCount    int
	Checksum     string
	Truncated    bool
	HeadingMarks []HeadingMark
}

type HeadingMark struct {
	CharOffset int
	Path       []string
}

type Chunk struct {
	Key         string
	Ordinal     int
	Text        string
	HeadingPath []string
	CharCount   int
	Checksum    string
	Truncated   bool
	Spans       []ChunkSpan
}

type ChunkSpan struct {
	UnitIndex int
	CharStart int
	CharEnd   int
}
```

The shorter `Chunk` and `ChunkSpan` names avoid stuttering in the `document`
package. Type names are not checksum inputs.

Version-2 chunks remain part of `NormalizeDocument`. They are stable lexical,
publication, provenance, and span units, not an embedding-model recipe. Their
checksums remain inputs to `NormalizedDocument.Checksum`. A later
`EmbeddingPlan` can merge or resplit normalized evidence under its own version
without changing normalization version 2.

## Package `document/mistral`

### Candidate formats and local detection

```go
type CandidateFormat struct {
	ID        string `json:"id"`
	Family    string `json:"family"`
	MediaType string `json:"media_type"`
	UnitKind  string `json:"unit_kind"`
}

func CandidateFormats() []CandidateFormat
func CandidateFormatByID(string) (CandidateFormat, bool)
func DetectFormat(io.ReaderAt, int64, string) (CandidateFormat, error)
```

`CandidateFormats` returns a defensive copy in stable order. A constructed or
detected `CandidateFormat` is descriptive only and cannot authorize an upload.
Detection is local and performs no credential lookup or network request.

### Capability evidence

```go
type UnitBoundMethod string

const (
	UnitBoundNone            UnitBoundMethod = "none"
	UnitBoundProviderRequest UnitBoundMethod = "provider_request"
	UnitBoundLocalExact      UnitBoundMethod = "local_exact"
)
```

The capability manifest retains its complete ordered candidate matrix, schema
and fixture-contract versions, observation date, pinned endpoint, region,
requested model, maximum unit authority, and per-format results. The JSON field
formerly named `max_pages` becomes provider-neutral `max_units`.

Each result retains format metadata, status, reason code, short fixture digest,
request fingerprint, returned model, ordinary unit count, processed units, and
provider bytes. It adds `UnitBoundMethod` and the observations required to
verify that method offline:

- `provider_request` records `FixtureUnits`, `BoundRequestedUnits`, and
  `BoundUnitsProcessed`. Validation requires
  `0 < BoundRequestedUnits < FixtureUnits` and exact equality between requested
  and processed units.
- `local_exact` records `LocalUnits`. Validation requires `LocalUnits` to equal
  the ordinary `UnitsProcessed` observation.
- `none` records no bound observations.

The public manifest values are:

```go
type ProbeStatus string

const (
	ProbeStatusPassed   ProbeStatus = "passed"
	ProbeStatusRejected ProbeStatus = "provider_rejected"
	ProbeStatusFailed   ProbeStatus = "probe_failed"
)

type CapabilityManifest struct {
	SchemaVersion        int                `json:"schema_version"`
	ProbeFixtureContract int                `json:"probe_fixture_contract"`
	ObservedOn           string             `json:"observed_on"`
	Endpoint             string             `json:"endpoint"`
	Region               string             `json:"region"`
	RequestedModel       string             `json:"requested_model"`
	MaxUnits             int                `json:"max_units"`
	Results              []CapabilityResult `json:"results"`
}

type CapabilityResult struct {
	FormatID             string          `json:"format_id"`
	Family               string          `json:"family"`
	MediaType            string          `json:"media_type"`
	UnitKind             string          `json:"unit_kind"`
	Status               ProbeStatus     `json:"status"`
	ReasonCode           string          `json:"reason_code,omitempty"`
	FixtureDigest        string          `json:"fixture_digest"`
	RequestFingerprint   string          `json:"request_fingerprint"`
	ReturnedModel        string          `json:"returned_model,omitempty"`
	UnitCount            int             `json:"unit_count,omitempty"`
	UnitsProcessed       int             `json:"units_processed,omitempty"`
	ProviderBytes        *int64          `json:"provider_bytes,omitempty"`
	UnitBoundMethod      UnitBoundMethod `json:"unit_bound_method"`
	FixtureUnits         int             `json:"fixture_units,omitempty"`
	BoundRequestedUnits  int             `json:"bound_requested_units,omitempty"`
	BoundUnitsProcessed  int             `json:"bound_units_processed,omitempty"`
	LocalUnits           int             `json:"local_units,omitempty"`
}
```

Expected bound methods live in an unexported table keyed by format ID. They do
not appear in `CandidateFormat` and grant no authority. If ordinary extraction
passes but the extra bound observation fails, the result remains visibly
`passed`, records `UnitBoundNone`, and records reason code
`bound_unverified`. The manifest remains structurally valid and explains why
the format cannot be authorized.

The capability schema increments from 2 to 3 for the new fields. The probe fixture
contract increments from 1 to 2 because the PDF fixture must contain more units
than its bound-test request. Contract-1 manifests cannot establish bounded
authority and are rejected.

```go
func EncodeCapabilityManifest(io.Writer, CapabilityManifest) error
func DecodeCapabilityManifest(io.Reader) (CapabilityManifest, error)
func (CapabilityManifest) ValidateComplete() error
```

`ValidateComplete` rejects partial, duplicated, reordered, malformed,
future-dated, target-mismatched, privacy-unsafe, or arithmetically invalid
evidence. It validates observation arithmetic without network access.

### Reusable Mistral policy

```go
type PolicyConfig struct {
	Region           string
	Model            string
	Retention        string
	Training         string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	ExtractHeader    bool
	ExtractFooter    bool
	NormalizePolicy  document.NormalizePolicy
}

func NewPolicy(PolicyConfig) (Policy, error)
func (Policy) CanonicalJSON(CapabilityManifest) ([]byte, error)
func (Policy) Fingerprint(CapabilityManifest) (string, error)
func (Policy) Authorize(CapabilityManifest, string) (FormatAuthorization, error)
```

`Policy` is opaque. The endpoint is derived from the pinned region; callers
cannot supply an endpoint, host allowlist, or media-type allowlist. Unknown
retention or training posture is invalid and cannot be serialized or
fingerprinted.

Canonical JSON is self-describing and includes `provider: "mistral"`, the
derived target, retention and training posture, document/response/unit bounds,
extraction flags, the complete normalization identity, and capability identity
for every format authorized under the policy. The capability identity is the
date-independent tuple:

```text
{format_id, unit_bound_method, request_fingerprint, fixture_digest}
```

Tuples are sorted by format ID. `ObservedOn` is validated but excluded from
identity. Reprobing identical behavior on another date therefore preserves the
fingerprint. `Fingerprint` is lowercase SHA-256 over the exact canonical JSON
bytes.

Timeout, retry count, retry-delay cap, credentials, HTTP client, spool paths,
spool quota, free-space reserve, manifest path, schedules, run budgets, message
scope, indexes, queues, storage, and serving are excluded from this identity.

### Request-fingerprint semantics

The request fingerprint remains version 2 and hashes the baseline payload:

```text
{version, endpoint, model, candidate, options}
```

The `pages` option is literal and therefore requires two distinct comparisons:

1. `Authorize` reconstructs the ordinary probe options from the manifest and
   policy. Extraction flags come from policy. Page-bounded families use the
   manifest `MaxUnits` range. The resulting fingerprint must equal the result's
   recorded request fingerprint.
2. Production options are derived separately. Extraction flags still equal
   policy, while the page range uses `Policy.MaxUnits`, which may be less than
   or equal to manifest authority.

The provider-bound over-range test is a second probe request. Its smaller page
range is represented by the bound-observation fields, not by the ordinary
recorded request fingerprint. An implementation must not compare a production
page-range fingerprint with the ordinary probe fingerprint and must not remove
the fingerprint check to accommodate smaller production bounds.

### Format authorization

`FormatAuthorization` has unexported fields, no JSON representation, and no
constructor other than `Policy.Authorize`:

```go
func (FormatAuthorization) Format() CandidateFormat
func (FormatAuthorization) PolicyFingerprint() string
```

Authorization fails unless:

- the complete manifest and target validate;
- the result corresponds to the requested static candidate;
- ordinary extraction passed;
- the expected request fingerprint matches;
- `UnitBoundMethod` is not `none`; and
- `Policy.MaxUnits` does not exceed manifest unit authority.

The full public policy fingerprint covers every format authorized by that
manifest, not only the requested format. The value attests that one format has
probe evidence and enforceable unit bounds under that policy. It does not
attest that any person approved an upload.

`Policy` also carries an unexported values-only digest over effective policy
fields without capability evidence. `Authorize` copies it into the opaque
authorization. A client stores the digest of its own policy and compares it
with the authorization before processing. This keeps `NewClient` manifest-free
while preventing use of authority issued under a different effective policy.

### Private staging

```go
type PrepareOptions struct {
	Directory         string
	DeclaredMediaType string
	ExpectedSize      int64
	ExpectedSHA256    string
	MaxSpoolBytes     int64
	MinFreeBytes      int64
}

func Prepare(
	context.Context,
	io.ReadCloser,
	Policy,
	PrepareOptions,
) (*PreparedDocument, error)
```

`Prepare` always closes the supplied source, including every failure and
cancellation path. It requires an existing private dedicated directory,
refuses symlinks and unsafe entries, takes the reservation lock, enforces
aggregate quota and free-space reserve, writes a package-prefixed 0600 file,
verifies expected size and lowercase SHA-256 while copying, detects the format
from the same bytes, syncs, and closes the file before returning.

`Policy.MaxDocumentBytes` is the one file-byte bound. `PrepareOptions` cannot
widen it. Disk quota and free-space reserve are reusable staging inputs but are
application-owned and excluded from semantic identity.

Local counters are static code capabilities keyed by candidate format ID.
`Prepare` runs a counter whenever one exists and stores the count privately;
it does not receive a manifest or authorization and does not decide whether
the counter grants authority. The manifest later proves whether that counter
matched provider semantics on the fixture.

`PreparedDocument` exposes metadata but never its path:

```go
func (*PreparedDocument) Format() CandidateFormat
func (*PreparedDocument) Size() int64
func (*PreparedDocument) SHA256() string
func (*PreparedDocument) MediaType() string
func (*PreparedDocument) Release() error
```

`Release` is idempotent. A released document is unprocessable. Cleanup removes
only the exact package-created file. `ScavengeSpoolDirectory` remains public,
uses the same reservation lock, and removes only stale regular files with the
package prefix; unrelated regular files remain and count toward quota, while
unexpected file types fail closed.

```go
func ScavengeSpoolDirectory(string, time.Time) (int, error)
```

### Client and processing

```go
type ClientConfig struct {
	APIKey        string
	Timeout       time.Duration
	MaxRetries    int
	MaxRetryDelay time.Duration
	HTTPClient    *http.Client
}

func NewClient(Policy, ClientConfig) (*Client, error)

func (*Client) Process(
	context.Context,
	*PreparedDocument,
	FormatAuthorization,
) (Result, error)
```

`NewClient` performs no network request. It validates operational settings
against package hard caps. The initial caps follow existing application and
retry behavior: timeout at most 30 minutes, at most 10 retries, and retry delay
at most 60 seconds. Defaults remain five minutes, three retries, and 30 seconds.
These values are reusable client configuration, not policy identity.

There is no public request `Options`, media-type allowlist, raw path, provider
page type, or generic processor interface. Request fields derive only from the
policy and authorization. Msgvault may define a consumer-side interface over
`Process` for worker tests or inject an `HTTPClient` backed by `httptest`.

Before every request attempt, `Process` checks context, release state,
regular-file identity, permissions, size, and SHA-256. It rejects a private
policy-digest mismatch and a detected-format/authorization mismatch before
upload.

For `local_exact`, `Process` requires the counter recorded during preparation
and rejects a count above `Policy.MaxUnits` before upload. Provider-reported
units above the local count after upload are a capability-contract breach. For
`provider_request`, production sends the bounded page range and treats a
post-response unit excess as the same kind of drift. These are the only runtime
signals that provider semantics departed from probe evidence.

Private wire structs validate model equality, complete and contiguous unit
indices, response size, processed-unit consistency, byte accounting, and
policy bounds before conversion. `Process` then returns only:

```go
type Result struct {
	Document       document.SourceDocument
	ReturnedModel  string
	UnitsProcessed int
	ProviderBytes  *int64
	Metrics        RequestMetrics
}

type RequestMetrics struct {
	Requests int
	Retries  int
	Latency  time.Duration
}

func MetricsFromError(error) RequestMetrics
```

`Document.Family` and `Document.UnitKind` come from the sniffed, authorized
candidate, never from provider JSON. Its source units are mapped exactly once
inside the client after response validation.

The public sentinel errors are `ErrPermanentResponse`, `ErrResponseTooLarge`,
`ErrTransientResponse`, `ErrSpoolCapacity`, and `ErrCapabilityContract`.
`ErrCapabilityContract` classifies post-response unit behavior that contradicts
either provider-request or local-exact evidence. The Mistral package keeps an
unexported copy of the existing bounded `Retry-After` parser and exponential
fallback with its behavior tests. Msgvault keeps its unrelated
`internal/httpretry` package for other clients.

## Probe paths

Fixture validation and authenticated probing are distinct public operations:

```go
type ProbeFixtureConfig struct {
	FixtureDirectory string
	SpoolDirectory   string
	MaxSpoolBytes    int64
	MinFreeBytes     int64
}

type ProbeConfig struct {
	Fixtures  ProbeFixtureConfig
	ObservedAt time.Time
}

func ValidateProbeFixtures(
	context.Context,
	Policy,
	ProbeFixtureConfig,
) error

func RunCapabilityProbe(
	context.Context,
	*Client,
	ProbeConfig,
) (CapabilityManifest, error)
```

There is no independent `MaxFixtureBytes` or probe `MaxUnits`; policy supplies
both limits. `ValidateProbeFixtures` verifies the complete matrix using the
same private staging mechanism and then releases it. It never needs a client,
credential, or network connection and preserves Msgvault's `--validate-only`
operation.

The loader creates an unexported `probeFixture`, not a
`*PreparedDocument`. The fixture-only request path accepts only
`probeFixture`; production `Process` accepts only `*PreparedDocument`.
Callers cannot convert between them, so user bytes cannot enter the probe path
and fixtures cannot enter production by construction.

Authenticated probes run the complete candidate matrix serially through the
fixture-only path. The manifest is safe to commit: it contains no extracted
text, filenames, raw responses, provider error bodies, credentials, user URLs,
or full fixture hashes. `ProbeFixtureSentinel` remains public for synthetic
fixture generation.

## Consent and application ownership

Local configuration, policy construction, manifest decoding and validation,
format detection, fixture validation, staging, and authorization do not
resolve credentials or perform HTTP. The application resolves an API key only
for an explicit provider operation and passes the value to `NewClient`.

Docbank does not define a `ConsentVerifier`, profile database, or consent
record. Before calling `Process`, Msgvault verifies its own consent record
against the authorization's public policy fingerprint, or against its frozen
legacy fingerprint derived from the same policy. The Docbank type name and
documentation state that capability authorization is not human consent.

Msgvault retains a thin legacy serializer because its profile policy contains
application-specific fields and its exact bytes are frozen. Shared fields come
from `mistral.Policy` and `NormalizePolicy.Identity`; application fields remain
in Msgvault. The wrapper preserves the original struct field order and exact
JSON/SHA-256 bytes. Docbank does not copy the whole legacy serializer or its
application-only fields.

## Frozen cross-repository compatibility bundle

Both repositories will contain a byte-identical immutable fixture named
`document-compat-v1.json`. Its metadata records:

- its bundle schema and fixture ID;
- `source_pr: kenn-io/msgvault#616`;
- `baseline_commit: 73d6c0b33f74c1fd072a7c0258f1cf1e80054698`;
- an explicit section ownership map; and
- synthetic inputs and exact expected values.

The sections are:

- `normalization_v2`, owned by Docbank;
- `mistral_request_fingerprint_v2`, owned by Docbank; and
- `msgvault_profile_policy_v1`, owned by Msgvault.

Each repository pins the same lowercase SHA-256 literal for the entire raw
file and rehashes the file bytes before decoding. Tests do not hash a
canonicalized reserialization. Docbank executes its production normalization
and request-fingerprint behavior against the sections it owns. Msgvault
executes its production legacy serializer against its exact JSON and
fingerprint section. Unowned sections stay in the file for traceability but
need not be asserted by that repository.

The bundle is copied once and never synchronized by tooling. Any correction or
new contract adds a new file; v1 is never edited. There is no cross-repository
test import, network fetch, CI artifact dependency, or exported compatibility
package.

The probe fixture contract-2 change does not alter the request-fingerprint
section because request fingerprints depend on the candidate and request
options, not fixture bytes. Fixture digests remain capability-manifest data.

## Migration sequence

1. Complete the focused top-level SQLite import migration, documentation
   update, changelog entry, and `pkg/` removal without aliases.
2. Invoke `superpowers:brainstorming` for `e9jz`; approval of this document is
   not approval to implement that issue.
3. Add and pin the frozen compatibility bundle before moving behavior.
4. Move normalization behavior and its production behavior tests into
   `document` without changing version-2 outputs.
5. Move Mistral formats, manifests, policy, staging, transport, private retry
   behavior, probes, and their tests into `document/mistral` while applying
   this contract's closed-construction boundaries.
6. Update Msgvault to import the public packages. Retain only its legacy policy
   wrapper and application-owned behavior listed in the non-goals.
7. Let Docbank's existing extractor adopt `document` in a separately scoped
   change when it needs the shared normalization output.

## Verification

Implementation evidence must verify behavior rather than source-file motion:

- normalization version, canonical text, headings, spans, all eight chunk
  fields, unit/chunk/document checksums, and truncation are exact;
- `NormalizeDocument` rejects `NormalizePolicy{}` and all invalid policies;
- canonical Mistral policy JSON is deterministic and its fingerprint covers
  only approved semantic identity;
- Msgvault legacy policy JSON and fingerprint remain byte-for-byte exact;
- ordinary request fingerprints remain exact when reconstructed with manifest
  options, while smaller production ranges remain authorized within manifest
  unit authority;
- manifests reject incomplete, reordered, target-mismatched, old-contract,
  malformed, or arithmetically invalid evidence;
- an unverified bound produces a valid result with `UnitBoundNone` and
  `bound_unverified`, and cannot produce a `FormatAuthorization`;
- `ValidateProbeFixtures` performs no HTTP and requires no credential;
- production cannot accept a fixture, and probing cannot accept a prepared user
  document;
- `Prepare` closes its source on every path, `Release` is idempotent, and
  cleanup/scavenging remove only package-created files;
- production re-verifies immutable bytes before every retry and rejects policy
  or format mismatch before upload;
- provider-request and local-exact post-response unit excess both return
  `ErrCapabilityContract`;
- request accounting survives classified failure and retry cancellation;
- package dependency inspection confirms that `document` and
  `document/mistral` import no vault, daemon, database, queue, or application
  internals; and
- `go test -tags fts5 ./...` passes with both `CGO_ENABLED=1` and
  `CGO_ENABLED=0`, followed by repository lint, documentation, and commit-hook
  gates required for the affected changes.

Live authenticated probes are explicit operator evidence. They are not part of
ordinary automated tests or local fixture validation.

## Approved decision summary

1. Baseline chunks remain in normalization.
2. Mistral returns `document.SourceDocument` plus accounting; wire types stay
   private.
3. Unit limits fail closed and non-`none` methods require probe observations.
4. Mistral owns staging behind opaque `PreparedDocument`; applications supply
   directories and disk budgets.
5. `mistral.Policy` owns canonical reusable identity; operational client
   settings do not enter it; Msgvault keeps an exact legacy wrapper.
6. Opaque `FormatAuthorization` represents capability authority, while consent
   remains application-owned.
7. Mistral keeps a private copy of the small retry helper and its behavior
   tests.
8. Both repositories carry one byte-identical immutable compatibility bundle.
9. Normalization version 2 is opaque with `MaxDocumentChars` as its only public
   input.
10. Public packages are top-level `document` and `document/mistral`; `pkg/` is
    removed first without compatibility aliases.
