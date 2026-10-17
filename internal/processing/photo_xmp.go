package processing

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/ingest"
	"go.kenn.io/docbank/internal/store"
)

var ErrPhotoXMPSidecarRead = errors.New("reading photo XMP sidecar")

const photoXMPMalformedReason = "The sidecar contains malformed or unsupported XMP. Repair the file, put a new version, or trash the sidecar. A later catalog change writes a fresh sidecar."

func readPhotoXMPBytes(ctx context.Context, ing *ingest.Ingester, hash string) ([]byte, error) {
	reader, size, err := ing.Blobs.OpenStreamContext(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPhotoXMPSidecarRead, err)
	}
	defer func() { _ = reader.Close() }()
	if size > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxPhotoSidecarBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPhotoXMPSidecarRead, err)
	}
	if len(data) > maxPhotoSidecarBytes {
		return nil, bundle.ErrLimit
	}
	return data, nil
}

func photoXMPSnapshot(packet photoSidecarPacket) store.PhotoXMPPacket {
	p := store.PhotoXMPPacket{Values: packet.PhotoAuthored, Keywords: packet.Keywords, Ignored: packet.Ignored, Orientation: packet.Orientation}
	if packet.RotationPresent {
		p.Rotation = new(packet.Rotation)
	}
	return p
}

// SyncPhotoXMPBackground settles one Pending original for the sidecar worker,
// including hidden photos.
func SyncPhotoXMPBackground(ctx context.Context, ing *ingest.Ingester, fileID string) error {
	work, run, err := ing.Store.ClassifyPhotoXMP(ctx, fileID)
	if err != nil || !run {
		return err
	}
	return syncPhotoXMP(ctx, ing, work, "")
}

// ResolvePhotoXMP applies a person's choice to the recorded conflict.
func ResolvePhotoXMP(ctx context.Context, ing *ingest.Ingester, fileID, conflictID, choice string) error {
	if choice != "catalog" && choice != "sidecar" {
		return store.ErrInvalidPhotoAsset
	}
	work, err := ing.Store.PhotoXMPConflict(ctx, fileID, conflictID)
	if err != nil {
		return err
	}
	return syncPhotoXMP(ctx, ing, work, choice)
}

func syncPhotoXMP(ctx context.Context, ing *ingest.Ingester, work store.PhotoXMPWork, choice string) error {
	plan := store.PhotoXMPPlan{Work: work, Choice: choice}
	var data []byte
	var packet photoSidecarPacket
	if work.Sidecar != nil {
		var err error
		data, err = readPhotoXMPBytes(ctx, ing, work.Sidecar.Node.BlobHash)
		if err == nil {
			packet, err = readPhotoSidecar(ctx, data)
		}
		if err == nil {
			err = packet.Refusal
		}
		if errors.Is(err, ErrPhotoXMPSidecarRead) || ctx.Err() != nil {
			return err
		}
		if err != nil {
			plan.Refusal = photoXMPMalformedReason
			return ing.PublishPhotoXMP(ctx, plan, nil)
		}
		plan.Extension = packet.SidecarForExtension
	}
	base, baseOrientation, synced := work.Base()
	prior := store.PhotoXMPPacket{Values: base.Catalog, Keywords: base.Keywords}
	catalog := store.PhotoXMPPacket{Values: work.Original.Authored(), Keywords: work.Keywords, Orientation: store.PhotoXMPOrientation(work.Orientation, work.Original.Authored().Rotation)}
	sidecar := photoXMPSnapshot(packet)
	resolvePhotoXMPRotation(&sidecar, base.Packet, baseOrientation)
	plan.Sidecar = sidecar
	decision := planPhotoXMP(base.Packet, prior, catalog, sidecar, choice)
	if decision.conflict != 0 {
		plan.Rows = photoXMPConflictRows(catalog, effectivePhotoXMPSidecar(base.Packet, catalog, sidecar), decision.conflict)
		return ing.PublishPhotoXMP(ctx, plan, nil)
	}
	// A changed original orientation or an unreadable TIFF orientation rewrites
	// the composed rotation even when the decision itself is unchanged.
	rotation := decision.packet.Values.Confirmed&store.PhotoConfirmedRotation != 0
	if synced && work.Orientation != baseOrientation && (sidecar.Ignored&store.PhotoConfirmedRotation == 0 && sidecar.Orientation != 0 || rotation && !photoXMPEqual(decision.packet, prior, store.PhotoConfirmedRotation)) ||
		packet.RotationPresent && packet.Orientation == 0 && rotation {
		decision.write |= store.PhotoConfirmedRotation
	}
	if packet.LegacyReject && decision.write != 0 {
		decision.write |= store.PhotoConfirmedRating | store.PhotoConfirmedFlag
	}
	plan.Values, plan.Keywords = decision.packet.Values, store.PhotoXMPKeywords(decision.packet.Keywords)
	plan.Import, plan.Write = decision.imports != 0, decision.write != 0 || work.Sidecar == nil
	plan.Packet = sidecar
	if plan.Write {
		merged, written, err := mergePhotoXMPSidecar(ctx, data, store.PhotoExportInput{Authored: plan.Values, Keywords: plan.Keywords}, work.Orientation, decision.write)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			plan = store.PhotoXMPPlan{Work: work, Choice: choice, Refusal: "The preserved sidecar data exceeds the XMP limits."}
			return ing.PublishPhotoXMP(ctx, plan, nil)
		}
		data = merged
		plan.Packet = photoXMPSnapshot(written)
		resolvePhotoXMPRotation(&plan.Packet, store.PhotoXMPPacket{}, work.Orientation)
		if decision.write&store.PhotoConfirmedRotation == 0 {
			photoXMPCopy(&plan.Packet, sidecar, store.PhotoConfirmedRotation)
		}
		plan.Packet.Orientation = written.Orientation
	}
	if err := ing.PublishPhotoXMP(ctx, plan, data); err != nil {
		return fmt.Errorf("syncing XMP sidecar: %w", err)
	}
	return nil
}
