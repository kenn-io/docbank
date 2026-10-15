package bundle

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/query"
)

// PhotoRenderProfile selects pixels and metadata for a shared copy.
type PhotoRenderProfile struct {
	Format          string `json:"format"`
	Quality         int    `json:"quality,omitzero"`
	LongEdge        int    `json:"long_edge"`
	IncludeMetadata bool   `json:"include_metadata"`
	RemoveGPS       bool   `json:"remove_gps"`
}

func (p *PhotoRenderProfile) UnmarshalJSON(raw []byte) error {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for name, value := range fields {
		if value.Kind() == 'n' {
			return fmt.Errorf("photo_render.%s must not be null", name)
		}
	}
	for _, name := range []string{"format", "remove_gps", "include_metadata", "long_edge"} {
		if _, present := fields[name]; !present {
			return fmt.Errorf("photo_render.%s is required", name)
		}
	}
	type profile PhotoRenderProfile
	return json.Unmarshal(raw, (*profile)(p), json.RejectUnknownMembers(true))
}

func (p PhotoRenderProfile) Validate() error {
	if p.Format != "jpeg" && p.Format != "png" || p.Quality < 0 || p.Quality > 100 || p.Format == "jpeg" && p.Quality == 0 || p.LongEdge < 0 || p.LongEdge > 100000 {
		return fmt.Errorf("%w: photo format must be jpeg or png, JPEG quality 1–100 (optional for PNG), long edge 0–100000", ErrConflict)
	}
	return nil
}

// Canonical removes settings that cannot affect a validated render profile.
func (p PhotoRenderProfile) Canonical() PhotoRenderProfile {
	if p.Format == "png" {
		p.Quality = 0
	}
	if !p.IncludeMetadata {
		p.RemoveGPS = false
	}
	return p
}

const PhotoRenderReceiptVersion = 1
const MaxPhotoExportMembers = 16

func (s PhotoExportSelection) Validate() error {
	if len(s.AssetIDs) > MaxPhotoExportMembers {
		return fmt.Errorf("%w: photo exports allow at most %d photos", ErrLimit, MaxPhotoExportMembers)
	}
	seen := make(map[string]bool, len(s.AssetIDs))
	for _, id := range s.AssetIDs {
		if seen[id] {
			return fmt.Errorf("%w: duplicate photo asset ID %s", ErrConflict, id)
		}
		seen[id] = true
		if canonical.ValidateUUIDv4(id) != nil {
			return fmt.Errorf("%w: photo asset ID must be a canonical UUIDv4", ErrConflict)
		}
	}
	if _, err := query.Canonical(s.Query); err != nil {
		return fmt.Errorf("%w: invalid photo query: %w", ErrConflict, err)
	}
	return nil
}

type PhotoRenderReceipt struct {
	Version         int                `json:"version"`
	Profile         PhotoRenderProfile `json:"profile"`
	Source          Member             `json:"source"`
	Width           int                `json:"width"`
	Height          int                `json:"height"`
	EmbeddedPreview bool               `json:"embedded_preview"`
}

func validatePhotoRoles(plan Plan, d Document) (int, error) {
	embedded := 0
	count := 0
	for _, role := range d.Roles {
		if role.Role != "photo_rendered" {
			continue
		}
		count++
		var receipt PhotoRenderReceipt
		if plan.PhotoRender == nil || role.Status != roleAvailable || json.Unmarshal(role.Recipe, &receipt, json.RejectUnknownMembers(true)) != nil {
			return 0, ErrConflict
		}
		if receipt.Profile != *plan.PhotoRender || receipt.Source != d.Member || receipt.Width < 1 || receipt.Height < 1 || int64(receipt.Width) > document.VisualPreviewMaxSourcePixels/int64(receipt.Height) || receipt.Version != PhotoRenderReceiptVersion || !canonical.IsSHA256Hex(role.SHA256) || role.Size < 1 || role.Page != nil {
			return 0, ErrConflict
		}
		if receipt.Profile.LongEdge > 0 && max(receipt.Width, receipt.Height) > receipt.Profile.LongEdge {
			return 0, ErrConflict
		}

		if role.Path != PhotoRenderedPath(d.Member, receipt.Profile) || role.MediaType != "image/"+receipt.Profile.Format {
			return 0, ErrConflict
		}

		if receipt.EmbeddedPreview {
			embedded++
		}
	}
	if plan.PhotoRender != nil && (count != 1) {
		return 0, ErrConflict
	}
	return embedded, nil
}

func PhotoRenderedPath(member Member, profile PhotoRenderProfile) string {
	ext := profile.Format
	if ext == "jpeg" {
		ext = "jpg"
	}
	return fmt.Sprintf("documents/%d/%s/photo.%s", member.NodeID, member.VersionID, ext)
}
