// Package media detects still images, animated images, and video from bytes
// and evaluates them against a bounded eligibility policy.
//
// Detection sniffs container signatures and reads only the metadata needed
// to bound provider input: dimensions, frame count, and duration. It never
// decodes pixels or samples. The declared media type is recorded for callers
// but is never trusted for detection.
//
// The package does not perform filesystem, network, storage, database,
// queue, daemon, vault, or application work, and it has no notion of
// attachment ownership, roles, or hashes.
package media
