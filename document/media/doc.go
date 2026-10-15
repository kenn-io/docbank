// Package media inspects provider-bound documents and media against finite
// local limits, and retains the lower-level visual detector used by callers.
//
// Capability inspection binds exact bytes to their declared identity, policy,
// provider descriptor, processing profile, and disclosure authority. It
// rejects formats whose expansion, semantic units, external references, or
// decode work cannot be bounded locally. Visual detection reads container
// metadata without decoding pixels or samples. MP3 inspection requires contiguous
// audio frames; APEv2 tags and gaps between frames are reported as malformed.
// M4A inspection streams audio-only AAC/ALAC containers, verifies local sample
// ownership, and measures duration as no shorter than the stored frames decode
// to. It retains bounded movie metadata without decoding samples. The
// lower-level detector remains visual-only.
//
// The package does not perform filesystem, network, storage, database,
// queue, daemon, vault, or application work.
package media
