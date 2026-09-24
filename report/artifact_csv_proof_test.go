package report

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// These draft tests describe the offline artifact boundary. The authenticated
// caller must obtain both streams for the same retained report ID, recheck owner
// and source visibility, and provide the trusted complete summary. A v1 ZIP
// manifest has no retained report ID or owner to verify here.

func csvArtifactProofFixture(t *testing.T) (Summary, []byte, []byte) {
	t.Helper()
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	result, err := Calculate(t.Context(), budget, oracleFrame())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildBundle(t.Context(), budget, result)
	if err != nil {
		t.Fatal(err)
	}
	var csv bytes.Buffer
	if err := WriteCSV(t.Context(), &csv, result); err != nil {
		t.Fatal(err)
	}
	return Summary{
		ID: "111111111111111111111111111111111111111111111111", State: "complete",
		CSVBytes: int64(csv.Len()), CSVSHA256: packetDigest(csv.Bytes()),
		BundleBytes: int64(len(bundle)), BundleSHA256: packetDigest(bundle),
	}, csv.Bytes(), bundle
}

func TestVerifyCSVArtifactAcceptsExactPacketDerivedCSV(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(csv), bytes.NewReader(bundle)); err != nil {
		t.Fatalf("exact CSV and verified companion packet rejected: %v", err)
	}
}

func TestVerifyCSVArtifactRejectsTransportHashMatchingTamperedCSV(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	tampered := bytes.Replace(csv, []byte(",3,6,2,2,5\r\n"), []byte(",9,6,2,2,5\r\n"), 1)
	if bytes.Equal(tampered, csv) {
		t.Fatal("synthetic count mutation did not change CSV")
	}
	// Even if the authenticated transport advertises the changed CSV's exact
	// digest and length, the untouched packet's independently checked hits.csv
	// must prevent publication.
	summary.CSVBytes, summary.CSVSHA256 = int64(len(tampered)), packetDigest(tampered)
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(tampered), bytes.NewReader(bundle)); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("tampered CSV with matching advertised hash accepted: %v", err)
	}
}

func TestVerifyCSVArtifactRejectsCorruptPacketWithMatchingTransportHash(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	tampered := bytes.Clone(bundle)
	tampered[len(tampered)/2] ^= 0xff
	summary.BundleSHA256 = packetDigest(tampered)
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(csv), bytes.NewReader(tampered)); err == nil {
		t.Fatal("corrupt companion packet accepted because its transport hash matched")
	}
}

func TestVerifyCSVArtifactRejectsPacketWithOutsideBytesAndMatchingClaim(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"trailing", append(bytes.Clone(bundle), []byte("synthetic trailing bytes")...)},
		{"leading", append([]byte("synthetic leading bytes"), bundle...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed := summary
			claimed.BundleBytes, claimed.BundleSHA256 = int64(len(tc.raw)), packetDigest(tc.raw)
			budget := NewBudget(32 << 20)
			defer func() { _ = budget.Close() }()
			if err := VerifyCSVArtifact(t.Context(), budget, claimed, bytes.NewReader(csv), bytes.NewReader(tc.raw)); !errors.Is(err, ErrInvalidPacket) {
				t.Fatalf("packet with matching transport claim and outside bytes accepted: %v", err)
			}
		})
	}
}

func TestVerifyCSVArtifactRejectsDifferentValidPacket(t *testing.T) {
	summary, csv, _ := csvArtifactProofFixture(t)
	frame := oracleFrame()
	frame.Members[0].RawMatches[0] = false
	buildBudget := NewBudget(32 << 20)
	defer func() { _ = buildBudget.Close() }()
	otherResult, err := Calculate(t.Context(), buildBudget, frame)
	if err != nil {
		t.Fatal(err)
	}
	otherBundle, err := BuildBundle(t.Context(), buildBudget, otherResult)
	if err != nil {
		t.Fatal(err)
	}
	// Both transport claims match their bytes and the other packet is valid.
	// Its regenerated CSV still differs from the standalone artifact.
	summary.BundleBytes, summary.BundleSHA256 = int64(len(otherBundle)), packetDigest(otherBundle)
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(csv), bytes.NewReader(otherBundle)); !errors.Is(err, ErrInvalidPacket) {
		t.Fatalf("CSV paired with another valid packet: %v", err)
	}
}

func TestVerifyCSVArtifactRejectsPartialOrChangedStreams(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	for _, tc := range []struct {
		name   string
		csv    []byte
		bundle []byte
	}{
		{"short CSV", csv[:len(csv)-1], bundle},
		{"extra CSV", append(bytes.Clone(csv), 'x'), bundle},
		{"short bundle", csv, bundle[:len(bundle)-1]},
		{"extra bundle", csv, append(bytes.Clone(bundle), 'x')},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBudget(32 << 20)
			defer func() { _ = budget.Close() }()
			if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(tc.csv), bytes.NewReader(tc.bundle)); err == nil {
				t.Fatal("accepted a stream differing from the exact retained summary")
			}
		})
	}
}

func TestVerifyCSVArtifactRequiresAvailableBoundedEvidence(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	for _, tc := range []struct {
		name    string
		summary Summary
		csv     []byte
		bundle  []byte
	}{
		{"missing CSV", summary, nil, bundle},
		{"missing bundle", summary, csv, nil},
		{"incomplete summary", func() Summary { s := summary; s.State = "needs_review"; return s }(), csv, bundle},
		{"missing bundle digest", func() Summary { s := summary; s.BundleSHA256 = ""; return s }(), csv, bundle},
		{"oversized bundle claim", func() Summary { s := summary; s.BundleBytes = maxBundleBytes + 1; return s }(), csv, bundle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := NewBudget(32 << 20)
			defer func() { _ = budget.Close() }()
			if err := VerifyCSVArtifact(t.Context(), budget, tc.summary, bytes.NewReader(tc.csv), bytes.NewReader(tc.bundle)); err == nil {
				t.Fatal("accepted unavailable artifact evidence")
			}
		})
	}
	budget := NewBudget(int64(len(bundle) - 1))
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(t.Context(), budget, summary, bytes.NewReader(csv), bytes.NewReader(bundle)); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("unbounded packet processing: %v", err)
	}
}

func TestVerifyCSVArtifactHonorsCancellation(t *testing.T) {
	summary, csv, bundle := csvArtifactProofFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := NewBudget(32 << 20)
	defer func() { _ = budget.Close() }()
	if err := VerifyCSVArtifact(ctx, budget, summary, bytes.NewReader(csv), bytes.NewReader(bundle)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification continued: %v", err)
	}
}
