package reducto

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
)

const (
	testFileID = "reducto://123e4567-e89b-12d3-a456-426614174000.pdf"
	testJobID  = "123e4567-e89b-12d3-a456-426614174001"
)

var _ document.ResumableRenditionProvider = (*Client)(nil)

func TestClientUploadsExactBytesAndMapsNaturalEvidence(t *testing.T) {
	tests := []struct {
		family, mediaType, filename string
		unitKind                    document.EvidenceUnitKind
		locatorKind                 document.EvidenceLocatorKind
	}{
		{family: "pdf", mediaType: "application/pdf", filename: "synthetic.pdf", unitKind: document.EvidenceUnitPage, locatorKind: document.EvidenceLocatorPage},
		{family: "presentation", mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", filename: "synthetic.pptx", unitKind: document.EvidenceUnitSlide, locatorKind: document.EvidenceLocatorSlide},
	}
	for _, testCase := range tests {
		t.Run(testCase.family, func(t *testing.T) {
			source := []byte("synthetic exact " + testCase.family + " bytes")
			fixture := newFixture(t, testCase.family, testCase.mediaType, testCase.filename, source)
			fixture.profile.RetainStructured = true
			fixture.profile.MaxArtifacts = 1
			var paths []string
			polls := 0
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				assert.Equal(t, "https", request.URL.Scheme)
				assert.Equal(t, apiHost, request.URL.Host)
				assert.Empty(t, request.URL.RawQuery)
				assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
				switch request.URL.Path {
				case uploadPath:
					assertUpload(t, request, fixture.metadata, source)
					return response(request, http.StatusOK, `{"file_id":"`+testFileID+`","presigned_url":"https://attacker.invalid/upload"}`), nil
				case parsePath:
					assert.Equal(t, http.MethodPost, request.Method)
					var body map[string]any
					require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
					assert.Equal(t, map[string]any{
						"document_url":     testFileID,
						"advanced_options": map[string]any{"add_page_markers": true, "return_ocr_data": false},
						"options": map[string]any{
							"chunking":         map[string]any{"chunk_mode": "page"},
							"force_url_result": false,
						},
						"priority": false,
					}, body)
					return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
				case jobPath(testJobID):
					assert.Equal(t, http.MethodGet, request.Method)
					polls++
					if polls == 1 {
						return response(request, http.StatusOK, `{"status":"Pending","progress":0.25,"reason":null,"result":null}`), nil
					}
					return response(request, http.StatusOK, completedBody()), nil
				default:
					t.Fatalf("unexpected route %s", request.URL.String())
					return nil, errors.New("unexpected route")
				}
			})
			client := fixture.client(t, transport)
			var checkpoint document.RenditionResumeHandle
			checkpointCount := 0
			result, err := document.RenderRenditionWithResume(
				t.Context(), client, fixture.upload(), fixture.authorization, nil,
				func(handle document.RenditionResumeHandle) error {
					checkpoint = handle
					checkpointCount++
					return nil
				},
			)
			require.NoError(t, err)
			assert.NotEqual(t, testJobID, checkpoint.Value)
			assert.LessOrEqual(t, len(checkpoint.Value), 512)
			issued := decodeTestResumeHandle(t, checkpoint.Value)
			assert.Equal(t, testJobID, issued.JobID)
			authorizationFingerprint, err := fixture.authorization.Fingerprint()
			require.NoError(t, err)
			assert.Equal(t, authorizationFingerprint, issued.AuthorizationFingerprint)
			assert.Equal(t, int64(4), issued.Requests)
			assert.Equal(t, fixture.metadata.ByteLength, issued.InputBytes)
			assert.Positive(t, issued.OutputBytes)
			assert.GreaterOrEqual(t, checkpointCount, 4)
			started, err := time.Parse(timeForm, issued.StartedAt)
			require.NoError(t, err)
			assert.False(t, time.Now().UTC().Before(started))
			assert.Equal(t, []string{uploadPath, parsePath, jobPath(testJobID), jobPath(testJobID)}, paths)
			assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
			assert.Equal(t, testCase.unitKind, result.Evidence.UnitKind)
			require.Len(t, result.Evidence.Units, 2)
			assert.Equal(t, testCase.locatorKind, result.Evidence.Units[0].Locator.Kind)
			assert.Equal(t, document.EvidenceIndexOriginOne, result.Evidence.Units[0].Locator.IndexOrigin)
			assert.Equal(t, int64(1), result.Evidence.Units[0].Locator.Start)
			assert.Empty(t, result.Evidence.Units[0].ProviderID)
			assert.Equal(t, "# First unit\n\nFirst body", result.Evidence.Units[0].Text)
			assert.Equal(t, "# First unit\n\nFirst body\n\n---\n\nSecond unit", string(result.ProviderMarkdown))
			require.Len(t, result.Artifacts, 1)
			assert.Equal(t, document.EvidenceArtifactStructured, result.Artifacts[0].Role)
			assert.Contains(t, string(result.Artifacts[0].Payload), `"pdf_url":"https://attacker.invalid/converted.pdf"`)
			assert.Equal(t, int64(4), result.Receipt.Usage.Requests)
			assert.Equal(t, int64(2), result.Receipt.Usage.Units)
		})
	}
}

func TestClientWithholdsSourceFilenameButPreservesFormatHint(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "private-name.pdf", []byte("hidden filename"))
	fixture.authorization.DiscloseFilename = false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath:
			expected := fixture.metadata
			expected.Filename = "document.pdf"
			assertUpload(t, request, expected, fixture.source)
			return response(request, http.StatusOK,
				`{"file_id":"`+testFileID+`","presigned_url":null}`), nil
		case parsePath:
			return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
		case jobPath(testJobID):
			return response(request, http.StatusOK, completedBody()), nil
		default:
			return nil, errors.New("unexpected route")
		}
	})

	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
}

func TestNaturalEvidenceRefusesUnreportedSpreadsheetNames(t *testing.T) {
	chunks := []resultChunk{{
		Content: "sheet content",
		Blocks: []resultBlock{{
			Content: "sheet content", Type: "Text",
			BBox: resultBoundingBox{
				Height: new(1.0), Left: new(0.0), Page: new(int64(1)),
				Top: new(0.0), Width: new(1.0), OriginalPage: new(int64(1)),
			},
		}},
	}}
	_, _, err := naturalEvidence(chunks, "spreadsheet", 1)
	require.ErrorContains(t, err, "stable sheet name")

	fixture := newFixture(t, "spreadsheet",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"synthetic.xlsx", []byte("spreadsheet source"))
	client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported spreadsheet reached egress")
		return nil, errors.New("spreadsheet reached egress")
	}))
	_, err = document.RenderRenditionWithResume(t.Context(), client,
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
	require.ErrorContains(t, err, "unsupported format")
}

func TestClientResumesOnlyTheFixedJobRoute(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("resume source"))
	started := time.Now().UTC().Add(-30 * time.Second)
	checkpointed := started.Add(time.Second)
	resumeValue := testResumeValue(t, fixture.authorization, testJobID, started, checkpointed, fixture.metadata.ByteLength)
	var paths []string
	var transport http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		return response(request, http.StatusOK, completedBody()), nil
	})
	result, err := fixture.client(t, transport).RenderResumable(
		t.Context(), nil, fixture.authorization,
		&document.RenditionResumeHandle{Value: resumeValue}, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, []string{jobPath(testJobID)}, paths)
	assert.Equal(t, "reducto-"+testJobID, result.Receipt.OperationID)

	for _, handle := range []string{"", testJobID, "../job", "https://attacker.invalid", strings.Repeat("a", 513)} {
		_, err = fixture.client(t, transport).RenderResumable(
			t.Context(), nil, fixture.authorization,
			&document.RenditionResumeHandle{Value: handle}, nil)
		assertCode(t, err, document.RenditionErrorUnknownJob)
	}

	invalidAuthorization := fixture.authorization
	invalidAuthorization.MaxTotalResultBytes = 0
	client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid resume authorization reached egress")
		return nil, errors.New("invalid resume reached egress")
	}))
	_, err = client.RenderResumable(t.Context(), nil, invalidAuthorization,
		&document.RenditionResumeHandle{Value: resumeValue}, nil)
	require.Error(t, err)
}

func TestClientCheckpointsAcceptedJobBeforeRejectingSchemaDrift(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("accepted job"))
	transport := routeTransport(t, map[string]string{
		uploadPath: `{"file_id":"` + testFileID + `","presigned_url":null}`,
		parsePath:  `{"job_id":"` + testJobID + `","new_field":true}`,
	})
	var checkpoint document.RenditionResumeHandle
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(handle document.RenditionResumeHandle) error {
			checkpoint = handle
			return nil
		})

	assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	assert.Equal(t, testJobID, decodeTestResumeHandle(t, checkpoint.Value).JobID)
}

func TestResumeHandleFitsTheCoreBoundAtMaximumAccounting(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("bounded resume"))
	authorizationFingerprint, err := fixture.authorization.Fingerprint()
	require.NoError(t, err)
	startedAt := time.Now().UTC()
	value, err := encodeResumeHandle(resumePayload{
		JobID: strings.Repeat("a", maxJobTokenBytes), AuthorizationFingerprint: authorizationFingerprint,
		StartedAt: startedAt.Format(timeForm), SubmittedAt: startedAt.Add(time.Nanosecond).Format(timeForm),
		Requests: maxResumeUsageValue, Retries: maxResumeUsageValue,
		InputBytes: fixture.authorization.SourceBytes, OutputBytes: maxResumeUsageValue,
		RetryDelayMillis: int64((24 * time.Hour) / time.Millisecond),
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(value), 512)
	_, err = decodeResumeHandle(value, fixture.authorization)
	require.NoError(t, err)
}

func TestClientResumesHistoricalAuthorizationWithRecordedReceiptInterval(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("historical source"))
	authorizedAt := time.Now().UTC().Add(-2 * time.Hour)
	expiresAt := authorizedAt.Add(time.Minute)
	startedAt := authorizedAt.Add(10 * time.Second)
	checkpointedAt := startedAt.Add(5 * time.Second)
	fixture.authorization.AuthorizedAt = authorizedAt.Format(timeForm)
	fixture.authorization.ExpiresAt = expiresAt.Format(timeForm)
	client := fixture.client(t, completedTransport(t))
	snapshot := executionSnapshot(t, startedAt, client, fixture)
	result, err := document.ResumeRendition(t.Context(), client, snapshot, document.RenditionResumeHandle{
		Value: testResumeValue(t, fixture.authorization, testJobID, startedAt, checkpointedAt, fixture.metadata.ByteLength),
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, startedAt.Format(timeForm), result.Receipt.StartedAt)
	completedAt, err := time.Parse(timeForm, result.Receipt.CompletedAt)
	require.NoError(t, err)
	assert.Equal(t, checkpointedAt, completedAt)
	assert.Equal(t, int64(3), result.Receipt.Usage.Requests)
}

func TestClientAcceptsOnlyPinnedSDKAsyncLifecycleStates(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("lifecycle source"))
	polls := 0
	var transport http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath:
			return response(request, http.StatusOK, `{"file_id":"`+testFileID+`","presigned_url":null}`), nil
		case parsePath:
			return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
		case jobPath(testJobID):
			polls++
			if polls == 1 {
				return response(request, http.StatusOK, `{"status":"Pending","progress":0.5,"reason":null,"result":null}`), nil
			}
			if polls == 2 {
				return response(request, http.StatusOK, `{"status":"Idle","progress":0.9,"reason":null,"result":null}`), nil
			}
			return response(request, http.StatusOK, completedBody()), nil
		default:
			t.Fatalf("unexpected route %s", request.URL.String())
			return nil, errors.New("unexpected route")
		}
	})
	result, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, int64(2), result.Receipt.Usage.Units)

	for _, status := range []string{"InProgress", "Completing"} {
		fixture = newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("status drift"))
		transport = routeTransport(t, map[string]string{
			uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
			parsePath:          `{"job_id":"` + testJobID + `"}`,
			jobPath(testJobID): `{"status":"` + status + `","progress":0.5,"reason":null,"result":null}`,
		})
		_, err = document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	}
}

func TestClientNeverFollowsProviderAuthoredURLs(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("url source"))
	var paths []string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case uploadPath:
			return response(request, http.StatusOK, `{"file_id":"`+testFileID+`","presigned_url":"https://attacker.invalid/upload"}`), nil
		case parsePath:
			return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
		case jobPath(testJobID):
			return response(request, http.StatusOK, `{
				"status":"Completed","progress":1,"reason":null,
				"result":{"result":{"type":"url","result_id":"result-1","url":"https://attacker.invalid/result"},"usage":{"num_pages":2},"duration":1,"job_id":"`+testJobID+`","pdf_url":"https://attacker.invalid/pdf"}
			}`), nil
		default:
			t.Fatalf("provider-authored URL was followed: %s", request.URL.String())
			return nil, errors.New("provider URL followed")
		}
	})
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	assertCode(t, err, document.RenditionErrorMalformedEvidence)
	assert.Equal(t, []string{uploadPath, parsePath, jobPath(testJobID)}, paths)
}

func TestClientRetainsRawResultOnlyWhenProfileAndAuthorizationPermit(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("artifact source"))
	transport := completedTransport(t)

	result, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
	assert.Empty(t, result.Artifacts)
	assert.Empty(t, result.Evidence.Artifacts)

	fixture.profile.RetainStructured = true
	fixture.profile.MaxArtifacts = 1
	client := fixture.client(t, completedTransport(t))
	descriptor := client.Descriptor()
	fixture.authorization.ProviderID = descriptor.ID
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	fixture.authorization.AllowedArtifactRoles = nil
	fixture.authorization.MaxArtifacts = 0
	fixture.authorization.MaxArtifactBytes = 0
	result, err = document.RenderRenditionWithResume(t.Context(), client,
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
	assert.Empty(t, result.Artifacts)
}

func TestClientHonorsMarkdownRetentionAndReservedFrontmatter(t *testing.T) {
	t.Run("omit unrequested Markdown", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("no markdown"))
		fixture.authorization.MaxProviderMarkdownBytes = 0
		result, err := document.RenderRenditionWithResume(t.Context(),
			fixture.client(t, completedTransport(t)), fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		require.NoError(t, err)
		assert.Empty(t, result.ProviderMarkdown)
		assert.NotEmpty(t, result.Evidence.Units)
	})

	t.Run("reject reserved frontmatter", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("frontmatter"))
		content := "---\ndocbank-sanitized-markdown/v1"
		chunks, err := json.Marshal([]resultChunk{{
			Content: content, Embed: "marker", Blocks: []resultBlock{{
				Content: content, Type: "Text", BBox: resultBoundingBox{
					Height: new(1.0), Left: new(0.0), Page: new(int64(1)),
					Top: new(0.0), Width: new(1.0), OriginalPage: new(int64(1)),
				},
			}},
		}})
		require.NoError(t, err)
		body := completedBodyWithChunks(testJobID, string(chunks), 1)
		transport := routeTransport(t, map[string]string{
			uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
			parsePath:          `{"job_id":"` + testJobID + `"}`,
			jobPath(testJobID): body,
		})
		_, err = document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})
}

func TestClientRejectsPartialOutputAndSchemaDrift(t *testing.T) {
	tests := []struct {
		name, body string
		want       document.RenditionErrorCode
	}{
		{name: "failed", body: `{"status":"Failed","progress":0.5,"reason":"private source detail","result":null}`, want: document.RenditionErrorMalformedEvidence},
		{name: "unknown status", body: `{"status":"Paused","progress":0.5,"reason":null,"result":null}`, want: document.RenditionErrorAmbiguousSubmission},
		{name: "unknown model field", body: `{"status":"Completed","progress":1,"reason":null,"model":"next-model","result":null}`, want: document.RenditionErrorMalformedEvidence},
		{name: "newer API usage drift", body: strings.Replace(completedBody(),
			`"num_pages":2`, `"num_pages":2,"credits":2.5`, 1), want: document.RenditionErrorMalformedEvidence},
		{name: "empty chunk", body: completedBodyWithChunks(testJobID, `[]`, 0), want: document.RenditionErrorMalformedEvidence},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("partial source"))
			transport := routeTransport(t, map[string]string{
				uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
				parsePath:          `{"job_id":"` + testJobID + `"}`,
				jobPath(testJobID): testCase.body,
			})
			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil,
				func(document.RenditionResumeHandle) error { return nil })
			assertCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "private source detail")
			assert.NotContains(t, err.Error(), "next-model")
		})
	}
}

func TestNaturalEvidenceRepresentsProviderIdentifiedMissingPages(t *testing.T) {
	chunks := []resultChunk{
		{
			Content: "first", Embed: "first", Blocks: []resultBlock{{
				Content: "first", Type: "Text", BBox: resultBoundingBox{
					Height: new(1.0), Left: new(0.0), Page: new(int64(1)), Top: new(0.0), Width: new(1.0),
				},
			}},
		},
		{
			Content: "", Embed: "", Blocks: []resultBlock{{
				Content: "", Type: "Text", BBox: resultBoundingBox{
					Height: new(1.0), Left: new(0.0), Page: new(int64(2)), Top: new(0.0), Width: new(1.0),
				},
			}},
		},
		{
			Content: "third", Embed: "third", Blocks: []resultBlock{{
				Content: "third", Type: "Text", BBox: resultBoundingBox{
					Height: new(1.0), Left: new(0.0), Page: new(int64(3)), Top: new(0.0), Width: new(1.0),
				},
			}},
		},
	}

	evidence, markdown, err := naturalEvidence(chunks, "pdf", 4)
	require.NoError(t, err)
	assert.Equal(t, document.EvidencePartial, evidence.Completeness)
	require.Len(t, evidence.Units, 2)
	assert.Equal(t, int64(1), evidence.Units[0].Locator.Start)
	assert.Equal(t, int64(3), evidence.Units[1].Locator.Start)
	require.Len(t, evidence.Omissions, 2)
	assert.Equal(t, document.EvidenceOmissionUnit, evidence.Omissions[0].Kind)
	assert.Equal(t, int64(2), evidence.Omissions[0].Locator.Start)
	assert.Equal(t, int64(4), evidence.Omissions[1].Locator.Start)
	assert.Equal(t, "first\n\n---\n\nthird", string(markdown))
}

func TestNaturalEvidenceRejectsUnlocatedContentlessChunk(t *testing.T) {
	_, _, err := naturalEvidence([]resultChunk{{Content: "", Blocks: nil}}, "pdf", 1)
	require.ErrorContains(t, err, "incomplete")
}

func TestClientClassifiesHTTPAndSubmissionFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   document.RenditionErrorCode
	}{
		{name: "authentication", status: http.StatusUnauthorized, want: document.RenditionErrorAuthentication},
		{name: "rate", status: http.StatusTooManyRequests, want: document.RenditionErrorRateLimited},
		{name: "capacity", status: http.StatusServiceUnavailable, want: document.RenditionErrorAmbiguousSubmission},
		{name: "terminal input", status: http.StatusUnsupportedMediaType, want: document.RenditionErrorUnsupportedInput},
		{name: "redirect", status: http.StatusTemporaryRedirect, want: document.RenditionErrorMalformedEvidence},
		{name: "transient", status: http.StatusBadGateway, want: document.RenditionErrorAmbiguousSubmission},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("http source"))
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				result := response(request, testCase.status, `{"detail":"private provider data"}`)
				result.Header.Set("Location", "https://attacker.invalid/redirect")
				return result, nil
			})
			_, err := fixture.client(t, transport).Render(t.Context(), fixture.upload(), fixture.authorization)
			assertCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "private provider data")
			assert.NotContains(t, err.Error(), "attacker.invalid")
		})
	}

	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("ambiguous source"))
	var transport http.RoundTripper = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private transport detail")
	})
	_, err := fixture.client(t, transport).Render(t.Context(), fixture.upload(), fixture.authorization)
	assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	assert.NotContains(t, err.Error(), "private transport detail")

	for _, body := range []string{`{}`, `{"job_id":"UPPER"}`, `{"job_id":"job~suffix"}`} {
		transport = routeTransport(t, map[string]string{
			uploadPath: `{"file_id":"` + testFileID + `","presigned_url":null}`,
			parsePath:  body,
		})
		_, err = fixture.client(t, transport).Render(t.Context(), fixture.upload(), fixture.authorization)
		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	}

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return response(request, status, `{"detail":"private expired job"}`), nil
		})
		_, err = fixture.client(t, transport).RenderResumable(t.Context(), nil, fixture.authorization,
			&document.RenditionResumeHandle{Value: testResumeValue(t, fixture.authorization, testJobID,
				time.Now().UTC().Add(-time.Second), time.Now().UTC(), fixture.metadata.ByteLength)}, nil)
		assertCode(t, err, document.RenditionErrorUnknownJob)
	}

	client, newErr := NewProvider(testProfile(), failingSecrets{}, completedTransport(t))
	require.NoError(t, newErr)
	descriptor := client.Descriptor()
	fixture.authorization.ProviderID = descriptor.ID
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertCode(t, err, document.RenditionErrorAuthentication)
	assert.NotContains(t, err.Error(), "private secret backend")
}

func TestClientEnforcesInputControlPollResultAndArtifactBounds(t *testing.T) {
	t.Run("input identity", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("authorized"))
		bad := &testUpload{Reader: strings.NewReader("different"), metadata: fixture.metadata}
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("identity mismatch reached egress")
			return nil, errors.New("reached egress")
		})
		_, err := fixture.client(t, transport).Render(t.Context(), bad, fixture.authorization)
		assertCode(t, err, document.RenditionErrorPolicyRejected)
	})

	t.Run("control response", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("control"))
		fixture.profile.MaxControlBytes = 32
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return response(request, http.StatusOK, strings.Repeat("x", 33)), nil
		})
		_, err := fixture.client(t, transport).Render(t.Context(), fixture.upload(), fixture.authorization)
		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	})

	t.Run("poll limit", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("poll"))
		fixture.profile.MaxPolls = 2
		transport := routeTransport(t, map[string]string{
			uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
			parsePath:          `{"job_id":"` + testJobID + `"}`,
			jobPath(testJobID): `{"status":"Idle","progress":0,"reason":null,"result":null}`,
		})
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	})

	t.Run("result response", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("result"))
		fixture.profile.MaxResultBytes = 128
		transport := routeTransport(t, map[string]string{
			uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
			parsePath:          `{"job_id":"` + testJobID + `"}`,
			jobPath(testJobID): strings.Repeat("x", 129),
		})
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("structured artifact", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("artifact"))
		fixture.profile.RetainStructured = true
		fixture.profile.MaxArtifacts = 1
		fixture.profile.MaxArtifactBytes = 64
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, completedTransport(t)),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})
}

func TestNaturalEvidenceRejectsUnboundedReportedUnitAllocation(t *testing.T) {
	chunks := []resultChunk{{
		Content: "one",
		Blocks: []resultBlock{{
			Content: "one", Type: "Text",
			BBox: resultBoundingBox{
				Height: new(1.0), Left: new(0.0), Page: new(int64(1)),
				Top: new(0.0), Width: new(1.0), OriginalPage: new(int64(1)),
			},
		}},
	}}
	_, _, err := naturalEvidence(chunks, "pdf", 100_001)
	require.EqualError(t, err, "natural unit count exceeds provider bound")
}

func TestNaturalEvidencePreservesCompleteChunkContent(t *testing.T) {
	chunks := []resultChunk{{
		Content: "Complete provider Markdown",
		Blocks: []resultBlock{{
			Content: "geometry text", Type: "Text",
			BBox: resultBoundingBox{
				Height: new(1.0), Left: new(0.0), Page: new(int64(1)),
				Top: new(0.0), Width: new(1.0), OriginalPage: new(int64(1)),
			},
		}},
	}}
	evidence, markdown, err := naturalEvidence(chunks, "pdf", 1)
	require.NoError(t, err)
	assert.Equal(t, "Complete provider Markdown", evidence.Units[0].Text)
	assert.Equal(t, "Complete provider Markdown", string(markdown))
}

func TestNaturalEvidenceAllowsEmptyImageBlockContent(t *testing.T) {
	chunks := []resultChunk{{
		Content: "Complete provider Markdown",
		Blocks: []resultBlock{
			{
				Content: "", Type: "Figure", ImageURL: new("https://example.invalid/figure.png"),
				BBox: resultBoundingBox{
					Height: new(0.5), Left: new(0.0), Page: new(int64(1)),
					Top: new(0.0), Width: new(0.5), OriginalPage: new(int64(1)),
				},
			},
			{
				Content: "Complete provider Markdown", Type: "Text",
				BBox: resultBoundingBox{
					Height: new(0.5), Left: new(0.0), Page: new(int64(1)),
					Top: new(0.5), Width: new(1.0), OriginalPage: new(int64(1)),
				},
			},
		},
	}}

	evidence, markdown, err := naturalEvidence(chunks, "pdf", 1)
	require.NoError(t, err)
	assert.Equal(t, "Complete provider Markdown", evidence.Units[0].Text)
	assert.Equal(t, "Complete provider Markdown", string(markdown))
}

func TestNaturalEvidenceRejectsInvalidBlockContentUTF8(t *testing.T) {
	chunks := []resultChunk{{
		Content: "Complete provider Markdown",
		Blocks: []resultBlock{{
			Content: string([]byte{0xff}), Type: "Text",
			BBox: resultBoundingBox{
				Height: new(1.0), Left: new(0.0), Page: new(int64(1)),
				Top: new(0.0), Width: new(1.0), OriginalPage: new(int64(1)),
			},
		}},
	}}

	_, _, err := naturalEvidence(chunks, "pdf", 1)
	require.ErrorContains(t, err, "block content is invalid")
}

func TestNaturalEvidenceRejectsBlockTypeOutsidePinnedSDK(t *testing.T) {
	chunks := []resultChunk{{
		Content: "Signed by Synthetic Person",
		Blocks: []resultBlock{{
			Content: "Signed by Synthetic Person", Type: "Signature",
			BBox: resultBoundingBox{
				Height: new(0.1), Left: new(0.1), Page: new(int64(1)),
				Top: new(0.1), Width: new(0.2), OriginalPage: new(int64(1)),
			},
		}},
	}}
	_, _, err := naturalEvidence(chunks, "pdf", 1)
	require.ErrorContains(t, err, "type")
}

func TestStrictJSONRejectsDuplicateMembers(t *testing.T) {
	var job jobResponse
	err := strictJSON([]byte(`{"status":"Pending","status":"Completed","progress":0,"reason":null,"result":null}`), &job)
	require.ErrorContains(t, err, "duplicate")
}

func TestCompletedRequiresPinnedSDKFields(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("required fields"))
	client := fixture.client(t, completedTransport(t))
	valid := completedResultJSON()

	for _, testCase := range []struct {
		name, body string
	}{
		{name: "duration", body: strings.Replace(valid, `,"duration":1.25`, "", 1)},
		{name: "job id", body: strings.Replace(valid, `,"job_id":"`+testJobID+`"`, "", 1)},
		{name: "usage", body: strings.Replace(valid, `"usage":{"num_pages":2},`, "", 1)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			operation := providerutil.NewResumedOperation(t.Context(), provider, time.Second)
			defer operation.Cancel()
			_, err := client.completed(json.RawMessage(testCase.body), testJobID, fixture.authorization,
				&operationState{operation: operation})
			assertCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestClientClassifiesCancellationAfterEgressAsAmbiguous(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("cancel"))
	ctx, cancel := context.WithCancel(t.Context())
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	_, err := fixture.client(t, transport).Render(ctx, fixture.upload(), fixture.authorization)
	assertCode(t, err, document.RenditionErrorCanceled)
}

func TestClientDistinguishesPreEgressContextFailures(t *testing.T) {
	t.Run("wall timeout while reading source", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("wall source"))
		fixture.profile.MaxWallTime = 10 * time.Millisecond
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("wall timeout reached egress")
			return nil, errors.New("unexpected egress")
		})
		upload := newBlockingTestUpload(fixture.metadata)
		_, err := fixture.client(t, transport).Render(t.Context(), upload, fixture.authorization)
		assertCode(t, err, document.RenditionErrorCapacity)
		assert.True(t, upload.wasClosed())
	})

	t.Run("wall timeout resolving secret", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("secret wall source"))
		fixture.profile.MaxWallTime = time.Millisecond
		client, err := NewProvider(fixture.profile, blockingSecrets{}, completedTransport(t))
		require.NoError(t, err)
		descriptor := client.Descriptor()
		fixture.authorization.ProviderID = descriptor.ID
		fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
		fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
		fixture.authorization.AllowedArtifactRoles = nil
		fixture.authorization.MaxArtifacts = 0
		fixture.authorization.MaxArtifactBytes = 0
		_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
		assertCode(t, err, document.RenditionErrorAuthentication)
	})

	t.Run("caller cancellation resolving secret", func(t *testing.T) {
		fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("secret cancel source"))
		ctx, cancel := context.WithCancel(t.Context())
		client, err := NewProvider(fixture.profile, cancelingSecrets{cancel: cancel}, completedTransport(t))
		require.NoError(t, err)
		descriptor := client.Descriptor()
		fixture.authorization.ProviderID = descriptor.ID
		fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
		fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
		fixture.authorization.AllowedArtifactRoles = nil
		fixture.authorization.MaxArtifacts = 0
		fixture.authorization.MaxArtifactBytes = 0
		_, err = client.Render(ctx, fixture.upload(), fixture.authorization)
		assertCode(t, err, document.RenditionErrorAuthentication)
	})
}

func TestClientTreatsExpiryAfterDurableCheckpointAsResumableTimeout(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("durable expiry source"))
	fixture.profile.PollInterval = 50 * time.Millisecond
	fixture.authorization.ExpiresAt = time.Now().UTC().Add(20 * time.Millisecond).Format(timeForm)
	transport := routeTransport(t, map[string]string{
		uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
		parsePath:          `{"job_id":"` + testJobID + `"}`,
		jobPath(testJobID): `{"status":"Pending","progress":0.5,"reason":null,"result":null}`,
	})
	checkpointed := false
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error {
			checkpointed = true
			return nil
		})
	assert.True(t, checkpointed)
	require.ErrorContains(t, err, "authorization is not current")
}

func TestClientTreatsExpiryAfterCompletedResultAsResumableTimeout(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("completed expiry"))
	fixture.authorization.ExpiresAt = time.Now().UTC().Add(100 * time.Millisecond).Format(timeForm)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath:
			return response(request, http.StatusOK, `{"file_id":"`+testFileID+`","presigned_url":null}`), nil
		case parsePath:
			return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
		case jobPath(testJobID):
			time.Sleep(150 * time.Millisecond)
			return response(request, http.StatusOK, completedBody()), nil
		default:
			return nil, errors.New("unexpected route")
		}
	})
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
	require.ErrorContains(t, err, "authorization is not current")
}

func TestClientClosesBlockedUploadOnWallTimeout(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("blocked upload"))
	fixture.profile.MaxWallTime = 10 * time.Millisecond
	upload := newBlockingTestUpload(fixture.metadata)
	client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("blocked upload reached egress")
		return nil, errors.New("blocked upload reached egress")
	}))

	_, err := client.Render(t.Context(), upload, fixture.authorization)
	assertCode(t, err, document.RenditionErrorCapacity)
	assert.True(t, upload.wasClosed())
}

func TestClientAppliesWallDeadlineAfterCompletedResultProcessing(t *testing.T) {
	fixture := newFixture(t, "pdf", "application/pdf", "synthetic.pdf", []byte("final wall"))
	fixture.profile.MaxWallTime = 10 * time.Millisecond
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath:
			return response(request, http.StatusOK, `{"file_id":"`+testFileID+`","presigned_url":null}`), nil
		case parsePath:
			return response(request, http.StatusOK, `{"job_id":"`+testJobID+`"}`), nil
		case jobPath(testJobID):
			time.Sleep(20 * time.Millisecond)
			return response(request, http.StatusOK, completedBody()), nil
		default:
			return nil, errors.New("unexpected route")
		}
	})
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })
	assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
}

func TestProviderRequiresFrozenNamedProfileAndHardenedTransport(t *testing.T) {
	profile := testProfile()
	_, err := NewProvider(profile, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused transport")
	}))
	require.ErrorContains(t, err, "credential")
	_, err = NewProvider(profile, testSecrets{value: "secret"}, nil)
	require.ErrorContains(t, err, "transport")
	profile.SecretBinding = "bad/binding"
	_, err = NewProvider(profile, testSecrets{value: "secret"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused transport")
	}))
	require.ErrorContains(t, err, "binding")

	profile = testProfile()
	client, err := NewProvider(profile, testSecrets{value: "secret"}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused transport")
	}))
	require.NoError(t, err)
	descriptor := client.Descriptor()
	descriptor.SupportedFormats[0].MediaFamily = "changed"
	assert.NotEqual(t, descriptor, client.Descriptor())
	assert.Equal(t, providerID, client.Descriptor().ID)
}

type fixture struct {
	profile       Profile
	metadata      document.AuthorizedUploadMetadata
	authorization document.RenditionAuthorization
	source        []byte
}

func newFixture(t *testing.T, family, mediaType, filename string, source []byte) *fixture {
	t.Helper()
	profile := testProfile()
	client, err := NewProvider(profile, testSecrets{value: "synthetic-secret"},
		roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") }))
	require.NoError(t, err)
	descriptor := client.Descriptor()
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: filename, MediaFamily: family, MediaType: mediaType,
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("1", 64),
		ProviderMetadataChecksum: strings.Repeat("2", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}
	now := time.Now().UTC()
	return &fixture{
		profile: profile, metadata: metadata, source: bytes.Clone(source),
		authorization: document.RenditionAuthorization{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint:           descriptor.PolicyFingerprint,
			RenditionRequestFingerprint: strings.Repeat("3", 64),
			SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
			CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
			ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
			MediaFamily:              family, MediaType: mediaType, InputKind: metadata.InputKind,
			DiscloseFilename:         true,
			MaxProviderMarkdownBytes: 16 << 10, MaxTotalResultBytes: 128 << 10,
			AuthorizedAt: now.Add(-time.Minute).Format(timeForm),
			ExpiresAt:    now.Add(time.Minute).Format(timeForm),
		},
	}
}

func testProfile() Profile {
	return Profile{
		SecretBinding: "reducto-production", MaxUploadBytes: 1 << 20,
		MaxRequestBytes: 2 << 20, MaxControlBytes: 4 << 10,
		MaxPolls: 3, PollInterval: time.Millisecond, RequestTimeout: time.Second,
		MaxResultBytes: 64 << 10, MaxArtifactBytes: 32 << 10,
		MaxWallTime: time.Second,
	}
}

func (fixture *fixture) client(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := NewProvider(fixture.profile, testSecrets{value: "synthetic-secret"}, transport)
	require.NoError(t, err)
	descriptor := client.Descriptor()
	fixture.authorization.ProviderID = descriptor.ID
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	if fixture.profile.RetainStructured {
		fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
		fixture.authorization.MaxArtifacts = 1
		fixture.authorization.MaxArtifactBytes = int(fixture.profile.MaxArtifactBytes)
	} else {
		fixture.authorization.AllowedArtifactRoles = nil
		fixture.authorization.MaxArtifacts = 0
		fixture.authorization.MaxArtifactBytes = 0
	}
	return client
}

func (fixture *fixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

func assertUpload(t *testing.T, request *http.Request, metadata document.AuthorizedUploadMetadata, source []byte) {
	t.Helper()
	assert.Equal(t, http.MethodPost, request.Method)
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	require.NoError(t, err)
	assert.Equal(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	part, err := reader.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "file", part.FormName())
	assert.Equal(t, metadata.Filename, part.FileName())
	assert.Equal(t, metadata.MediaType, part.Header.Get("Content-Type"))
	payload, err := io.ReadAll(part)
	require.NoError(t, err)
	assert.Equal(t, source, payload)
	_, err = reader.NextPart()
	assert.ErrorIs(t, err, io.EOF)
}

func completedBody() string {
	return completedBodyWithChunks(testJobID, `[
		{"blocks":[
			{"bbox":{"height":1,"left":0,"page":1,"top":0,"width":1,"original_page":1},"content":"# First unit","type":"Title","image_url":null},
			{"bbox":{"height":1,"left":0,"page":1,"top":1,"width":1,"original_page":1},"content":"First body","type":"Text","image_url":"https://attacker.invalid/image.png"}
		],"content":"# First unit\n\nFirst body","embed":"First unit First body","enriched":null,"enrichment_success":false},
		{"blocks":[
			{"bbox":{"height":1,"left":0,"page":2,"top":0,"width":1,"original_page":2},"content":"Second unit","type":"Text","image_url":null}
		],"content":"Second unit","embed":"Second unit","enriched":null,"enrichment_success":false}
	]`, 2)
}

func completedBodyWithChunks(jobID, chunks string, pages int) string {
	return `{"status":"Completed","progress":1,"reason":null,"result":{` +
		`"result":{"type":"full","chunks":` + chunks + `,"custom":null,"ocr":null},` +
		`"usage":{"num_pages":` + jsonInt(pages) + `},"duration":1.25,` +
		`"job_id":"` + jobID + `","pdf_url":"https://attacker.invalid/converted.pdf"}}`
}

func completedResultJSON() string {
	body := completedBody()
	var job struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &job); err != nil {
		panic(err)
	}
	return string(job.Result)
}

func jsonInt(value int) string {
	return strconv.Itoa(value)
}

func completedTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return routeTransport(t, map[string]string{
		uploadPath:         `{"file_id":"` + testFileID + `","presigned_url":null}`,
		parsePath:          `{"job_id":"` + testJobID + `"}`,
		jobPath(testJobID): completedBody(),
	})
}

func routeTransport(t *testing.T, routes map[string]string) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, ok := routes[request.URL.Path]
		if !ok {
			t.Fatalf("unexpected route %s", request.URL.String())
		}
		return response(request, http.StatusOK, body), nil
	})
}

type testResumePayload struct {
	JobID                    string `json:"j"`
	AuthorizationFingerprint string `json:"f"`
	StartedAt                string `json:"s"`
	SubmittedAt              string `json:"a"`
	Requests                 int64  `json:"q"`
	Retries                  int64  `json:"r"`
	InputBytes               int64  `json:"i"`
	OutputBytes              int64  `json:"o"`
	RetryDelayMillis         int64  `json:"d"`
}

func testResumeValue(
	t *testing.T, authorization document.RenditionAuthorization,
	jobID string, startedAt, submittedAt time.Time, inputBytes int64,
) string {
	t.Helper()
	authorizationFingerprint, err := authorization.Fingerprint()
	require.NoError(t, err)
	raw, err := json.Marshal(testResumePayload{
		JobID: jobID, AuthorizationFingerprint: authorizationFingerprint,
		StartedAt: startedAt.UTC().Format(timeForm), SubmittedAt: submittedAt.UTC().Format(timeForm),
		Requests: 2, InputBytes: inputBytes, OutputBytes: 1,
	})
	require.NoError(t, err)
	return "r3." + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTestResumeHandle(t *testing.T, value string) testResumePayload {
	t.Helper()
	require.True(t, strings.HasPrefix(value, "r3."))
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, "r3."))
	require.NoError(t, err)
	var payload testResumePayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	return payload
}

func executionSnapshot(
	t *testing.T, at time.Time, client *Client, fixture *fixture,
) document.RenditionExecutionSnapshotV1 {
	t.Helper()
	evidence, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	rendition, err := document.NewRenditionPolicy(document.RenditionLimits{
		MaxDocumentChars: 100_000, MaxUnitRunes: 1_000_000, MaxSegmentRunes: 1_000,
	})
	require.NoError(t, err)
	snapshot, err := document.SealRenditionExecutionAt(
		at, client, fixture.upload(), fixture.authorization, evidence, rendition)
	require.NoError(t, err)
	return snapshot
}

type testUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }
func (*testUpload) Close() error                                       { return nil }

type blockingTestUpload struct {
	metadata document.AuthorizedUploadMetadata
	closed   chan struct{}
}

func newBlockingTestUpload(metadata document.AuthorizedUploadMetadata) *blockingTestUpload {
	return &blockingTestUpload{metadata: metadata, closed: make(chan struct{})}
}

func (upload *blockingTestUpload) Metadata() document.AuthorizedUploadMetadata {
	return upload.metadata
}

func (upload *blockingTestUpload) Read([]byte) (int, error) {
	<-upload.closed
	return 0, io.EOF
}

func (upload *blockingTestUpload) Close() error {
	select {
	case <-upload.closed:
	default:
		close(upload.closed)
	}
	return nil
}

func (upload *blockingTestUpload) wasClosed() bool {
	select {
	case <-upload.closed:
		return true
	default:
		return false
	}
}

type testSecrets struct {
	value string
}

type failingSecrets struct{}

type blockingSecrets struct{}

func (blockingSecrets) ResolveSecret(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

type cancelingSecrets struct{ cancel context.CancelFunc }

func (secrets cancelingSecrets) ResolveSecret(ctx context.Context, _ string) (string, error) {
	secrets.cancel()
	<-ctx.Done()
	return "", ctx.Err()
}

func (failingSecrets) ResolveSecret(context.Context, string) (string, error) {
	return "", errors.New("private secret backend detail")
}

func (secrets testSecrets) ResolveSecret(context.Context, string) (string, error) {
	return secrets.value, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err := request.Body.Close(); err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
	}
	return function(request)
}

func response(request *http.Request, status int, body string) *http.Response {
	response := &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
	response.Header.Set("Content-Type", providerutil.JSONMediaType)
	return response
}

func assertCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerError, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "expected classified provider error, got %T: %v", err, err)
	assert.Equal(t, want, providerError.Code())
}
