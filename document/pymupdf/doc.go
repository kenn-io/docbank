// Package pymupdf renders exact supplied PDF bytes through an isolated local bridge.
//
// Set Profile.ExecutableSHA256 to the lowercase SHA-256 of the complete bridge
// executable. New rejects a missing or mismatched digest and verifies a bounded
// regular non-symlink executable. Each render checks its digest and runner identity
// again. RuntimeIdentity still identifies the configured provider runtime.
// Profile v2 binds the digest, runner identity and fixed environment alongside
// the executable path, protocol, runtime identity and bounds.
//
// Profile.Runner accepts a trusted isolate.IsolatedRunner. A nil runner selects
// native Linux isolation. Unsupported hosts, including Windows and macOS, require
// explicit trusted injection. Unavailable required controls cause refusal.
// The adapter sends only --protocol docbank-pymupdf/v1 and exact verified stdin,
// then checks every attested identity and control before parsing PDF pages.
//
// document/isolate owns namespaces, private read-only /proc, no_new_privs,
// seccomp, sealed executable and launch records, parent death, deadlines,
// bounded output and descendant cleanup. It imposes no CPU or memory quotas
// and no general host filesystem confinement. Injected attestations prove
// adapter compliance; deployments must audit the runner's operating-system controls.
package pymupdf
