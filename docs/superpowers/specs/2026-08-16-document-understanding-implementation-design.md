# Document-understanding implementation delivery

**Status:** Approved design

**Date:** 2026-08-16

**Implementation work:** `e9jz`

**Public contract:**
`2026-08-16-document-understanding-package-contract-design.md`

**Compatibility source:** `kenn-io/msgvault#616` at
`73d6c0b33f74c1fd072a7c0258f1cf1e80054698`

## Summary

The approved public contract will land in two pull requests. The first adds the
provider-neutral `go.kenn.io/docbank/document` package and its independent
compatibility evidence. The second adds the complete
`go.kenn.io/docbank/document/mistral` package, including formats, policy,
capability evidence, private staging, bounded transport, fixture generation,
and authenticated probing.

The split follows the package boundary. `document` is independently useful to
local consumers and has no Mistral concepts. `document/mistral` must land as a
complete package because staging, policy, authorization, transport, and probes
form one upload-safety boundary.

The implementation uses behavior-first vertical slices. Every slice starts
with an oracle: either the frozen compatibility bundle or the baseline tests
that move with the behavior. The code will not pass through an intermediate
public state with exported provider wire types, reusable filesystem paths, or
upload methods that lack capability authorization.

## Shared constraints

- Msgvault remains read-only source. Temporary exports of the pinned commit are
  used for compatibility generation and downstream verification.
- No committed test or production code imports across repositories.
- The complete `document-compat-v1.json` bundle lands once and never changes.
- No live capability manifest is committed. Capability evidence is generated
  by an operator using their credential and deployment target.
- Docbank does not own application consent, scheduling, queues, persistence,
  run budgets, or search serving.
- Existing normalization, request-fingerprint, capability-fingerprint, and
  Msgvault legacy profile-fingerprint bytes remain unchanged.
- `internal/extract` adoption is a later, separately scoped change.

## Pull request 1: package `document`

### Compatibility bundle provenance

The bundle is generated before normalization is implemented in Docbank:

1. Export Msgvault commit
   `73d6c0b33f74c1fd072a7c0258f1cf1e80054698` into an owner-private temporary
   directory.
2. Add a throwaway package-local generator inside that export so it can call
   baseline `documentindex.NormalizeDocument`, the unexported Mistral
   `requestFingerprint`, and `DocumentsConfig.ProfilePolicyJSON`.
3. Generate the complete three-section bundle and hash its raw file bytes with
   SHA-256.
4. Record the exact generator source, invocation, baseline commit, and output
   digest as evidence on `e9jz`.
5. Copy only the generated bundle into
   `document/testdata/document-compat-v1.json`, then remove the temporary
   export.

The bundle metadata records:

- `source_pr: kenn-io/msgvault#616`;
- `baseline_commit: 73d6c0b33f74c1fd072a7c0258f1cf1e80054698`;
- `generated_by: msgvault@73d6c0b33f74c1fd072a7c0258f1cf1e80054698`;
  and
- the ownership of `normalization_v2`,
  `mistral_request_fingerprint_v2`, and
  `msgvault_profile_policy_v1`.

Pull request 1 pins the complete raw-file digest and asserts only
`normalization_v2`, the section owned by the package landing in that pull
request. Pull request 2 adds the `mistral_request_fingerprint_v2` assertion.
Docbank never asserts the Msgvault legacy profile-policy section.

### Package contents

The package contains:

- provider-neutral source-document and source-unit evidence;
- normalized documents, units, headings, spans, and all eight chunk fields;
- the opaque version-2 normalization policy and its read-only identity; and
- deterministic normalization, baseline chunking, truncation, and checksums.

The data flow is:

```text
SourceDocument + NormalizePolicy
        -> NormalizeDocument
        -> NormalizedDocument
```

`MaxDocumentChars` is the only public policy input. Structural version-2
values remain private. `NormalizeDocument` also rejects a zero-value or invalid
policy so callers cannot accidentally create another version-2 identity.

Goldmark is pinned to v1.7.17 and `golang.org/x/net` to v0.56.0 before the
normalization code is added. Their versions are part of the compatibility
boundary because parser behavior changes normalized bytes and checksums. The
module tidy diff is reviewed with those pins.

### Tests

- Move the baseline normalization behavior tests with the implementation.
- Keep boundary tests in package `document` and use a private test policy
  constructor where the baseline tests vary structural limits.
- Add package `document_test` coverage that constructs the policy, normalizes a
  source document, and inspects results using only the public API.
- Rehash the raw compatibility bundle before decoding it.
- Execute production normalization against every `normalization_v2` expected
  value rather than regenerating expectations.
- Run the complete Docbank suite with `-tags fts5` under both
  `CGO_ENABLED=1` and `CGO_ENABLED=0` before each slice commit.

Pull request 1 ends with repository lint, strict documentation build, and
commit-hook checks.

## Pull request 2: package `document/mistral`

Pull request 2 begins from `main` after pull request 1 lands. Its internal
commits remain reviewable by component, but the public package lands only when
the complete safety boundary is present.

### Package contents

The package contains:

- the static candidate-format registry and format detection;
- bounded, strict capability-manifest encoding and decoding;
- observed provider-request and local-exact unit-bound evidence;
- canonical reusable policy JSON, public fingerprints, and opaque format
  authorization;
- private, immutable staging through `PreparedDocument`;
- local unit counting when a format has a counter implementation;
- the bounded Mistral client and private retry helper;
- validated conversion from provider wire results to
  `document.SourceDocument`;
- deterministic synthetic fixture generation and validated native seed
  copying;
- local fixture validation and authenticated capability probing; and
- package documentation explaining the fail-closed default and version-1
  limits.

`Policy` exposes both views of normalization:

```go
func (Policy) Values() PolicyValues
func (Policy) NormalizePolicy() document.NormalizePolicy
```

`PolicyValues.Normalization` is the serializable identity.
`NormalizePolicy()` returns the executable policy that must normalize the
provider result authorized by the same Mistral policy.

### Operator evidence flow

```text
WriteProbeFixtures
        -> ValidateProbeFixtures
           local; no credential or HTTP

Client(APIKey) + validated fixtures
        -> RunCapabilityProbe
        -> CapabilityManifest
        -> operator reviews and supplies or commits it
        -> Policy.Authorize
```

The library commits no live manifest. A manifest is dated evidence for an
operator's endpoint, model, fixture contract, and credentialed probe. It is not
library-wide authority.

Version 1 can authorize at most PDF. PDF has a provider-request page bound.
Other formats may extract successfully during a probe but record
`UnitBoundNone` and remain unauthorized. Package documentation tells operators
to run the authenticated probe and supply its manifest when no upload authority
exists.

### Production flow

```text
Policy + validated manifest
        -> authorize declared format
        -> application verifies consent against auth.PolicyFingerprint()
        -> Prepare source
        -> Process rechecks sniffed format, policy, and immutable bytes
        -> Result.Document + policy.NormalizePolicy()
        -> document.NormalizeDocument
        -> prepared.Release() on success or failure
```

Authorization precedes staging so an application need not consume spool space
for a format that lacks upload authority. `Process` still compares the sniffed
format with the opaque authorization. Docbank exposes the policy fingerprint
needed by application consent but does not verify or store consent itself.

Credentials enter only when the application explicitly constructs a client.
Policy construction, manifest decoding, authorization, fixture generation,
fixture validation, format detection, and staging perform no provider request.

### Deterministic fixture generation

The public generator is:

```go
type FixtureOptions struct {
	SeedDirectory string
}

func WriteProbeFixtures(
	context.Context,
	string,
	FixtureOptions,
) error
```

It synthesizes 21 candidate formats from one deterministic implementation. ZIP
timestamps, identifiers, entry ordering, and sentinel content are fixed. The
contract-2 PDF fixture contains at least two pages so a provider-request bound
can be observed with a smaller requested range.

Five native formats require operator-supplied synthetic seeds:

- `doc`;
- `ppt`;
- `xls`;
- `numbers`; and
- `msg`.

The generator reads those exact filenames from `SeedDirectory`, requires
regular non-symlink files, copies their bytes, and validates each copy with
`DetectFormat`. Their manifest digests are operator-specific. Production code
does not synthesize Compound File Binary containers or Numbers IWA data.

The destination must not exist. Generation occurs in a temporary sibling
directory with mode 0700 and files with mode 0600. Missing seeds are reported
together. Any missing, mislabeled, unsafe, failed, or cancelled input prevents
the final rename. Failure cleanup removes only paths created by the generator.

Fixture contract 2 still requires the complete 26-format matrix. The later
`km1t` design may allow missing native seeds to become non-authorizing manifest
results; this implementation does not.

### Failure and safety behavior

Authorization fails closed when no complete manifest is supplied or no format
has authority. The error explains the recovery action: run the authenticated
capability probe and supply its manifest.

Manifest input is capped at 1 MiB and decoded strictly. Validation rejects
partial, duplicated, reordered, target-mismatched, old-contract, malformed, or
arithmetically inconsistent evidence.

`Prepare` always closes its source. Quota and free-space refusals return
`ErrSpoolCapacity` so an application may reschedule them. Size, hash, format,
and permission failures are terminal. Failure and cancellation remove the
partial file created by that call. `Release` is idempotent and makes its
prepared document unprocessable.

`ScavengeSpoolDirectory` removes only stale package-prefixed regular files.
Unrelated regular files remain and continue to count toward quota. A symlink or
other non-regular entry stops scavenging with an error.

Before each request, `Process` rejects:

- a released prepared document;
- a client-policy or authorization-policy digest mismatch;
- a sniffed-format and authorization mismatch;
- a prepared size above the client policy's `MaxDocumentBytes`;
- changed size, identity, permissions, or SHA-256; and
- every redirect, including when the caller supplied the HTTP client.

`NewClient` shallow-copies the supplied client before replacing its redirect
policy. It does not mutate caller-owned configuration. Each retry reopens and
rehashes the staged file.

`ErrPermanentResponse` and `ErrResponseTooLarge` never retry. Only
`ErrTransientResponse` retries. Retry-After accepts bounded integer seconds,
rejects overflow, and otherwise uses the existing capped exponential fallback.
Each attempt has its own timeout. Cancellation during an attempt or wait
returns with request accounting intact.

For `provider_request`, provider units above the requested bound return
`ErrCapabilityContract`. For `local_exact`, the provider count must equal the
prepared local count; either direction of mismatch returns
`ErrCapabilityContract`. Equality is the meaning of `local_exact`, not merely
a cost bound. A provider that may omit empty sheets, slides, or other units
cannot use this method. A future less-than-or-equal method such as
`local_bound` would require a new contract and new probe evidence rather than a
relaxation of `local_exact`. Version 1 defines no local-exact formats.

Private wire data is checked for model equality, response size, complete and
contiguous unit indices, processed-unit consistency, byte accounting, and
policy bounds before conversion to `document.SourceDocument`.

A format whose additional bound probe does not verify may retain successful
extraction evidence while recording `UnitBoundNone` and `bound_unverified`.
That result remains a valid manifest entry but cannot produce a
`FormatAuthorization`.

### Tests

Baseline format, detection, staging, client, fixture, and probe tests move with
their production components. Tests add the approved closed-construction and
failure cases rather than rewriting baseline behavior from the specification.

Capability manifests are constructed programmatically in test code. No
synthetic manifest JSON is committed. The full-matrix factory includes PDF
provider-request observations and supports deliberate invalid variants for
manifest validation, authorization, processing, and `bound_unverified`
coverage.

Fixture tests:

- generate the 21 synthesized formats twice and compare a pinned SHA-256 for
  each format across both runs;
- verify the contract-2 PDF page count is at least two;
- run production format detection and sentinel checks on all synthesized
  files;
- use the baseline test-only minimal Compound File Binary and Numbers ZIP
  builders as seed inputs for the five native formats;
- verify native outputs are exact byte copies of validated seed inputs;
- verify missing, mislabeled, symlinked, cancelled, or otherwise unsafe seeds
  leave no destination; and
- verify 0700 directory and 0600 file permissions where the platform exposes
  Unix modes.

Transport and lifecycle tests cover redirects with default and injected
clients, response classification, retry limits, Retry-After overflow,
per-attempt timeout, cancellation accounting, byte revalidation, cross-policy
byte limits, policy mismatch, exact local counts, provider-request excess,
idempotent release, failure cleanup, and fail-closed scavenging.

Package `mistral_test` exercises the public surface, including the actionable
no-authority error. Local fixture validation tests use an HTTP transport that
fails the test if called, proving the operation performs no network request.

The complete Docbank suite runs with `-tags fts5` under both CGO settings before
each slice commit. Pull request 2 ends with repository lint, strict
documentation build, and commit-hook checks.

## Temporary Msgvault consumer verification

After the Docbank API is complete:

1. Export Msgvault commit
   `73d6c0b33f74c1fd072a7c0258f1cf1e80054698` to an owner-private temporary
   directory.
2. Apply the real import and API migration there, retaining Msgvault's legacy
   profile-policy wrapper and all application-owned behavior.
3. Add a temporary module replacement pointing `go.kenn.io/docbank` at the
   exact local Docbank commit under verification.
4. Run focused document-index behavior tests with Msgvault's required
   `fts5 sqlite_vec` tags.
5. Run whole-module `go build -tags "fts5 sqlite_vec" ./...` and
   `go vet -tags "fts5 sqlite_vec" ./...`.
6. Record on `e9jz` the exact migration patch, patch digest, Docbank commit,
   commands, and results.
7. Remove the complete temporary export and its build output.

This verification proves the real downstream consumer compiles and retains its
application boundary without committing a cross-repository dependency.

## Completion boundary

Pull request 1 is complete when the provider-neutral package passes the frozen
normalization oracle and public API tests in both supported SQLite build modes.
Pull request 2 is complete when the entire Mistral safety boundary, fixture and
probe tooling, request-fingerprint oracle, and temporary Msgvault consumer all
pass their specified checks.

`e9jz` remains open until both pull requests are complete. The Superpowers
design artifacts remain available until the related Msgvault document-indexing
work can proceed against the released Docbank packages.
