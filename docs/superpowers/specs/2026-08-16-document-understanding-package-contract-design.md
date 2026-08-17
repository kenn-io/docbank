# Public document-understanding package contract

**Status:** Approved design

**Date:** 2026-08-16

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

## Goals

The contract must:

- provide one authoritative implementation of deterministic normalization,
  headings, spans, baseline chunks, truncation, and checksums;
- preserve normalization policy version 2, fix the baseline soft-break
  separator defect before release, and preserve all other normalization,
  request-fingerprint, capability-fingerprint, and Msgvault legacy profile
  fingerprint behavior;
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

### Parser dependencies

The baseline normalizer directly depends on:

- `github.com/yuin/goldmark` and `github.com/yuin/goldmark/extension` from
  Goldmark `v1.7.17`; and
- `golang.org/x/net/html` from `golang.org/x/net v0.56.0`.

Docbank will add those module versions as direct requirements when the code
moves. Parser behavior is part of normalization version 2 because it can
change canonical text and every downstream checksum. A later dependency
upgrade may remain version 2 only when the complete normalization suite and
the frozen compatibility bundle remain byte-identical. After version 2 first
ships, any output change requires normalization version 3.

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
  `0 < BoundRequestedUnits < FixtureUnits < CapabilityManifest.MaxUnits`, exact
  equality between requested and bound-test processed units, and
  `FixtureUnits == UnitCount == UnitsProcessed` for the ordinary request. The
  ordinary full-range request therefore observes the complete fixture instead
  of trusting a declared fixture count.
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

type PolicyValues struct {
	Provider         string
	Endpoint         string
	Region           string
	Model            string
	Retention        string
	Training         string
	MaxDocumentBytes int64
	MaxResponseBytes int64
	MaxUnits         int
	ExtractHeader    bool
	ExtractFooter    bool
	Normalization    document.NormalizePolicyIdentity
}

func NewPolicy(PolicyConfig) (Policy, error)
func (Policy) Values() PolicyValues
func (Policy) CanonicalJSON(CapabilityManifest) ([]byte, error)
func (Policy) Fingerprint(CapabilityManifest) (string, error)
func (Policy) Authorize(CapabilityManifest, string) (FormatAuthorization, error)
```

`Policy` is opaque. The endpoint is derived from the pinned region; callers
cannot supply an endpoint, host allowlist, or media-type allowlist. `Values`
returns a read-only copy of every reusable effective value, including the
derived endpoint. Mutating the copy cannot change the policy. Canonical JSON,
the private values-only digest, and Msgvault's legacy wrapper all read this
view, so the wrapper does not duplicate the region-to-endpoint mapping or any
other shared constant. Unknown retention or training posture is invalid and
cannot be serialized or fingerprinted.

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

The now-private options value retains the exact baseline fingerprint JSON
shape and struct-field order: `pages`, `extract_header`, then `extract_footer`.
`pages` does not use `omitempty`, so a non-page-bounded format encodes
`"pages":""`. The `mistral_request_fingerprint_v2` compatibility section
guards this private representation as observable behavior.

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
aggregate quota and free-space reserve, writes a package-prefixed file with
owner-only Unix permissions or a restricted Windows DACL, verifies expected
size and lowercase SHA-256 while copying, detects the format from the same
bytes, syncs, and closes the file before returning.

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
uses a verified reservation lock inside the private spool directory, and
removes only stale regular files with the package prefix. The persistent lock
and unrelated regular files remain and count toward quota, while unexpected
file types fail closed.

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
against package hard caps. The timeout cap of 30 minutes and retry-count cap of
10 move the existing Msgvault application bounds into the reusable client.
The 60-second retry-delay cap is new package enforcement chosen to match the
existing retry helper's maximum exponential fallback. Defaults remain five
minutes, three retries, and 30 seconds. As in the baseline client, a negative
retry count is invalid, zero selects the default of three, and values 1 through
10 are explicit. Version 1 does not represent an explicit zero-retry mode.
These values are reusable client configuration, not policy identity.

`NewClient` shallow-copies the supplied `HTTPClient`, or a package default, and
replaces its redirect policy so every redirect is rejected. A redirect must
never replay document bytes to an origin other than the exact endpoint derived
from policy. The package does not mutate a caller-owned client, and an injected
client cannot relax this boundary.

There is no public request `Options`, media-type allowlist, raw path, provider
page type, or generic processor interface. Request fields derive only from the
policy and authorization. Msgvault may define a consumer-side interface over
`Process` for worker tests or inject an `HTTPClient` backed by `httptest`.

Before every request attempt, `Process` checks context, release state,
regular-file identity, Unix permissions or Windows DACL, size, and SHA-256
while copying the bytes into a request-local immutable buffer. The provider
request reads only that verified snapshot. `Process` rejects a private
policy-digest mismatch and a detected-format/authorization mismatch before
upload. It also requires the prepared size to be no greater than the client
policy's `MaxDocumentBytes`, even when another policy originally prepared the
document. Preparation cannot therefore carry a larger byte allowance into a
stricter processing policy.

For `local_exact`, `Process` requires the counter recorded during preparation
and rejects a count above `Policy.MaxUnits` before upload. Provider-reported
units must equal the local count after upload; either a smaller or larger count
is a capability-contract breach. For `provider_request`, production sends the
bounded page range and treats a post-response unit excess as the same kind of
drift. These are the only runtime signals that provider semantics departed
from probe evidence.

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

## Additional public surface

The public contract includes these additions:

- `Policy.NormalizePolicy() document.NormalizePolicy` returns the executable
  normalization policy covered by the Mistral policy identity. This makes the
  safe production flow direct: the authorized provider result is normalized
  with the policy that authorized it.
- `FixtureOptions` and `WriteProbeFixtures` provide one public deterministic
  fixture writer for Docbank and downstream applications. The writer
  synthesizes 21 formats and copies validated operator seeds for `doc`, `ppt`,
  `xls`, `numbers`, and `msg`, avoiding either duplicate fixture bytes or a
  dependency on a Docbank executable.
- Capability-manifest input is capped at 1 MiB before strict decoding. This
  makes the operator-supplied recovery path explicitly bounded as well as
  structurally validated.

The fixture writer performs no network request and requires no credential. It
publishes only a complete fixture-contract-2 directory; the five native seed
digests remain operator-specific.

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
- `generated_by: msgvault@73d6c0b33f74c1fd072a7c0258f1cf1e80054698+soft-break-separation`;
- an explicit section ownership map; and
- synthetic inputs and exact expected values.

The sections are:

- `normalization_v2`, owned by Docbank;
- `mistral_request_fingerprint_v2`, owned by Docbank; and
- `msgvault_profile_policy_v1`, owned by Msgvault.

Version-1 expected values are generated before code motion by a throwaway
package-local generator run against the Msgvault baseline commit. The export
applies the approved soft-break separator correction to the baseline
normalizer, then calls `documentindex.NormalizeDocument`. It calls the
unmodified Mistral `requestFingerprint` and
`DocumentsConfig.ProfilePolicyJSON` implementations. The moved Docbank
implementation never generates or rewrites the expected values it is tested
against. This provenance makes the bundle an independent compatibility oracle
rather than a self-consistency fixture.

Each repository pins the same lowercase SHA-256 literal for the entire raw
file and rehashes the file bytes before decoding. Tests do not hash a
canonicalized reserialization. Docbank executes its production normalization
and request-fingerprint behavior against the sections it owns. Msgvault
executes its production legacy serializer against its exact JSON and
fingerprint section. Unowned sections stay in the file for traceability but
need not be asserted by that repository.

The corrected pre-release bundle is frozen when the package first merges and
is never synchronized by tooling. Any later correction or new contract adds a
new file; the merged v1 file is never edited. There is no cross-repository test
import, network fetch, CI artifact dependency, or exported compatibility
package.

The probe fixture contract-2 change does not alter the request-fingerprint
section because request fingerprints depend on the candidate and request
options, not fixture bytes. Fixture digests remain capability-manifest data.

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
7. Mistral keeps its retry behavior private to the provider package.
8. Both repositories carry one byte-identical immutable compatibility bundle.
9. Normalization version 2 is opaque with `MaxDocumentChars` as its only public
   input.
10. Public packages are top-level `document` and `document/mistral`, without
    compatibility aliases under `pkg/`.
