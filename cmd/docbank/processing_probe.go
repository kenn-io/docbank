package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/kit/safefileio"
)

func newProcessingProbeCommand() *cobra.Command {
	var profile, seeds string
	var overwrite bool
	cmd := &cobra.Command{Use: "probe", Short: "Probe Mistral OCR and save its capability manifest", Args: cobra.NoArgs,
		Example: "  docbank processing probe --profile ocr --seeds ./mistral-seeds",
		RunE: func(cmd *cobra.Command, _ []string) error {
			layout, err := home.Resolve()
			if err != nil {
				return err
			}
			cfg, err := config.Load(layout.Root)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			// An interrupt cancels the probe so its temporary files are removed.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			policy, client, configured, err := newMistralProbeClient(ctx, cfg, profile)
			if err != nil {
				return err
			}
			return runMistralProbe(ctx, cmd.OutOrStdout(), client, policy, configured, seeds, overwrite)
		}}
	cmd.Flags().StringVar(&profile, "profile", "", "named processing profile using Mistral OCR")
	cmd.Flags().StringVar(&seeds, "seeds", "", "directory containing synthetic doc, ppt, xls, numbers, and msg seeds")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing capability manifest")
	_ = cmd.MarkFlagRequired("profile")
	_ = cmd.MarkFlagRequired("seeds")
	return cmd
}

func newMistralProbeClient(ctx context.Context, cfg config.Config, profile string) (mistral.Policy, *mistral.Client, config.RenditionProfileConfig, error) {
	selected, ok := cfg.ProcessingProfiles[profile]
	if !ok {
		return mistral.Policy{}, nil, config.RenditionProfileConfig{}, fmt.Errorf("processing profile %q is not configured", profile)
	}
	configured, ok := cfg.RenditionProfiles[selected.Rendition]
	if !ok || configured.AdapterContract != config.MistralOCRAdapterContract || configured.Runtime == nil {
		return mistral.Policy{}, nil, config.RenditionProfileConfig{}, errors.New("processing probe requires a Mistral OCR rendition runtime")
	}
	policy, err := configured.MistralPolicy()
	if err != nil {
		return mistral.Policy{}, nil, config.RenditionProfileConfig{}, err
	}
	binding := strings.TrimPrefix(configured.CredentialBinding, "credential:")
	secrets, httpClient, err := renditionProviderClient(cfg, configured.Runtime.ProviderEgressConfig)
	if err != nil {
		return mistral.Policy{}, nil, config.RenditionProfileConfig{}, err
	}
	key, err := secrets.ResolveSecret(ctx, binding)
	if err != nil {
		return mistral.Policy{}, nil, config.RenditionProfileConfig{}, err
	}
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: key, Timeout: configured.Runtime.RequestTimeout.Std(), HTTPClient: httpClient})
	return policy, client, configured, err
}

func runMistralProbe(ctx context.Context, out io.Writer, client *mistral.Client, policy mistral.Policy,
	configured config.RenditionProfileConfig, seeds string, overwrite bool,
) (retErr error) {
	destination, err := prepareOutputDestination("capability manifest", configured.Runtime.CapabilityManifest, overwrite)
	if err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "docbank-mistral-probe-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(directory)) }()
	if err := safefileio.EnsurePrivateDir(directory); err != nil {
		return fmt.Errorf("prepare probe directory: %w", err)
	}
	fixtures, spool := filepath.Join(directory, "fixtures"), filepath.Join(directory, "spool")
	if err := mistral.WriteProbeFixtures(ctx, fixtures, mistral.FixtureOptions{SeedDirectory: seeds}); err != nil {
		return err
	}
	if err := safefileio.EnsurePrivateDir(spool); err != nil {
		return fmt.Errorf("prepare probe spool: %w", err)
	}
	if _, err := fmt.Fprintf(out, "Probing %d Mistral OCR formats; this can take several minutes\n", len(mistral.CandidateFormats())); err != nil {
		return fmt.Errorf("print probe progress: %w", err)
	}
	manifest, err := mistral.RunCapabilityProbe(ctx, client, mistral.ProbeConfig{Fixtures: mistral.ProbeFixtureConfig{
		FixtureDirectory: fixtures, SpoolDirectory: spool, MaxSpoolBytes: int64(len(mistral.CandidateFormats())) * policy.Values().MaxDocumentBytes, MinFreeBytes: 1,
	}})
	if err != nil {
		return err
	}
	for _, result := range manifest.Results {
		if _, err := fmt.Fprintf(out, "%s %s %s\n", result.FormatID, result.Status, result.ReasonCode); err != nil {
			return fmt.Errorf("print probe result: %w", err)
		}
	}
	if _, err := policy.Authorize(manifest, "pdf"); err != nil {
		return fmt.Errorf("probe manifest cannot authorize daemon PDF processing: %w", err)
	}
	descriptor, err := mistral.DocumentDescriptor(policy, manifest)
	if err != nil {
		return fmt.Errorf("probe manifest cannot describe daemon PDF processing: %w", err)
	}
	disclosure := mistral.DisclosureFingerprint(descriptor, configured.Runtime.Endpoint, configured.DeploymentFingerprint)
	stage, err := filepublish.CreateStage(filepath.Dir(destination), "docbank-mistral-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.Cleanup()) }()
	if err := mistral.EncodeCapabilityManifest(stage.File, manifest); err != nil {
		return err
	}
	if err := errors.Join(stage.File.Sync(), stage.File.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := filepublish.Publish(stage.Path(), destination, overwrite); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Capability manifest saved to %s\n", destination); err != nil {
		return fmt.Errorf("print saved manifest path: %w", err)
	}
	next := "These fingerprints already match the rendition profile."
	if descriptor.Fingerprint != configured.DescriptorFingerprint || disclosure != configured.DisclosureFingerprint {
		next = "Set these fingerprints in the rendition profile, then restart the daemon."
	}
	if _, err := fmt.Fprintf(out, "descriptor_fingerprint = %q\ndisclosure_fingerprint = %q\n%s\n",
		descriptor.Fingerprint, disclosure, next); err != nil {
		return fmt.Errorf("print manifest fingerprints: %w", err)
	}
	return nil
}
