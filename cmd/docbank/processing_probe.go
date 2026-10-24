package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

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
			policy, client, output, err := newMistralProbeClient(cmd.Context(), cfg, profile)
			if err != nil {
				return err
			}
			return runMistralProbe(cmd.Context(), cmd.OutOrStdout(), client, policy, seeds, output, overwrite)
		}}
	cmd.Flags().StringVar(&profile, "profile", "", "named processing profile using Mistral OCR")
	cmd.Flags().StringVar(&seeds, "seeds", "", "directory containing synthetic doc, ppt, xls, numbers, and msg seeds")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing capability manifest")
	_ = cmd.MarkFlagRequired("profile")
	_ = cmd.MarkFlagRequired("seeds")
	return cmd
}

func newMistralProbeClient(ctx context.Context, cfg config.Config, profile string) (mistral.Policy, *mistral.Client, string, error) {
	selected, ok := cfg.ProcessingProfiles[profile]
	if !ok {
		return mistral.Policy{}, nil, "", fmt.Errorf("processing profile %q is not configured", profile)
	}
	configured, ok := cfg.RenditionProfiles[selected.Rendition]
	if !ok || configured.AdapterContract != config.MistralOCRAdapterContract || configured.Runtime == nil {
		return mistral.Policy{}, nil, "", errors.New("processing probe requires a Mistral OCR rendition runtime")
	}
	policy, err := configured.MistralPolicy()
	if err != nil {
		return mistral.Policy{}, nil, "", err
	}
	binding := strings.TrimPrefix(configured.CredentialBinding, "credential:")
	secrets, httpClient, err := renditionProviderClient(cfg, configured.Runtime.ProviderEgressConfig)
	if err != nil {
		return mistral.Policy{}, nil, "", err
	}
	key, err := secrets.ResolveSecret(ctx, binding)
	if err != nil {
		return mistral.Policy{}, nil, "", err
	}
	client, err := mistral.NewClient(policy, mistral.ClientConfig{APIKey: key, Timeout: configured.Runtime.RequestTimeout.Std(), HTTPClient: httpClient})
	return policy, client, configured.Runtime.CapabilityManifest, err
}

func runMistralProbe(ctx context.Context, out io.Writer, client *mistral.Client, policy mistral.Policy, seeds, output string, overwrite bool) (retErr error) {
	destination, err := prepareProbeDestination(output, overwrite)
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
	return nil
}

func prepareProbeDestination(raw string, overwrite bool) (string, error) {
	if !utf8.ValidString(raw) {
		return "", usageError(fmt.Errorf("capability manifest %s is not valid UTF-8", strconv.QuoteToASCII(raw)))
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolving capability manifest %q: %w", raw, err)
	}
	info, err := os.Lstat(abs)
	if err == nil {
		if info.IsDir() {
			return "", usageError(fmt.Errorf("capability manifest %s is a directory", strconv.Quote(abs)))
		}
		if !overwrite {
			return "", usageError(fmt.Errorf("capability manifest %s already exists; pass --overwrite to replace it", strconv.Quote(abs)))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking capability manifest %s: %w", strconv.Quote(abs), err)
	}
	parent, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("checking capability manifest directory: %w", err)
	}
	if !parent.IsDir() {
		return "", usageError(fmt.Errorf("capability manifest parent is not a directory: %s", strconv.Quote(filepath.Dir(abs))))
	}
	return abs, nil
}
