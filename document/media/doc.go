// Package media inspects provider-bound documents and media against finite
// local limits, and retains the lower-level visual detector used by callers.
//
// Capability inspection binds exact bytes to their declared identity, policy,
// provider descriptor, processing profile, and disclosure authority. It
// rejects formats whose expansion, semantic units, external references, or
// decode work cannot be bounded locally. Visual detection reads container
// metadata without decoding pixels or samples.
//
// The package does not perform filesystem, network, storage, database,
// queue, daemon, vault, or application work.
package media
