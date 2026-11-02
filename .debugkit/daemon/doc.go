// Package daemon provides reusable infrastructure for local background
// daemons used by CLI tools.
//
// It covers the lifecycle pieces that should be shared across tools:
// endpoint parsing and HTTP clients for TCP or Unix sockets, atomic
// daemon.<pid>.json runtime files with opaque process-creation identity,
// distinct start and lifetime-owner locks, PID plus ping-based discovery, and
// a small Manager that can auto-start an application daemon through a
// caller-provided StartFunc. Process creation identity is optional when the
// platform cannot inspect it and an unknown identity never proves ownership.
// Manager uses runtime-file discovery by default and accepts a FindFunc when
// callers need custom probing while retaining its start locking, re-discovery,
// polling, and timeout behavior.
//
// RuntimeStore.TryAcquireStartLock supports nonblocking startup markers and
// probes. A successful caller holds the same lock as Manager.Ensure until it
// invokes the release function. Callers may inspect and clean up their own
// startup state before releasing. Contention includes holders in this process;
// callers that need to recognize their own marker must synchronize acquisition,
// registration of the release function, probing, and release themselves.
// Acquisition errors leave the state unknown. A caller may retry after resolving
// an error, but only successful acquisition authorizes cleanup or launch.
// Snapshot age, process identity checks, and retry policy belong to the caller.
// A released probe grants no reservation for a subsequent launch.
//
// Authenticated discovery must prove an endpoint before any bearer credential
// is sent. Servers construct a Proof from the shared daemon token and register
// its NewPingHandler result using the same RuntimeRecord they write to
// RuntimeStore. Clients construct their own Proof from that token, place it in
// DiscoverOptions.Proof, and wait for Manager.Find or Manager.Ensure to succeed
// before using a credential-bearing client. The package sends a random
// challenge and verifies an identity-bound HMAC; it does not send or expose the
// token. Custom FindFunc callbacks must preserve this ordering and can use
// Proof.Probe directly.
//
// Stop verifies a recorded endpoint, runs the caller's shutdown request, and
// waits for exit with a timeout. StopOptions.Proof is required for TCP records
// in every process scope; StopOptions.Exited alone decides exit when set.
//
// Application-specific server setup, database wiring, auth policy, CLI
// commands, and shutdown behavior remain in the owning tool.
package daemon
