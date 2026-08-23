package document

import "errors"

const normalizationPolicyVersion = 3

// NormalizePolicy fixes the version-3 normalization behavior. Its structural
// values are private because changing any of them requires a new version.
type NormalizePolicy struct {
	version                int
	maxDocumentChars       int
	maxUnitChars           int
	maxSourceUnitBytes     int
	maxMetadataSourceBytes int
	maxLinkChars           int
	maxChunkRunes          int
	chunkOverlap           int
	maxChunks              int
}

// NormalizePolicyIdentity is a read-only copy of every effective policy value.
type NormalizePolicyIdentity struct {
	Version                int `json:"version"`
	MaxDocumentChars       int `json:"max_document_chars"`
	MaxUnitChars           int `json:"max_unit_chars"`
	MaxSourceUnitBytes     int `json:"max_source_unit_bytes"`
	MaxMetadataSourceBytes int `json:"max_metadata_source_bytes"`
	MaxLinkChars           int `json:"max_link_chars"`
	MaxChunkRunes          int `json:"max_chunk_runes"`
	ChunkOverlap           int `json:"chunk_overlap"`
	MaxChunks              int `json:"max_chunks"`
}

// NewNormalizePolicy returns the fixed version-3 policy for a document limit.
func NewNormalizePolicy(maxDocumentChars int) (NormalizePolicy, error) {
	policy := NormalizePolicy{
		version:                normalizationPolicyVersion,
		maxDocumentChars:       maxDocumentChars,
		maxUnitChars:           1_000_000,
		maxSourceUnitBytes:     4_000_000,
		maxMetadataSourceBytes: 65_536,
		maxLinkChars:           2_048,
		maxChunkRunes:          4_000,
		chunkOverlap:           200,
		maxChunks:              20_000,
	}
	if err := policy.validate(); err != nil {
		return NormalizePolicy{}, err
	}
	return policy, nil
}

// Identity returns a copy of every value that defines normalization output.
func (p NormalizePolicy) Identity() NormalizePolicyIdentity {
	return NormalizePolicyIdentity{
		Version:                p.version,
		MaxDocumentChars:       p.maxDocumentChars,
		MaxUnitChars:           p.maxUnitChars,
		MaxSourceUnitBytes:     p.maxSourceUnitBytes,
		MaxMetadataSourceBytes: p.maxMetadataSourceBytes,
		MaxLinkChars:           p.maxLinkChars,
		MaxChunkRunes:          p.maxChunkRunes,
		ChunkOverlap:           p.chunkOverlap,
		MaxChunks:              p.maxChunks,
	}
}

func (p NormalizePolicy) validate() error {
	if p.version != normalizationPolicyVersion {
		return errors.New("document normalization policy version is invalid; use NewNormalizePolicy")
	}
	if p.maxUnitChars <= 0 || p.maxDocumentChars <= 0 || p.maxSourceUnitBytes <= 0 ||
		p.maxMetadataSourceBytes <= 0 || p.maxLinkChars <= 0 || p.maxChunkRunes <= 0 || p.maxChunks <= 0 {
		return errors.New("document normalization limits must be positive")
	}
	if p.chunkOverlap < 0 || p.chunkOverlap >= p.maxChunkRunes {
		return errors.New("document normalization limits are inconsistent")
	}
	return nil
}
