package uploadproof

import (
	"errors"
	"mime"
	"strings"
)

const (
	sha256HexLength     = 64
	maxMediaFamilyBytes = 63
	maxMediaTypeBytes   = 255
	maxFormatBytes      = 63
)

// Facts is the filename-free, value-only capability snapshot retained after
// independent upload reinspection.
type Facts struct {
	SourceBytes              int64
	SourceSHA256             string
	CapabilityRecordChecksum string
	DescriptorFingerprint    string
	ProfileFingerprint       string
	DisclosureFingerprint    string
	InputKind                string
	MediaFamily              string
	MediaType                string
	Format                   string
	Pages                    int64
	Pixels                   int64
	Frames                   int64
	DurationMS               int64
	MaxSourceBytes           int64
	MaxPages                 int64
	MaxPixels                int64
	MaxFrames                int64
	MaxDurationMS            int64
}

// Proof is an opaque in-process authority issued only after upload
// reinspection. Its zero value is invalid.
type Proof struct {
	facts  Facts
	issued bool
}

// Issue validates and seals a detached facts value.
func Issue(facts Facts) (Proof, error) {
	if facts.SourceBytes <= 0 || facts.MaxSourceBytes <= 0 || facts.SourceBytes > facts.MaxSourceBytes {
		return Proof{}, errors.New("upload proof source byte bounds are invalid")
	}
	for _, value := range []string{
		facts.SourceSHA256, facts.CapabilityRecordChecksum, facts.DescriptorFingerprint,
		facts.ProfileFingerprint, facts.DisclosureFingerprint,
	} {
		if !validSHA256(value) {
			return Proof{}, errors.New("upload proof digest or fingerprint is invalid")
		}
	}
	if facts.InputKind != "original_file" && facts.InputKind != "derived_upload" {
		return Proof{}, errors.New("upload proof input kind is invalid")
	}
	if !validStableToken(facts.MediaFamily, maxMediaFamilyBytes) ||
		!validStableToken(facts.Format, maxFormatBytes) || !validMediaType(facts.MediaType) {
		return Proof{}, errors.New("upload proof media token is invalid")
	}
	measurements := []int64{facts.Pages, facts.Pixels, facts.Frames, facts.DurationMS}
	limits := []int64{facts.MaxPages, facts.MaxPixels, facts.MaxFrames, facts.MaxDurationMS}
	for index, measurement := range measurements {
		limit := limits[index]
		if measurement < 0 || limit < 0 || limit != 0 && measurement > limit {
			return Proof{}, errors.New("upload proof measurement bounds are invalid")
		}
	}
	return Proof{facts: facts, issued: true}, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256HexLength {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validStableToken(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validMediaType(value string) bool {
	if value == "" || len(value) > maxMediaTypeBytes {
		return false
	}
	parsed, parameters, err := mime.ParseMediaType(value)
	return err == nil && parsed == value && len(parameters) == 0 && strings.Contains(value, "/")
}

// Valid reports whether Issue created the proof in this process.
func (proof Proof) Valid() bool { return proof.issued }

// Snapshot returns a detached scalar copy of the retained facts.
func (proof Proof) Snapshot() Facts { return proof.facts }

func (proof Proof) MarshalJSON() ([]byte, error) {
	return nil, errors.New("upload proof is not serializable")
}

func (proof *Proof) UnmarshalJSON(_ []byte) error {
	*proof = Proof{}
	return errors.New("upload proof cannot be decoded")
}
