package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/jsontext"
)

var auditVerifyJSON bool
var auditVerifyExpected string

const maxAuditEvidenceFileBytes = 1 << 20

var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Replay audit authority and verify every protected blob",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var expected *api.AuditEvidence
		if auditVerifyExpected != "" {
			var err error
			expected, err = readExpectedAuditEvidence(auditVerifyExpected)
			if err != nil {
				return err
			}
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.VerifyAudit(cmd.Context(), expected)
		if err != nil {
			return err
		}
		if auditVerifyJSON {
			err = writeAuditJSON(cmd.OutOrStdout(), report)
		} else {
			err = writeAuditVerification(cmd.OutOrStdout(), report)
		}
		if err != nil {
			return err
		}
		problems := len(report.MetadataProblems) + len(report.Problems)
		if report.EvidenceCheck != nil {
			problems += len(report.EvidenceCheck.Problems)
		}
		if problems != 0 {
			return integrityError(fmt.Errorf("audit verification found %d problem(s)", problems))
		}
		return nil
	},
}

func writeAuditVerification(w io.Writer, report api.AuditVerifyReport) error {
	for _, problem := range report.MetadataProblems {
		if _, err := fmt.Fprintln(w, "metadata: "+problem); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
	}
	for _, problem := range report.Problems {
		if _, err := fmt.Fprintf(w, "%s: %s\n", problem.Problem, problem.Hash); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
	}
	if report.EvidenceCheck != nil {
		for _, problem := range report.EvidenceCheck.Problems {
			if _, err := fmt.Fprintf(w, "evidence %s: %s\n", problem.Code, problem.Message); err != nil {
				return fmt.Errorf("writing audit verification: %w", err)
			}
		}
	}
	if len(report.MetadataProblems) != 0 {
		return nil
	}
	if !report.Enabled {
		if _, err := fmt.Fprintln(w, "audit is not enabled; no audit authority to verify"); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
		return nil
	}
	if report.Evidence == nil {
		return errors.New("audit verification lacks terminal evidence")
	}
	evidence := report.Evidence
	lines := []string{
		"audit authority verified",
		"vault: " + evidence.VaultID,
		fmt.Sprintf("allocation lineage %s: entry %d, operation %d",
			evidence.LineageID, evidence.AllocationEntryCount,
			evidence.OperationSequenceHighWater),
		"allocation head: " + evidence.AllocationHead,
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
	}
	for _, scope := range evidence.Scopes {
		if _, err := fmt.Fprintf(w, "scope %s: entry %d, chain head %s\n",
			scope.ID, scope.EntryCount, scope.ChainHead); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
	}
	if report.EvidenceCheck != nil && report.EvidenceCheck.Extends {
		if _, err := fmt.Fprintln(w,
			"recorded evidence is an exact prefix of current authority"); err != nil {
			return fmt.Errorf("writing audit verification: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "%d of %d protected blob(s) verified, %d unique byte(s)\n",
		report.VerifiedBlobs, report.ProtectedBlobs, report.ProtectedBytes); err != nil {
		return fmt.Errorf("writing audit verification: %w", err)
	}
	return nil
}

func readExpectedAuditEvidence(path string) (*api.AuditEvidence, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening expected audit report: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxAuditEvidenceFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading expected audit report: %w", err)
	}
	if len(raw) > maxAuditEvidenceFileBytes {
		return nil, fmt.Errorf("expected audit report exceeds %d bytes", maxAuditEvidenceFileBytes)
	}
	if err := jsontext.Validate(raw, "expected audit report"); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var report api.AuditVerifyReport
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("decoding expected audit report: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decoding expected audit report: %w", err)
	}
	if !report.Enabled || report.Evidence == nil || len(report.MetadataProblems) != 0 ||
		len(report.Problems) != 0 || report.VerifiedBlobs != report.ProtectedBlobs ||
		report.ProtectedBlobs < 0 || report.ProtectedBytes < 0 ||
		(report.EvidenceCheck != nil &&
			(!report.EvidenceCheck.Extends || len(report.EvidenceCheck.Problems) != 0)) {
		return nil, errors.New("expected audit report was not a successful active verification")
	}
	if err := client.ValidateAuditEvidence(*report.Evidence); err != nil {
		return nil, fmt.Errorf("invalid evidence in expected audit report: %w", err)
	}
	return report.Evidence, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values are not allowed")
	}
	return err
}

func init() {
	auditVerifyCmd.Flags().BoolVar(&auditVerifyJSON, "json", false, "machine-readable output")
	auditVerifyCmd.Flags().StringVar(&auditVerifyExpected, "expected", "",
		"successful prior --json report to prove as an exact prefix")
	auditCmd.AddCommand(auditVerifyCmd)
}
