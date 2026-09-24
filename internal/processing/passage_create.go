package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

// PassageCreateRequest names local retained authority, never a path or current
// rendition head. Its half-open coordinates address the original Markdown body.
type PassageCreateRequest struct {
	NodeID           int64
	ContentVersionID string
	RenditionBuildID string
	AttachmentID     string
	ByteStart        int
	ByteEnd          int
}

type PassageCreation struct {
	Ref       document.PassageRefV1
	PassageID string
	Text      string
}

type passageCreationCatalog interface {
	VaultID() string
	PassageCreationAuthority(ctx context.Context, nodeID int64, versionID, buildID, attachmentID string) (store.PassageAuthority, error)
	EnsurePassageDocumentIdentity(ctx context.Context, claim store.PassageIdentityClaim) (store.DocumentIdentity, error)
	RenditionInputBinding(ctx context.Context, buildID string) (string, error)
	MediaSourceBindingForContentVersion(ctx context.Context, principal, contentVersionID string) (string, string, error)
	MediaInputBindingVisible(ctx context.Context, principal, sourceID, sourceVersionID, inputID string) (bool, error)
}

// CreatePassage verifies an exact retained artifact before allocating a stable
// standalone document UID. It never enters the rendition provider path.
func (service *Service) CreatePassage(ctx context.Context, request PassageCreateRequest) (PassageCreation, error) {
	if service == nil || service.catalog == nil || service.blobs == nil {
		return PassageCreation{}, ErrPassageUnavailable
	}
	return createPassage(ctx, service.catalog, service.blobs, service.principal, request)
}

func createPassage(ctx context.Context, catalog passageCreationCatalog, blobs verifiedBlobReader,
	principal string, request PassageCreateRequest,
) (PassageCreation, error) {
	if catalog == nil || blobs == nil {
		return PassageCreation{}, ErrPassageUnavailable
	}
	if request.NodeID < 1 || request.ByteStart < 0 || request.ByteEnd <= request.ByteStart ||
		request.ByteEnd-request.ByteStart > MaxPassageReadBytes {
		return PassageCreation{}, ErrPassageInvalid
	}
	authority, err := catalog.PassageCreationAuthority(ctx, request.NodeID,
		request.ContentVersionID, request.RenditionBuildID, request.AttachmentID)
	if err != nil {
		return PassageCreation{}, passageCreationAuthorityError(err)
	}
	if _, err := passageCreationVisible(ctx, catalog, principal, request, authority); err != nil {
		return PassageCreation{}, err
	}
	// NewPassageRefV1 seals the full original body. Bound this allocation to
	// the service's ordinary 64 MiB rendition limit plus its machine envelope.
	const maxCreateArtifactBytes = MaxRenditionBytes + 256<<10
	if authority.Artifact.Size < 1 || authority.Artifact.Size > maxCreateArtifactBytes {
		return PassageCreation{}, ErrPassageUnavailable
	}
	stream, size, err := blobs.OpenStreamContext(ctx, authority.Artifact.BlobHash)
	if err != nil {
		return PassageCreation{}, ErrPassageUnavailable
	}
	defer func() { _ = stream.Close() }()
	if size != authority.Artifact.Size {
		return PassageCreation{}, ErrPassageCorrupt
	}
	artifact, err := io.ReadAll(io.LimitReader(stream, size+1))
	if err != nil || int64(len(artifact)) != size {
		return PassageCreation{}, ErrPassageCorrupt
	}
	if err := stream.Verify(); err != nil || !stream.Verified() {
		return PassageCreation{}, ErrPassageCorrupt
	}
	artifactDigest := sha256.Sum256(artifact)
	if hex.EncodeToString(artifactDigest[:]) != authority.Artifact.BlobHash {
		return PassageCreation{}, ErrPassageCorrupt
	}
	frontmatter, body, err := document.ParseRenditionFrontMatterV1(artifact)
	if err != nil || frontmatter.Source.SHA256 != authority.Version.BlobHash ||
		frontmatter.Rendition.BuildID != authority.Build.ID {
		return PassageCreation{}, ErrPassageCorrupt
	}
	quote, err := document.SlicePassage(body, request.ByteStart, request.ByteEnd)
	if err != nil {
		return PassageCreation{}, ErrPassageInvalid
	}
	// Recheck catalog and input visibility after reading retained bytes. A
	// pruned, replaced or revoked tuple must not allocate an identity.
	latest, err := catalog.PassageCreationAuthority(ctx, request.NodeID,
		request.ContentVersionID, request.RenditionBuildID, request.AttachmentID)
	if err != nil || latest.Artifact.BlobHash != authority.Artifact.BlobHash ||
		latest.Version.BlobHash != authority.Version.BlobHash {
		return PassageCreation{}, ErrPassageUnavailable
	}
	inputID, err := passageCreationVisible(ctx, catalog, principal, request, latest)
	if err != nil {
		return PassageCreation{}, err
	}
	identity, err := catalog.EnsurePassageDocumentIdentity(ctx, store.PassageIdentityClaim{
		NodeID: request.NodeID, ContentVersionID: request.ContentVersionID,
		RenditionBuildID: request.RenditionBuildID, AttachmentID: request.AttachmentID,
		ArtifactHash: authority.Artifact.BlobHash, InputID: inputID, Principal: principal,
	})
	if err != nil {
		return PassageCreation{}, passageCreationAuthorityError(err)
	}
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID: catalog.VaultID(), DocumentUID: identity.DocumentUID,
		ContentVersionID: request.ContentVersionID, SourceSHA256: authority.Version.BlobHash,
		RenditionBuildID: request.RenditionBuildID, AttachmentID: request.AttachmentID,
	}, body, request.ByteStart, request.ByteEnd)
	if err != nil {
		return PassageCreation{}, ErrPassageInvalid
	}
	passageID, err := document.PassageIdentityV1(ref)
	if err != nil {
		return PassageCreation{}, ErrPassageCorrupt
	}
	return PassageCreation{Ref: ref, PassageID: passageID, Text: string(quote)}, nil
}

func passageCreationVisible(ctx context.Context, catalog passageCreationCatalog,
	principal string, request PassageCreateRequest, authority store.PassageAuthority,
) (string, error) {
	if authority.Node.ID != request.NodeID || authority.Version.ID != request.ContentVersionID ||
		authority.Version.NodeID != request.NodeID || authority.Build.ID != request.RenditionBuildID ||
		authority.Attachment.ID != request.AttachmentID ||
		authority.Version.BlobHash != authority.Build.SourceSHA256 {
		return "", ErrPassageUnavailable
	}
	inputID, err := catalog.RenditionInputBinding(ctx, request.RenditionBuildID)
	if err != nil {
		return "", ErrPassageUnavailable
	}
	if inputID == "" {
		return "", nil
	}
	sourceID, versionID, err := catalog.MediaSourceBindingForContentVersion(ctx, principal, request.ContentVersionID)
	if err != nil {
		return "", ErrPassageUnavailable
	}
	visible, err := catalog.MediaInputBindingVisible(ctx, principal, sourceID, versionID, inputID)
	if err != nil || !visible {
		return "", ErrPassageUnavailable
	}
	return inputID, nil
}

func passageCreationAuthorityError(err error) error {
	if errors.Is(err, store.ErrPassageAuthorityUnavailable) ||
		errors.Is(err, store.ErrDocumentIdentityUnavailable) || errors.Is(err, store.ErrNotFound) {
		return ErrPassageUnavailable
	}
	return err
}
