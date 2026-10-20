package main

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
)

func newProcessingVisionFingerprintsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "vision-fingerprints",
		Short:   "Compute photo description configuration fingerprints",
		Long:    "Print descriptor_fingerprint and disclosure_fingerprint as TOML assignments.\nUse the exact model, trust boundary, endpoint, and deployment from config.toml.\nThis command runs locally and requires no daemon or provider credential.",
		Example: `  docbank processing vision-fingerprints --model vision-model --trust-boundary operator_network --endpoint http://127.0.0.1:8080 --deployment-fingerprint aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`,
		Args:    cobra.NoArgs,
	}
	var model, trust, endpoint, deployment string
	cmd.Flags().StringVar(&model, "model", "", "provider model (required)")
	cmd.Flags().StringVar(&trust, "trust-boundary", "", "operator_network or hosted_provider (required)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "provider root origin (required)")
	cmd.Flags().StringVar(&deployment, "deployment-fingerprint", "", "deployment SHA-256 from configuration (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if !canonicalSHA256(deployment) {
			return usageError(errors.New("--deployment-fingerprint must be lowercase SHA-256"))
		}
		provider, err := vision.New(vision.Profile{
			Origin: endpoint, Model: model, TrustBoundary: document.RenditionTrustBoundary(trust),
			RequestTimeout: time.Minute, MaxResponseBytes: 1 << 20,
		}, nil, &http.Client{})
		if err != nil {
			return usageError(err)
		}
		descriptor := provider.Descriptor()
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "descriptor_fingerprint = %q\ndisclosure_fingerprint = %q\n", descriptor.Fingerprint, vision.DisclosureFingerprint(descriptor, endpoint, deployment))
		if err != nil {
			return fmt.Errorf("writing vision fingerprints: %w", err)
		}
		return nil
	}
	return cmd
}
