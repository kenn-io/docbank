package report

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	BundleFormatV1 = "term-report-v1"
	maxBundleBytes = 512 << 20
	maxPacketLine  = 8 << 20
)

var ErrInvalidPacket = errors.New("invalid search-term report packet")

var bundleNames = []string{"hits.csv", "manifest.json", "members.jsonl", "families.jsonl", "dates.jsonl"}

type packetInventory struct {
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type packetManifestCore struct {
	Format            string                     `json:"format"`
	DateRule          string                     `json:"date_rule"`
	VaultID           string                     `json:"vault_id"`
	GenerationID      string                     `json:"generation_id,omitempty"`
	GenerationKind    string                     `json:"generation_kind"`
	ObservedAt        time.Time                  `json:"observed_at"`
	Request           Request                    `json:"request"`
	CoverageSelection CoverageSelection          `json:"coverage_selection"`
	Dependencies      []Dependency               `json:"dependencies"`
	Coverage          Coverage                   `json:"coverage"`
	RowCoverage       []Coverage                 `json:"row_coverage"`
	Counts            []Counts                   `json:"counts"`
	Inventory         map[string]packetInventory `json:"inventory"`
}

type packetManifest struct {
	packetManifestCore

	ID string `json:"id"`
}

type packetMember struct {
	Identity            Identity            `json:"identity"`
	Kind                string              `json:"kind"`
	FamilyID            string              `json:"family_id"`
	CollectionWitnesses []CollectionWitness `json:"collection_witnesses,omitempty"`
	Coverage            MemberCoverage      `json:"coverage"`
	Selection           DateSelection       `json:"selection"`
	RawMatches          []bool              `json:"raw_matches"`
	Eligible            []bool              `json:"eligible"`
	Hits                []bool              `json:"hits"`
}

type packetDate struct {
	Document      Identity        `json:"document"`
	Candidates    []DateCandidate `json:"candidates"`
	RawDateFields []RawDateField  `json:"raw_date_fields,omitempty"`
	Texts         []TextBinding   `json:"texts,omitempty"`
	Choice        *DateChoice     `json:"choice,omitempty"`
}

func packetDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateBundleResult(ctx context.Context, budget Budget, result Result) error {
	request, err := NormalizeRequest(result.Frame.Request)
	if err != nil {
		return err
	}
	if len(result.Counts) != len(request.Terms) || len(result.Frame.Members) > 50000 ||
		len(result.Frame.Relations) > 100000 {
		return ErrInvalidPacket
	}
	frame := result.Frame
	frame.Request = request
	if err := verifyFrameEvidence(ctx, budget, frame); err != nil {
		return err
	}
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	recomputed, err := Calculate(ctx, scratch, frame)
	if err != nil {
		return err
	}
	if !slices.Equal(result.Counts, recomputed.Counts) {
		return fmt.Errorf("%w: counts differ from frozen members", ErrInvalidPacket)
	}
	for i := range frame.Members {
		if !slices.Equal(frame.Members[i].Eligible, recomputed.Frame.Members[i].Eligible) ||
			!slices.Equal(frame.Members[i].Hits, recomputed.Frame.Members[i].Hits) {
			return fmt.Errorf("%w: eligibility or hit bits differ", ErrInvalidPacket)
		}
	}
	return nil
}

// BuildBundle emits a deterministic, fixed-layout ZIP from one frozen result.
// It validates the retained evidence and counts before sealing them.
func BuildBundle(ctx context.Context, budget Budget, result Result) ([]byte, error) {
	if budget == nil {
		return nil, errors.New("missing report budget")
	}
	if err := validateBundleResult(ctx, budget, result); err != nil {
		return nil, err
	}
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	var csv bytes.Buffer
	if err := WriteCSV(ctx, &csv, result); err != nil {
		return nil, err
	}
	payloads, err := encodePacketPayloads(ctx, scratch, result)
	if err != nil {
		return nil, err
	}
	payloads["hits.csv"] = csv.Bytes()
	inventory := make(map[string]packetInventory, 4)
	var decodedBytes int64
	for _, name := range bundleNames {
		if name == "manifest.json" {
			continue
		}
		payload := payloads[name]
		decodedBytes += int64(len(payload))
		if decodedBytes > maxBundleBytes {
			return nil, fmt.Errorf("%w: decoded payload exceeds limit", ErrInvalidPacket)
		}
		inventory[name] = packetInventory{Bytes: int64(len(payload)), SHA256: packetDigest(payload)}
	}
	core := packetManifestCore{
		Format: BundleFormatV1, DateRule: DateRuleV1,
		VaultID: result.Frame.VaultID, GenerationID: result.Frame.GenerationID,
		GenerationKind: result.Frame.GenerationKind, ObservedAt: result.Frame.ObservedAt,
		Request: result.Frame.Request, CoverageSelection: result.Frame.CoverageSelection,
		Dependencies: result.Frame.Dependencies, Coverage: result.Frame.Coverage,
		RowCoverage: result.Frame.RowCoverage, Counts: result.Counts, Inventory: inventory,
	}
	coreBytes, err := canonical.Marshal(core)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := canonical.Marshal(packetManifest{packetManifestCore: core, ID: packetDigest(coreBytes)})
	if err != nil {
		return nil, err
	}
	payloads["manifest.json"] = manifestBytes
	decodedBytes += int64(len(manifestBytes))
	if decodedBytes > maxBundleBytes {
		return nil, fmt.Errorf("%w: decoded payload exceeds limit", ErrInvalidPacket)
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, name := range bundleNames {
		if err := ctx.Err(); err != nil {
			_ = writer.Close()
			return nil, err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate,
			Modified: time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)}
		header.SetMode(0644)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return nil, fmt.Errorf("creating report packet entry %s: %w", name, err)
		}
		if _, err := entry.Write(payloads[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("closing report packet: %w", err)
	}
	if output.Len() > maxBundleBytes {
		return nil, fmt.Errorf("%w: encoded payload exceeds limit", ErrInvalidPacket)
	}
	if _, err := budget.Reserve(ctx, int64(output.Len())); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func encodePacketPayloads(ctx context.Context, budget Budget, result Result) (map[string][]byte, error) {
	frame := result.Frame
	memberOrder := make([]int, len(frame.Members))
	byIdentity := make(map[Identity]int, len(frame.Members))
	for i, member := range frame.Members {
		memberOrder[i] = i
		byIdentity[member.Identity] = i
	}
	slices.SortFunc(memberOrder, func(a, b int) int {
		x, y := frame.Members[a].Identity, frame.Members[b].Identity
		if x.NodeID < y.NodeID {
			return -1
		}
		if x.NodeID > y.NodeID {
			return 1
		}
		if x.VersionID < y.VersionID {
			return -1
		}
		if x.VersionID > y.VersionID {
			return 1
		}
		return 0
	})
	choiceByIdentity := make(map[Identity]*DateChoice, len(frame.Request.DateChoices))
	for i := range frame.Request.DateChoices {
		choice := &frame.Request.DateChoices[i]
		choiceByIdentity[choice.Document] = choice
	}
	var members, dates, families bytes.Buffer
	for _, index := range memberOrder {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		member := frame.Members[index]
		encoded := packetMember{Identity: member.Identity, Kind: member.Kind, FamilyID: member.FamilyID,
			CollectionWitnesses: member.CollectionWitnesses, Coverage: member.Coverage,
			Selection: member.Selection, RawMatches: member.RawMatches, Eligible: member.Eligible, Hits: member.Hits}
		if err := writePacketLine(ctx, budget, &members, encoded); err != nil {
			return nil, err
		}
		date := packetDate{Document: member.Identity, Candidates: slices.Clone(member.Candidates),
			Choice: choiceByIdentity[member.Identity]}
		slices.SortFunc(date.Candidates, func(a, b DateCandidate) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		for _, field := range frame.RawDateFields {
			if field.Document == member.Identity {
				date.RawDateFields = append(date.RawDateFields, field)
			}
		}
		for _, binding := range frame.Texts {
			if binding.Document == member.Identity {
				date.Texts = append(date.Texts, binding)
			}
		}
		if err := writePacketLine(ctx, budget, &dates, date); err != nil {
			return nil, err
		}
	}
	relations := slices.Clone(frame.Relations)
	slices.SortFunc(relations, func(a, b Relation) int {
		if a.Parent.NodeID < b.Parent.NodeID {
			return -1
		}
		if a.Parent.NodeID > b.Parent.NodeID {
			return 1
		}
		if a.Child.NodeID < b.Child.NodeID {
			return -1
		}
		if a.Child.NodeID > b.Child.NodeID {
			return 1
		}
		if a.EvidenceID < b.EvidenceID {
			return -1
		}
		if a.EvidenceID > b.EvidenceID {
			return 1
		}
		return 0
	})
	for _, relation := range relations {
		if err := writePacketLine(ctx, budget, &families, relation); err != nil {
			return nil, err
		}
	}
	_ = byIdentity
	return map[string][]byte{"members.jsonl": members.Bytes(), "dates.jsonl": dates.Bytes(), "families.jsonl": families.Bytes()}, nil
}

func writePacketLine(ctx context.Context, budget Budget, output io.Writer, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > maxPacketLine {
		return fmt.Errorf("%w: evidence line exceeds limit", ErrInvalidPacket)
	}
	if _, err := budget.Reserve(ctx, int64(len(encoded)+1)); err != nil {
		return err
	}
	if _, err := output.Write(encoded); err != nil {
		return err
	}
	_, err = output.Write([]byte{'\n'})
	return err
}
