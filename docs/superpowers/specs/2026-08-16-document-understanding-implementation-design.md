# Document-understanding package architecture

**Date:** 2026-08-16

## Purpose

Docbank's document-understanding architecture separates reusable behavior into
two top-level packages. The provider-neutral package is current; the Mistral
package is explicitly planned below.

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

!!! info "Planned"

    `document/mistral` will form one upload-safety boundary. The package will
    include format detection, capability evidence, canonical policy identity,
    opaque authorization, immutable private staging, bounded transport,
    deterministic fixtures, local validation, and authenticated probes.

    The library will ship no live capability manifest. A manifest is dated
    evidence for an operator's endpoint, model, fixture contract, and
    credentialed probe. Without a complete validated manifest, production
    upload authorization fails closed and explains that the operator must run
    the authenticated probe and supply its manifest.

### Policy and authorization

!!! info "Planned"

    `Policy` will expose both a serializable identity and the executable
    normalization policy:

    ```go
    func (Policy) Values() PolicyValues
    func (Policy) NormalizePolicy() document.NormalizePolicy
    ```

    `PolicyValues.Normalization` will carry the read-only normalization
    identity. `NormalizePolicy()` will return the policy that must normalize
    the provider result authorized by the same Mistral policy.

    `Policy.Authorize` will combine policy limits with a validated capability
    manifest. It will return an opaque, non-serializable
    `FormatAuthorization`. The value will attest only that probe evidence and
    enforceable bounds authorize a format under the policy. It will not attest
    consent.

    The authorization will carry a private values-only policy digest and the
    public full fingerprint. `Process` will compare the private digest with its
    client policy without requiring the client to retain the manifest. The
    application will compare its consent record with the public fingerprint.

### Capability evidence

!!! info "Planned"

    A capability manifest will record the target, model, fixture contract,
    detected format, request fingerprint, fixture digest, ordinary extraction
    observations, and unit-bound observations. Input will be limited to 1 MiB
    and decoded strictly. Validation will reject partial, duplicated,
    reordered, target-mismatched, old-contract, malformed, or arithmetically
    inconsistent evidence.

    Unit-bound methods will be:

    - `provider_request`, where the provider request limits processed units;
    - `local_exact`, where a local counter must equal the provider count; and
    - `none`, which grants no production upload authority.

    Each non-`none` value will be backed by recorded probe observations. An
    unverified bound claim may retain successful extraction evidence while
    recording `none` and `bound_unverified`; it cannot produce an
    authorization.

    Version 1 will authorize at most PDF because PDF has a provider-request
    page bound. Other formats may extract successfully during a probe but will
    remain unauthorized until they have an enforceable, observed bound.

### Operator evidence flow

!!! info "Planned"

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

    Credentials will enter only when the application explicitly constructs a
    client. Policy construction, manifest decoding, authorization, fixture
    generation, fixture validation, format detection, and staging will perform
    no provider request.

### Production flow

!!! info "Planned"

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

    Authorization precedes staging so refused formats do not consume spool
    space. `Process` will still compare the sniffed format with the opaque
    authorization. Docbank will expose the policy fingerprint needed by
    application consent but will not verify or store consent itself.

### Fixture generation

!!! info "Planned"

    The public generator will be:

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

    It will synthesize 21 candidate formats deterministically. ZIP timestamps,
    identifiers, entry ordering, and sentinel content will be fixed. The
    contract-2 PDF fixture will contain at least two pages so a smaller
    provider-request range can demonstrate a bound.

    Five native formats will require operator-supplied synthetic seeds: `doc`,
    `ppt`, `xls`, `numbers`, and `msg`. The generator will copy only regular,
    non-symlink files with the expected names and will validate their detected
    formats. Their manifest digests will be operator-specific.

    The destination must not exist. Generation will use a temporary sibling
    directory with mode 0700 and files with mode 0600. Missing seeds will be
    reported together. Any missing, mislabeled, unsafe, failed, or cancelled
    input will prevent publication. Cleanup will remove only paths created by
    the generator.

### Staging and transport safety

!!! info "Planned"

    `Prepare` will always close its source. Quota and free-space refusals will
    return `ErrSpoolCapacity`; size, hash, format, and permission failures will
    be terminal. Failure and cancellation will remove the partial file created
    by that call. `Release` will be idempotent and will make its prepared
    document unprocessable.

    `ScavengeSpoolDirectory` will remove only stale package-prefixed regular
    files. Unrelated regular files will remain and continue to count toward
    quota. A symlink or other non-regular entry will stop scavenging with an
    error.

    Before every request, `Process` will reject:

    - a released prepared document;
    - a client-policy or authorization-policy digest mismatch;
    - a sniffed-format and authorization mismatch;
    - a prepared size above the client policy's `MaxDocumentBytes`;
    - changed size, identity, permissions, or SHA-256; and
    - every redirect, including when the caller supplies the HTTP client.

    An injected HTTP client will be shallow-copied. Its timeout will be
    replaced when it is non-positive or greater than the configured attempt
    timeout. Each retry will reopen and rehash the staged file.

    `ErrPermanentResponse` and `ErrResponseTooLarge` will not retry. Only
    `ErrTransientResponse` will retry. Retry-After will accept bounded integer
    seconds, reject overflow, and otherwise use a capped exponential fallback.
    Cancellation during an attempt or wait will preserve request accounting.

    Provider units above a `provider_request` bound will return
    `ErrCapabilityContract`. Under `local_exact`, the provider count must equal
    the prepared local count in both directions. A provider that may omit empty
    units cannot use `local_exact`; a less-than-or-equal method would require a
    separate contract and probe observation.

    Private wire data will be checked for model equality, response size,
    complete and contiguous unit indices, processed-unit consistency, byte
    accounting, and policy bounds before conversion to
    `document.SourceDocument`.
