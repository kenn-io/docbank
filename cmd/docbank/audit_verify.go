package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var auditVerifyJSON bool

var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Replay audit authority and verify every protected blob",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.VerifyAudit(cmd.Context())
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
		if problems != 0 {
			return fmt.Errorf("audit verification found %d problem(s)", problems)
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
	if _, err := fmt.Fprintf(w, "%d of %d protected blob(s) verified, %d unique byte(s)\n",
		report.VerifiedBlobs, report.ProtectedBlobs, report.ProtectedBytes); err != nil {
		return fmt.Errorf("writing audit verification: %w", err)
	}
	return nil
}

func init() {
	auditVerifyCmd.Flags().BoolVar(&auditVerifyJSON, "json", false, "machine-readable output")
	auditCmd.AddCommand(auditVerifyCmd)
}
