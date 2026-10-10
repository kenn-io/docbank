package bundle

import (
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/docbank/internal/canonical"
)

// PhotoRenderProfile selects pixels and metadata for a shared copy.
type PhotoRenderProfile struct {
	Format          string `json:"format"`
	Quality         int    `json:"quality"`
	LongEdge        int    `json:"long_edge"`
	IncludeMetadata bool   `json:"include_metadata"`
	RemoveGPS       bool   `json:"remove_gps"`
}

func (p PhotoRenderProfile) Validate() error {
	if p.Format != "jpeg" && p.Format != "png" || p.Quality < 1 || p.Quality > 100 || p.LongEdge < 0 || p.LongEdge > 100000 {
		return fmt.Errorf("%w: photo format must be jpeg or png, quality 1–100, long edge 0–100000", ErrConflict)
	}
	return nil
}

const PhotoRenderReceiptVersion = 1
const MaxPhotoExportMembers = 16

type PhotoRenderReceipt struct {
	Version         int                `json:"version"`
	Profile         PhotoRenderProfile `json:"profile"`
	Source          Member             `json:"source"`
	Width           int                `json:"width"`
	Height          int                `json:"height"`
	EmbeddedPreview bool               `json:"embedded_preview"`
}

func ValidatePhotoRoles(plan Plan, d Document) error {
	_, err := validatePhotoRoles(plan, d)
	return err
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
		if plan.PhotoRender == nil || role.Status != "available" && role.Status != "collapsed" || json.Unmarshal(role.Recipe, &receipt, json.RejectUnknownMembers(true)) != nil {
			return 0, ErrConflict
		}
		if receipt.Profile != *plan.PhotoRender || receipt.Profile.Validate() != nil || receipt.Source != d.Member || receipt.Width < 1 || receipt.Height < 1 || int64(receipt.Width)*int64(receipt.Height) > 100000000 || receipt.Version != PhotoRenderReceiptVersion || !canonical.IsSHA256Hex(role.SHA256) || role.Size < 1 || role.Page != nil {
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
	if plan.PhotoRender != nil && (plan.PhotoRender.Validate() != nil || len(plan.Roles) != 1 || plan.Roles[0] != (RolePolicy{Role: "photo_rendered"}) || count != 1) {
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
