package main

import (
	"errors"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/vision"
	"go.kenn.io/docbank/internal/config"
)

func newProcessingVisionFingerprintsCommand() *cobra.Command {
	var model, boundary, endpoint, deployment string
	command := &cobra.Command{
		Use:     "vision-fingerprints",
		Short:   "Derive vision profile fingerprints for configuration",
		Example: `  docbank processing vision-fingerprints --model example-vision --boundary operator_network --endpoint http://127.0.0.1:8000 --deployment-fingerprint <deployment-sha256>`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			descriptor, err := vision.Descriptor(model, document.RenditionTrustBoundary(boundary))
			if err != nil {
				return usageError(err)
			}
			if err := config.ValidateRenditionEndpoint(endpoint, document.RenditionTrustBoundary(boundary)); err != nil {
				return usageError(err)
			}
			if !canonicalSHA256(deployment) {
				return usageError(errors.New("--deployment-fingerprint must be lowercase SHA-256"))
			}
			return writeCLIJSON(cmd.OutOrStdout(), map[string]string{
				"descriptor_fingerprint": descriptor.Fingerprint,
				"disclosure_fingerprint": vision.DisclosureFingerprint(descriptor, endpoint, deployment),
			})
		},
	}
	command.Flags().StringVar(&model, "model", "", "deployed model name")
	command.Flags().StringVar(&boundary, "boundary", "", "operator_network or hosted_provider")
	command.Flags().StringVar(&endpoint, "endpoint", "", "exact configured endpoint origin")
	command.Flags().StringVar(&deployment, "deployment-fingerprint", "", "configured deployment SHA-256")
	return command
}
