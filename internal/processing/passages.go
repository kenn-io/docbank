package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const (
	DefaultPassageReadBytes = 32 << 10
	MaxPassageReadBytes     = 256 << 10
	// Processing profiles admit up to 1 GiB of rendition response data;
	// the retained envelope can add up to 256 KiB of frontmatter.
	maxPassageArtifactBytes = int64((1 << 30) + (256 << 10))
)

var (
	ErrPassageInvalid      = errors.New("passage request is invalid")
	ErrPassageUnauthorized = errors.New("passage source is unauthorized")
	ErrPassageUnavailable  = errors.New("passage is unavailable")
	ErrPassageCorrupt      = errors.New("passage authority is corrupt")
)

type PassageResolveRequest struct {
	Ref       document.PassageRefV1
	MaxBytes  int
	principal string
}

type PassageResolution struct {
	Availability  string
	Freshness     string
	PassageID     string
	Ref           document.PassageRefV1
	Text          string
	SectionPath   []string
	SourceLocator *document.EvidenceLocatorV1
	SourcePath    string
}

type passageAuthorityCatalog interface {
	VaultID() string
	ResolvePassageAuthority(
		ctx context.Context, ref document.PassageRefV1,
	) (store.PassageAuthority, error)
	RenditionInputBinding(ctx context.Context, buildID string) (string, error)
	MediaSourceBindingForContentVersion(ctx context.Context, principal, contentVersionID string) (string, string, error)
	MediaInputBindingVisible(ctx context.Context, principal, sourceID, sourceVersionID, inputID string) (bool, error)
}

// ResolvePassage returns bytes only from the exact retained rendition named
// by the reference. It never invokes a provider or substitutes a current head.
func (service *Service) ResolvePassage(
	ctx context.Context, request PassageResolveRequest,
) (PassageResolution, error) {
	if service == nil || service.catalog == nil || service.blobs == nil {
		return PassageResolution{}, ErrPassageUnavailable
	}
	request.principal = service.principal
	return resolvePassage(ctx, service.catalog, service.blobs, request)
}

func resolvePassage(
	ctx context.Context, catalog passageAuthorityCatalog, blobs verifiedBlobReader,
	request PassageResolveRequest,
) (PassageResolution, error) {
	if catalog == nil || blobs == nil {
		return PassageResolution{}, ErrPassageUnavailable
	}
	if request.MaxBytes == 0 {
		request.MaxBytes = DefaultPassageReadBytes
	}
	if request.MaxBytes < 1 || request.MaxBytes > MaxPassageReadBytes ||
		request.Ref.ByteStart < 0 || request.Ref.ByteEnd <= request.Ref.ByteStart ||
		request.Ref.ByteEnd-request.Ref.ByteStart > request.MaxBytes {
		return PassageResolution{}, ErrPassageInvalid
	}
	// The catalog admits an origin tuple only through exact retained local
	// authority or an explicit adopted alias, before any blob is opened.
	authority, err := catalog.ResolvePassageAuthority(ctx, request.Ref)
	if errors.Is(err, store.ErrDocumentIdentityUnavailable) ||
		errors.Is(err, store.ErrPassageAuthorityUnavailable) || errors.Is(err, store.ErrNotFound) {
		return PassageResolution{Availability: "unavailable", Ref: request.Ref}, ErrPassageUnavailable
	}
	if err != nil {
		return PassageResolution{}, err
	}
	// Historical authority does not override a supplied-media occurrence's
	// current visibility. Check it before opening retained bytes.
	if err := checkPassageInputVisibility(ctx, catalog, authority, request.principal); err != nil {
		return PassageResolution{}, err
	}
	if authority.Artifact.Size < 1 || authority.Artifact.Size > maxPassageArtifactBytes {
		return PassageResolution{}, ErrPassageUnavailable
	}
	stream, size, err := blobs.OpenStreamContext(ctx, authority.Artifact.BlobHash)
	if err != nil {
		return PassageResolution{Availability: "unavailable", Ref: request.Ref}, ErrPassageUnavailable
	}
	defer func() { _ = stream.Close() }()
	if size != authority.Artifact.Size {
		return PassageResolution{}, ErrPassageCorrupt
	}
	artifactHash := sha256.New()
	frontmatter, quote, err := document.ReadRenditionPassageV1(
		io.TeeReader(stream, artifactHash), size, request.Ref)
	if err != nil || frontmatter.Source.SHA256 != request.Ref.SourceSHA256 ||
		frontmatter.Rendition.BuildID != authority.Build.ID ||
		frontmatter.Rendition.BodySHA256 != request.Ref.BodySHA256 {
		return PassageResolution{}, ErrPassageCorrupt
	}
	if err := stream.Verify(); err != nil || !stream.Verified() ||
		hex.EncodeToString(artifactHash.Sum(nil)) != authority.Artifact.BlobHash {
		return PassageResolution{}, ErrPassageCorrupt
	}
	passageID, err := document.PassageIdentityV1(request.Ref)
	if err != nil {
		return PassageResolution{}, fmt.Errorf("%w: %w", ErrPassageCorrupt, err)
	}
	section, locator := passageSourceContext(
		frontmatter.Navigation.Entries, authority.Build.Units,
		request.Ref.ByteStart, request.Ref.ByteEnd,
	)
	freshness := "historical"
	if authority.Fresh {
		freshness = "current"
	}
	return PassageResolution{Availability: "available", Freshness: freshness,
		PassageID: passageID, Ref: request.Ref, Text: string(quote),
		SectionPath: section, SourceLocator: locator, SourcePath: authority.Path}, nil
}

func checkPassageInputVisibility(
	ctx context.Context, catalog passageAuthorityCatalog, authority store.PassageAuthority, principal string,
) error {
	inputBinding, err := catalog.RenditionInputBinding(ctx, authority.Build.ID)
	if err != nil {
		return ErrPassageUnavailable
	}
	if inputBinding != "" {
		sourceID, sourceVersionID, bindingErr := catalog.MediaSourceBindingForContentVersion(
			ctx, principal, authority.Version.ID)
		if bindingErr != nil {
			return ErrPassageUnavailable
		}
		visible, bindingErr := catalog.MediaInputBindingVisible(
			ctx, principal, sourceID, sourceVersionID, inputBinding)
		if bindingErr != nil || !visible {
			return ErrPassageUnavailable
		}
	}
	return nil
}

func passageSourceContext(
	navigation []document.RenditionNavigationEntryV1,
	units []store.RenditionUnitRecord,
	start, end int,
) ([]string, *document.EvidenceLocatorV1) {
	key := ""
	next := -1
	for _, entry := range navigation {
		if entry.Byte > start {
			next = entry.Byte
			break
		}
		key = entry.Key
	}
	for _, unit := range units {
		if unit.EvidenceUnitID != key {
			continue
		}
		section := append([]string(nil), unit.HeadingPath...)
		if next >= 0 && end > next {
			return section, nil
		}
		locator := unit.Locator
		return section, &locator
	}
	return []string{}, nil
}
