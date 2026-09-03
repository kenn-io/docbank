// Package trafilatura renders supplied HTML bytes through an operator-pinned
// local Trafilatura bridge.
//
// A nil Profile.Runner selects the production Linux runner. It executes a
// digest-verified sealed copy of the bridge inside new user, network, PID, and
// mount namespaces. Landlock hides host files outside a read-only system-runtime
// allowlist, while private mounts provide /proc and temporary storage. The runner
// bounds stdout and kills the PID-namespace init on cancellation or overflow so
// the kernel reaps every descendant. It fails closed when the host disables the
// required namespace or Landlock controls. The native runner is not available on
// macOS or Windows; callers there must inject a separately audited IsolatedRunner
// or construction fails before any child can launch.
//
// An injected runner is an explicit trusted deployment boundary. Its immutable
// identity and exact per-run attestation are checked, but a dishonest runner can
// still lie. Deployments using one must audit and pin its platform-specific
// isolation implementation.
package trafilatura
