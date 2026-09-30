package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// PhotoImportMember is one already-published source file in an import group.
// The importer supplies the source observation and physical receipt; the
// store decides membership and pairing in one transaction.
type PhotoImportMember struct {
	Name          string
	Role          string
	BlobHash      string
	Size          int64
	MediaType     string
	OriginalPath  string
	OriginalMtime string
	Physical      BlobPhysical
}

// PhotoImportGroup is one same-folder, same-stem unit. DestinationID is a
// virtual folder node and zero means the vault root.
type PhotoImportGroup struct {
	SourceFolder  string
	Stem          string
	DestinationID int64
	Members       []PhotoImportMember
}

// Ambiguity reasons tell the operator why a group was left for manual pairing.
const (
	PhotoImportMultipleRAW    = "multiple_raw"
	PhotoImportSeparatePhotos = "separate_photos"
)

// PhotoImportAmbiguity lists the same-name files an import would not pair
// on its own. The operator pairs them with a revision-checked attach.
type PhotoImportAmbiguity struct {
	Reason string                     `json:"reason"`
	Files  []PhotoImportAmbiguousFile `json:"files"`
}

type PhotoImportAmbiguousFile struct {
	SourcePath string `json:"source_path"`
	NodeID     int64  `json:"node_id"`
	AssetID    string `json:"asset_id,omitzero"`
	Role       string `json:"role"`
}

type PhotoImportResult struct {
	Asset     PhotoAsset
	Nodes     []Node
	Added     bool
	Skipped   bool
	Ambiguity *PhotoImportAmbiguity
}

// PhotoImportRequest is the request JSON of a photo_import operation.
type PhotoImportRequest struct {
	SourceRoot  string `json:"source_root"`
	Destination string `json:"destination"`
}

// PhotoImportReceipt is the progress and final receipt JSON of a
// photo_import operation. Ambiguities keeps at most PhotoImportMaxAmbiguities.
type PhotoImportReceipt struct {
	Added       int64                  `json:"added"`
	Skipped     int64                  `json:"skipped"`
	Changed     int64                  `json:"changed"`
	Failed      int64                  `json:"failed"`
	Ambiguous   int64                  `json:"ambiguous"`
	Ambiguities []PhotoImportAmbiguity `json:"ambiguities,omitzero"`
}

const PhotoImportMaxAmbiguities = 100

type photoImportCandidateRow struct {
	NodeID     int64
	AssetID    string
	Role       string
	SourcePath string
}

func photoImportSourceKey(path string) (folder, stem string) {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	stem = strings.TrimSuffix(base, filepath.Ext(base))
	for {
		source := ClassifyPhotoSource(stem)
		if source.Kind != PhotoSourceRAW && source.Kind != PhotoSourceImage && source.Kind != PhotoSourceSidecar {
			break
		}
		next := strings.TrimSuffix(stem, filepath.Ext(stem))
		if next == stem {
			break
		}
		stem = next
	}
	return norm.NFC.String(filepath.Clean(filepath.Dir(clean))), norm.NFC.String(stem)
}

func photoImportSourcePath(path string) string {
	return norm.NFC.String(filepath.Clean(path))
}

// PhotoImportSourceKey returns the normalized source folder and stem used by
// grouped imports.
func PhotoImportSourceKey(path string) (folder, stem string) {
	return photoImportSourceKey(path)
}

// PhotoImportGroupKey returns the grouping identity for one discovered source.
// A video keys on its full path so it never joins a same-name photo.
func PhotoImportGroupKey(path string, kind PhotoSourceKind) string {
	if kind == PhotoSourceVideo {
		return base64.RawURLEncoding.EncodeToString([]byte("video\x00" + filepath.Clean(path)))
	}
	folder, stem := photoImportSourceKey(path)
	return base64.RawURLEncoding.EncodeToString([]byte("photo\x00" + folder + "\x00" + stem))
}

func photoImportMemberRole(member PhotoImportMember) (string, string, string, error) {
	source := ClassifyPhotoSource(member.Name)
	role := member.Role
	if role == "" {
		role = source.Role
	}
	mediaType := member.MediaType
	if mediaType == "" {
		mediaType = source.MediaType
	}
	kind := source.AssetKind
	if role == PhotoRoleRAW || role == PhotoRoleImage || role == PhotoRoleSidecar {
		kind = PhotoKindPhoto
	}
	if role == PhotoRoleVideo {
		kind = PhotoKindVideo
	}
	if !photoRoleValid(role) || kind == "" {
		return "", "", "", fmt.Errorf("%w: unsupported photo import member %q", ErrPhotoNodeNotEligible, member.Name)
	}
	return role, mediaType, kind, nil
}

func photoImportNodeFacts(node Node, member PhotoImportMember, role string) PhotoNodeFacts {
	facts := photoNodeFacts(node)
	source := ClassifyPhotoSource(member.Name)
	if source.Role == role && source.AssetKind != "" {
		facts.MediaFamily = "image"
		if source.Kind == PhotoSourceVideo {
			facts.MediaFamily = "audio_video"
		}
		facts.MediaType = source.MediaType
		facts.Qualifies = role != PhotoRoleSidecar
		facts.AssetKind = source.AssetKind
	}
	return facts
}

// photoImportCandidatesTx finds RAW and image files already in photos whose
// current source observation shares the group's folder and stem.
func (s *Store) photoImportCandidatesTx(ctx context.Context, tx *sql.Tx, folder, stem string) ([]photoImportCandidateRow, error) {
	folder = filepath.Clean(folder)
	filter := ""
	args := []any{PhotoRoleRAW, PhotoRoleImage}
	if isASCIIPhotoImportFolder(folder) {
		prefix := folder
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		filter = " AND p.original_path LIKE ? ESCAPE '!'"
		args = append(args, strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(prefix)+"%")
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT pf.node_id, pf.asset_id, pf.role, p.original_path
		FROM provenance p INDEXED BY provenance_original_path_nocase
		JOIN photo_files pf ON pf.node_id=p.node_id
		JOIN nodes n ON n.id=pf.node_id AND n.trashed_at IS NULL
		WHERE pf.role IN (?, ?)
		  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)`+
		filter+` ORDER BY pf.node_id, p.identity`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading photo import candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[int64]struct{})
	var result []photoImportCandidateRow
	for rows.Next() {
		var row photoImportCandidateRow
		if err := rows.Scan(&row.NodeID, &row.AssetID, &row.Role, &row.SourcePath); err != nil {
			return nil, fmt.Errorf("scanning photo import candidate: %w", err)
		}
		candidateFolder, candidateStem := photoImportSourceKey(row.SourcePath)
		if candidateFolder != folder || candidateStem != stem {
			continue
		}
		if _, ok := seen[row.NodeID]; ok {
			continue
		}
		seen[row.NodeID] = struct{}{}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading photo import candidates: %w", err)
	}
	return result, nil
}

func isASCIIPhotoImportFolder(folder string) bool {
	for _, r := range folder {
		if r > 127 {
			return false
		}
	}
	return true
}

func (s *Store) photoImportCurrentDuplicateTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, member PhotoImportMember, role, target string, claimed map[int64]bool,
) (Node, bool, error) {
	var node Node
	// Plain nodes count only for sidecars: a lone sidecar was imported as a plain file.
	photoFilter := "pf.role=?"
	if role == PhotoRoleSidecar {
		photoFilter = "(pf.node_id IS NULL OR pf.role=?)"
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+nodeCols+`, COALESCE(p.original_path, '')
		FROM `+nodeFrom+`
		LEFT JOIN photo_files pf ON pf.node_id=n.id
		LEFT JOIN provenance p ON p.node_id=n.id
		  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
		WHERE n.trashed_at IS NULL AND `+photoFilter+` AND cv.blob_hash=?
		ORDER BY n.id, p.identity`, role, member.BlobHash)
	if err != nil {
		return Node{}, false, fmt.Errorf("finding duplicate photo member: %w", err)
	}
	defer func() { _ = rows.Close() }()
	source := photoImportSourcePath(member.OriginalPath)
	found, fallback := false, Node{}
	for rows.Next() {
		var row Node
		var sourcePath string
		if err := rows.Scan(
			&row.ID, &row.ParentID, &row.Name, &row.Kind, &row.CurrentVersionID,
			&row.BlobHash, &row.MD5, &row.Size, &row.MimeType, &row.Revision,
			&row.CreatedAt, &row.ModifiedAt, &row.TrashedAt, &sourcePath,
		); err != nil {
			return Node{}, false, fmt.Errorf("scanning duplicate photo member: %w", err)
		}
		// Another member of this group already holds the node, so it can't also be this file.
		if claimed[row.ID] {
			continue
		}
		if sourcePath != "" && photoImportSourcePath(sourcePath) == source {
			node, found = row, true
			break
		}
		if fallback.ID == 0 {
			fallback = row
		}
	}
	if err := rows.Err(); err != nil {
		return Node{}, false, fmt.Errorf("reading duplicate photo members: %w", err)
	}
	_ = rows.Close()
	switch {
	case found:
	case role != PhotoRoleSidecar && fallback.ID != 0:
		// RAW and image files skip duplicates by content across folders; only the same source file is a sidecar's duplicate.
		node, found = fallback, true
	case role == PhotoRoleSidecar && target != "":
		// A moved or re-cased folder changes the path, but the photo already holding these bytes is the same shot.
		if node, found, err = photoImportTargetSidecarTx(ctx, tx, target, member.BlobHash, claimed); err != nil {
			return Node{}, false, err
		}
	}
	if !found {
		return Node{}, false, nil
	}
	inserted, err := s.ensureIngestRunForMutationTx(ctx, tx, run)
	if err != nil {
		return Node{}, false, err
	}
	physical := []BlobPhysical(nil)
	if member.Physical.Encoding != "" {
		physical = []BlobPhysical{member.Physical}
	}
	if err := s.EnsureBlobTx(tx, member.BlobHash, member.Size, physical...); err != nil {
		return Node{}, false, fmt.Errorf("reconciling duplicate photo content: %w", err)
	}
	var originalMtime *string
	if member.OriginalMtime != "" {
		originalMtime = &member.OriginalMtime
	}
	fact := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID,
		IngestID: run.record.ID, OriginalPath: member.OriginalPath, OriginalMTime: originalMtime}
	if err := validateProvenanceFields(fact); err != nil {
		return Node{}, false, fmt.Errorf("validating duplicate photo observation: %w", err)
	}
	fact.Identity, err = provenanceIdentity(fact)
	if err != nil {
		return Node{}, false, fmt.Errorf("identifying duplicate photo observation: %w", err)
	}
	observed, err := s.observeOperationalIngestTx(ctx, tx, run, node, fact, inserted)
	if err != nil {
		return Node{}, false, err
	}
	return observed, true, nil
}

func photoImportTargetSidecarTx(
	ctx context.Context, tx *sql.Tx, assetID, blobHash string, claimed map[int64]bool,
) (Node, bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN photo_files pf ON pf.node_id=n.id AND pf.asset_id=? AND pf.role=?
		WHERE n.trashed_at IS NULL AND cv.blob_hash=?
		ORDER BY n.id`, assetID, PhotoRoleSidecar, blobHash)
	if err != nil {
		return Node{}, false, fmt.Errorf("finding photo sidecar by content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var node Node
		if err := rows.Scan(
			&node.ID, &node.ParentID, &node.Name, &node.Kind, &node.CurrentVersionID,
			&node.BlobHash, &node.MD5, &node.Size, &node.MimeType, &node.Revision,
			&node.CreatedAt, &node.ModifiedAt, &node.TrashedAt,
		); err != nil {
			return Node{}, false, fmt.Errorf("scanning photo sidecar by content: %w", err)
		}
		if !claimed[node.ID] {
			return node, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Node{}, false, fmt.Errorf("reading photo sidecars by content: %w", err)
	}
	return Node{}, false, nil
}

// IngestPhotoGroup publishes one group of already-durable bytes and commits
// its nodes, provenance, and photo membership in one logical transaction.
// One RAW and at most one existing photo pair automatically; anything more is
// left unpaired and reported for the operator. A lone sidecar stays a plain
// file until a same-name RAW or image arrives.
func (s *Store) IngestPhotoGroup(ctx context.Context, run IngestRun, group PhotoImportGroup) (result PhotoImportResult, retErr error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return result, err
	}
	if len(group.Members) == 0 {
		return result, errors.New("photo import group has no members")
	}
	if group.DestinationID == 0 {
		group.DestinationID = s.RootID()
	}
	if group.SourceFolder == "" || group.Stem == "" {
		group.SourceFolder, group.Stem = photoImportSourceKey(group.Members[0].OriginalPath)
	}
	return result, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		count := len(group.Members)
		nodes := make([]Node, count)
		roles := make([]string, count)
		kinds := make([]string, count)
		owners := make([]string, count)
		mediaTypes := make([]string, count)
		added, isolated := false, false
		for i, member := range group.Members {
			role, mediaType, kind, err := photoImportMemberRole(member)
			if err != nil {
				return err
			}
			if role == PhotoRoleVideo {
				if count != 1 {
					return errors.New("video import must have one member")
				}
				isolated = true
			}
			if member.BlobHash == "" || member.Size < 0 || member.OriginalPath == "" {
				return fmt.Errorf("photo import member %q lacks a verified source identity", member.Name)
			}
			roles[i], kinds[i], mediaTypes[i] = role, kind, mediaType
		}
		decided := make([]bool, count)
		claimed := make(map[int64]bool, count)
		settle := func(i int, node Node, duplicate bool) error {
			member, role := group.Members[i], roles[i]
			if !duplicate {
				physical := []BlobPhysical(nil)
				if member.Physical.Encoding != "" {
					physical = []BlobPhysical{member.Physical}
				}
				name, options := member.Name, ingestFileOptions{observeMembership: true, deferPhotoEnrollment: true}
				if role == PhotoRoleSidecar {
					// Generic reuse matches by basename alone, so another folder's identical sidecar gets its own name.
					var err error
					if name, err = NormalizeName(name); err != nil {
						return err
					}
					if name, _, _, err = resolveIngestNameTx(tx, group.DestinationID, name, "", run.record.SourceKind); err != nil {
						return err
					}
					options.exact = true
				}
				receipt, created, _, err := s.ingestFileTx(ctx, tx, run, group.DestinationID,
					name, member.BlobHash, member.Size, mediaTypes[i], member.OriginalPath,
					member.OriginalMtime, options, physical...)
				if err != nil {
					return err
				}
				node = receipt.Node
				added = added || created
			}
			nodes[i], decided[i], claimed[node.ID] = node, true, true
			var err error
			owners[i], _, err = photoAssetOwningNodeTx(ctx, tx, node.ID)
			return err
		}
		// RAW and image files settle first so sidecars can see which photo the group joins,
		// then sidecars matched by their own path, then the rest against that photo.
		for i, member := range group.Members {
			if roles[i] == PhotoRoleSidecar {
				continue
			}
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], "", claimed)
			if err != nil {
				return err
			}
			if err := settle(i, node, duplicate); err != nil {
				return err
			}
		}
		for i, member := range group.Members {
			if roles[i] != PhotoRoleSidecar {
				continue
			}
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], "", claimed)
			if err != nil {
				return err
			}
			if duplicate {
				if err := settle(i, node, true); err != nil {
					return err
				}
			}
		}
		joined := ""
		for i := range owners {
			if !decided[i] || owners[i] == "" || owners[i] == joined {
				continue
			}
			if joined != "" {
				joined = ""
				break
			}
			joined = owners[i]
		}
		for i, member := range group.Members {
			if decided[i] {
				continue
			}
			node, duplicate, err := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, roles[i], joined, claimed)
			if err != nil {
				return err
			}
			if err := settle(i, node, duplicate); err != nil {
				return err
			}
		}
		result.Nodes = nodes

		var earlier []photoImportCandidateRow
		if !isolated {
			var err error
			if earlier, err = s.photoImportCandidatesTx(ctx, tx, group.SourceFolder, group.Stem); err != nil {
				return err
			}
		}
		assets := make(map[string]struct{})
		raws := make(map[int64]struct{})
		unownedRAW := false
		for i := range nodes {
			if owners[i] != "" {
				assets[owners[i]] = struct{}{}
			}
			if roles[i] == PhotoRoleRAW {
				raws[nodes[i].ID] = struct{}{}
				unownedRAW = unownedRAW || owners[i] == ""
			}
		}
		for _, candidate := range earlier {
			assets[candidate.AssetID] = struct{}{}
		}
		for assetID := range assets {
			files, err := loadPhotoFiles(ctx, tx, assetID)
			if err != nil {
				return err
			}
			for _, file := range files {
				if file.Role == PhotoRoleRAW {
					raws[file.NodeID] = struct{}{}
				}
			}
		}
		reason := ""
		switch {
		// RAWs the operator already paired into one photo are settled.
		case len(raws) > 1 && (unownedRAW || len(assets) > 1):
			reason = PhotoImportMultipleRAW
		case len(assets) > 1:
			reason = PhotoImportSeparatePhotos
		}
		if reason != "" {
			created, err := s.importUnpairedPhotosTx(ctx, tx, nodes, roles, kinds, owners)
			if err != nil {
				return err
			}
			result.Ambiguity = photoImportAmbiguity(reason, group.Members, nodes, roles, owners, earlier)
			result.Added = added || created
			result.Skipped = !result.Added
			return nil
		}

		targetID := ""
		for assetID := range assets {
			targetID = assetID
		}
		var target PhotoAsset
		if targetID == "" {
			primary := -1
			for i, role := range roles {
				if role == PhotoRoleRAW || primary < 0 && role != PhotoRoleSidecar {
					primary = i
				}
			}
			if primary < 0 {
				result.Added, result.Skipped = added, !added
				return nil
			}
			created, err := s.createPhotoAssetWithReceiptTx(ctx, tx, nodes[primary].ID, roles[primary], kinds[primary], "create")
			if err != nil {
				return err
			}
			owners[primary], target, added = created.ID, created, true
		} else {
			var err error
			if target, err = photoAssetByIDQuery(ctx, tx, targetID); err != nil {
				return err
			}
		}
		attached := false
		for i, node := range nodes {
			if owners[i] != "" {
				continue
			}
			file := PhotoFile{AssetID: target.ID, NodeID: node.ID, Role: roles[i], CreatedAt: nowRFC3339()}
			if roles[i] == PhotoRoleSidecar {
				sourceID, err := photoSidecarSourceTx(ctx, tx, target.ID)
				if err != nil {
					return err
				}
				file.SidecarOfID = &sourceID
			} else if err := validatePhotoFileForAsset(target.Kind, roles[i], photoImportNodeFacts(node, group.Members[i], roles[i])); err != nil {
				return err
			}
			if err := s.insertPhotoFileTx(ctx, tx, file); err != nil {
				return err
			}
			attached = true
		}
		if attached {
			updated, err := commitPhotoAssetTx(ctx, tx, target, target, "import")
			if err != nil {
				return err
			}
			if err := validatePhotoAssetGraph(ctx, tx, updated.ID); err != nil {
				return err
			}
			target = updated
		}
		result.Asset = target
		result.Added = added || attached
		result.Skipped = !result.Added
		return nil
	})
}

// importUnpairedPhotosTx gives each unowned RAW, image, or video its own
// photo. Sidecars stay plain files until the operator pairs the group.
func (s *Store) importUnpairedPhotosTx(ctx context.Context, tx *sql.Tx, nodes []Node, roles, kinds, owners []string) (bool, error) {
	created := false
	for i, node := range nodes {
		if owners[i] != "" || roles[i] == PhotoRoleSidecar {
			continue
		}
		asset, err := s.createPhotoAssetWithReceiptTx(ctx, tx, node.ID, roles[i], kinds[i], "create")
		if err != nil {
			return false, err
		}
		owners[i], created = asset.ID, true
	}
	return created, nil
}

func photoSidecarSourceTx(ctx context.Context, tx *sql.Tx, assetID string) (string, error) {
	files, err := loadPhotoFiles(ctx, tx, assetID)
	if err != nil {
		return "", err
	}
	sourceID := ""
	for _, file := range files {
		if file.Role == PhotoRoleRAW || sourceID == "" && file.Role == PhotoRoleImage {
			sourceID = file.ID
		}
	}
	if sourceID == "" {
		return "", fmt.Errorf("%w: photo %s has no RAW or image for a sidecar", ErrInvalidPhotoAsset, assetID)
	}
	return sourceID, nil
}

func photoImportAmbiguity(
	reason string, members []PhotoImportMember, nodes []Node, roles, owners []string, earlier []photoImportCandidateRow,
) *PhotoImportAmbiguity {
	ambiguity := &PhotoImportAmbiguity{Reason: reason}
	seen := make(map[int64]struct{}, len(nodes)+len(earlier))
	for i, node := range nodes {
		seen[node.ID] = struct{}{}
		ambiguity.Files = append(ambiguity.Files, PhotoImportAmbiguousFile{
			SourcePath: members[i].OriginalPath, NodeID: node.ID, AssetID: owners[i], Role: roles[i],
		})
	}
	for _, candidate := range earlier {
		if _, ok := seen[candidate.NodeID]; ok {
			continue
		}
		seen[candidate.NodeID] = struct{}{}
		ambiguity.Files = append(ambiguity.Files, PhotoImportAmbiguousFile{
			SourcePath: candidate.SourcePath, NodeID: candidate.NodeID, AssetID: candidate.AssetID, Role: candidate.Role,
		})
	}
	return ambiguity
}

// EnsurePhotoImportDestination resolves or creates the complete destination
// in one logical transaction, which also refuses an audited vault.
func (s *Store) EnsurePhotoImportDestination(ctx context.Context, path string) (Node, error) {
	var destination Node
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		destination, err = liveDirTx(tx, s.rootID)
		if err != nil {
			return err
		}
		for _, segment := range splitPath(path) {
			name, normalizeErr := NormalizeName(segment)
			if normalizeErr != nil {
				return fmt.Errorf("photo import destination %q: %w", path, normalizeErr)
			}
			next, childErr := childByName(ctx, tx, destination.ID, name)
			switch {
			case childErr == nil:
				if !next.IsDir() {
					return fmt.Errorf("%q is a file: %w", name, ErrNotDir)
				}
				destination = next
			case errors.Is(childErr, ErrNotFound):
				destination, err = s.mkdirTx(tx, destination.ID, name, nowRFC3339())
				if err != nil {
					return err
				}
			default:
				return childErr
			}
		}
		return nil
	})
	if err != nil {
		return Node{}, err
	}
	return destination, nil
}
