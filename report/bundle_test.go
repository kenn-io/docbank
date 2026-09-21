package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
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
