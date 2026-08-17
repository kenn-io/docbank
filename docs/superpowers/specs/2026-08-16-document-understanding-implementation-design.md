# Document-understanding package architecture

**Date:** 2026-08-16

## Purpose

Docbank's document-understanding architecture separates reusable behavior into
two top-level packages.

- `go.kenn.io/docbank/document` defines provider-neutral source evidence,
  deterministic normalization, headings, spans, and baseline chunks.
- `go.kenn.io/docbank/document/mistral` contains Mistral-specific formats,
  capability evidence, policy, private staging, transport, and probes.

The package boundary keeps local extraction independent of provider concepts.
Applications retain consent records, scheduling, queues, persistence, budgets,
configuration, and search serving.

No committed production or test code imports across the Docbank and Msgvault
repositories. Msgvault imports released Docbank packages as an ordinary module
dependency and retains its application-specific legacy policy serializer.

## Provider-neutral document package

The `document` package contains:

- `SourceDocument` and `SourceUnit` input evidence;
- normalized documents and units;
- heading marks and chunk spans;
- all eight chunk fields: key, ordinal, text, heading path, character count,
  checksum, truncation state, and spans;
- an opaque version-2 normalization policy; and
- deterministic normalization, truncation, baseline chunking, and checksums.

The data flow is:

```text
SourceDocument + NormalizePolicy
        -> NormalizeDocument
        -> NormalizedDocument
```

`MaxDocumentChars` is the only public policy input. Structural version-2
values remain private. `NormalizePolicy.Identity()` exposes the effective
values read-only for serialization. `NormalizeDocument` rejects a zero-value
or invalid policy, so callers cannot accidentally create another version-2
identity.

Baseline chunks are part of normalized evidence. They support lexical search,
publication, provenance, and stable document checksums. Model-specific
embedding recipes can merge or split this evidence without changing the
normalization contract.

Markdown soft breaks remain separating whitespace, and whitespace at inline
code and link boundaries stays on the same side of the boundary as the source.
These rules prevent adjacent evidence or punctuation from moving when HTML
tokens are converted to canonical text.

### Compatibility evidence

`document/internal/compattest/testdata/document-compat-v1.json` is a frozen,
byte-identical compatibility bundle shared with Msgvault. Its expected values
were produced from Msgvault source at
`73d6c0b33f74c1fd072a7c0258f1cf1e80054698`, with the approved pre-release
soft-break correction applied to normalization version 2.

The bundle contains three independently owned sections:

- `normalization_v2`;
- `mistral_request_fingerprint_v2`; and
- `msgvault_profile_policy_v1`.

Docbank verifies the raw-file digest before decoding the bundle. Each package
asserts only the section it owns. Msgvault owns the legacy profile-policy
section. A future contract adds a new bundle instead of editing this file.

Goldmark v1.7.17 and `golang.org/x/net` v0.56.0 are pinned parser dependencies.
Parser behavior is part of normalization identity because it can change text
and checksums. A dependency update must preserve the byte-level compatibility
evidence or use a new normalization version.

## Mistral package

`document/mistral` forms one upload-safety boundary. It owns format detection,
capability evidence, canonical policy identity, opaque authorization,
immutable private staging, bounded transport, deterministic fixtures, local
validation, and authenticated probes.

The library ships no live capability manifest. A manifest is dated evidence
for an operator's endpoint, model, fixture contract, and credentialed probe.
Without a complete validated manifest, production upload authorization fails
closed and explains that the operator must run the authenticated probe and
supply its manifest.

### Policy and authorization

`Policy` exposes both a serializable identity and the executable normalization
policy:

`MaxUnits` must be between 3 and 5,000. The lower bound keeps the two-unit PDF
fixture strictly below the policy limit while the bound request processes only
one unit, demonstrating provider-side enforcement before any production upload
is allowed.

```go
func (Policy) Values() PolicyValues
func (Policy) NormalizePolicy() document.NormalizePolicy
```

`PolicyValues.Normalization` carries the read-only normalization identity.
`NormalizePolicy()` returns the policy that must normalize the provider result
authorized by the same Mistral policy.

The pinned `eu` policy target is
`https://api.eu.mistral.ai/v1/ocr`. Policy construction accepts only the
package-pinned regional model, and a successful authenticated capability probe
provides the live availability evidence before any format can be authorized.

`Policy.Authorize` combines policy limits with a validated capability
manifest and returns an opaque, non-serializable `FormatAuthorization`. The
value attests only that probe evidence and enforceable bounds authorize a
format under the policy. It does not attest consent.

The authorization carries a private values-only policy digest and the public
full fingerprint. `Process` compares the private digest with its client policy
without requiring the client to retain the manifest. The application compares
its consent record with the public fingerprint.

### Capability evidence

A capability manifest records the target, model, fixture contract, detected
format, request fingerprint, fixture digest, ordinary extraction observations,
and unit-bound observations. Input is limited to 1 MiB and decoded strictly.
Validation rejects partial, duplicated, reordered, target-mismatched,
old-contract, malformed, privacy-unsafe, or arithmetically inconsistent
evidence.

Unit-bound methods are:

- `provider_request`, where the provider request limits processed units;
- `local_exact`, where a local counter must equal the provider count; and
- `none`, which grants no production upload authority.

Each non-`none` value is backed by recorded probe observations. An unverified
bound claim may retain successful extraction evidence while recording `none`
and `bound_unverified`; it cannot produce an authorization.

Version 1 authorizes at most PDF because PDF has a provider-request page bound.
Other formats may extract successfully during a probe but remain unauthorized
until they have an enforceable, observed bound.

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

Credentials enter only when the application explicitly constructs a client.
Policy construction, manifest decoding, authorization, fixture generation,
fixture validation, format detection, and staging perform no provider request.

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

Authorization precedes staging so refused formats do not consume spool space.
`Process` still compares the sniffed format with the opaque authorization.
Docbank exposes the policy fingerprint needed by application consent but does
not verify or store consent itself.

### Fixture generation

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

It synthesizes 21 candidate formats deterministically. ZIP timestamps,
identifiers, entry ordering, and sentinel content are fixed. The contract-2
PDF fixture contains two pages so a smaller provider-request range can
demonstrate a bound.

Five native formats require operator-supplied synthetic seeds: `doc`, `ppt`,
`xls`, `numbers`, and `msg`. The generator copies only regular, non-symlink
files with the expected names and validates their detected formats. Their
manifest digests are operator-specific.

The destination must not exist and its parent must already be private.
Generation uses a private temporary sibling directory and private files: mode
0700 and 0600 on Unix, and restricted current-user DACLs on Windows. Missing
seeds are reported together. Any missing, mislabeled, unsafe, failed, or
cancelled input prevents publication. Cleanup removes only paths created by the
generator. Loading revalidates the fixture directory and files and pins their
identities before staging them for a probe.

### Staging and transport safety

`Prepare` always closes its source. Quota and free-space refusals return
`ErrSpoolCapacity`. Platform disk-full and disk-quota failures while creating,
copying, syncing, securing, or closing the staged file use the same
classification. Unsafe directory entries, unrelated filesystem I/O failures,
and size, hash, format, or permission failures are terminal. Failure and
cancellation remove the partial file created by that call; close or removal
failures are joined into the returned error rather than discarded.
`Release` takes the spool reservation lock, is idempotent, and makes its
prepared document unprocessable.

`ScavengeSpoolDirectory` uses a verified persistent lock inside the private
spool directory and removes only stale package-prefixed regular files. The lock
and unrelated regular files remain and continue to count toward quota. A
symlink or other non-regular entry stops scavenging before any stale file is
removed.

Before every request, `Process` rejects:

- a released prepared document;
- a client-policy or authorization-policy digest mismatch;
- a sniffed-format and authorization mismatch;
- a prepared size above the client policy's `MaxDocumentBytes`;
- changed size, identity, Unix permissions or Windows DACL, or SHA-256; and
- every redirect, including when the caller supplies the HTTP client.

An injected HTTP client is shallow-copied. Its timeout is replaced when it is
non-positive or greater than the configured attempt timeout. Each retry reopens
the staged file and copies it into a request-local immutable buffer while
verifying its hash. Only that snapshot is streamed to the provider.

`ErrPermanentResponse` and `ErrResponseTooLarge` do not retry. Only
`ErrTransientResponse` retries. Retry-After accepts bounded integer seconds,
rejects overflow, and otherwise uses a capped exponential fallback.
Cancellation during an attempt or wait preserves request accounting.

Provider units above a `provider_request` bound return
`ErrCapabilityContract`. Under `local_exact`, the provider count must equal the
prepared local count in both directions. A provider that may omit empty units
cannot use `local_exact`; a less-than-or-equal method requires a separate
contract and probe observation.

Private wire data is checked for model equality, response size, a nonempty page
set, duplicate JSON object keys, complete and contiguous unit indices,
source-unit dimension bounds, processed-unit consistency, byte accounting, and
policy bounds before conversion to `document.SourceDocument`.
