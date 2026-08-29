package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var (
	processingProfilesJSON     bool
	processingPlanProfile      string
	processingPlanJSON         bool
	processingBuildProfile     string
	processingBuildFingerprint string
	processingBuildConsent     bool
	processingBuildJSON        bool
	processingBuildNDJSON      bool
	processingStatusJSON       bool
)

var processingCmd = &cobra.Command{
	Use:   "processing",
	Short: "Preview and run document processing",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var processingProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List processing profiles this daemon can execute",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runProcessingProfiles(cmd, c, processingProfilesJSON)
	},
}

var processingPlanCmd = &cobra.Command{
	Use:   "plan <path-or-id>",
	Short: "Preview provider disclosure for one exact document version",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if processingPlanProfile == "" {
			return usageError(errors.New("--profile is required"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runProcessingPlan(cmd, c, args[0], processingPlanProfile, processingPlanJSON)
	},
}

var processingBuildCmd = &cobra.Command{
	Use:   "build <path-or-id>",
	Short: "Run one exact reviewed processing plan",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if processingBuildProfile == "" {
			return usageError(errors.New("--profile is required"))
		}
		if err := validateProcessingBuild(processingBuildFingerprint, processingBuildConsent); err != nil {
			return err
		}
		if processingBuildJSON && processingBuildNDJSON {
			return usageError(errors.New("--json and --ndjson are mutually exclusive"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runProcessingBuild(cmd, c, args[0], processingBuildProfile,
			processingBuildFingerprint, processingBuildConsent, processingBuildJSON, processingBuildNDJSON)
	},
}

var processingStatusCmd = &cobra.Command{
	Use:   "status <job-id>",
	Short: "Show aggregate processing status",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !canonicalSHA256(args[0]) {
			return usageError(errors.New("job ID must be lowercase SHA-256"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return runProcessingStatus(cmd, c, args[0], processingStatusJSON)
	},
}

func runProcessingProfiles(cmd *cobra.Command, c *client.Client, jsonOutput bool) error {
	profiles, err := c.ProcessingProfiles(cmd.Context())
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), profiles)
	}
	if len(profiles) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "no executable processing profiles")
		if err != nil {
			return fmt.Errorf("writing empty processing profile list: %w", err)
		}
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PROFILE\tRENDITION\tEMBEDDINGS\tFINGERPRINT")
	for _, profile := range profiles {
		rendition := "-"
		if profile.Rendition {
			rendition = "rendition"
		}
		embeddings := "-"
		if len(profile.EmbeddingBindings) > 0 {
			embeddings = strings.Join(profile.EmbeddingBindings, ",")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			profile.Name, rendition, embeddings, profile.Fingerprint)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing processing profiles: %w", err)
	}
	return nil
}

func runProcessingPlan(cmd *cobra.Command, c *client.Client, rawSelector, profile string, jsonOutput bool) error {
	selector, err := resolveProcessingSelector(cmd, c, rawSelector, profile)
	if err != nil {
		return err
	}
	plan, err := c.PlanProcessing(cmd.Context(), api.ProcessingPlanRequest{Selector: selector})
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), plan)
	}
	return writeProcessingPlan(cmd, plan)
}

func writeProcessingPlan(cmd *cobra.Command, plan api.ProcessingPlan) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "plan:\t%s\n", plan.Fingerprint)
	_, _ = fmt.Fprintf(w, "vault:\t%s\n", plan.VaultUID)
	_, _ = fmt.Fprintf(w, "document:\t%s version %s\n",
		formatNodeSelector(plan.Selector.NodeID), plan.Selector.ContentVersionID)
	_, _ = fmt.Fprintf(w, "profile:\t%s (%s)\n", plan.Selector.Profile, plan.ProfileFingerprint)
	_, _ = fmt.Fprintf(w, "discloses:\t%s\n", displayList(plan.DisclosedClasses))
	_, _ = fmt.Fprintf(w, "retains:\t%s\n", displayList(plan.RetainedClasses))
	_, _ = fmt.Fprintf(w, "estimate:\t%d provider call(s), %d vector space(s), %d source byte(s)\n",
		plan.Estimate.ProviderCalls, plan.Estimate.VectorSpaces, plan.Estimate.SourceBytes)
	_, _ = fmt.Fprintf(w, "consent required:\t%t\n", plan.ConsentRequired)
	_, _ = fmt.Fprintf(w, "backup:\t%s\n", plan.BackupConsequence)
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing processing plan: %w", err)
	}
	if len(plan.Flow) == 0 {
		return nil
	}
	flow := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(flow, "CAPABILITY\tPROVIDER\tBOUNDARY\tINPUTS")
	for _, hop := range plan.Flow {
		_, _ = fmt.Fprintf(flow, "%s\t%s\t%s\t%s\n", hop.Capability, hop.ProviderID,
			hop.TrustBoundary, displayList(hop.InputClasses))
		runtime := hop.RuntimeDisclosure
		_, _ = fmt.Fprintf(flow, "  processors\t%s -> %s\n", runtime.ImmediateProcessor, runtime.UltimateProcessor)
		_, _ = fmt.Fprintf(flow, "  endpoint\t%s\n", runtime.Endpoint)
		_, _ = fmt.Fprintf(flow, "  deployment\t%s\n", runtime.Deployment)
		if runtime.Model != "" || runtime.ModelRevision != "" {
			_, _ = fmt.Fprintf(flow, "  model\t%s\n", processingModelIdentity(runtime.Model, runtime.ModelRevision))
		}
		if runtime.VectorSpace != "" {
			_, _ = fmt.Fprintf(flow, "  vector space\t%s\n", runtime.VectorSpace)
		}
		_, _ = fmt.Fprintf(flow, "  provider metadata\t%s\n", displayList(runtime.MetadataClasses))
		_, _ = fmt.Fprintf(flow, "  retained artifacts\t%s\n", displayList(runtime.RetainedArtifactRoles))
	}
	if err := flow.Flush(); err != nil {
		return fmt.Errorf("writing processing flow: %w", err)
	}
	return nil
}

func processingModelIdentity(model, revision string) string {
	if model == "" {
		return revision
	}
	if revision == "" {
		return model
	}
	return model + "@" + revision
}

func runProcessingBuild(cmd *cobra.Command, c *client.Client, rawSelector, profile, fingerprint string,
	consent, jsonOutput, ndjsonOutput bool,
) error {
	if err := validateProcessingBuild(fingerprint, consent); err != nil {
		return err
	}
	selector, err := resolveProcessingSelector(cmd, c, rawSelector, profile)
	if err != nil {
		return err
	}
	stream, err := c.StartProcessingStream(cmd.Context(), api.StartProcessingRequest{
		Selector: selector, PlanFingerprint: fingerprint, Consent: true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	jobEvent, err := stream.Next()
	if err != nil {
		return err
	}
	job := *jobEvent.Job
	if ndjsonOutput {
		if err := writeCLIJSON(cmd.OutOrStdout(), jobEvent); err != nil {
			return err
		}
		if err := flushProcessingOutput(cmd.OutOrStdout()); err != nil {
			return err
		}
	} else if !jsonOutput {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "processing job: %s\n", job.ID)
		if err := flushProcessingOutput(cmd.OutOrStdout()); err != nil {
			return err
		}
	}
	statusEvent, err := stream.Next()
	if err != nil {
		return err
	}
	status := *statusEvent.Status
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), job)
	}
	if ndjsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), statusEvent)
	}
	return writeProcessingStatus(cmd, status)
}

func flushProcessingOutput(writer io.Writer) error {
	if flusher, ok := writer.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			return fmt.Errorf("flushing processing progress: %w", err)
		}
	}
	if flusher, ok := writer.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func validateProcessingBuild(fingerprint string, consent bool) error {
	if !canonicalSHA256(fingerprint) {
		return usageError(errors.New("--plan-fingerprint must be the exact lowercase SHA-256 plan fingerprint"))
	}
	if !consent {
		return usageError(errors.New("--consent is required to run the reviewed plan"))
	}
	return nil
}

func runProcessingStatus(cmd *cobra.Command, c *client.Client, jobID string, jsonOutput bool) error {
	status, err := c.ProcessingStatus(cmd.Context(), jobID)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeCLIJSON(cmd.OutOrStdout(), status)
	}
	return writeProcessingStatus(cmd, status)
}

func writeProcessingStatus(cmd *cobra.Command, status api.ProcessingStatus) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "job:\t%s\n", status.JobID)
	_, _ = fmt.Fprintf(w, "state:\t%s\n", status.State)
	_, _ = fmt.Fprintf(w, "phase:\t%s\n", status.Phase)
	_, _ = fmt.Fprintf(w, "embeddings:\t%d/%d complete\n",
		status.CompletedBindings, len(status.EmbeddingJobIDs))
	if status.FailureCode != "" {
		_, _ = fmt.Fprintf(w, "failure:\t%s\n", status.FailureCode)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("writing processing status: %w", err)
	}
	return nil
}

func resolveProcessingSelector(cmd *cobra.Command, c *client.Client, rawSelector, profile string) (api.ProcessingSelector, error) {
	selector, err := parseNodeSelector(rawSelector)
	if err != nil {
		return api.ProcessingSelector{}, err
	}
	node, err := selector.resolve(cmd.Context(), c)
	if err != nil {
		return api.ProcessingSelector{}, err
	}
	if node.Kind != "file" {
		return api.ProcessingSelector{}, fmt.Errorf("processing %q: %w", rawSelector, store.ErrNotFile)
	}
	if node.CurrentVersionID == "" {
		return api.ProcessingSelector{}, errors.New("document has no current content version")
	}
	return api.ProcessingSelector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: profile}, nil
}

func displayList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func canonicalSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func init() {
	processingProfilesCmd.Flags().BoolVar(&processingProfilesJSON, "json", false, "emit machine-readable JSON")
	processingPlanCmd.Flags().StringVar(&processingPlanProfile, "profile", "", "named executable processing profile")
	processingPlanCmd.Flags().BoolVar(&processingPlanJSON, "json", false, "emit machine-readable JSON")
	processingBuildCmd.Flags().StringVar(&processingBuildProfile, "profile", "", "named executable processing profile")
	processingBuildCmd.Flags().StringVar(&processingBuildFingerprint, "plan-fingerprint", "", "exact reviewed plan fingerprint")
	processingBuildCmd.Flags().BoolVar(&processingBuildConsent, "consent", false, "consent to the exact reviewed provider flow")
	processingBuildCmd.Flags().BoolVar(&processingBuildJSON, "json", false, "emit machine-readable JSON")
	processingBuildCmd.Flags().BoolVar(&processingBuildNDJSON, "ndjson", false,
		"emit one job record followed by one terminal status record")
	processingStatusCmd.Flags().BoolVar(&processingStatusJSON, "json", false, "emit machine-readable JSON")
	processingCmd.AddCommand(processingProfilesCmd, processingPlanCmd, processingBuildCmd, processingStatusCmd)
	rootCmd.AddCommand(processingCmd)
}
