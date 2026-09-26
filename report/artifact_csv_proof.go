package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// VerifyCSVArtifact compares a standalone CSV with an independently verified
// companion packet. The caller must obtain both streams for the same retained
// report ID and reauthorize the owner and source before each read. A v1 packet
// does not itself prove source completeness or ownership.
func VerifyCSVArtifact(ctx context.Context, budget Budget, summary Summary, csv, bundle io.Reader) error {
	if budget == nil || csv == nil || bundle == nil || summary.State != "complete" ||
		summary.CSVBytes < 1 || summary.CSVBytes > maxBundleBytes ||
		summary.BundleBytes < 1 || summary.BundleBytes > maxBundleBytes ||
		!validSHA256(summary.CSVSHA256) || !validSHA256(summary.BundleSHA256) {
		return ErrInvalidPacket
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scratch := budget.Child()
	defer func() { _ = scratch.Close() }()
	if _, err := scratch.Reserve(ctx, summary.CSVBytes); err != nil {
		return err
	}
	csvBytes := make([]byte, summary.CSVBytes)
	if _, err := io.ReadFull(csv, csvBytes); err != nil {
		return fmt.Errorf("%w: short CSV: %w", ErrInvalidPacket, err)
	}
	if err := requireReportEOF(ctx, csv); err != nil {
		return err
	}
	csvHash := sha256.Sum256(csvBytes)
	if hex.EncodeToString(csvHash[:]) != summary.CSVSHA256 {
		return ErrInvalidPacket
	}
	bundleHash := sha256.New()
	packetCSV, err := ExtractVerifiedCSV(ctx, scratch, io.TeeReader(bundle, bundleHash), summary.BundleBytes)
	if err != nil {
		return err
	}
	if err := requireReportEOF(ctx, bundle); err != nil {
		return err
	}
	if hex.EncodeToString(bundleHash.Sum(nil)) != summary.BundleSHA256 || !bytes.Equal(csvBytes, packetCSV) {
		return ErrInvalidPacket
	}
	return nil
}

func requireReportEOF(ctx context.Context, input io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var extra [1]byte
	n, err := io.ReadFull(input, extra[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		return ErrInvalidPacket
	}
	return nil
}
