package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/docbank/internal/query"
)

const (
	PhotoKindPhoto = "photo"
	PhotoKindVideo = "video"

	PhotoRoleRAW     = "raw"
	PhotoRoleImage   = "image"
	PhotoRoleVideo   = "video"
	PhotoRoleSidecar = "sidecar"

	PhotoDisplayAsset   = "asset"
	PhotoDisplayVault   = "vault"
	PhotoDisplayDefault = "default"
	PhotoDisplayNone    = "none"
	PhotoMaxFiles       = 256
)

// PhotoNodeFacts contains the identity and classification inputs used by the
// enrollment policy. It deliberately contains no copied bytes or hashes.
type PhotoNodeFacts struct {
	Node        Node
	MediaFamily string
	MediaType   string
	Qualifies   bool
	AssetKind   string
	EmailChild  bool
	AuditActive bool
}

// PhotoFile is one role-bearing reference to an ordinary Docbank file node.
type PhotoFile struct {
	ID          string  `json:"id"`
	AssetID     string  `json:"asset_id"`
	NodeID      int64   `json:"node_id"`
	Role        string  `json:"role"`
	SidecarOfID *string `json:"sidecar_of_file_id,omitzero"`
	CreatedAt   string  `json:"created_at"`
}

// PhotoAsset is the store-owned photo grouping state. Files are bounded to
// keep inspection and receipts safe for daemon callers.
type PhotoAsset struct {
	ID                    string      `json:"id"`
	Kind                  string      `json:"kind"`
	Revision              int64       `json:"revision"`
	ExcludedAt            *string     `json:"excluded_at,omitzero"`
	DisplayFileID         *string     `json:"display_file_id,omitzero"`
	DisplayOverrideFileID *string     `json:"display_override_file_id,omitzero"`
	DisplaySource         string      `json:"display_source"`
	CreatedAt             string      `json:"created_at"`
	UpdatedAt             string      `json:"updated_at"`
	Files                 []PhotoFile `json:"files"`
}

// PhotoSettings is the vault preference fence. A missing row is the virtual
// RAW preference at revision one; a stored nil preference is a reset value.
type PhotoSettings struct {
	Preference *string `json:"preference,omitzero"`
	Revision   int64   `json:"revision"`
	UpdatedAt  string  `json:"updated_at"`
}

// PhotoChangeReceipt records a bounded before/after decision without storing
// the bytes or duplicating content identity.
type PhotoChangeReceipt struct {
	ID             string `json:"id"`
	Operation      string `json:"operation"`
	AssetID        string `json:"asset_id,omitzero"`
	SettingsKey    string `json:"settings_key,omitzero"`
	BeforeRevision int64  `json:"before_revision"`
	AfterRevision  int64  `json:"after_revision"`
	BeforeJSON     string `json:"before"`
	AfterJSON      string `json:"after"`
	CreatedAt      string `json:"created_at"`
}

// PhotoDetachOptions controls the only destructive dependent-member action.
// A RAW with sidecars is refused unless ClearDependentSidecars is explicit.
type PhotoDetachOptions struct {
	ClearDependentSidecars bool
	ReplacementFileID      *string
}

type photoDisplayChoice struct {
	FileID *string
	Source string
}

func classifyPhotoMedia(mediaType, filename string) (family, normalized string, qualifies bool, kind string) {
	family = query.ClassifyMedia(mediaType, filename)
	normalized = query.NormalizeMediaType(mediaType)
	switch {
	case family == "image":
		return family, normalized, true, PhotoKindPhoto
	case family == "audio_video" && strings.HasPrefix(normalized, "video/"):
		return family, normalized, true, PhotoKindVideo
	default:
		return family, normalized, false, ""
	}
}

func photoNodeFacts(n Node) PhotoNodeFacts {
	family, mediaType, qualifies, kind := classifyPhotoMedia(n.MimeType, n.Name)
	return PhotoNodeFacts{Node: n, MediaFamily: family, MediaType: mediaType, Qualifies: qualifies, AssetKind: kind}
}

func photoRoleValid(role string) bool {
	switch role {
	case PhotoRoleRAW, PhotoRoleImage, PhotoRoleVideo, PhotoRoleSidecar:
		return true
	default:
		return false
	}
}

func photoKindValid(kind string) bool {
	return kind == PhotoKindPhoto || kind == PhotoKindVideo
}

func isPhotoCameraRawMedia(mediaType string) bool {
	switch mediaType {
	case "image/x-sony-arw", "image/x-fuji-raf", "image/x-adobe-dng",
		"image/x-canon-cr2", "image/x-nikon-nef":
		return true
	default:
		return false
	}
}

func validatePhotoRoleForNode(role string, facts PhotoNodeFacts) error {
	if !photoRoleValid(role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	if role == PhotoRoleSidecar {
		return nil
	}
	if role == PhotoRoleRAW {
		if facts.Qualifies && !isPhotoCameraRawMedia(facts.MediaType) {
			return fmt.Errorf("%w: raw role cannot label classified %s media", ErrInvalidPhotoAsset, facts.AssetKind)
		}
		return nil
	}
	if !facts.Qualifies {
		return fmt.Errorf("%w: node %d does not have a photo media type", ErrPhotoNodeNotEligible, facts.Node.ID)
	}
	if role == PhotoRoleImage && facts.AssetKind != PhotoKindPhoto {
		return fmt.Errorf("%w: image role needs image media", ErrInvalidPhotoAsset)
	}
	if role == PhotoRoleVideo && facts.AssetKind != PhotoKindVideo {
		return fmt.Errorf("%w: video role needs video media", ErrInvalidPhotoAsset)
	}
	return nil
}

func validatePhotoFileForAsset(kind, role string, facts PhotoNodeFacts) error {
	if err := validatePhotoRoleForAsset(kind, role); err != nil {
		return err
	}
	if err := validatePhotoRoleForNode(role, facts); err != nil {
		return err
	}
	if facts.Qualifies && role != PhotoRoleSidecar && kind != facts.AssetKind {
		return fmt.Errorf("%w: node media does not match asset kind", ErrInvalidPhotoAsset)
	}
	return nil
}

func validatePhotoRoleForAsset(kind, role string) error {
	if !photoRoleValid(role) {
		return fmt.Errorf("%w: unknown role %q", ErrInvalidPhotoAsset, role)
	}
	if role == PhotoRoleImage && kind != PhotoKindPhoto ||
		role == PhotoRoleVideo && kind != PhotoKindVideo ||
		(role == PhotoRoleRAW || role == PhotoRoleSidecar) && kind != PhotoKindPhoto {
		return fmt.Errorf("%w: role %s does not match asset kind %s", ErrInvalidPhotoAsset, role, kind)
	}
	return nil
}

func selectPhotoDisplay(files []PhotoFile, preference *string, override *string) photoDisplayChoice {
	if override != nil {
		for _, file := range files {
			if file.ID == *override && file.Role != PhotoRoleSidecar {
				id := file.ID
				return photoDisplayChoice{FileID: &id, Source: PhotoDisplayAsset}
			}
		}
	}
	roles := []string{PhotoRoleRAW, PhotoRoleImage, PhotoRoleVideo}
	source := PhotoDisplayDefault
	if preference != nil {
		source = PhotoDisplayVault
		if *preference == "image" {
			roles = []string{PhotoRoleImage, PhotoRoleRAW, PhotoRoleVideo}
		}
	}
	for _, role := range roles {
		var best *PhotoFile
		for index := range files {
			file := &files[index]
			if file.Role != role {
				continue
			}
			if best == nil || file.ID < best.ID {
				best = file
			}
		}
		if best != nil {
			id := best.ID
			return photoDisplayChoice{FileID: &id, Source: source}
		}
	}
	return photoDisplayChoice{Source: PhotoDisplayNone}
}

func validatePhotoGraph(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) error {
	rows, err := q.QueryContext(ctx, `
		SELECT a.asset_id, a.kind, a.revision, a.display_file_id,
		       a.display_override_file_id, f.file_id, f.asset_id, f.node_id,
		       f.role, f.sidecar_of_file_id, f.created_at, n.kind, n.name,
		       v.version_id, COALESCE(v.mime_type, '')
		FROM photo_assets a
		LEFT JOIN photo_files f ON f.asset_id=a.asset_id
		LEFT JOIN nodes n ON n.id=f.node_id
		LEFT JOIN content_versions v ON v.version_id=n.current_version_id
		ORDER BY a.asset_id, f.file_id`)
	if err != nil {
		return fmt.Errorf("reading photo graph: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type graphAsset struct {
		asset PhotoAsset
		files []PhotoFile
	}
	assets := map[string]*graphAsset{}
	for rows.Next() {
		var (
			assetID, kind                                    string
			revision                                         int64
			display, override                                sql.NullString
			fileID, fileAsset, role, created                 string
			nodeKind, nodeName, versionID, mediaType         sql.NullString
			nodeID                                           int64
			nodeIDNull                                       sql.NullInt64
			sidecar                                          sql.NullString
			fileIDNull, fileAssetNull, roleNull, createdNull sql.NullString
		)
		if err := rows.Scan(&assetID, &kind, &revision, &display, &override, &fileIDNull, &fileAssetNull, &nodeIDNull, &roleNull, &sidecar, &createdNull, &nodeKind, &nodeName, &versionID, &mediaType); err != nil {
			return fmt.Errorf("scanning photo graph: %w", err)
		}
		entry := assets[assetID]
		if entry == nil {
			entry = &graphAsset{asset: PhotoAsset{ID: assetID, Kind: kind, Revision: revision}}
			if display.Valid {
				entry.asset.DisplayFileID = new(display.String)
			}
			if override.Valid {
				entry.asset.DisplayOverrideFileID = new(override.String)
			}
			assets[assetID] = entry
		}
		if !fileIDNull.Valid {
			continue
		}
		fileID, fileAsset, role, created, nodeID = fileIDNull.String, fileAssetNull.String, roleNull.String, createdNull.String, nodeIDNull.Int64
		if validateUUIDv4(fileID) != nil || fileAsset != assetID || validateUUIDv4(fileAsset) != nil ||
			nodeID <= 0 || !photoRoleValid(role) {
			return fmt.Errorf("%w: malformed member %s", ErrInvalidPhotoAsset, fileID)
		}
		if !nodeKind.Valid {
			return fmt.Errorf("%w: member %s references a missing node", ErrInvalidPhotoAsset, fileID)
		}
		if nodeKind.String != nodeKindFile || !nodeName.Valid || !versionID.Valid || !mediaType.Valid {
			return fmt.Errorf("%w: member %s does not reference a file node", ErrInvalidPhotoAsset, fileID)
		}
		facts := photoNodeFacts(Node{ID: nodeID, Kind: nodeKind.String, Name: nodeName.String, MimeType: mediaType.String})
		if err := validatePhotoRoleForAsset(kind, role); err != nil {
			return fmt.Errorf("member %s: %w", fileID, err)
		}
		if err := validatePhotoFileForAsset(kind, role, facts); err != nil {
			compatible, initialVersionPruned, historyErr := photoNodeHistoryMatches(ctx, q, nodeID, nodeName.String, kind, role)
			if historyErr != nil {
				return historyErr
			}
			if !compatible && !initialVersionPruned {
				return fmt.Errorf("member %s: %w", fileID, err)
			}
		}
		file := PhotoFile{ID: fileID, AssetID: fileAsset, NodeID: nodeID, Role: role, CreatedAt: created}
		if sidecar.Valid {
			if validateUUIDv4(sidecar.String) != nil {
				return fmt.Errorf("%w: malformed sidecar pointer %s", ErrInvalidPhotoAsset, fileID)
			}
			file.SidecarOfID = new(sidecar.String)
		}
		entry.files = append(entry.files, file)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading photo graph: %w", err)
	}
	settings, err := photoSettingsTx(ctx, q)
	if err != nil {
		return err
	}
	for assetID, entry := range assets {
		if validateUUIDv4(assetID) != nil || !photoKindValid(entry.asset.Kind) || entry.asset.Revision < 1 || len(entry.files) > PhotoMaxFiles {
			return fmt.Errorf("%w: asset %s", ErrInvalidPhotoAsset, assetID)
		}
		choice := selectPhotoDisplay(entry.files, settings.Preference, entry.asset.DisplayOverrideFileID)
		if !equalPhotoString(entry.asset.DisplayFileID, choice.FileID) {
			return fmt.Errorf("%w: asset %s has a stale display pointer", ErrInvalidPhotoAsset, assetID)
		}
		if err := validatePhotoAssetPointers(entry.asset, entry.files); err != nil {
			return err
		}
	}
	return nil
}

func photoNodeHistoryMatches(ctx context.Context, q interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, nodeID int64, name, kind, role string) (bool, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(mime_type, ''), node_revision FROM content_versions WHERE node_id=?`, nodeID)
	if err != nil {
		return false, false, fmt.Errorf("reading photo member history %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	minimumRevision := int64(0)
	for rows.Next() {
		var mediaType string
		var revision int64
		if err := rows.Scan(&mediaType, &revision); err != nil {
			return false, false, fmt.Errorf("scanning photo member history %d: %w", nodeID, err)
		}
		if minimumRevision == 0 || revision < minimumRevision {
			minimumRevision = revision
		}
		if validatePhotoFileForAsset(kind, role, photoNodeFacts(Node{ID: nodeID, Kind: nodeKindFile, Name: name, MimeType: mediaType})) == nil {
			return true, false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("reading photo member history %d: %w", nodeID, err)
	}
	return false, minimumRevision > 1, nil
}

func validatePhotoGraphTx(ctx context.Context, tx *sql.Tx) error {
	return validatePhotoGraph(ctx, tx)
}

func validatePhotoAssetPointers(asset PhotoAsset, files []PhotoFile) error {
	byID := make(map[string]PhotoFile, len(files))
	for _, file := range files {
		if _, exists := byID[file.ID]; exists {
			return fmt.Errorf("%w: duplicate file %s", ErrInvalidPhotoAsset, file.ID)
		}
		byID[file.ID] = file
		if file.Role == PhotoRoleSidecar && file.SidecarOfID == nil {
			return fmt.Errorf("%w: sidecar %s must point to same-asset raw", ErrInvalidPhotoAsset, file.ID)
		}
		if file.SidecarOfID != nil {
			target, ok := byID[*file.SidecarOfID]
			if !ok {
				for _, candidate := range files {
					if candidate.ID == *file.SidecarOfID {
						target, ok = candidate, true
						break
					}
				}
			}
			if file.Role != PhotoRoleSidecar || !ok || target.AssetID != asset.ID || target.Role != PhotoRoleRAW {
				return fmt.Errorf("%w: sidecar %s must point to same-asset raw", ErrInvalidPhotoAsset, file.ID)
			}
		}
	}
	for _, pointer := range []*string{asset.DisplayFileID, asset.DisplayOverrideFileID} {
		if pointer == nil {
			continue
		}
		file, ok := byID[*pointer]
		if !ok || file.Role == PhotoRoleSidecar {
			return fmt.Errorf("%w: display pointer %s is not a member", ErrInvalidPhotoAsset, *pointer)
		}
	}
	return nil
}

func photoPreferenceValid(preference *string) bool {
	return preference == nil || *preference == "raw" || *preference == "image"
}
