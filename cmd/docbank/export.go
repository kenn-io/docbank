package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	exportJSON        bool
	exportRequestPath string
	exportFingerprint string
	exportOperationID string
	exportOverwrite   bool
)

var exportCmd = &cobra.Command{
	Long: `Export exact original versions in a verified ZIP. Get version identities from
stat --json. Save sel.json with this shape (use your own UUIDv4s and receipt):
{"source_operation_id":"<uuid>","plan_operation_id":"<uuid>","members":[
  {"node_id":12,"version_id":"<version-uuid>","sha256":"<sha256>","size":0}]}
Typical flow: preview --request sel.json (plan ID and fingerprint) ->
start <plan-id> --fingerprint F --operation-id <new-uuidv4> -> status <job-id>
-> download <job-id> out.zip -> release <job-id>.
Output: --json prints plans, jobs and verified download receipts.`,
	GroupID: groupProductions,
	Use:     "export",
	Short:   "Export exact original versions as verified ZIP bundles",
}

var exportPreviewCmd = &cobra.Command{
	Example: `  docbank export preview --request sel.json --json`,
	Use:     "preview --request selection.json",
	Short:   "Freeze and review up to 1,000 exact original versions",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		request, err := readExportRequest(exportRequestPath)
		if err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		source, err := connection.API().CreateExportSource(cmd.Context(),
			&apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{
				OperationID: request.SourceOperationID, Kind: "explicit", Members: request.Members,
			}})
		if err != nil {
			return err
		}
		plan, err := connection.API().CreateExportPlan(cmd.Context(),
			&apiclient.CreateExportPlanRequestOptions{Body: &bundle.PlanRequest{
				OperationID: request.PlanOperationID, SourceID: source.ID,
				MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: "original"}},
			}})
		if err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), plan)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"plan %s\nsource %s\nmember hash %s\nfingerprint %s\n"+
				"planned contents: %d document versions, %d original bytes, %d metadata bytes\n"+
				"admission expires %s\n"+
				"Original contents are included without redaction or sanitization.\n",
			plan.ID, plan.Source.ID, plan.Source.MemberHash, plan.Fingerprint,
			plan.Total, plan.RoleBytes, plan.MetadataBytes, plan.ExpiresAt)
		if err != nil {
			return fmt.Errorf("writing export preview: %w", err)
		}
		return nil
	},
}

var exportStartCmd = &cobra.Command{
	Example: `  docbank export start <plan-id> --fingerprint <sha256> --operation-id <new-uuidv4>`,
	Use:     "start <plan-id> --fingerprint <sha256> --operation-id <job-id>",
	Short:   "Start an export without waiting for completion",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("plan ID", args[0]); err != nil {
			return err
		}
		if err := validateExportID("--operation-id", exportOperationID); err != nil {
			return err
		}
		if !canonical.IsSHA256Hex(exportFingerprint) {
			return usageError(errors.New(
				"--fingerprint must be 64 lowercase hexadecimal characters"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		job, err := connection.API().CreateExportJob(cmd.Context(),
			&apiclient.CreateExportJobRequestOptions{Body: &bundle.JobRequest{
				OperationID: exportOperationID, PlanID: args[0], Fingerprint: exportFingerprint,
			}})
		if err != nil {
			return err
		}
		return writeExportJob(cmd, *job)
	},
}

var exportStatusCmd = &cobra.Command{
	Example: `  docbank export status <job-id> --json`,
	Use:     "status <job-id>",
	Short:   "Read export progress once",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("job ID", args[0]); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		job, err := connection.API().GetExportJob(cmd.Context(),
			&apiclient.GetExportJobRequestOptions{
				PathParams: &apiclient.GetExportJobPath{ID: args[0]},
			})
		if err != nil {
			return err
		}
		return writeExportJob(cmd, *job)
	},
}

var exportCancelCmd = &cobra.Command{
	Example: `  docbank export cancel <job-id>`,
	Use:     "cancel <job-id>",
	Short:   "Request cancellation of an active export",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("job ID", args[0]); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		_, err = connection.API().CancelExportJob(cmd.Context(),
			&apiclient.CancelExportJobRequestOptions{
				PathParams: &apiclient.CancelExportJobPath{ID: args[0]},
				Body:       &apiclient.CancelExportJobBody{},
			})
		if err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), struct {
				JobID    string `json:"job_id"`
				Accepted bool   `json:"accepted"`
			}{args[0], true})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "cancellation request accepted for %s\n", args[0])
		if err != nil {
			return fmt.Errorf("writing export cancellation result: %w", err)
		}
		return nil
	},
}

var exportReleaseCmd = &cobra.Command{
	Example: `  docbank export release <job-id>`,
	Use:     "release <job-id>",
	Long: "Remove a finished job and its retained archive to free a slot.\n" +
		"If export_retained is returned, retry release after the download finishes.\n" +
		"Even download && release can need a retry; unused tickets expire after two minutes.",
	Short: "Remove a finished job and its retained archive to free a slot",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("job ID", args[0]); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if _, err = connection.API().ReleaseExportJob(cmd.Context(),
			&apiclient.ReleaseExportJobRequestOptions{
				PathParams: &apiclient.ReleaseExportJobPath{ID: args[0]},
			}); err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), struct {
				JobID    string `json:"job_id"`
				Released bool   `json:"released"`
			}{args[0], true})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"released export %s and its retained archive; the local download is unchanged\n",
			args[0])
		if err != nil {
			return fmt.Errorf("writing export release result: %w", err)
		}
		return nil
	},
}

func writeExportJob(cmd *cobra.Command, job bundle.Job) error {
	if exportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), job)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"job %s\nplan %s\nfingerprint %s\nstate %s\n"+
			"completed: %d roles, %d bytes\ndeadline %s\nretention expires %s\n",
		job.ID, job.PlanID, job.Fingerprint, job.State,
		job.CompletedRoles, job.CompletedBytes, job.Deadline, job.ExpiresAt); err != nil {
		return fmt.Errorf("writing export status: %w", err)
	}
	if job.Failure != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "failure: %s\n", job.Failure); err != nil {
			return fmt.Errorf("writing export status: %w", err)
		}
	}
	if job.Receipt != nil {
		_, err := fmt.Fprintf(cmd.OutOrStdout(),
			"verified archive: %d bytes, %d entries, SHA-256 %s\n",
			job.Receipt.Size, job.Receipt.Entries, job.Receipt.SHA256)
		if err != nil {
			return fmt.Errorf("writing export receipt: %w", err)
		}
	}
	return nil
}

func init() {
	exportCmd.PersistentFlags().BoolVar(&exportJSON, "json", false, "print JSON to stdout")
	exportPreviewCmd.Flags().StringVar(
		&exportRequestPath, "request", "",
		"explicit document-version request JSON file (at most 1 MiB) (required)",
	)
	exportStartCmd.Flags().StringVar(
		&exportFingerprint, "fingerprint", "", "reviewed plan fingerprint (required)",
	)
	exportStartCmd.Flags().StringVar(
		&exportOperationID, "operation-id", "", "caller-chosen job UUIDv4; retain it for retries (required)",
	)
	exportDownloadCmd.Flags().BoolVar(
		&exportOverwrite, "overwrite", false, "replace an existing destination after verification",
	)
	exportCmd.AddCommand(exportPreviewCmd, exportStartCmd, exportStatusCmd,
		exportCancelCmd, exportDownloadCmd, exportReleaseCmd,
		newExportShowPlanCommand(), newExportProblemsCommand())
	rootCmd.AddCommand(exportCmd)
}
