package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

var ErrPhotoImportAmbiguous = errors.New("photo import group is ambiguous")

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

// PhotoImportChoice pins an explicit retry to the candidate the operator saw.
// AssetRevision protects an existing candidate from a concurrent graph edit;
// RawBlobHash identifies a candidate which has not yet received an asset.
type PhotoImportChoice struct {
	GroupKey      string
	RawAssetID    string
	RawFileID     string
	AssetRevision int64
	RawSourcePath string
	RawBlobHash   string
}

// PhotoImportGroup is one same-folder, same-stem unit. DestinationID is a
// virtual folder node and zero means the vault root.
type PhotoImportGroup struct {
	Key           string
	SourceFolder  string
	Stem          string
	DestinationID int64
	RunID         string
	Isolated      bool
	Members       []PhotoImportMember
	Choice        *PhotoImportChoice
}

type PhotoImportCandidate struct {
	AssetID    string `json:"asset_id"`
	FileID     string `json:"file_id"`
	NodeID     int64  `json:"node_id"`
	Revision   int64  `json:"revision"`
	SourcePath string `json:"source_path"`
	BlobHash   string `json:"blob_hash"`
}

type PhotoImportAmbiguity struct {
	GroupKey   string                 `json:"group_key"`
	Candidates []PhotoImportCandidate `json:"candidates"`
}

type PhotoImportAmbiguityError struct {
	PhotoImportAmbiguity
}

func (a *PhotoImportAmbiguityError) Error() string {
	if a == nil {
		return ErrPhotoImportAmbiguous.Error()
	}
	return fmt.Sprintf("%s %q has %d RAW candidates", ErrPhotoImportAmbiguous, a.GroupKey, len(a.Candidates))
}

func (a *PhotoImportAmbiguityError) Unwrap() error { return ErrPhotoImportAmbiguous }

type PhotoImportResult struct {
	Asset     PhotoAsset
	Nodes     []Node
	Added     bool
	Skipped   bool
	Paired    bool
	Ambiguity *PhotoImportAmbiguity
}

type photoImportCandidateRow struct {
	Candidate PhotoImportCandidate
	Role      string
	Name      string
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
	return strings.ToLower(norm.NFC.String(filepath.Clean(filepath.Dir(clean)))), strings.ToLower(norm.NFC.String(stem))
}

// PhotoImportSourceKey returns the normalized source folder and stem used by
// grouped imports.
func PhotoImportSourceKey(path string) (folder, stem string) {
	return photoImportSourceKey(path)
}

// PhotoImportGroupKeyParts returns the printable identity for a photo group.
func PhotoImportGroupKeyParts(folder, stem string) string {
	value := "photo\x00" + strings.ToLower(norm.NFC.String(filepath.Clean(folder))) + "\x00" +
		strings.ToLower(norm.NFC.String(stem))
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// PhotoImportGroupKey returns the printable identity for one discovered source.
func PhotoImportGroupKey(path string, kind PhotoSourceKind) string {
	if kind == PhotoSourceVideo {
		return base64.RawURLEncoding.EncodeToString([]byte("video\x00" + filepath.Clean(path)))
	}
	folder, stem := photoImportSourceKey(path)
	return PhotoImportGroupKeyParts(folder, stem)
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

func photoImportGroupKey(group PhotoImportGroup) string {
	if group.Key != "" {
		return group.Key
	}
	if group.SourceFolder != "" || group.Stem != "" {
		return PhotoImportGroupKeyParts(group.SourceFolder, group.Stem)
	}
	for _, member := range group.Members {
		if member.OriginalPath != "" {
			return PhotoImportGroupKey(member.OriginalPath, ClassifyPhotoSource(member.Name).Kind)
		}
	}
	return ""
}

func (s *Store) photoImportCandidatesTx(ctx context.Context, tx *sql.Tx, folder, stem string) ([]photoImportCandidateRow, error) {
	folder = strings.ToLower(filepath.Clean(folder))
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
		SELECT pf.file_id, pf.asset_id, pf.node_id, pf.role, a.revision,
		       n.name, cv.blob_hash, p.original_path
		FROM provenance p INDEXED BY provenance_original_path_nocase
		JOIN photo_files pf ON pf.node_id=p.node_id
		JOIN photo_assets a ON a.asset_id=pf.asset_id
		JOIN nodes n ON n.id=pf.node_id AND n.trashed_at IS NULL
		JOIN content_versions cv ON cv.version_id=n.current_version_id
		WHERE pf.role IN (?, ?)
		  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)`+
		filter+` ORDER BY pf.file_id, p.identity`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading photo import candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]struct{})
	var result []photoImportCandidateRow
	for rows.Next() {
		var row photoImportCandidateRow
		var sourcePath string
		if err := rows.Scan(&row.Candidate.FileID, &row.Candidate.AssetID, &row.Candidate.NodeID,
			&row.Role, &row.Candidate.Revision, &row.Name, &row.Candidate.BlobHash, &sourcePath); err != nil {
			return nil, fmt.Errorf("scanning photo import candidate: %w", err)
		}
		candidateFolder, candidateStem := photoImportSourceKey(sourcePath)
		if candidateFolder != folder || candidateStem != stem {
			continue
		}
		row.Candidate.SourcePath = sourcePath
		if _, ok := seen[row.Candidate.FileID]; ok {
			continue
		}
		seen[row.Candidate.FileID] = struct{}{}
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

func choiceMatchesCandidate(choice *PhotoImportChoice, candidate PhotoImportCandidate) bool {
	if choice == nil {
		return false
	}
	if choice.RawAssetID != "" && choice.RawAssetID != candidate.AssetID {
		return false
	}
	if choice.RawFileID != "" && choice.RawFileID != candidate.FileID {
		return false
	}
	if choice.RawSourcePath != "" && choice.RawSourcePath != candidate.SourcePath {
		return false
	}
	if choice.RawBlobHash != "" && choice.RawBlobHash != candidate.BlobHash {
		return false
	}
	return choice.RawAssetID != "" || choice.RawFileID != "" || choice.RawSourcePath != "" || choice.RawBlobHash != ""
}

// ValidatePhotoImportChoice accepts the two identities an import can verify.
func ValidatePhotoImportChoice(choice *PhotoImportChoice) error {
	if choice == nil {
		return nil
	}
	if choice.GroupKey == "" {
		return errors.New("photo import choice needs a group key")
	}
	if choice.RawAssetID != "" || choice.RawFileID != "" || choice.AssetRevision != 0 {
		if choice.RawAssetID == "" || choice.RawFileID == "" || choice.AssetRevision < 1 {
			return errors.New("existing RAW choice needs asset ID, file ID, and revision")
		}
		return nil
	}
	if choice.RawSourcePath == "" || choice.RawBlobHash == "" {
		return errors.New("new RAW choice needs source path and blob hash")
	}
	return nil
}

func photoImportMemberMatchesCandidate(member PhotoImportMember, candidate PhotoImportCandidate) bool {
	if member.OriginalPath != "" && candidate.SourcePath != "" {
		return filepath.Clean(member.OriginalPath) == filepath.Clean(candidate.SourcePath)
	}
	return member.BlobHash != "" && candidate.BlobHash != "" && member.BlobHash == candidate.BlobHash
}

func photoImportResolvedChoice(candidates []photoImportCandidateRow, members []PhotoImportMember) *PhotoImportChoice {
	assets := make(map[string]struct{})
	matchedMedia := false
	for _, member := range members {
		role, _, _, err := photoImportMemberRole(member)
		if err != nil || role == PhotoRoleSidecar {
			continue
		}
		var match *photoImportCandidateRow
		for index := range candidates {
			candidate := &candidates[index]
			if candidate.Role == role && photoImportMemberMatchesCandidate(member, candidate.Candidate) {
				if match != nil && match.Candidate.AssetID != candidate.Candidate.AssetID {
					return nil
				}
				match = candidate
			}
		}
		if match == nil {
			return nil
		}
		matchedMedia = true
		assets[match.Candidate.AssetID] = struct{}{}
	}
	if !matchedMedia || len(assets) != 1 {
		return nil
	}
	var raw *photoImportCandidateRow
	for index := range candidates {
		candidate := &candidates[index]
		if candidate.Role != PhotoRoleRAW {
			continue
		}
		if _, ok := assets[candidate.Candidate.AssetID]; !ok {
			continue
		}
		if raw != nil {
			return nil
		}
		raw = candidate
	}
	if raw == nil {
		return nil
	}
	return &PhotoImportChoice{
		GroupKey:      "",
		RawAssetID:    raw.Candidate.AssetID,
		RawFileID:     raw.Candidate.FileID,
		AssetRevision: raw.Candidate.Revision,
		RawSourcePath: raw.Candidate.SourcePath,
		RawBlobHash:   raw.Candidate.BlobHash,
	}
}

func photoImportCandidatesForIncomingChoice(candidates []photoImportCandidateRow, members []PhotoImportMember, choice *PhotoImportChoice) []photoImportCandidateRow {
	if choice == nil {
		return candidates
	}
	incomingChoice := false
	for _, member := range members {
		role, _, _, err := photoImportMemberRole(member)
		if err != nil || role != PhotoRoleRAW {
			continue
		}
		candidate := PhotoImportCandidate{SourcePath: member.OriginalPath, BlobHash: member.BlobHash}
		if choiceMatchesCandidate(choice, candidate) {
			incomingChoice = true
			break
		}
	}
	if !incomingChoice {
		return candidates
	}
	excludedAssets := make(map[string]struct{})
	for _, candidate := range candidates {
		if candidate.Role == PhotoRoleRAW && !choiceMatchesCandidate(choice, candidate.Candidate) {
			excludedAssets[candidate.Candidate.AssetID] = struct{}{}
		}
	}
	filtered := make([]photoImportCandidateRow, 0, len(candidates))
	for _, candidate := range candidates {
		if _, excluded := excludedAssets[candidate.Candidate.AssetID]; !excluded {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func validatePhotoImportChoice(choice *PhotoImportChoice, candidate PhotoImportCandidate) error {
	if choice == nil {
		return nil
	}
	if choice.AssetRevision > 0 && choice.AssetRevision != candidate.Revision {
		return fmt.Errorf("photo import candidate revision changed: got %d, wanted %d: %w", candidate.Revision, choice.AssetRevision, ErrStaleRevision)
	}
	if !choiceMatchesCandidate(choice, candidate) {
		return fmt.Errorf("photo import choice does not identify candidate %s: %w", candidate.FileID, ErrPhotoImportAmbiguous)
	}
	return nil
}

func (s *Store) photoImportCurrentDuplicateTx(
	ctx context.Context, tx *sql.Tx, run IngestRun, member PhotoImportMember, role, sourceFolder, sourceStem string,
) (Node, bool, error) {
	var node Node
	if role == PhotoRoleSidecar {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+nodeCols+`, p.original_path
			FROM `+nodeFrom+`
			JOIN photo_files pf ON pf.node_id=n.id AND pf.role=?
			JOIN provenance p ON p.node_id=n.id
			WHERE n.trashed_at IS NULL AND cv.blob_hash=?
			  AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
			ORDER BY n.id, p.identity`, role, member.BlobHash)
		if err != nil {
			return Node{}, false, fmt.Errorf("finding duplicate photo sidecar: %w", err)
		}
		defer func() { _ = rows.Close() }()
		found := false
		for rows.Next() {
			var sourcePath string
			if err := rows.Scan(
				&node.ID, &node.ParentID, &node.Name, &node.Kind, &node.CurrentVersionID,
				&node.BlobHash, &node.MD5, &node.Size, &node.MimeType, &node.Revision,
				&node.CreatedAt, &node.ModifiedAt, &node.TrashedAt, &sourcePath,
			); err != nil {
				return Node{}, false, fmt.Errorf("scanning duplicate photo sidecar: %w", err)
			}
			folder, stem := photoImportSourceKey(sourcePath)
			if folder == sourceFolder && stem == sourceStem {
				found = true
				break
			}
		}
		if err := rows.Err(); err != nil {
			return Node{}, false, fmt.Errorf("reading duplicate photo sidecars: %w", err)
		}
		if !found {
			return Node{}, false, nil
		}
	} else {
		err := tx.QueryRowContext(ctx, `
		SELECT `+nodeCols+`
		FROM `+nodeFrom+`
		JOIN photo_files pf ON pf.node_id=n.id AND pf.role=?
		WHERE n.trashed_at IS NULL AND cv.blob_hash=?
		ORDER BY n.id LIMIT 1`, role, member.BlobHash).Scan(
			&node.ID, &node.ParentID, &node.Name, &node.Kind, &node.CurrentVersionID,
			&node.BlobHash, &node.MD5, &node.Size, &node.MimeType, &node.Revision,
			&node.CreatedAt, &node.ModifiedAt, &node.TrashedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return Node{}, false, nil
		}
		if err != nil {
			return Node{}, false, fmt.Errorf("finding duplicate photo member: %w", err)
		}
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

func photoImportCandidateAssets(candidates []photoImportCandidateRow, choice *PhotoImportChoice) (string, string, map[string]struct{}, error) {
	assets := make(map[string]struct{})
	targetAssetID := ""
	targetRawFileID := ""
	var rawCandidates []photoImportCandidateRow
	for _, candidate := range candidates {
		if candidate.Role == PhotoRoleRAW {
			rawCandidates = append(rawCandidates, candidate)
		}
	}
	if len(rawCandidates) > 1 {
		var chosen *photoImportCandidateRow
		for index := range rawCandidates {
			if choiceMatchesCandidate(choice, rawCandidates[index].Candidate) {
				if chosen != nil {
					return "", "", assets, fmt.Errorf("photo import choice matches multiple RAW candidates: %w", ErrPhotoImportAmbiguous)
				}
				chosen = &rawCandidates[index]
			}
		}
		if chosen == nil {
			return "", "", assets, fmt.Errorf("photo import has multiple RAW assets: %w", ErrPhotoImportAmbiguous)
		}
		targetAssetID = chosen.Candidate.AssetID
		targetRawFileID = chosen.Candidate.FileID
		assets[targetAssetID] = struct{}{}
		rawAssetIDs := make(map[string]struct{}, len(rawCandidates))
		for _, candidate := range rawCandidates {
			rawAssetIDs[candidate.Candidate.AssetID] = struct{}{}
		}
		for _, candidate := range candidates {
			if candidate.Role == PhotoRoleImage {
				if _, hasRaw := rawAssetIDs[candidate.Candidate.AssetID]; hasRaw && candidate.Candidate.AssetID != targetAssetID {
					continue
				}
				assets[candidate.Candidate.AssetID] = struct{}{}
			}
		}
		return targetAssetID, targetRawFileID, assets, nil
	}
	for _, candidate := range candidates {
		assets[candidate.Candidate.AssetID] = struct{}{}
		if candidate.Role == PhotoRoleRAW {
			targetAssetID = candidate.Candidate.AssetID
			targetRawFileID = candidate.Candidate.FileID
		}
	}
	if targetAssetID == "" {
		for assetID := range assets {
			if targetAssetID != "" && targetAssetID != assetID {
				return "", "", assets, fmt.Errorf("photo import has multiple image assets: %w", ErrPhotoImportAmbiguous)
			}
			targetAssetID = assetID
		}
	}
	return targetAssetID, targetRawFileID, assets, nil
}

// IngestPhotoGroup publishes one group of already-durable bytes and commits
// its node, provenance, asset, and pairing changes in one logical transaction.
func (s *Store) IngestPhotoGroup(ctx context.Context, run IngestRun, group PhotoImportGroup) (result PhotoImportResult, retErr error) {
	if err := requireOperationalIngestRun(run); err != nil {
		return result, err
	}
	if len(group.Members) == 0 {
		return result, errors.New("photo import group has no members")
	}
	if err := ValidatePhotoImportChoice(group.Choice); err != nil {
		return result, err
	}
	group.Key = photoImportGroupKey(group)
	if group.Key == "" {
		return result, errors.New("photo import group has no source identity")
	}
	if group.DestinationID == 0 {
		group.DestinationID = s.RootID()
	}
	if group.SourceFolder == "" || group.Stem == "" {
		for _, member := range group.Members {
			if member.OriginalPath != "" {
				group.SourceFolder, group.Stem = photoImportSourceKey(member.OriginalPath)
				break
			}
		}
	}
	if group.SourceFolder == "" || group.Stem == "" {
		return result, errors.New("photo import group has no source folder or stem")
	}
	return result, s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		var err error
		var candidates []photoImportCandidateRow
		for _, member := range group.Members {
			role, _, _, roleErr := photoImportMemberRole(member)
			if roleErr != nil {
				return roleErr
			}
			if role == PhotoRoleVideo {
				if len(group.Members) != 1 {
					return errors.New("video import must have one member")
				}
				group.Isolated = true
			}
		}
		var rawMembers []PhotoImportMember
		for _, member := range group.Members {
			role, _, _, roleErr := photoImportMemberRole(member)
			if roleErr != nil {
				return roleErr
			}
			if role == PhotoRoleRAW {
				rawMembers = append(rawMembers, member)
			}
		}
		var nodes []Node
		var roles []string
		var kinds []string
		allExisting := true
		for _, member := range group.Members {
			role, mediaType, kind, roleErr := photoImportMemberRole(member)
			if roleErr != nil {
				return roleErr
			}
			if member.BlobHash == "" || member.Size < 0 || member.OriginalPath == "" {
				return fmt.Errorf("photo import member %q lacks a verified source identity", member.Name)
			}
			physical := []BlobPhysical(nil)
			if member.Physical.Encoding != "" {
				physical = []BlobPhysical{member.Physical}
			}
			sourceFolder, sourceStem := group.SourceFolder, group.Stem
			if member.OriginalPath != "" {
				memberFolder, memberStem := photoImportSourceKey(member.OriginalPath)
				if sourceFolder == "" {
					sourceFolder = memberFolder
				}
				if sourceStem == "" {
					sourceStem = memberStem
				}
			}
			node, duplicate, duplicateErr := s.photoImportCurrentDuplicateTx(ctx, tx, run, member, role, sourceFolder, sourceStem)
			if duplicateErr != nil {
				return duplicateErr
			}
			if !duplicate {
				allExisting = false
				receipt, _, _, ingestErr := s.ingestFileTx(ctx, tx, run, group.DestinationID,
					member.Name, member.BlobHash, member.Size, mediaType, member.OriginalPath,
					member.OriginalMtime, ingestFileOptions{observeMembership: true, deferPhotoEnrollment: true}, physical...)
				if ingestErr != nil {
					return ingestErr
				}
				node = receipt.Node
			}
			nodes = append(nodes, node)
			roles = append(roles, role)
			kinds = append(kinds, kind)
		}
		result.Nodes = nodes
		if !group.Isolated {
			candidates, err = s.photoImportCandidatesTx(ctx, tx, group.SourceFolder, group.Stem)
			if err != nil {
				return err
			}
		}
		var rawCandidates []PhotoImportCandidate
		rawAssets := make(map[string]struct{})
		for _, candidate := range candidates {
			if candidate.Role == PhotoRoleRAW {
				rawCandidates = append(rawCandidates, candidate.Candidate)
				rawAssets[candidate.Candidate.AssetID] = struct{}{}
			}
		}
		allOwned := allExisting
		var ownedAssetID string
		for _, node := range nodes {
			assetID, owned, ownerErr := photoAssetOwningNodeTx(ctx, tx, node.ID)
			if ownerErr != nil {
				return ownerErr
			}
			if !owned {
				allOwned = false
				break
			}
			if ownedAssetID == "" {
				ownedAssetID = assetID
			}
		}
		if allOwned && len(rawAssets) > 0 {
			for _, candidate := range candidates {
				if candidate.Role == PhotoRoleImage {
					if _, paired := rawAssets[candidate.Candidate.AssetID]; !paired {
						allOwned = false
						break
					}
				}
			}
		}
		if allOwned {
			if group.Choice != nil {
				chosenAssetID := ""
				for _, candidate := range rawCandidates {
					if choiceMatchesCandidate(group.Choice, candidate) {
						if err := validatePhotoImportChoice(group.Choice, candidate); err != nil {
							return err
						}
						if chosenAssetID != "" && chosenAssetID != candidate.AssetID {
							return fmt.Errorf("photo import choice matches multiple RAWs: %w", ErrPhotoImportAmbiguous)
						}
						chosenAssetID = candidate.AssetID
					}
				}
				if chosenAssetID == "" {
					return fmt.Errorf("photo import choice does not identify a RAW: %w", ErrPhotoImportAmbiguous)
				}
				for _, candidate := range candidates {
					if candidate.Role == PhotoRoleImage && candidate.Candidate.AssetID != chosenAssetID {
						allOwned = false
						break
					}
				}
			}
		}
		if allOwned {
			result.Asset, err = photoAssetByIDQuery(ctx, tx, ownedAssetID)
			if err != nil {
				return err
			}
			result.Skipped = true
			if group.RunID != "" {
				return updatePhotoImportRunTx(ctx, tx, group.RunID, 0, 1, 0, 0, nil)
			}
			return nil
		}
		effectiveChoice := group.Choice
		if effectiveChoice == nil {
			effectiveChoice = photoImportResolvedChoice(candidates, group.Members)
		}
		allRawCandidates := append([]PhotoImportCandidate(nil), rawCandidates...)
		for _, member := range rawMembers {
			matched := false
			for _, candidate := range rawCandidates {
				if photoImportMemberMatchesCandidate(member, candidate) {
					matched = true
					break
				}
			}
			if !matched {
				allRawCandidates = append(allRawCandidates, PhotoImportCandidate{SourcePath: member.OriginalPath, BlobHash: member.BlobHash})
			}
		}
		if len(allRawCandidates) > 1 {
			chosen := PhotoImportCandidate{}
			for _, candidate := range allRawCandidates {
				if choiceMatchesCandidate(effectiveChoice, candidate) {
					if chosen.FileID != "" || chosen.SourcePath != "" {
						return fmt.Errorf("photo import choice matches multiple RAW candidates: %w", ErrPhotoImportAmbiguous)
					}
					chosen = candidate
				}
			}
			if chosen.FileID == "" && chosen.SourcePath == "" {
				ambiguity := PhotoImportAmbiguity{GroupKey: group.Key, Candidates: allRawCandidates}
				result.Ambiguity = &ambiguity
				return &PhotoImportAmbiguityError{PhotoImportAmbiguity: ambiguity}
			}
			if chosen.FileID != "" {
				if err := validatePhotoImportChoice(effectiveChoice, chosen); err != nil {
					return err
				}
			}
		} else if len(allRawCandidates) == 1 && effectiveChoice != nil {
			chosen := allRawCandidates[0]
			if chosen.FileID != "" {
				if err := validatePhotoImportChoice(effectiveChoice, chosen); err != nil {
					return err
				}
			} else if !choiceMatchesCandidate(effectiveChoice, chosen) {
				return fmt.Errorf("photo import choice does not identify incoming RAW: %w", ErrPhotoImportAmbiguous)
			}
		}
		candidates = photoImportCandidatesForIncomingChoice(candidates, group.Members, effectiveChoice)
		targetAssetID, targetRawFileID, candidateAssets, candidateErr := photoImportCandidateAssets(candidates, effectiveChoice)
		if candidateErr != nil {
			allCandidates := make([]PhotoImportCandidate, 0, len(candidates))
			for _, candidate := range candidates {
				allCandidates = append(allCandidates, candidate.Candidate)
			}
			ambiguity := PhotoImportAmbiguity{GroupKey: group.Key, Candidates: allCandidates}
			result.Ambiguity = &ambiguity
			return &PhotoImportAmbiguityError{PhotoImportAmbiguity: ambiguity}
		}
		touchedAssets := make(map[string]struct{}, len(candidateAssets)+1)
		for assetID := range candidateAssets {
			if assetID != "" {
				touchedAssets[assetID] = struct{}{}
			}
		}
		assetChanged := false
		for i, node := range nodes {
			assetID, owned, ownerErr := photoAssetOwningNodeTx(ctx, tx, node.ID)
			if ownerErr != nil {
				return ownerErr
			}
			if owned {
				touchedAssets[assetID] = struct{}{}
			}
			if owned && roles[i] == PhotoRoleRAW {
				targetAssetID = assetID
				var fileID string
				if err := tx.QueryRowContext(ctx, `SELECT file_id FROM photo_files WHERE node_id=?`, node.ID).Scan(&fileID); err != nil {
					return err
				}
				targetRawFileID = fileID
			}
		}
		if targetAssetID == "" {
			for i, node := range nodes {
				assetID, owned, ownerErr := photoAssetOwningNodeTx(ctx, tx, node.ID)
				if ownerErr != nil {
					return ownerErr
				}
				if owned {
					touchedAssets[assetID] = struct{}{}
				}
				if owned && (roles[i] == PhotoRoleImage || roles[i] == PhotoRoleVideo) {
					targetAssetID = assetID
					break
				}
			}
		}
		if targetAssetID == "" {
			primary := -1
			for i, role := range roles {
				if role == PhotoRoleRAW {
					primary = i
					break
				}
			}
			if primary < 0 {
				for i, role := range roles {
					if role == PhotoRoleImage || role == PhotoRoleVideo {
						primary = i
						break
					}
				}
			}
			if primary < 0 {
				return errors.New("photo import group has no primary media")
			}
			asset, createErr := s.insertPhotoAssetTx(ctx, tx, nodes[primary].ID, roles[primary], kinds[primary])
			if createErr != nil {
				return createErr
			}
			targetAssetID = asset.ID
			touchedAssets[targetAssetID] = struct{}{}
			result.Added = true
		}
		touchedAssets[targetAssetID] = struct{}{}
		for assetID := range candidateAssets {
			if assetID == targetAssetID {
				continue
			}
			if err := s.mergePhotoAssetsTxWithContext(ctx, tx, assetID, targetAssetID); err != nil {
				return err
			}
			assetChanged = true
		}
		target, err := photoAssetByIDQuery(ctx, tx, targetAssetID)
		if err != nil {
			return err
		}
		for i, role := range roles {
			if role != PhotoRoleRAW {
				continue
			}
			for _, existing := range target.Files {
				if existing.Role == PhotoRoleRAW && existing.NodeID != nodes[i].ID {
					return fmt.Errorf("JPEG already belongs to another RAW: %w", ErrInvalidPhotoAsset)
				}
			}
		}
		beforeTarget := target
		for i, node := range nodes {
			ownerID, owned, ownerErr := photoAssetOwningNodeTx(ctx, tx, node.ID)
			if ownerErr != nil {
				return ownerErr
			}
			if owned {
				touchedAssets[ownerID] = struct{}{}
				if ownerID != targetAssetID {
					if err := s.mergePhotoAssetsTxWithContext(ctx, tx, ownerID, targetAssetID); err != nil {
						return err
					}
					assetChanged = true
				}
				continue
			}
			role := roles[i]
			if role == PhotoRoleSidecar {
				targetID := targetRawFileID
				if targetID == "" {
					files, filesErr := loadPhotoFiles(ctx, tx, targetAssetID)
					if filesErr != nil {
						return filesErr
					}
					for _, file := range files {
						if photoSidecarTargetRole(file.Role) {
							targetID = file.ID
							break
						}
					}
				}
				if targetID == "" {
					return fmt.Errorf("%w: sidecar %q has no photo target", ErrInvalidPhotoAsset, nodes[i].Name)
				}
				if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: targetAssetID, NodeID: node.ID, Role: role, SidecarOfID: &targetID, CreatedAt: nowRFC3339()}); err != nil {
					return err
				}
				assetChanged = true
				continue
			}
			if err := validatePhotoFileForAsset(target.Kind, role, photoImportNodeFacts(node, group.Members[i], role)); err != nil {
				return err
			}
			if err := s.insertPhotoFileTx(ctx, tx, PhotoFile{AssetID: targetAssetID, NodeID: node.ID, Role: role, CreatedAt: nowRFC3339()}); err != nil {
				return err
			}
			assetChanged = true
		}
		if assetChanged {
			updated, commitErr := commitPhotoAssetTx(ctx, tx, beforeTarget, target, "import")
			if commitErr != nil {
				return commitErr
			}
			target = updated
			result.Added = true
		}
		for assetID := range touchedAssets {
			if err := validatePhotoAssetGraph(ctx, tx, assetID); err != nil {
				return err
			}
		}
		result.Asset = target
		result.Paired = assetChanged
		result.Skipped = !result.Added && !result.Paired
		if group.RunID != "" {
			if err := updatePhotoImportRunTx(ctx, tx, group.RunID, int64(boolInt(result.Added)), int64(boolInt(result.Skipped)), 0, 0, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) EnsurePhotoImportAllowed(ctx context.Context) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		active, err := auditAuthorityActiveTx(ctx, tx)
		if err != nil {
			return err
		}
		if active {
			return ErrAuditMutationUnsupported
		}
		return nil
	})
}

// EnsurePhotoImportDestination resolves or creates the complete destination
// while the photo import admission check still owns the same transaction.
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

func (s *Store) mergePhotoAssetsTxWithContext(ctx context.Context, tx *sql.Tx, sourceID, targetID string) error {
	if sourceID == targetID {
		return nil
	}
	source, err := photoAssetByIDQuery(ctx, tx, sourceID)
	if err != nil {
		return err
	}
	target, err := photoAssetByIDQuery(ctx, tx, targetID)
	if err != nil {
		return err
	}
	if source.Kind != target.Kind || source.ExcludedAt != nil && target.ExcludedAt == nil || source.ExcludedAt == nil && target.ExcludedAt != nil || source.DisplayOverrideFileID != nil && target.DisplayOverrideFileID != nil {
		return fmt.Errorf("photo assets %s and %s have conflicting decisions: %w", sourceID, targetID, ErrPhotoImportAmbiguous)
	}
	for _, sourceFile := range source.Files {
		if sourceFile.Role != PhotoRoleRAW {
			continue
		}
		for _, targetFile := range target.Files {
			if targetFile.Role == PhotoRoleRAW && targetFile.NodeID != sourceFile.NodeID {
				return fmt.Errorf("JPEG already belongs to another RAW: %w", ErrInvalidPhotoAsset)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE photo_files SET asset_id=? WHERE asset_id=?`, targetID, sourceID); err != nil {
		return fmt.Errorf("moving photo members from %s to %s: %w", sourceID, targetID, err)
	}
	if source.ExcludedAt != nil {
		target.ExcludedAt = source.ExcludedAt
	}
	if target.DisplayOverrideFileID == nil {
		target.DisplayOverrideFileID = source.DisplayOverrideFileID
	}
	if _, err := commitPhotoAssetTx(ctx, tx, source, source, "import"); err != nil {
		return err
	}
	_, err = commitPhotoAssetTx(ctx, tx, target, target, "import")
	return err
}

// PhotoImportCandidates is a bounded read used by API clients that need
// to render an ambiguity choice without exposing source paths in browser text.
func (s *Store) PhotoImportCandidates(ctx context.Context, group PhotoImportGroup) ([]PhotoImportCandidate, error) {
	group.Key = photoImportGroupKey(group)
	if group.SourceFolder == "" || group.Stem == "" {
		for _, member := range group.Members {
			if member.OriginalPath != "" {
				group.SourceFolder, group.Stem = photoImportSourceKey(member.OriginalPath)
				break
			}
		}
	}
	var candidates []PhotoImportCandidate
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		rows, err := s.photoImportCandidatesTx(ctx, tx, group.SourceFolder, group.Stem)
		for _, row := range rows {
			if row.Role == PhotoRoleRAW {
				candidates = append(candidates, row.Candidate)
			}
		}
		return err
	})
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].FileID < candidates[j].FileID })
	return candidates, err
}
