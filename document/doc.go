// Package document defines the storage-neutral rendition-provider boundary and
// converts provider-neutral source evidence into deterministic normalized
// units, headings, spans, and chunks.
//
// Provider adapters receive only one-shot, hash-bound AuthorizedUpload readers.
// Descriptors, authorizations, results, receipts, and errors are validated here;
// provider-specific network and credential handling remains outside this
// package. The package does not perform filesystem, network, storage, database,
// queue, daemon, vault, or application work.
package document
