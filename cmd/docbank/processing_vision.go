package main

import (
	"errors"
	"fmt"

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
		descriptor, err := vision.Descriptor(model, document.RenditionTrustBoundary(trust))
		if err != nil {
			flag := "--model"
			if errors.Is(err, vision.ErrInvalidTrustBoundary) {
				flag = "--trust-boundary"
			}
			return usageError(fmt.Errorf("%s: %w", flag, err))
		}
		if _, err := vision.ValidateOrigin(endpoint, descriptor.TrustBoundary); err != nil {
			return usageError(fmt.Errorf("--endpoint: %w", err))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "descriptor_fingerprint = %q\ndisclosure_fingerprint = %q\n", descriptor.Fingerprint, vision.DisclosureFingerprint(descriptor, endpoint, deployment))
		if err != nil {
			return fmt.Errorf("writing vision fingerprints: %w", err)
		}
		return nil
	}
	return cmd
}
