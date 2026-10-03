package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

func newExportShowPlanCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "show-plan <plan-id>", Short: "Inspect a retained export plan",
		Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("plan ID", args[0]); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		plan, err := connection.API().GetExportPlan(cmd.Context(),
			&apiclient.GetExportPlanRequestOptions{PathParams: &apiclient.GetExportPlanPath{ID: args[0]}})
		if err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), plan)
		}
		return writeExportPlanHeader(cmd.OutOrStdout(), *plan)
	}
	return cmd
}

func newExportProblemsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "problems <plan-id>", Short: "Inspect one page of unavailable export outputs",
		Args: cobra.ExactArgs(1),
	}
	var after int64
	cmd.Flags().Int64Var(&after, "after", 0, "Number of problems to skip (0–2600000)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := validateExportID("plan ID", args[0]); err != nil {
			return err
		}
		if after < 0 || after > bundle.MaxOutputProblems {
			return usageError(errors.New("after must be 0–2600000"))
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := connection.API().GetExportOutputProblems(cmd.Context(),
			&apiclient.GetExportOutputProblemsRequestOptions{
				PathParams: &apiclient.GetExportOutputProblemsPath{ID: args[0]},
				Query:      &apiclient.GetExportOutputProblemsQuery{After: new(after)},
			})
		if err != nil {
			return err
		}
		if exportJSON {
			return writeCLIJSON(cmd.OutOrStdout(), page)
		}
		return writeExportProblems(cmd.OutOrStdout(), *page)
	}
	return cmd
}

func writeExportPlanHeader(output io.Writer, plan bundle.Plan) error {
	var text strings.Builder
	fmt.Fprintf(&text, "plan %s\nfingerprint: %s\nsource: %s · %s\nmember hash: %s\n",
		plan.ID, plan.Fingerprint, plan.Source.ID, plan.Source.Kind, plan.Source.MemberHash)
	fmt.Fprintf(&text, "selected members: %d\nrole entries: %d\nrole bytes: %d\nmetadata bytes: %d\n",
		plan.Total, plan.RoleEntries, plan.RoleBytes, plan.MetadataBytes)
	fmt.Fprintf(&text, "created: %s\nadmission deadline: %s\n", plan.CreatedAt, plan.ExpiresAt)
	for _, policy := range plan.Roles {
		fmt.Fprintf(&text, "role: %s · allow unavailable: %t", policy.Role, policy.AllowUnavailable)
		if policy.ProfileFingerprint != "" {
			fmt.Fprintf(&text, " · profile: %s", policy.ProfileFingerprint)
		}
		if policy.RecipeSHA256 != "" {
			fmt.Fprintf(&text, " · recipe: %s", policy.RecipeSHA256)
		}
		text.WriteByte('\n')
	}
	if plan.DocumentRows != 0 {
		fmt.Fprintf(&text, "document rows: %d\n", plan.DocumentRows)
	}
	if plan.VolumeLimits != nil {
		fmt.Fprintf(&text, "volume limits: %d role bytes · %d roles\n",
			plan.VolumeLimits.RoleBytes, plan.VolumeLimits.Roles)
	}
	if plan.Volumes != 0 {
		fmt.Fprintf(&text, "volumes: %d\n", plan.Volumes)
	}
	if plan.DuplicatePolicy != "" {
		fmt.Fprintf(&text, "duplicate policy: %s\n", plan.DuplicatePolicy)
	}
	if counts := plan.Counts; counts != nil {
		fmt.Fprintf(&text, "messages: %d\nattachments: %d\nemail PDFs: %d\nattachment PDFs: %d\n"+
			"pages: %d\ncollapsed: %d\nunavailable: %d\nunavailable inventories: %d\n",
			counts.Messages, counts.Attachments, counts.EmailPDFs, counts.AttachmentPDFs,
			counts.Pages, counts.Collapsed, counts.Unavailable, counts.UnavailableInventories)
	}
	_, err := io.WriteString(output, text.String())
	return err
}

func writeExportProblems(output io.Writer, page bundle.OutputProblems) error {
	var text strings.Builder
	fmt.Fprintf(&text, "plan %s\nfingerprint: %s\n%d returned · %d problems total\n",
		page.PlanID, page.Fingerprint, len(page.Items), page.Total)
	for _, problem := range page.Items {
		fmt.Fprintf(&text, "node %d · version %s · %s", problem.NodeID, problem.VersionID, problem.Role)
		if problem.PartPath != "" {
			fmt.Fprintf(&text, " · part %q", problem.PartPath)
		}
		fmt.Fprintf(&text, ": %q\n", problem.Reason)
	}
	if page.Total == 0 {
		text.WriteString("No unavailable outputs.\n")
	} else if len(page.Items) == 0 {
		text.WriteString("No more unavailable outputs.\n")
	}
	if page.Next != 0 {
		fmt.Fprintf(&text, "Next page: docbank export problems %s --after %d\n", page.PlanID, page.Next)
	}
	_, err := io.WriteString(output, text.String())
	return err
}
