package report

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"go.kenn.io/docbank/internal/canonical"
)

type relationGroups struct{ parent map[Identity]Identity }

func newRelationGroups() *relationGroups { return &relationGroups{parent: make(map[Identity]Identity)} }

func (g *relationGroups) root(id Identity) Identity {
	if _, found := g.parent[id]; !found {
		g.parent[id] = id
	}
	for g.parent[id] != id {
		g.parent[id] = g.parent[g.parent[id]]
		id = g.parent[id]
	}
	return id
}

func (g *relationGroups) join(a, b Identity) {
	a, b = g.root(a), g.root(b)
	if a == b {
		return
	}
	if a.VersionID < b.VersionID || a.VersionID == b.VersionID && a.NodeID < b.NodeID {
		g.parent[b] = a
	} else {
		g.parent[a] = b
	}
}

func verifyFrameEvidence(ctx context.Context, budget Budget, frame Frame) error {
	request, err := NormalizeRequest(frame.Request)
	if err != nil {
		return err
	}
	if frame.VaultID == "" || frame.GenerationKind != "native" && frame.GenerationKind != "rendition" ||
		frame.ObservedAt.IsZero() || len(frame.Members) > 50000 || len(frame.Relations) > 100000 {
		return ErrInvalidPacket
	}
	selected := make(map[string]bool, len(request.CollectionIDs))
	for _, id := range request.CollectionIDs {
		selected[id] = true
	}
	identities := make(map[int64]Identity, len(frame.Members))
	memberIndex := make(map[Identity]int, len(frame.Members))
	choices := make(map[Identity]*DateChoice, len(request.DateChoices))
	for i := range request.DateChoices {
		choices[request.DateChoices[i].Document] = &request.DateChoices[i]
	}
	for i, member := range frame.Members {
		if err := ctx.Err(); err != nil {
			return err
		}
		if member.Identity.NodeID <= 0 || !validSHA256(member.Identity.SHA256) || member.Identity.VersionID == "" ||
			len(member.RawMatches) != len(request.Terms) || len(member.Eligible) != len(request.Terms) ||
			len(member.Hits) != len(request.Terms) {
			return fmt.Errorf("%w: invalid member identity or term bits", ErrInvalidPacket)
		}
		coverage := member.Coverage
		if coverage.SearchState != StateComplete && coverage.SearchState != "missing" ||
			coverage.DateEvidenceState != StateComplete ||
			coverage.FamilyState != StateComplete && coverage.FamilyState != "incomplete" {
			return fmt.Errorf("%w: invalid member coverage state", ErrInvalidPacket)
		}
		if _, exists := identities[member.Identity.NodeID]; exists {
			return fmt.Errorf("%w: conflicting node identities", ErrInvalidPacket)
		}
		identities[member.Identity.NodeID] = member.Identity
		memberIndex[member.Identity] = i
		if !request.AllDocuments {
			witnessed := false
			for _, witness := range member.CollectionWitnesses {
				if witness.MembershipID == "" || witness.OriginalPath == "" ||
					witness.MembershipSHA256 != WitnessDigest(member.Identity.NodeID, witness) {
					return fmt.Errorf("%w: collection witness digest differs", ErrInvalidPacket)
				}
				if selected[witness.CollectionID] {
					witnessed = true
				}
			}
			if !witnessed {
				return fmt.Errorf("%w: member lacks selected collection witness", ErrInvalidPacket)
			}
		}
		candidateIDs := make(map[string]bool, len(member.Candidates))
		for _, candidate := range member.Candidates {
			if candidate.Document != member.Identity || candidate.ID == "" || candidateIDs[candidate.ID] {
				return fmt.Errorf("%w: conflicting candidate binding", ErrInvalidPacket)
			}
			if candidate.SourceClass == "content" &&
				(candidate.ID != contentCandidateID(candidate) || candidate.Locator.TextSHA256 == "" ||
					candidate.Locator.EvidenceSHA256 != candidate.Locator.TextSHA256 ||
					candidate.Locator.EndByte <= candidate.Locator.StartByte) {
				return fmt.Errorf("%w: content candidate locator differs", ErrInvalidPacket)
			}
			candidateIDs[candidate.ID] = true
		}
		choice := choices[member.Identity]
		delete(choices, member.Identity)
		selection, err := SelectDate(member.Kind, member.Candidates, choice, request)
		if errors.Is(err, ErrUnusableDate) && request.CoverageMode == "available_only" && member.Selection.Date == "" {
			continue
		}
		if err != nil || selection != member.Selection {
			return fmt.Errorf("%w: date decision differs for member %d: %w", ErrInvalidPacket, i, err)
		}
	}
	if len(choices) != 0 {
		return fmt.Errorf("%w: date choice targets absent member", ErrInvalidPacket)
	}
	fieldBudget := budget.Child()
	defer func() { _ = fieldBudget.Close() }()
	adapted, err := AdaptDateFields(ctx, fieldBudget, frame.RawDateFields)
	if err != nil {
		return fmt.Errorf("%w: raw date fields: %w", ErrInvalidPacket, err)
	}
	adaptedByID := make(map[string]DateCandidate, len(adapted))
	for _, candidate := range adapted {
		adaptedByID[candidate.ID] = candidate
		index, found := memberIndex[candidate.Document]
		if !found {
			return fmt.Errorf("%w: raw field targets absent member", ErrInvalidPacket)
		}
		matched := slices.Contains(frame.Members[index].Candidates, candidate)
		if !matched {
			return fmt.Errorf("%w: raw date candidate differs", ErrInvalidPacket)
		}
	}
	for _, member := range frame.Members {
		for _, candidate := range member.Candidates {
			if candidate.SourceClass == "native" || candidate.SourceClass == "source_metadata" {
				if adaptedByID[candidate.ID] != candidate {
					return fmt.Errorf("%w: source date lacks raw authority", ErrInvalidPacket)
				}
			}
		}
	}
	for _, binding := range frame.Texts {
		if _, found := memberIndex[binding.Document]; !found ||
			binding.Size < 0 || binding.Size > 16<<20 ||
			binding.Native != nil && binding.Native.Text != nil {
			return fmt.Errorf("%w: invalid retained text binding", ErrInvalidPacket)
		}
	}
	groups := newRelationGroups()
	for _, relation := range frame.Relations {
		if relation.Parent.NodeID <= 0 || relation.Child.NodeID <= 0 ||
			!validSHA256(relation.Parent.SHA256) || !validSHA256(relation.Child.SHA256) ||
			relation.Parent.VersionID == "" || relation.Child.VersionID == "" ||
			relation.EvidenceID == "" || !validSHA256(relation.EvidenceSHA256) {
			return fmt.Errorf("%w: malformed family relation", ErrInvalidPacket)
		}
		for _, endpoint := range []Identity{relation.Parent, relation.Child} {
			if known, exists := identities[endpoint.NodeID]; exists && known != endpoint {
				return fmt.Errorf("%w: relation conflicts with member identity", ErrInvalidPacket)
			}
		}
		groups.join(relation.Parent, relation.Child)
	}
	for _, member := range frame.Members {
		_, connected := groups.parent[member.Identity]
		want := ""
		if connected {
			want = groups.root(member.Identity).VersionID
		}
		if member.FamilyID != want {
			return fmt.Errorf("%w: family component differs", ErrInvalidPacket)
		}
	}
	return nil
}

// VerifyBundle checks the packet's internal consistency independently of a
// vault. It cannot establish that the producer searched all source documents.
func VerifyBundle(ctx context.Context, budget Budget, input io.Reader, size int64) (Verification, error) {
	if budget == nil || size < 0 || size > maxBundleBytes {
		return Verification{}, ErrInvalidPacket
	}
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	if _, err := scratch.Reserve(ctx, size); err != nil {
		return Verification{}, err
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(input, raw); err != nil {
		return Verification{}, err
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), size)
	if err != nil {
		return Verification{}, fmt.Errorf("%w: %w", ErrInvalidPacket, err)
	}
	if len(archive.File) != len(bundleNames) {
		return Verification{}, ErrInvalidPacket
	}
	payloads := make(map[string][]byte, len(bundleNames))
	var decodedBytes int64
	for i, file := range archive.File {
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		if file.Name != bundleNames[i] || file.FileInfo().IsDir() ||
			file.UncompressedSize64 > maxBundleBytes || file.CompressedSize64 > maxBundleBytes {
			return Verification{}, ErrInvalidPacket
		}
		decodedBytes += int64(file.UncompressedSize64)
		if decodedBytes > maxBundleBytes {
			return Verification{}, ErrInvalidPacket
		}
		if _, err := scratch.Reserve(ctx, int64(file.UncompressedSize64)); err != nil {
			return Verification{}, err
		}
		reader, err := file.Open()
		if err != nil {
			return Verification{}, fmt.Errorf("opening report packet entry %s: %w", file.Name, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(reader, int64(file.UncompressedSize64)+1))
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return Verification{}, err
		}
		if uint64(len(payload)) != file.UncompressedSize64 {
			return Verification{}, ErrInvalidPacket
		}
		payloads[file.Name] = payload
	}
	manifest, err := canonical.Decode[packetManifest](payloads["manifest.json"])
	if err != nil {
		return Verification{}, fmt.Errorf("%w: manifest: %w", ErrInvalidPacket, err)
	}
	if manifest.Format != BundleFormatV1 || manifest.DateRule != DateRuleV1 || len(manifest.Inventory) != 4 {
		return Verification{}, ErrInvalidPacket
	}
	coreBytes, err := canonical.Marshal(manifest.packetManifestCore)
	if err != nil {
		return Verification{}, err
	}
	if manifest.ID != packetDigest(coreBytes) {
		return Verification{}, ErrInvalidPacket
	}
	for _, name := range bundleNames {
		if name == "manifest.json" {
			continue
		}
		inventory, exists := manifest.Inventory[name]
		if !exists || inventory.Bytes != int64(len(payloads[name])) ||
			inventory.SHA256 != packetDigest(payloads[name]) {
			return Verification{}, fmt.Errorf("%w: payload inventory mismatch", ErrInvalidPacket)
		}
	}
	members, err := decodePacketLines[packetMember](ctx, payloads["members.jsonl"], 50000)
	if err != nil {
		return Verification{}, err
	}
	dates, err := decodePacketLines[packetDate](ctx, payloads["dates.jsonl"], 50000)
	if err != nil {
		return Verification{}, err
	}
	relations, err := decodePacketLines[Relation](ctx, payloads["families.jsonl"], 100000)
	if err != nil {
		return Verification{}, err
	}
	if len(members) != len(dates) {
		return Verification{}, ErrInvalidPacket
	}
	frame := Frame{VaultID: manifest.VaultID, GenerationID: manifest.GenerationID,
		GenerationKind: manifest.GenerationKind, ObservedAt: manifest.ObservedAt,
		Request: manifest.Request, CoverageSelection: manifest.CoverageSelection,
		Dependencies: manifest.Dependencies, Coverage: manifest.Coverage,
		RowCoverage: manifest.RowCoverage, Relations: relations, Members: make([]Member, len(members))}
	for i, item := range members {
		if dates[i].Document != item.Identity {
			return Verification{}, ErrInvalidPacket
		}
		frame.Members[i] = Member{Identity: item.Identity, Kind: item.Kind, FamilyID: item.FamilyID,
			CollectionWitnesses: item.CollectionWitnesses, Coverage: item.Coverage,
			Selection: item.Selection, RawMatches: item.RawMatches, Eligible: item.Eligible,
			Hits: item.Hits, Candidates: dates[i].Candidates}
		frame.Texts = append(frame.Texts, dates[i].Texts...)
		frame.RawDateFields = append(frame.RawDateFields, dates[i].RawDateFields...)
		if dates[i].Choice != nil {
			found := false
			for _, choice := range manifest.Request.DateChoices {
				if reflect.DeepEqual(choice, *dates[i].Choice) {
					found = true
					break
				}
			}
			if !found {
				return Verification{}, ErrInvalidPacket
			}
		}
	}
	result := Result{Frame: frame, Counts: manifest.Counts}
	if err := validateBundleResult(ctx, scratch, result); err != nil {
		return Verification{}, err
	}
	var csv bytes.Buffer
	if err := WriteCSV(ctx, &csv, result); err != nil {
		return Verification{}, err
	}
	if !bytes.Equal(csv.Bytes(), payloads["hits.csv"]) {
		return Verification{}, ErrInvalidPacket
	}
	return Verification{InternallyConsistent: true, SourceVerified: false}, nil
}

// ExtractVerifiedCSV returns the exchangeable count sheet only after the whole
// packet has passed independent internal-consistency verification. It keeps
// one immutable input copy so a concurrent file edit cannot change the CSV
// between verification and extraction.
func ExtractVerifiedCSV(ctx context.Context, budget Budget, input io.Reader, size int64) (_ []byte, retErr error) {
	if budget == nil || size < 0 || size > maxBundleBytes {
		return nil, ErrInvalidPacket
	}
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	if _, err := scratch.Reserve(ctx, size); err != nil {
		return nil, err
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(input, raw); err != nil {
		return nil, err
	}
	if _, err := VerifyBundle(ctx, scratch, bytes.NewReader(raw), size); err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), size)
	if err != nil || len(archive.File) == 0 || archive.File[0].Name != "hits.csv" ||
		archive.File[0].UncompressedSize64 > maxBundleBytes {
		return nil, ErrInvalidPacket
	}
	file := archive.File[0]
	// The archive size was bounded by maxBundleBytes above, which fits int64.
	//nolint:gosec // UncompressedSize64 is already below the packet byte limit.
	release, err := budget.Reserve(ctx, int64(file.UncompressedSize64))
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			release()
		}
	}()
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("opening report CSV: %w", err)
	}
	csv := make([]byte, file.UncompressedSize64)
	_, readErr := io.ReadFull(reader, csv)
	var extra [1]byte
	if readErr == nil {
		if n, nextErr := reader.Read(extra[:]); n != 0 || !errors.Is(nextErr, io.EOF) {
			readErr = ErrInvalidPacket
		}
	}
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if uint64(len(csv)) != file.UncompressedSize64 {
		return nil, ErrInvalidPacket
	}
	return csv, nil
}

func decodePacketLines[T any](ctx context.Context, raw []byte, maximum int) ([]T, error) {
	if len(raw) != 0 && raw[len(raw)-1] != '\n' {
		return nil, ErrInvalidPacket
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if len(raw) == 0 {
		return nil, nil
	}
	if len(lines)-1 > maximum {
		return nil, ErrInvalidPacket
	}
	result := make([]T, 0, len(lines)-1)
	for _, line := range lines[:len(lines)-1] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(line) == 0 || len(line) > maxPacketLine {
			return nil, ErrInvalidPacket
		}
		value, err := canonical.Decode[T](line)
		if err != nil {
			return nil, fmt.Errorf("%w: evidence JSONL: %w", ErrInvalidPacket, err)
		}
		result = append(result, value)
	}
	return result, nil
}
