package production

import (
	"errors"

	documentproduction "go.kenn.io/docbank/document/production"
)

var ErrArtifactProvenance = errors.New("production artifact provenance conflicts with published authority")

// RetainSource is the source version pinned by one finalized occurrence.
// Repeated occurrences may legitimately name the same source version.
type RetainSource struct {
	MemberID        string
	MemberOrdinal   int64
	SourceVersionID string
}

// BuildArtifactProvenance binds every published member artifact to its exact
// finalized occurrence and source version. Artifact names are never parsed for
// identity. The published receipt's time and ID make retries byte-identical.
func BuildArtifactProvenance(job Job, sources []RetainSource) (documentproduction.ArtifactProvenanceReceipt, error) {
	bad := func() (documentproduction.ArtifactProvenanceReceipt, error) {
		return documentproduction.ArtifactProvenanceReceipt{}, ErrArtifactProvenance
	}
	if job.State != ProductionJobSucceeded || job.ID == "" || job.Receipt.ID != job.ID ||
		job.Receipt.JobID != job.ID || job.Receipt.SetID != job.SetID ||
		job.Receipt.Revision != job.Revision || job.Receipt.RevisionSHA256 != job.RevisionSHA256 ||
		job.Receipt.PreparedInputSHA256 != job.PreparedInputSHA256 ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 ||
		documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		len(sources) == 0 || len(sources) > documentproduction.MaxArtifacts {
		return bad()
	}
	byMember := make(map[string]RetainSource, len(sources))
	for _, source := range sources {
		if source.MemberID == "" || source.MemberOrdinal < 1 || source.SourceVersionID == "" ||
			byMember[source.MemberID].MemberID != "" {
			return bad()
		}
		byMember[source.MemberID] = source
	}
	receipt := documentproduction.ArtifactProvenanceReceipt{
		Contract: documentproduction.ArtifactProvenanceContractV1,
		ID:       job.ID, ProductionReceiptSHA256: job.Receipt.SHA256,
		ArtifactManifestSHA256: job.Manifest.SHA256, CreatedAt: job.Receipt.CreatedAt,
		Entries: make([]documentproduction.ArtifactProvenance, 0, len(job.Manifest.Artifacts)),
	}
	seen := make(map[string]bool, len(sources))
	for _, artifact := range job.Manifest.Artifacts {
		source, ok := byMember[artifact.MemberID]
		if !ok || source.MemberOrdinal != artifact.MemberOrdinal || artifact.Volume == "" {
			return bad()
		}
		seen[source.MemberID] = true
		receipt.Entries = append(receipt.Entries, documentproduction.ArtifactProvenance{
			ArtifactID: artifact.ID, ArtifactSHA256: artifact.SHA256,
			SourceVersionID: source.SourceVersionID, MemberID: source.MemberID,
			MemberOrdinal: source.MemberOrdinal, Page: artifact.Page, Volume: artifact.Volume,
		})
	}
	if len(seen) != len(sources) {
		return bad()
	}
	_, digest, err := documentproduction.CanonicalArtifactProvenanceReceipt(receipt)
	if err != nil {
		return bad()
	}
	receipt.SHA256 = digest
	if documentproduction.ValidateArtifactProvenanceReceipt(receipt) != nil {
		return bad()
	}
	return receipt, nil
}
