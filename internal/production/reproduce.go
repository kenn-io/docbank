package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"slices"

	documentproduction "go.kenn.io/docbank/document/production"
)

var ErrReproductionConflict = errors.New("reproduction conflicts with original production authority")

// ReproductionSelection binds a request to historical authority. It is a
// preflight result, not a delivery or reproduction receipt.
type ReproductionSelection struct {
	OriginalReceiptSHA256     string
	OriginalReservationSHA256 string
	ArtifactManifestSHA256    string
	DeliveryPolicySHA256      string
	ArtifactIDs               []string
	SourceVersionIDs          []string
}

// PrepareReproduction accepts only the complete set of retained output
// artifacts from one successful job. Packaging and receipt persistence consume
// this immutable selection in a later step; this function never allocates
// numbers or rerenders a changed source.
func PrepareReproduction(ctx context.Context, catalog PublishedPackageCatalog,
	opener PackageArtifactOpener, jobID string, request documentproduction.ReproductionRequest,
	policy PackageDeliveryPolicy) (ReproductionSelection, error) {
	bad := func() (ReproductionSelection, error) { return ReproductionSelection{}, ErrReproductionConflict }
	if ctx == nil || catalog == nil || opener == nil || jobID == "" {
		return bad()
	}
	if err := ctx.Err(); err != nil {
		return ReproductionSelection{}, err
	}
	if _, _, err := documentproduction.CanonicalReproductionRequest(request); err != nil {
		return bad()
	}
	policySHA, err := deliveryPolicyDigest(policy)
	if err != nil || policySHA != request.DeliveryPolicySHA256 {
		return bad()
	}
	inputs, err := catalog.LoadProductionPackageInputs(ctx, jobID)
	if err != nil {
		return ReproductionSelection{}, err
	}
	job, reservation := inputs.Job, inputs.Reservation
	if job.State != ProductionJobSucceeded || job.ID != jobID || job.Receipt.ID != jobID ||
		job.Receipt.JobID != jobID || job.SetID != job.Receipt.SetID || job.Revision != job.Receipt.Revision ||
		job.RevisionSHA256 != job.Receipt.RevisionSHA256 || job.PreparedInputSHA256 != job.Receipt.PreparedInputSHA256 ||
		job.Receipt.SHA256 != request.OriginalProductionReceiptSHA256 ||
		job.Receipt.ArtifactManifestSHA256 != job.Manifest.SHA256 ||
		job.Receipt.NumberReservationSHA256 != reservation.SHA256 ||
		reservation.OperationID != jobID || reservation.RevisionSHA256 != job.RevisionSHA256 ||
		documentproduction.ValidateProductionReceipt(job.Receipt) != nil ||
		documentproduction.ValidateArtifactManifest(job.Manifest) != nil ||
		documentproduction.ValidateNumberReservation(reservation) != nil ||
		len(inputs.Members) == 0 || len(job.Manifest.Artifacts) != len(request.ArtifactIDs) {
		return bad()
	}
	members := make(map[string]PackageMember, len(inputs.Members))
	sourceVersions := make([]string, len(inputs.Members))
	for index, member := range inputs.Members {
		if member.ID == "" || member.Ordinal != int64(index+1) || member.SourceVersionID == "" || member.FamilyID == "" {
			return bad()
		}
		if _, exists := members[member.ID]; exists {
			return bad()
		}
		members[member.ID] = member
		sourceVersions[index] = member.SourceVersionID
	}
	selected := make(map[string]bool, len(request.ArtifactIDs))
	for _, id := range request.ArtifactIDs {
		if selected[id] {
			return bad()
		}
		selected[id] = true
	}
	for _, artifact := range job.Manifest.Artifacts {
		member, exists := members[artifact.MemberID]
		if !exists || member.Ordinal != artifact.MemberOrdinal || !selected[artifact.ID] ||
			artifact.Size < 1 || artifact.Size == math.MaxInt64 {
			return bad()
		}
		if err := verifyReproductionArtifact(ctx, opener, jobID, artifact); err != nil {
			return bad()
		}
	}
	return ReproductionSelection{
		OriginalReceiptSHA256: job.Receipt.SHA256, OriginalReservationSHA256: reservation.SHA256,
		ArtifactManifestSHA256: job.Manifest.SHA256, DeliveryPolicySHA256: policySHA,
		ArtifactIDs: slices.Clone(request.ArtifactIDs), SourceVersionIDs: sourceVersions,
	}, nil
}

func verifyReproductionArtifact(ctx context.Context, opener PackageArtifactOpener, jobID string,
	artifact documentproduction.Artifact) error {
	stream, size, err := opener.OpenVerifiedProductionArtifact(ctx, jobID, artifact)
	if err != nil || stream == nil || size != artifact.Size {
		if stream != nil {
			_ = stream.Close()
		}
		return ErrReproductionConflict
	}
	digest := sha256.New()
	n, readErr := io.Copy(digest, io.LimitReader(stream, artifact.Size+1))
	verifyErr := stream.Verify()
	verified := stream.Verified()
	closeErr := stream.Close()
	if readErr != nil || verifyErr != nil || closeErr != nil || !verified || n != artifact.Size ||
		hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
		return ErrReproductionConflict
	}
	return nil
}

// ReproductionRuntime stages a fresh recipient archive and its own actual
// archive QC for a previously published job. The caller retains the verified
// package and its separate reproduction authority through Retain.
type ReproductionRuntime struct {
	Catalog    PublishedPackageCatalog
	Opener     PackageArtifactOpener
	StagingDir string
	Retain     func(context.Context, documentproduction.ReproductionRequest,
		ReproductionSelection, RecipientPackageRequest, PublishedRecipientPackage) error
}

type PublishedReproductionPackage struct {
	Selection ReproductionSelection
	Package   PublishedRecipientPackage
}

func (r ReproductionRuntime) Run(ctx context.Context, jobID string,
	request documentproduction.ReproductionRequest, policy PackageDeliveryPolicy,
	profileID string, limits PackageLimits) (PublishedReproductionPackage, error) {
	if r.Retain == nil {
		return PublishedReproductionPackage{}, ErrReproductionConflict
	}
	selection, err := PrepareReproduction(ctx, r.Catalog, r.Opener, jobID, request, policy)
	if err != nil {
		return PublishedReproductionPackage{}, err
	}
	packageRuntime := PackageRuntime{
		Catalog: r.Catalog, Opener: r.Opener, StagingDir: r.StagingDir,
		Retain: func(ctx context.Context, paths RecipientPackageRequest,
			published PublishedRecipientPackage) error {
			return r.Retain(ctx, request, selection, paths, published)
		},
	}
	published, err := packageRuntime.Run(ctx, jobID, profileID, limits)
	if err != nil {
		return PublishedReproductionPackage{}, err
	}
	return PublishedReproductionPackage{Selection: selection, Package: published}, nil
}
