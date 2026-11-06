package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/workerprotocol"
)

func TestWorkerRuntimeRequiresBoundedOperatorRoute(t *testing.T) {
	for _, adapter := range []string{workerprotocol.DocumentAdapter, workerprotocol.ASRAdapter} {
		t.Run(adapter, func(t *testing.T) {
			valid := func() Config {
				cfg := validRenditionRuntimeConfig(t)
				p := cfg.RenditionProfiles["primary"]
				p.AdapterContract = adapter
				p.TrustBoundary = "operator_network"
				p.Runtime.AllowedCIDRs = []string{"127.0.0.0/8"}
				p.MaxDocumentBytes = 64 << 20
				p.MaxResponseBytes = 64 << 20
				p.MaxUnits = 200
				if adapter == workerprotocol.DocumentAdapter {
					p.MaxTranscriptChars = 0
					p.RequestedArtifacts = []string{"provider_markdown", "structured_evidence"}
				}
				cfg.RenditionProfiles["primary"] = p
				return cfg
			}
			require.NoError(t, valid().Validate())
			for _, change := range []string{"hosted", "source", "result", "pages"} {
				if change == "pages" && adapter == workerprotocol.ASRAdapter {
					continue
				}
				t.Run(change, func(t *testing.T) {
					cfg := valid()
					p := cfg.RenditionProfiles["primary"]
					switch change {
					case "hosted":
						p.TrustBoundary = "hosted_provider"
					case "source":
						p.MaxDocumentBytes = 100<<20 + 1
					case "result":
						p.MaxResponseBytes = 128<<20 + 1
					case "pages":
						p.MaxUnits = 201
					}
					cfg.RenditionProfiles["primary"] = p
					require.Error(t, cfg.Validate())
				})
			}
		})
	}
}
