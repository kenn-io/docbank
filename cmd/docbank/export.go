package main

import (
	"errors"
	"fmt"
	"io"

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
	Use:   "export",
	Short: "Export originals or rendered photos as verified ZIP bundles",
}

var exportPreviewCmd = &cobra.Command{
	Use:   "preview --request selection.json",
	Short: "Review up to 1,000 original versions or 16 rendered photos",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		request, err := readExportRequest(exportRequestPath)
		if err != nil {
			return err
		}
		kind := "explicit"
		if request.Photos != nil {
			kind = "photos"
		}
		role := "original"
		if request.PhotoRender != nil {
			role = "photo_rendered"
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		source, err := connection.API().CreateExportSource(cmd.Context(),
			&apiclient.CreateExportSourceRequestOptions{Body: &bundle.SourceRequest{
				OperationID: request.SourceOperationID, Kind: kind, Members: request.Members, Photos: request.Photos,
			}})
		if err != nil {
			return err
		}
		plan, err := connection.API().CreateExportPlan(cmd.Context(),
			&apiclient.CreateExportPlanRequestOptions{Body: &bundle.PlanRequest{
				OperationID: request.PlanOperationID, SourceID: source.ID,
				MemberHash: source.MemberHash, Roles: []bundle.RolePolicy{{Role: role}}, PhotoRender: request.PhotoRender,
			}})
		if err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), plan)
		}
		return writeExportPreview(cmd.OutOrStdout(), *plan)
	},
}

func writeExportPreview(w io.Writer, plan bundle.Plan) error {
	note := "Original contents are included without redaction or sanitization."
	byteLabel, memberLabel := "original bytes", "document versions"
	if p := plan.PhotoRender; p != nil {
		byteLabel, memberLabel = "output bytes", "photos"
		quality := ""
		if p.Format == "jpeg" {
			quality = fmt.Sprintf(", quality %d", p.Quality)
		}
		note = fmt.Sprintf("photo format %s%s, long edge %d pixels (0 keeps original size), metadata %t, remove GPS %t\nembedded RAW previews %d", p.Format, quality, p.LongEdge, p.IncludeMetadata, p.RemoveGPS, plan.EmbeddedPreviews)
	}
	_, err := fmt.Fprintf(w, "plan %s\nsource %s\nmember hash %s\nfingerprint %s\nplanned contents: %d %s, %d %s, %d metadata bytes\nadmission expires %s\n%s\n", plan.ID, plan.Source.ID, plan.Source.MemberHash, plan.Fingerprint, plan.Total, memberLabel, plan.RoleBytes, byteLabel, plan.MetadataBytes, plan.ExpiresAt, note)
	if err != nil {
		return fmt.Errorf("writing export preview: %w", err)
	}
	return nil
}

var exportStartCmd = &cobra.Command{
	Use:   "start <plan-id> --fingerprint <sha256> --operation-id <job-id>",
	Short: "Start an export without waiting for completion",
	Args:  cobra.ExactArgs(1),
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
	Use:   "status <job-id>",
	Short: "Read export progress once",
	Args:  cobra.ExactArgs(1),
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
	Use:   "cancel <job-id>",
	Short: "Request cancellation of an active export",
	Args:  cobra.ExactArgs(1),
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
	Use: "release <job-id>",
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
	exportCmd.PersistentFlags().BoolVar(&exportJSON, "json", false, "Write the result as JSON")
	exportPreviewCmd.Flags().StringVar(
		&exportRequestPath, "request", "",
		"Original-version or photo-scope request JSON file (at most 1 MiB)",
	)
	exportStartCmd.Flags().StringVar(
		&exportFingerprint, "fingerprint", "", "Reviewed plan fingerprint",
	)
	exportStartCmd.Flags().StringVar(
		&exportOperationID, "operation-id", "", "Caller-chosen job UUIDv4; retain it for retries",
	)
	exportDownloadCmd.Flags().BoolVar(
		&exportOverwrite, "overwrite", false, "Replace an existing destination after verification",
	)
	exportCmd.AddCommand(exportPreviewCmd, exportStartCmd, exportStatusCmd,
		exportCancelCmd, exportDownloadCmd, exportReleaseCmd,
		newExportShowPlanCommand(), newExportProblemsCommand())
	rootCmd.AddCommand(exportCmd)
}
