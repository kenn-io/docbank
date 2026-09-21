package report

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"go.kenn.io/docbank/internal/canonical"
)

func TestBundleRoundTripRecomputesSevenDocumentOracle(t *testing.T) {
	budget := NewBudget(4 << 20)
	defer func() { _ = budget.Close() }()
	result, err := Calculate(context.Background(), budget, oracleFrame())
	if err != nil {
		t.Fatal(err)
	}
	packet, err := BuildBundle(context.Background(), budget, result)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := VerifyBundle(context.Background(), budget, bytes.NewReader(packet), int64(len(packet)))
	if err != nil || !verification.InternallyConsistent || verification.SourceVerified {
		t.Fatalf("verification=%+v err=%v", verification, err)
	}
	again, err := BuildBundle(context.Background(), budget, result)
	if err != nil || !bytes.Equal(packet, again) {
		t.Fatalf("same frozen result produced different bytes: err=%v", err)
	}
}

func TestBundleRejectsMutatedCountAndCSV(t *testing.T) {
	budget := NewBudget(4 << 20)
	defer func() { _ = budget.Close() }()
	result, err := Calculate(context.Background(), budget, oracleFrame())
	if err != nil {
		t.Fatal(err)
	}
	result.Counts[0].Hits++
	if _, err := BuildBundle(context.Background(), budget, result); err == nil {
		t.Fatal("accepted count inconsistent with frozen members")
	}
}

func TestBundleRejectsFalseCollectionWitness(t *testing.T) {
	frame := oracleFrame()
	frame.Request.AllDocuments = false
	frame.Request.CollectionIDs = []string{"selected"}
	for i := range frame.Members {
		frame.Members[i].CollectionWitnesses = []CollectionWitness{{
			CollectionID: "selected", MembershipID: "made-up",
			MembershipSHA256: hex.EncodeToString(sha256.New().Sum(nil)),
		}}
	}
	budget := NewBudget(4 << 20)
	defer func() { _ = budget.Close() }()
	result, err := Calculate(context.Background(), budget, frame)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildBundle(context.Background(), budget, result)
	if err == nil || errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("accepted false witness or wrong error: %v", err)
	}
}

func TestVerifyBundleRejectsResealedCoverageChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*packetManifest)
	}{
		{"whole scoped", func(m *packetManifest) { m.Coverage.Scoped++ }},
		{"negative missing text", func(m *packetManifest) { m.Coverage.MissingText = -1 }},
		{"row searchable", func(m *packetManifest) { m.RowCoverage[0].Searchable++ }},
		{"missing row", func(m *packetManifest) { m.RowCoverage = m.RowCoverage[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBudget(4 << 20)
			defer func() { _ = budget.Close() }()
			result, err := Calculate(t.Context(), budget, oracleFrame())
			if err != nil {
				t.Fatal(err)
			}
			result.Frame.Coverage = Coverage{Scoped: 7, Searchable: 7, FallbackDates: 7}
			result.Frame.RowCoverage = []Coverage{result.Frame.Coverage, result.Frame.Coverage}
			packet, err := BuildBundle(t.Context(), budget, result)
			if err != nil {
				t.Fatal(err)
			}
			changed := resealBundle(t, packet, func(_ map[string][]byte, manifest *packetManifest) {
				tc.edit(manifest)
			})
			if _, err := VerifyBundle(t.Context(), budget, bytes.NewReader(changed), int64(len(changed))); !errors.Is(err, ErrInvalidPacket) {
				t.Fatalf("accepted coverage inconsistent with members: %v", err)
			}
		})
	}
}

func TestVerifyBundleRejectsUnknownMemberCoverage(t *testing.T) {
	for _, state := range []string{"search", "date", "family"} {
		t.Run(state, func(t *testing.T) {
			budget := NewBudget(4 << 20)
			defer func() { _ = budget.Close() }()
			result, err := Calculate(t.Context(), budget, oracleFrame())
			if err != nil {
				t.Fatal(err)
			}
			packet, err := BuildBundle(t.Context(), budget, result)
			if err != nil {
				t.Fatal(err)
			}
			changed := resealBundle(t, packet, func(payloads map[string][]byte, manifest *packetManifest) {
				lines := bytes.Split(payloads["members.jsonl"], []byte{'\n'})
				member, err := canonical.Decode[packetMember](lines[0])
				if err != nil {
					t.Fatal(err)
				}
				switch state {
				case "search":
					member.Coverage.SearchState = "unknown"
					manifest.Coverage.Searchable--
					manifest.Coverage.MissingText++
				case "date":
					member.Coverage.DateEvidenceState = "unknown"
				case "family":
					member.Coverage.FamilyState = "unknown"
					manifest.Coverage.IncompleteFamilies++
				}
				for i := range manifest.RowCoverage {
					manifest.RowCoverage[i] = manifest.Coverage
				}
				lines[0], err = canonical.Marshal(member)
				if err != nil {
					t.Fatal(err)
				}
				payloads["members.jsonl"] = bytes.Join(lines, []byte{'\n'})
			})
			if _, err := VerifyBundle(t.Context(), budget, bytes.NewReader(changed), int64(len(changed))); !errors.Is(err, ErrInvalidPacket) {
				t.Fatalf("accepted unknown %s coverage with matching totals: %v", state, err)
			}
		})
	}
}

func TestVerifyBundleRejectsDisconnectedFamilyEvidence(t *testing.T) {
	budget := NewBudget(4 << 20)
	defer func() { _ = budget.Close() }()
	result, err := Calculate(t.Context(), budget, oracleFrame())
	if err != nil {
		t.Fatal(err)
	}
	packet, err := BuildBundle(t.Context(), budget, result)
	if err != nil {
		t.Fatal(err)
	}
	changed := resealBundle(t, packet, func(payloads map[string][]byte, _ *packetManifest) {
		relation := Relation{
			Parent:     Identity{NodeID: 1001, VersionID: "unrelated-parent", SHA256: packetDigest([]byte("synthetic parent"))},
			Child:      Identity{NodeID: 1002, VersionID: "unrelated-child", SHA256: packetDigest([]byte("synthetic child"))},
			EvidenceID: "unrelated-relation", EvidenceSHA256: packetDigest([]byte("synthetic relation")),
		}
		encoded, err := canonical.Marshal(relation)
		if err != nil {
			t.Fatal(err)
		}
		payloads["families.jsonl"] = append(payloads["families.jsonl"], append(encoded, '\n')...)
	})
	if _, err := VerifyBundle(t.Context(), budget, bytes.NewReader(changed), int64(len(changed))); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("accepted family evidence unrelated to the report population: %v", err)
	}
}

func resealBundle(t *testing.T, packet []byte, edit func(map[string][]byte, *packetManifest)) []byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(packet), int64(len(packet)))
	if err != nil {
		t.Fatal(err)
	}
	payloads := make(map[string][]byte, len(archive.File))
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(reader)
		if closeErr := reader.Close(); err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		payloads[file.Name] = payload
	}
	manifest, err := canonical.Decode[packetManifest](payloads["manifest.json"])
	if err != nil {
		t.Fatal(err)
	}
	edit(payloads, &manifest)
	for name := range manifest.Inventory {
		manifest.Inventory[name] = packetInventory{Bytes: int64(len(payloads[name])), SHA256: packetDigest(payloads[name])}
	}
	core, err := canonical.Marshal(manifest.packetManifestCore)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ID = packetDigest(core)
	payloads["manifest.json"], err = canonical.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var changed bytes.Buffer
	writer := zip.NewWriter(&changed)
	for _, name := range bundleNames {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(payloads[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return changed.Bytes()
}
