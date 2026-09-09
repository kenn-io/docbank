// Package uploadcapability issues local inspection evidence without retaining
// source filenames, paths, or bytes. Only document's inspectors may mint it.
package uploadcapability

import "errors"

// Facts is the bounded portion of an inspection needed by embedding adapters.
type Facts struct {
	Checksum, SourceSHA256                                           string
	SourceBytes                                                      int64
	MediaFamily, MediaType                                           string
	DescriptorFingerprint, ProfileFingerprint, DisclosureFingerprint string
	Pixels, Frames, DurationMS, Pages                                int64
	MaxSourceBytes, MaxPixels, MaxFrames, MaxDurationMS, MaxPages    int64
}

// Proof is an immutable, process-local inspection result. Its zero value has
// no authority. Facts contain no filename, even when filename disclosure is allowed.
type Proof struct {
	facts Facts
	local bool
}

// New is restricted to document's internal package boundary. The caller must
// validate the locally inspected record before issuing a proof.
func New(facts Facts) Proof { return Proof{facts: facts, local: true} }

// Facts returns a copy and whether this proof was issued locally.
func (proof Proof) Facts() (Facts, bool) { return proof.facts, proof.local }

// MarshalJSON prevents local authority from becoming portable metadata.
func (Proof) MarshalJSON() ([]byte, error) {
	return nil, errors.New("upload capability proof is local and cannot be serialized")
}

// UnmarshalJSON also revokes any proof previously held by the destination.
func (proof *Proof) UnmarshalJSON([]byte) error {
	*proof = Proof{}
	return errors.New("upload capability proof cannot be restored from JSON")
}
