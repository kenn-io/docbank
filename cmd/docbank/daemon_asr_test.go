package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/docling"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

const (
	daemonASRAPIKey      = "synthetic-daemon-asr-key"
	daemonASRProviderKey = "synthetic-provider-secret"
)

func TestDaemonDoclingASRMedia(t *testing.T) {
	for _, test := range []struct {
		name, filename, mediaType, extension string
		content                              []byte
	}{
		{name: "WAV", filename: "recording.wav", mediaType: "audio/wav", extension: ".wav", content: mediatest.WAV()},
		{name: "MP3", filename: "recording.mp3", mediaType: "audio/mpeg", extension: ".mp3", content: mediatest.MP3()},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := newDaemonDoclingServer(t)
			provider.resultGate = make(chan struct{})
			_, daemon, _ := startDaemonASRTest(t, provider, "synthetic-provider-secret", false)

			digest := sha256.Sum256(test.content)
			sha := hex.EncodeToString(digest[:])
			receipt, err := daemon.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
				OperationID: "00000000-0000-4000-8000-000000000601", Filename: test.filename,
				MediaType: test.mediaType, SHA256: sha, ByteLength: int64(len(test.content)),
				Occurrence: api.MediaOccurrenceBody{Ref: "call-" + test.name, Revision: "1", Filename: test.filename},
			}, bytes.NewReader(test.content))
			require.NoError(t, err)
			require.NotEmpty(t, receipt.SourceID)
			require.NotEmpty(t, receipt.ContentVersionID)

			node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/media/" + sha[:2] + "/" + sha + test.extension}})
			require.NoError(t, err)
			selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID, Profile: "asr"}
			plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
			require.NoError(t, err)
			require.Len(t, plan.Flow, 1)
			assert.Equal(t, provider.URL(), plan.Flow[0].RuntimeDisclosure.Endpoint)
			assert.Equal(t, "docbank-docling-asr/v1", plan.Flow[0].RuntimeDisclosure.ImmediateProcessor)
			assert.Equal(t, "docling.serve-v1", plan.Flow[0].RuntimeDisclosure.UltimateProcessor)
			_, err = daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
				Selector: selector, PlanFingerprint: plan.Fingerprint,
			}})
			require.NoError(t, err)

			queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
				OperationID: "00000000-0000-4000-8000-000000000602",
				Processing:  &api.MediaProcessingBody{Profile: "asr"},
			})
			require.NoError(t, err)
			require.Equal(t, "queued", queued.OperationState)
			select {
			case <-provider.resultStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("Docling result request did not start")
			}
			pending, err := daemon.MediaStatus(t.Context(), receipt.SourceID)
			require.NoError(t, err)
			assert.NotEqual(t, "succeeded", pending.OperationState)
			close(provider.resultGate)

			var completed api.MediaReceipt
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				var statusErr error
				completed, statusErr = daemon.MediaStatus(t.Context(), receipt.SourceID)
				require.NoError(collect, statusErr)
				require.Equal(collect, "succeeded", completed.OperationState, "%+v", completed)
				require.Equal(collect, "transcribed", completed.CoverageState)
			}, 20*time.Second, 20*time.Millisecond)
			require.Equal(t, receipt.ContentVersionID, completed.ContentVersionID)
			require.Equal(t, int64(1), provider.submissions.Load())
			require.Equal(t, test.content, provider.submissionSource())
			assert.Zero(t, provider.authFailures.Load())

			stream, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
			require.NoError(t, err)
			var rendition bytes.Buffer
			_, err = stream.CopyVerified(&rendition)
			require.NoError(t, err)
			require.NoError(t, stream.Close())
			require.Contains(t, rendition.String(), "Telescope delivery arrives Friday at 3")

			search, err := daemon.SearchDocuments(t.Context(), api.DocumentSearchRequest{
				Query: "Telescope delivery", Mode: "lexical", Profile: "asr", Limit: 10,
				Fence: api.DocumentSourceFence{VaultUID: plan.VaultUID, ContentVersionIDs: []string{receipt.ContentVersionID}},
			})
			require.NoError(t, err)
			require.Len(t, search.Results, 1)
			require.Equal(t, receipt.ContentVersionID, search.Results[0].ContentVersionID)
			require.NotEmpty(t, search.Results[0].Evidence)
			require.NotNil(t, search.Results[0].Evidence[0].TimeSpan)
		})
	}
}

func TestDaemonDoclingASRFailures(t *testing.T) {
	t.Run("boundary value 0.0 for poll interval is rejected", func(t *testing.T) {
		provider := newDaemonDoclingServer(t)
		cfg, _ := doclingASRProcessingConfig(t, provider.URL())
		profile := cfg.RenditionProfiles["asr"]
		profile.Runtime = cloneRenditionRuntime(profile.Runtime)
		profile.Runtime.PollInterval = 0
		cfg.RenditionProfiles["asr"] = profile
		require.ErrorContains(t, cfg.Validate(), "request and poll bounds")
	})

	t.Run("missing credential retains source and makes no request", func(t *testing.T) {
		provider := newDaemonDoclingServer(t)
		_, daemon, _ := startDaemonASRTest(t, provider, "", false)
		content := mediatest.WAV()
		digest := sha256.Sum256(content)
		sha := hex.EncodeToString(digest[:])
		receipt, err := daemon.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
			OperationID: "00000000-0000-4000-8000-000000000611", Filename: "missing.wav",
			MediaType: "audio/wav", SHA256: sha, ByteLength: int64(len(content)),
			Occurrence: api.MediaOccurrenceBody{Ref: "missing-key", Revision: "1", Filename: "missing.wav"},
		}, bytes.NewReader(content))
		require.NoError(t, err)
		_, err = daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/media/" + sha[:2] + "/" + sha + ".wav"}})
		require.NoError(t, err)

		node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/media/" + sha[:2] + "/" + sha + ".wav"}})
		require.NoError(t, err)
		selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID, Profile: "asr"}
		plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
		require.NoError(t, err)
		_, err = daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
			Selector: selector, PlanFingerprint: plan.Fingerprint,
		}})
		require.NoError(t, err)
		queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
			OperationID: "00000000-0000-4000-8000-000000000612", Processing: &api.MediaProcessingBody{Profile: "asr"},
		})
		require.NoError(t, err)
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			status, statusErr := daemon.MediaStatus(t.Context(), receipt.SourceID)
			require.NoError(collect, statusErr)
			require.Equal(collect, queued.OperationID, status.OperationID)
			require.Equal(collect, "failed", status.OperationState)
		}, 10*time.Second, 20*time.Millisecond)
		assert.Zero(t, provider.requests.Load())
	})

	t.Run("malformed provider schema fails the attempt", func(t *testing.T) {
		provider := newDaemonDoclingServer(t)
		provider.resultSchemaVersion = "1.11.0"
		_, daemon, _ := startDaemonASRTest(t, provider, "synthetic-provider-secret", false)
		receipt, selector, plan := daemonASRSourceAndPlan(t, daemon, "malformed.wav", mediatest.WAV())
		_, err := daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
			Selector: selector, PlanFingerprint: plan.Fingerprint,
		}})
		require.NoError(t, err)
		queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
			OperationID: "00000000-0000-4000-8000-000000000622", Processing: &api.MediaProcessingBody{Profile: "asr"},
		})
		require.NoError(t, err)
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			status, statusErr := daemon.MediaStatus(t.Context(), receipt.SourceID)
			require.NoError(collect, statusErr)
			require.Equal(collect, queued.OperationID, status.OperationID)
			require.Equal(collect, "failed", status.OperationState)
		}, 10*time.Second, 20*time.Millisecond)
	})

	t.Run("retry is durable after a transient poll error", func(t *testing.T) {
		provider := newDaemonDoclingServer(t)
		provider.transientPolls.Store(1)
		_, daemon, _ := startDaemonASRTest(t, provider, "synthetic-provider-secret", false)
		receipt, selector, plan := daemonASRSourceAndPlan(t, daemon, "retry.wav", mediatest.WAV())
		_, err := daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
			Selector: selector, PlanFingerprint: plan.Fingerprint,
		}})
		require.NoError(t, err)
		queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
			OperationID: "00000000-0000-4000-8000-000000000632", Processing: &api.MediaProcessingBody{Profile: "asr"},
		})
		require.NoError(t, err)
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			status, statusErr := daemon.MediaStatus(t.Context(), receipt.SourceID)
			require.NoError(collect, statusErr)
			require.Equal(collect, queued.OperationID, status.OperationID)
			require.Equal(collect, "succeeded", status.OperationState)
			require.Equal(collect, "transcribed", status.CoverageState)
		}, 10*time.Second, 20*time.Millisecond)
		require.GreaterOrEqual(t, provider.polls.Load(), int64(2))
	})

	t.Run("rejected consent and endpoint changes stop before provider work", func(t *testing.T) {
		provider := newDaemonDoclingServer(t)
		cfg, _ := doclingASRProcessingConfig(t, provider.URL())
		endpointProfile := cfg.RenditionProfiles["asr"]
		endpointProfile.Runtime = cloneRenditionRuntime(endpointProfile.Runtime)
		endpointProfile.Runtime.Endpoint = provider.URL() + "/v1"
		cfg.RenditionProfiles["asr"] = endpointProfile
		require.ErrorContains(t, cfg.Validate(), "absolute root origin")

		_, daemon, _ := startDaemonASRTest(t, provider, "synthetic-provider-secret", false)
		receipt, selector, _ := daemonASRSourceAndPlan(t, daemon, "consent.wav", mediatest.WAV())
		_, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
			OperationID: "00000000-0000-4000-8000-000000000642", Processing: &api.MediaProcessingBody{Profile: selector.Profile},
		})
		require.Error(t, err)
		assert.Zero(t, provider.requests.Load())
	})
}

func TestDaemonDoclingASRAdjacentProfiles(t *testing.T) {
	provider := newDaemonDoclingServer(t)
	_, daemon, _ := startDaemonASRTest(t, provider, "", true)
	profiles, err := daemon.API().ListDocumentProcessingProfiles(t.Context())
	require.NoError(t, err)
	var names []string
	for _, profile := range *profiles {
		names = append(names, profile.Name)
	}
	assert.Contains(t, names, "asr")
	assert.Contains(t, names, "private-text")
	assert.Contains(t, names, "supplied-transcript")
	assert.Contains(t, names, "supplied-captions")

	content := mediatest.WAV()
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	receipt, err := daemon.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000651", Filename: "adjacent.wav",
		MediaType: "audio/wav", SHA256: sha, ByteLength: int64(len(content)),
		Occurrence: api.MediaOccurrenceBody{Ref: "adjacent", Revision: "1", Filename: "adjacent.wav"},
	}, bytes.NewReader(content))
	require.NoError(t, err)
	transcript := []byte("supplied transcript remains exact\n")
	transcriptDigest := sha256.Sum256(transcript)
	transcriptSHA := hex.EncodeToString(transcriptDigest[:])
	artifact, err := daemon.ImportMediaArtifact(t.Context(), receipt.SourceID, api.MediaArtifactMetadata{
		OperationID: "00000000-0000-4000-8000-000000000652", OccurrenceID: receipt.OccurrenceID,
		Kind: "transcript", Origin: "supplied", Filename: "transcript.txt", MediaType: "text/plain",
		SHA256: transcriptSHA, ByteLength: int64(len(transcript)),
	}, bytes.NewReader(transcript))
	require.NoError(t, err)
	node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/media/" + sha[:2] + "/" + sha + ".wav"}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID, Profile: "supplied-transcript"}
	plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
		Selector: selector, PlanFingerprint: plan.Fingerprint,
	}})
	require.NoError(t, err)
	queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000653",
		Processing:  &api.MediaProcessingBody{Profile: "supplied-transcript", SuppliedInputID: artifact.SuppliedInputID},
	})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, statusErr := daemon.MediaStatus(t.Context(), receipt.SourceID)
		require.NoError(collect, statusErr)
		require.Equal(collect, queued.OperationID, status.OperationID)
		require.Equal(collect, "succeeded", status.OperationState)
		require.Equal(collect, "transcribed", status.CoverageState)
	}, 10*time.Second, 20*time.Millisecond)
	assert.Zero(t, provider.requests.Load(), "supplied transcript selection must not call Docling")

	stream, err := daemon.RenditionForSelector(t.Context(), selector, 1<<20)
	require.NoError(t, err)
	var rendition bytes.Buffer
	_, err = stream.CopyVerified(&rendition)
	require.NoError(t, err)
	require.NoError(t, stream.Close())
	assert.Contains(t, rendition.String(), "supplied transcript remains exact")

	plainSource := writeSourceFile(t, "ordinary.txt", "plaintext remains available without the ASR secret\n")
	_, err = runCLI(t, "add", plainSource, "--dest", "/plain")
	require.NoError(t, err)
	plainNode, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/plain/ordinary.txt"}})
	require.NoError(t, err)
	plainSelector := api.ProcessingSelector{NodeID: plainNode.ID,
		ContentVersionID: plainNode.CurrentVersionID, Profile: "private-text"}
	plainPlan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: plainSelector}})
	require.NoError(t, err)
	plainJob, err := daemon.StartProcessing(t.Context(), api.StartProcessingRequest{
		Selector: plainSelector, PlanFingerprint: plainPlan.Fingerprint, Consent: true,
	}, plainPlan.ProfileFingerprint)
	require.NoError(t, err)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		status, statusErr := daemon.ProcessingStatus(t.Context(), plainJob.ID)
		require.NoError(collect, statusErr)
		require.Equal(collect, "completed", status.State)
	}, 10*time.Second, 20*time.Millisecond)
	plainRendition, err := daemon.RenditionForSelector(t.Context(), plainSelector, 1<<20)
	require.NoError(t, err)
	var plainBody bytes.Buffer
	_, err = plainRendition.CopyVerified(&plainBody)
	require.NoError(t, err)
	require.NoError(t, plainRendition.Close())
	assert.Contains(t, plainBody.String(), "plaintext remains available")
	assert.Zero(t, provider.requests.Load(), "missing ASR credentials must not affect plaintext processing")
}

func TestDaemonDoclingASRRestoredWork(t *testing.T) {
	provider := newDaemonDoclingServer(t)
	provider.resultGate = make(chan struct{})
	root, daemon, stop := startDaemonASRTest(t, provider, "synthetic-provider-secret", false)
	receipt, selector, plan := daemonASRSourceAndPlan(t, daemon, "restored.wav", mediatest.WAV())
	_, err := daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
		Selector: selector, PlanFingerprint: plan.Fingerprint,
	}})
	require.NoError(t, err)
	_, err = daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000662", Processing: &api.MediaProcessingBody{Profile: "asr"},
	})
	require.NoError(t, err)
	select {
	case <-provider.resultStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("restored-work provider request did not start")
	}
	beforeRestart := provider.requests.Load()
	stop()
	waitForDaemonStop(t, root)
	close(provider.resultGate)

	_, restarted, _ := startDaemonASRTest(t, provider, "synthetic-provider-secret", false, root)
	status, err := restarted.MediaStatus(t.Context(), receipt.SourceID)
	require.NoError(t, err)
	assert.NotEqual(t, "succeeded", status.OperationState)
	assert.Equal(t, beforeRestart, provider.requests.Load(), "an interrupted provider submission is not replayed on restart")

	newPlan, err := restarted.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	_, err = restarted.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
		Selector: selector, PlanFingerprint: newPlan.Fingerprint,
	}})
	require.NoError(t, err)
	retried, err := restarted.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
		OperationID: "00000000-0000-4000-8000-000000000663", Processing: &api.MediaProcessingBody{Profile: "asr"},
	})
	require.NoError(t, err)
	assert.Equal(t, "queued", retried.OperationState)
	closeProviderResultGate(provider)
	assert.Equal(t, beforeRestart, provider.requests.Load(), "a restart must not replay a held claim")
}

func TestDaemonDoclingASRQueuedWorkAfterProfileChange(t *testing.T) {
	for _, change := range []string{"request limits", "transcript policy"} {
		t.Run(change, func(t *testing.T) {
			provider := newDaemonDoclingServer(t)
			provider.resultGate = make(chan struct{})
			root, daemon, stop := startDaemonASRTest(t, provider, daemonASRProviderKey, false)
			var queued api.MediaReceipt
			for index, source := range []struct {
				filename, mediaType string
				content             []byte
			}{
				{"running.wav", "audio/wav", mediatest.WAV()},
				{"queued.mp3", "audio/mpeg", mediatest.MP3()},
			} {
				receipt, selector, plan := daemonASRSourceAndPlanWith(t, daemon,
					fmt.Sprintf("00000000-0000-4000-8000-%012d", 801+index*2),
					source.filename, source.mediaType, source.content)
				_, err := daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
					Selector: selector, PlanFingerprint: plan.Fingerprint,
				}})
				require.NoError(t, err)
				queued, err = daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
					OperationID: fmt.Sprintf("00000000-0000-4000-8000-%012d", 802+index*2),
					Processing:  &api.MediaProcessingBody{Profile: "asr"},
				})
				require.NoError(t, err)
				if index == 0 {
					select {
					case <-provider.resultStarted:
					case <-time.After(10 * time.Second):
						t.Fatal("first provider request did not start")
					}
				}
			}
			status, err := daemon.ProcessingStatus(t.Context(), queued.JobID)
			require.NoError(t, err)
			require.Equal(t, "queued", status.State)
			stop()
			waitForDaemonStop(t, root)
			closeProviderResultGate(provider)
			beforeRestart := provider.requests.Load()

			cfg := config.Default()
			_, err = toml.DecodeFile(filepath.Join(root, "config.toml"), &cfg)
			require.NoError(t, err)
			profile := cfg.RenditionProfiles["asr"]
			wantFailure := "terminal"
			if change == "transcript policy" {
				configured, err := configuredDoclingASRProfile(profile)
				require.NoError(t, err)
				profile.MaxTranscriptChars++
				descriptor := configured.Descriptor
				descriptor.PolicyFingerprint, err = docling.ASRPolicyFingerprint(profile.MaxTranscriptChars)
				require.NoError(t, err)
				descriptor.Fingerprint = ""
				descriptor, err = document.NewRenditionDescriptor(descriptor)
				require.NoError(t, err)
				profile.DescriptorFingerprint = descriptor.Fingerprint
				profile.DisclosureFingerprint = docling.ASRDisclosureFingerprint(
					descriptor, profile.Runtime.Endpoint, profile.DeploymentFingerprint)
				wantFailure = "stale_authority"
			} else {
				profile.MaxUnits++
			}
			cfg.RenditionProfiles["asr"] = profile
			require.NoError(t, cfg.Validate())
			require.NoError(t, writeDaemonASRConfig(root, cfg))
			startServe(t)
			record := waitForDaemon(t, root)
			restarted := daemonconn.New("http://"+record.Address, cfg.Server.APIKey)
			t.Cleanup(func() { require.NoError(t, restarted.Close()) })
			require.EventuallyWithT(t, func(collect *assert.CollectT) {
				status, err := restarted.ProcessingStatus(t.Context(), queued.JobID)
				require.NoError(collect, err)
				require.Equal(collect, "failed", status.State)
				require.Equal(collect, wantFailure, status.FailureCode)
			}, 10*time.Second, 20*time.Millisecond)
			require.Equal(t, beforeRestart, provider.requests.Load())
		})
	}
}

func TestDaemonDoclingASRMetadataRestoreRequiresFreshConsent(t *testing.T) {
	provider := newDaemonDoclingServer(t)
	provider.resultGate = make(chan struct{})
	sourceRoot, daemon, stop := startDaemonASRTest(t, provider, daemonASRProviderKey, false)
	thirdWAV := bytes.Clone(mediatest.WAV())
	thirdWAV[len(thirdWAV)-1] ^= 1
	cases := []daemonASRRestoreCase{
		{
			name: "running", state: "running", filename: "restored-running.wav",
			mediaType: "audio/wav", content: mediatest.WAV(),
			submitOperationID:    "00000000-0000-4000-8000-000000000701",
			retryOperationID:     "00000000-0000-4000-8000-000000000702",
			noConsentOperationID: "00000000-0000-4000-8000-000000000703",
			freshOperationID:     "00000000-0000-4000-8000-000000000704",
		},
		{
			name: "queued", state: "queued", filename: "restored-queued.mp3",
			mediaType: "audio/mpeg", content: mediatest.MP3(),
			submitOperationID:    "00000000-0000-4000-8000-000000000711",
			retryOperationID:     "00000000-0000-4000-8000-000000000712",
			noConsentOperationID: "00000000-0000-4000-8000-000000000713",
			freshOperationID:     "00000000-0000-4000-8000-000000000714",
		},
		{
			name: "retry_wait", state: "retry_wait", filename: "restored-retry.wav",
			mediaType: "audio/wav", content: thirdWAV,
			submitOperationID:    "00000000-0000-4000-8000-000000000721",
			retryOperationID:     "00000000-0000-4000-8000-000000000722",
			noConsentOperationID: "00000000-0000-4000-8000-000000000723",
			freshOperationID:     "00000000-0000-4000-8000-000000000724",
		},
	}
	for index := range cases {
		testCase := &cases[index]
		receipt, selector, plan := daemonASRSourceAndPlanWith(
			t, daemon, testCase.submitOperationID, testCase.filename,
			testCase.mediaType, testCase.content)
		testCase.receipt, testCase.selector = receipt, selector
		_, err := daemon.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
			Selector: selector, PlanFingerprint: plan.Fingerprint,
		}})
		require.NoError(t, err)
		queued, err := daemon.RetryMedia(t.Context(), receipt.SourceID, api.MediaRetryBody{
			OperationID: testCase.retryOperationID,
			Processing:  &api.MediaProcessingBody{Profile: "asr"},
		})
		require.NoError(t, err)
		require.Equal(t, "queued", queued.OperationState)
		testCase.receipt = queued
		if index == 0 {
			select {
			case <-provider.resultStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("metadata-restore provider request did not start")
			}
		}
	}
	closeProviderResultGate(provider)
	stop()
	waitForDaemonStop(t, sourceRoot)
	beforeRestore := provider.requests.Load()

	var sourceMetadata bytes.Buffer
	sourceCatalog, err := store.Open((home.Layout{Root: sourceRoot}).DBPath())
	require.NoError(t, err)
	require.NoError(t, sourceCatalog.ExportMetadata(t.Context(), &sourceMetadata))
	require.NoError(t, sourceCatalog.Close())
	mutatedMetadata, jobByWaiter := rewriteDaemonASRRestoreMetadata(t, sourceMetadata.Bytes(), cases)
	for index := range cases {
		cases[index].jobID = jobByWaiter[cases[index].receipt.JobID]
	}
	restoredRoot := restoreDaemonASRMetadata(t, mutatedMetadata, cases)

	_, restored, _ := startDaemonASRTest(t, provider, daemonASRProviderKey, false, restoredRoot)
	for index := range cases {
		testCase := &cases[index]
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			status, statusErr := restored.ProcessingStatus(t.Context(), testCase.receipt.JobID)
			require.NoError(collect, statusErr)
			require.Equal(collect, "failed", status.State)
			require.Equal(collect, "consent", status.FailureCode)
		}, 10*time.Second, 20*time.Millisecond)
		_, err := restored.RetryMedia(t.Context(), testCase.receipt.SourceID, api.MediaRetryBody{
			OperationID: testCase.noConsentOperationID,
			Processing:  &api.MediaProcessingBody{Profile: "asr"},
		})
		require.Error(t, err)
	}
	require.Equal(t, beforeRestore, provider.requests.Load(),
		"restored queued, running, and retry_wait jobs must not reach the provider without fresh consent")

	for index := range cases {
		testCase := &cases[index]
		freshPlan, err := restored.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{
			Selector: testCase.selector,
		}})
		require.NoError(t, err)
		_, err = restored.API().GrantDocumentProcessingConsent(t.Context(), &apiclient.GrantDocumentProcessingConsentRequestOptions{Body: &api.ProcessingConsentGrantRequest{
			Selector: testCase.selector, PlanFingerprint: freshPlan.Fingerprint,
		}})
		require.NoError(t, err)
		retried, err := restored.RetryMedia(t.Context(), testCase.receipt.SourceID, api.MediaRetryBody{
			OperationID: testCase.freshOperationID,
			Processing:  &api.MediaProcessingBody{Profile: "asr"},
		})
		require.NoError(t, err)
		require.Equal(t, "queued", retried.OperationState)
	}
	for index := range cases {
		testCase := &cases[index]
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			status, statusErr := restored.MediaStatus(t.Context(), testCase.receipt.SourceID)
			require.NoError(collect, statusErr)
			require.Equal(collect, "succeeded", status.OperationState)
			require.Equal(collect, "transcribed", status.CoverageState)
		}, 20*time.Second, 20*time.Millisecond)
	}
	require.Greater(t, provider.requests.Load(), beforeRestore)
}

type daemonASRRestoreCase struct {
	name, state, filename, mediaType string
	content                          []byte
	submitOperationID                string
	retryOperationID                 string
	noConsentOperationID             string
	freshOperationID                 string
	receipt                          api.MediaReceipt
	selector                         api.ProcessingSelector
	jobID                            string
}

type daemonASRMetadataWaiter struct {
	id, jobID string
	record    map[string]jsontext.Value
}

func rewriteDaemonASRRestoreMetadata(
	t *testing.T, exported []byte, cases []daemonASRRestoreCase,
) ([]byte, map[string]string) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(exported), []byte{'\n'})
	waiters := make(map[string]daemonASRMetadataWaiter)
	for _, line := range lines {
		var identity struct {
			Type     string `json:"type"`
			JobID    string `json:"job_id"`
			WaiterID string `json:"waiter_id"`
		}
		require.NoError(t, json.Unmarshal(line, &identity))
		if identity.Type != "rendition_job_waiter" {
			continue
		}
		var record map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(line, &record))
		waiters[identity.WaiterID] = daemonASRMetadataWaiter{
			id: identity.WaiterID, jobID: identity.JobID, record: record,
		}
	}

	type restoreJob struct {
		state  string
		waiter daemonASRMetadataWaiter
	}
	desired := make(map[string]restoreJob, len(cases))
	jobByWaiter := make(map[string]string, len(cases))
	for _, testCase := range cases {
		waiter, ok := waiters[testCase.receipt.JobID]
		require.True(t, ok, "metadata waiter %s is missing", testCase.receipt.JobID)
		_, duplicate := desired[waiter.jobID]
		require.False(t, duplicate, "metadata job %s is shared by restore cases", waiter.jobID)
		desired[waiter.jobID] = restoreJob{state: testCase.state, waiter: waiter}
		jobByWaiter[testCase.receipt.JobID] = waiter.jobID
	}

	var mutated bytes.Buffer
	jobCount := 0
	for _, line := range lines {
		var identity struct {
			Type  string `json:"type"`
			JobID string `json:"job_id"`
		}
		require.NoError(t, json.Unmarshal(line, &identity))
		if identity.Type == "rendition_job" {
			wanted, ok := desired[identity.JobID]
			require.True(t, ok, "unexpected rendition job %s in restore metadata", identity.JobID)
			var record map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(line, &record))
			setDaemonASRMetadataValue(t, record, "state", wanted.state)
			setDaemonASRMetadataValue(t, record, "phase", "queued")
			setDaemonASRMetadataValue(t, record, "available_at", daemonASRRestorePast)
			setDaemonASRMetadataValue(t, record, "provider_started", false)
			setDaemonASRMetadataValue(t, record, "provider_attempts", 0)
			setDaemonASRMetadataValue(t, record, "provider_resume_handle", nil)
			setDaemonASRMetadataValue(t, record, "execution_snapshot", nil)
			setDaemonASRMetadataValue(t, record, "lexical_generation_id", nil)
			setDaemonASRMetadataValue(t, record, "selected_waiter_id", wanted.waiter.id)
			for _, field := range []string{
				"authorization_grant_id", "authorization_incarnation_id",
				"authorization_revocation_fence",
			} {
				restored, exists := wanted.waiter.record[field]
				require.True(t, exists, "waiter metadata field %s is missing", field)
				record[field] = restored
			}
			if wanted.state == "running" {
				setDaemonASRMetadataValue(t, record, "claim_owner", "worker:restored-running")
				setDaemonASRMetadataValue(t, record, "claim_epoch", 1)
				setDaemonASRMetadataValue(t, record, "lease_expires_at", daemonASRRestorePast)
			} else {
				setDaemonASRMetadataValue(t, record, "claim_owner", nil)
				setDaemonASRMetadataValue(t, record, "claim_epoch", 0)
				setDaemonASRMetadataValue(t, record, "lease_expires_at", nil)
			}
			var failure any
			if wanted.state == "retry_wait" {
				failure = "transient"
			}
			setDaemonASRMetadataValue(t, record, "failure_code", failure)
			encoded, err := json.Marshal(record, json.Deterministic(true))
			require.NoError(t, err)
			line = encoded
			jobCount++
		}
		mutated.Write(line)
		mutated.WriteByte('\n')
	}
	require.Equal(t, len(cases), jobCount)
	return mutated.Bytes(), jobByWaiter
}

const daemonASRRestorePast = "2020-01-01T00:00:00.000000000Z"

func setDaemonASRMetadataValue(
	t *testing.T, record map[string]jsontext.Value, field string, value any,
) {
	t.Helper()
	encoded, err := json.Marshal(value, json.Deterministic(true))
	require.NoError(t, err)
	record[field] = jsontext.Value(encoded)
}

func restoreDaemonASRMetadata(
	t *testing.T, metadata []byte, cases []daemonASRRestoreCase,
) string {
	t.Helper()
	root := t.TempDir()
	layout := home.Layout{Root: root}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(layout.DBPath())
	require.NoError(t, err)
	require.NoError(t, catalog.ImportMetadata(t.Context(), bytes.NewReader(metadata)))
	for _, testCase := range cases {
		job, err := catalog.RenditionJobByID(t.Context(), testCase.jobID)
		require.NoError(t, err)
		require.Equal(t, store.RenditionJobState(testCase.state), job.State)
		require.Equal(t, store.RenditionPhaseQueued, job.Phase)
	}
	var imported bytes.Buffer
	require.NoError(t, catalog.ExportMetadata(t.Context(), &imported))
	for _, testCase := range cases {
		for line := range bytes.SplitSeq(bytes.TrimSpace(imported.Bytes()), []byte{'\n'}) {
			var identity struct {
				Type  string `json:"type"`
				JobID string `json:"job_id"`
			}
			require.NoError(t, json.Unmarshal(line, &identity))
			if identity.Type != "rendition_job" || identity.JobID != testCase.jobID {
				continue
			}
			var record map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(line, &record))
			for _, field := range []string{
				"selected_waiter_id", "authorization_grant_id",
				"authorization_incarnation_id", "authorization_revocation_fence",
			} {
				require.Contains(t, record, field)
				require.Equal(t, "null", string(record[field]),
					"restored %s job retained %s", testCase.state, field)
			}
			break
		}
	}
	restoredBlobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	defer func() {
		_ = restoredBlobs.Close()
		_ = catalog.Close()
	}()
	for _, testCase := range cases {
		written, err := restoredBlobs.WriteDetailedContext(
			t.Context(), bytes.NewReader(testCase.content))
		require.NoError(t, err)
		digest := sha256.Sum256(testCase.content)
		require.Equal(t, hex.EncodeToString(digest[:]), written.Hash)
		require.Equal(t, int64(len(testCase.content)), written.Size)
	}
	require.NoError(t, catalog.VerifyRenditionBlobBytes(t.Context(), restoredBlobs))
	require.NoError(t, restoredBlobs.Close())
	if err := os.Remove(filepath.Join(layout.BlobsDir(), ".packstore-owner.json")); err != nil {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, catalog.Checkpoint(t.Context()))
	require.NoError(t, catalog.Close())
	return root
}

type daemonDoclingTask struct {
	filename string
	source   []byte
	fields   map[string][]string
}

type daemonDoclingServer struct {
	server *httptest.Server

	mu                  sync.Mutex
	tasks               map[string]daemonDoclingTask
	resultGate          chan struct{}
	resultStarted       chan struct{}
	resultStartOnce     sync.Once
	resultSchemaVersion string
	apiKey              string

	requests       atomic.Int64
	authFailures   atomic.Int64
	submissions    atomic.Int64
	polls          atomic.Int64
	transientPolls atomic.Int32
}

func newDaemonDoclingServer(t *testing.T) *daemonDoclingServer {
	t.Helper()
	provider := &daemonDoclingServer{tasks: make(map[string]daemonDoclingTask), resultStarted: make(chan struct{}),
		resultSchemaVersion: "1.10.0", apiKey: daemonASRProviderKey}
	provider.server = httptest.NewServer(provider)
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *daemonDoclingServer) URL() string { return provider.server.URL }

func (provider *daemonDoclingServer) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	provider.requests.Add(1)
	if request.Header.Get("X-Api-Key") != provider.apiKey {
		provider.authFailures.Add(1)
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/convert/file/async":
		provider.handleSubmit(response, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/status/poll/"):
		provider.handlePoll(response, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/result/"):
		provider.handleResult(response, request)
	default:
		http.NotFound(response, request)
	}
}

func (provider *daemonDoclingServer) handleSubmit(response http.ResponseWriter, request *http.Request) {
	mediaType, params, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		http.Error(response, "invalid content type", http.StatusBadRequest)
		return
	}
	reader := multipart.NewReader(request.Body, params["boundary"])
	file, err := reader.NextPart()
	if err != nil || file.FormName() != "files" {
		http.Error(response, "invalid file part", http.StatusBadRequest)
		return
	}
	source, err := io.ReadAll(file)
	if err != nil {
		http.Error(response, "invalid file", http.StatusBadRequest)
		return
	}
	fields := make(map[string][]string)
	for {
		part, partErr := reader.NextPart()
		if errors.Is(partErr, io.EOF) {
			break
		}
		if partErr != nil {
			http.Error(response, "invalid fields", http.StatusBadRequest)
			return
		}
		value, readErr := io.ReadAll(part)
		if readErr != nil {
			http.Error(response, "invalid field", http.StatusBadRequest)
			return
		}
		fields[part.FormName()] = append(fields[part.FormName()], string(value))
	}
	if !equalDaemonASRFields(fields) {
		http.Error(response, "unexpected fields", http.StatusBadRequest)
		return
	}
	sequence := provider.submissions.Add(1)
	taskID := fmt.Sprintf("task-%d", sequence)
	provider.mu.Lock()
	provider.tasks[taskID] = daemonDoclingTask{filename: file.FileName(), source: source, fields: fields}
	provider.mu.Unlock()
	status := "success"
	if provider.transientPolls.Load() > 0 {
		status = "pending"
	}
	writeDaemonJSON(response, map[string]string{"task_id": taskID, "task_type": "convert", "task_status": status})
}

func (provider *daemonDoclingServer) handlePoll(response http.ResponseWriter, request *http.Request) {
	provider.polls.Add(1)
	if provider.transientPolls.Load() > 0 && provider.transientPolls.Add(-1) >= 0 {
		response.Header().Set("Retry-After", "0")
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	taskID := strings.TrimPrefix(request.URL.Path, "/v1/status/poll/")
	provider.mu.Lock()
	_, ok := provider.tasks[taskID]
	provider.mu.Unlock()
	if !ok {
		http.NotFound(response, request)
		return
	}
	writeDaemonJSON(response, map[string]string{"task_id": taskID, "task_type": "convert", "task_status": "success"})
}

func (provider *daemonDoclingServer) handleResult(response http.ResponseWriter, request *http.Request) {
	provider.resultStartOnce.Do(func() { close(provider.resultStarted) })
	if provider.resultGate != nil {
		select {
		case <-provider.resultGate:
		case <-request.Context().Done():
			return
		}
	}
	taskID := strings.TrimPrefix(request.URL.Path, "/v1/result/")
	provider.mu.Lock()
	task, ok := provider.tasks[taskID]
	provider.mu.Unlock()
	if !ok {
		http.NotFound(response, request)
		return
	}
	writeDaemonJSON(response, daemonASRResult(task.filename, provider.resultSchemaVersion))
}

func (provider *daemonDoclingServer) submissionSource() []byte {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, task := range provider.tasks {
		return append([]byte(nil), task.source...)
	}
	return nil
}

func equalDaemonASRFields(fields map[string][]string) bool {
	want := map[string][]string{"from_formats": {"audio"}, "to_formats": {"json"}, "target_type": {"inbody"}}
	if len(fields) != len(want) {
		return false
	}
	for key, values := range want {
		if !slicesEqual(values, fields[key]) {
			return false
		}
	}
	return true
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func daemonASRResult(filename, schemaVersion string) map[string]any {
	return map[string]any{
		"status": "success", "errors": []string{},
		"document": map[string]any{
			"filename": filename, "md_content": nil,
			"json_content": map[string]any{
				"schema_name": "DoclingDocument", "version": schemaVersion,
				"origin": map[string]any{"filename": filename}, "pages": map[string]any{},
				"texts": []any{map[string]any{
					"text":   "Telescope delivery arrives Friday at 3.",
					"source": []any{map[string]any{"kind": "track", "start_time": 0.0, "end_time": 0.005}},
				}},
			},
		},
	}
}

func writeDaemonJSON(response http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_, _ = response.Write(data)
}

func startDaemonASRTest(t *testing.T, provider *daemonDoclingServer, secret string, withPlaintext bool, existingRoot ...string) (string, *daemonconn.Connection, func()) {
	t.Helper()
	root := t.TempDir()
	if len(existingRoot) != 0 {
		root = existingRoot[0]
	}
	cfg, _ := doclingASRProcessingConfig(t, provider.URL())
	cfg.Server.APIKey = daemonASRAPIKey
	asrProfile := cfg.RenditionProfiles["asr"]
	asrProfile.DiscloseFilename = true
	cfg.RenditionProfiles["asr"] = asrProfile
	if withPlaintext {
		plain := plaintextProcessingConfig(strings.Repeat("0", 64))
		plaintextFingerprint, err := plaintextProviderForDaemonTest()
		require.NoError(t, err)
		plaintextProfile := plain.RenditionProfiles["plaintext"]
		plaintextProfile.DescriptorFingerprint = plaintextFingerprint
		cfg.RenditionProfiles["plaintext"] = plaintextProfile
		cfg.ProcessingProfiles["private-text"] = plain.ProcessingProfiles["private-text"]
	}
	require.NoError(t, cfg.Validate())
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_TEST_DOCLING_KEY", secret)
	t.Setenv("DOCBANK_HOME", root)
	stop := startServe(t)
	record := waitForDaemon(t, root)
	daemon := daemonconn.New("http://"+record.Address, cfg.Server.APIKey)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })
	return root, daemon, stop
}

func plaintextProviderForDaemonTest() (string, error) {
	provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: plaintext.MaxDocumentBytes})
	if err != nil {
		return "", err
	}
	return provider.Descriptor().Fingerprint, nil
}

func writeDaemonASRConfig(root string, cfg config.Config) error {
	var encoded bytes.Buffer
	renditionProfiles := make(map[string]any, len(cfg.RenditionProfiles))
	for name, profile := range cfg.RenditionProfiles {
		value := map[string]any{
			"adapter_contract": profile.AdapterContract, "authorization_fingerprint": profile.AuthorizationFingerprint,
			"credential_binding": profile.CredentialBinding, "deployment_fingerprint": profile.DeploymentFingerprint,
			"descriptor_id": profile.DescriptorID, "descriptor_fingerprint": profile.DescriptorFingerprint,
			"disclose_filename": profile.DiscloseFilename, "disclosure_fingerprint": profile.DisclosureFingerprint,
			"max_document_bytes": profile.MaxDocumentBytes, "max_response_bytes": profile.MaxResponseBytes,
			"max_units": profile.MaxUnits, "max_transcript_chars": profile.MaxTranscriptChars,
			"requested_artifacts": profile.RequestedArtifacts,
			"trust_boundary":      profile.TrustBoundary, "upload_options_fingerprint": profile.UploadOptionsFingerprint,
		}
		if runtime := profile.Runtime; runtime != nil {
			value["runtime"] = map[string]any{
				"endpoint": runtime.Endpoint, "request_timeout": runtime.RequestTimeout.Std().String(),
				"total_timeout": runtime.TotalTimeout.Std().String(), "poll_interval": runtime.PollInterval.Std().String(),
				"max_poll_attempts": runtime.MaxPollAttempts, "allowed_cidrs": runtime.AllowedCIDRs,
				"spki_sha256": runtime.SPKISHA256, "proxy_mode": runtime.ProxyMode,
				"connect_timeout": runtime.ConnectTimeout.Std().String(), "keep_alive": runtime.KeepAlive.Std().String(),
				"tls_handshake_timeout": runtime.TLSHandshakeTimeout.Std().String(),
			}
		}
		renditionProfiles[name] = value
	}
	if err := toml.NewEncoder(&encoded).Encode(map[string]any{
		"server":              map[string]string{"api_key": cfg.Server.APIKey},
		"credential_bindings": cfg.CredentialBindings,
		"rendition_profiles":  renditionProfiles,
		"retrieval_profiles":  cfg.RetrievalProfiles,
		"processing_profiles": cfg.ProcessingProfiles,
	}); err != nil {
		return fmt.Errorf("encoding daemon configuration: %w", err)
	}
	return os.WriteFile(filepath.Join(root, "config.toml"), encoded.Bytes(), 0o600)
}

func daemonASRSourceAndPlan(t *testing.T, daemon *daemonconn.Connection, filename string, content []byte) (api.MediaReceipt, api.ProcessingSelector, api.ProcessingPlan) {
	t.Helper()
	return daemonASRSourceAndPlanWith(t, daemon,
		"00000000-0000-4000-8000-000000000671", filename, "audio/wav", content)
}

func daemonASRSourceAndPlanWith(
	t *testing.T, daemon *daemonconn.Connection, operationID, filename, mediaType string, content []byte,
) (api.MediaReceipt, api.ProcessingSelector, api.ProcessingPlan) {
	t.Helper()
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	extension := filepath.Ext(filename)
	receipt, err := daemon.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: operationID, Filename: filename, MediaType: mediaType,
		SHA256: sha, ByteLength: int64(len(content)),
		Occurrence: api.MediaOccurrenceBody{Ref: filename, Revision: "1", Filename: filename},
	}, bytes.NewReader(content))
	require.NoError(t, err)
	node, err := daemon.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/media/" + sha[:2] + "/" + sha + extension}})
	require.NoError(t, err)
	selector := api.ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID, Profile: "asr"}
	plan, err := daemon.API().PlanDocumentProcessing(t.Context(), &apiclient.PlanDocumentProcessingRequestOptions{Body: &api.ProcessingPlanRequest{Selector: selector}})
	require.NoError(t, err)
	return receipt, selector, *plan
}

func waitForDaemonStop(t *testing.T, root string) {
	t.Helper()
	require.Eventually(t, func() bool {
		records, listErr := daemonconn.RuntimeStore(root).List()
		return listErr == nil && len(records) == 0
	}, daemonShutdownTimeout, 50*time.Millisecond)
}

func closeProviderResultGate(provider *daemonDoclingServer) {
	if provider.resultGate != nil {
		select {
		case <-provider.resultGate:
		default:
			close(provider.resultGate)
		}
	}
}
