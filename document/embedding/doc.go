// Package embedding prepares deterministic, provider-neutral document inputs
// for text embedding and optional pre-embedding distillation.
//
// The package performs no network, storage, queue, database, or consent work.
// Applications own those effects and use the fingerprints returned here as
// stable identities for derived artifacts and egress approvals.
package embedding
