// Package trafilatura renders supplied HTML bytes through an operator-pinned local bridge.
//
// Profile.ExecutableSHA256 pins the complete executable with a lowercase SHA-256
// digest. Construction and every render verify it. Profile.Runner defaults to
// document/isolate's native Linux runner, which verifies the executable actually
// launched and admits only the provider's fixed --protocol docbank-trafilatura/v2
// arguments and environment. HTML validation and response parsing stay here.
//
// IsolatedRunner, IsolatedRunRequest, IsolatedRunResult, IsolationRequirements and
// IsolationAttestation alias document/isolate's types. Existing error sentinels
// share their values with that package. The shared native identity v3 changes
// native-backed provider policy and rendition descriptor fingerprints.
//
// Unsupported hosts, including Windows and macOS, require an explicitly trusted
// runner. Linux also refuses execution when required controls are unavailable.
// Injected runners retain their own pinned identities; deployments must audit
// their implementations independently of their returned attestations.
// Native controls impose no CPU or memory quotas or general host filesystem confinement.
package trafilatura
