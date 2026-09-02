package llamaparse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/media/mediatest"
)

const testJobID = "123e4567-e89b-12d3-a456-426614174000"

var _ document.ResumableRenditionProvider = (*Client)(nil)

func TestClientUploadsExactAuthorizedBytesAndMapsNaturalPages(t *testing.T) {
	source := []byte("%PDF-1.7\nsynthetic exact bytes\n%%EOF\n")
	fixture := newFixture(t, source)
	var requests []*http.Request
	pollCount := 0
	var transport http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.Clone(request.Context()))
		assert.Equal(t, "Bearer synthetic-secret", request.Header.Get("Authorization"))
		switch request.URL.Path {
		case uploadPath:
			assert.Equal(t, http.MethodPost, request.Method)
			mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			require.NoError(t, err)
			assert.Equal(t, "multipart/form-data", mediaType)
			reader := multipart.NewReader(request.Body, parameters["boundary"])
			fields := map[string]string{}
			for {
				part, nextErr := reader.NextPart()
				if errors.Is(nextErr, io.EOF) {
					break
				}
				require.NoError(t, nextErr)
				payload, readErr := io.ReadAll(part)
				require.NoError(t, readErr)
				if part.FormName() == "file" {
					assert.Equal(t, "synthetic.pdf", part.FileName())
					assert.Equal(t, "application/pdf", part.Header.Get("Content-Type"))
					assert.Equal(t, source, payload)
				} else {
					fields[part.FormName()] = string(payload)
				}
			}
			assert.Equal(t, "parse-model-v1", fields["model"])
			assert.Equal(t, "document-v1", fields["preset"])
			assert.Equal(t, "0", fields["page_error_tolerance"])
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"PENDING"}`), nil
		case statusPath(testJobID):
			pollCount++
			if pollCount == 1 {
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"PENDING"}`), nil
			}
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case jsonResultPath(testJobID):
			return response(request, http.StatusOK, `{
				"pages":[
					{"page":1,"text":"First page","md":"# First page","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false},
					{"page":2,"text":"Second page","md":"Second page","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}
				],
				"job_metadata":{"job_pages":2}
			}`), nil
		default:
			t.Fatalf("unexpected route %s", request.URL.String())
			return nil, errors.New("unexpected route")
		}
	})
	client := fixture.client(t, transport)
	var checkpoint document.RenditionResumeHandle
	result, err := document.RenderRenditionWithResume(
		t.Context(), client, fixture.upload(), fixture.authorization, nil,
		func(handle document.RenditionResumeHandle) error {
			checkpoint = handle
			return nil
		},
	)
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(checkpoint.Value, "lp2."+testJobID+"."))
	handleParts := strings.Split(checkpoint.Value, ".")
	require.Len(t, handleParts, 5)
	authorizationFingerprint, err := fixture.authorization.Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, authorizationFingerprint, handleParts[2])
	checkpointNanos, err := strconv.ParseInt(handleParts[4], 10, 64)
	require.NoError(t, err)
	completedAt, err := time.Parse(timeForm, result.Receipt.CompletedAt)
	require.NoError(t, err)
	assert.True(t, completedAt.After(time.Unix(0, checkpointNanos)),
		"fresh completion must follow the durable checkpoint")
	assert.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitPage, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 2)
	assert.Equal(t, document.EvidenceIndexOriginOne, result.Evidence.Units[0].Locator.IndexOrigin)
	assert.Equal(t, int64(1), result.Evidence.Units[0].Locator.Start)
	assert.Equal(t, int64(2), result.Evidence.Units[1].Locator.Start)
	assert.Equal(t, "# First page", result.Evidence.Units[0].Text)
	assert.Equal(t, "# First page\n\n---\n\nSecond page", string(result.ProviderMarkdown))
	assert.Equal(t, int64(1), result.Receipt.Usage.Retries)
	assert.Equal(t, testJobID[:8], result.Receipt.OperationID[len("llamaparse-"):])
	require.Len(t, requests, 4)
	for _, request := range requests {
		assert.Equal(t, "https", request.URL.Scheme)
		assert.Equal(t, apiHost, request.URL.Host)
		assert.Empty(t, request.URL.RawQuery)
	}
	assert.Equal(t, 4, fixture.secrets.calls())
}

func TestClientResumesWithoutUploadingSource(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nresume\n%%EOF\n"))
	var paths []string
	var transport http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case statusPath(testJobID):
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case jsonResultPath(testJobID):
			return response(request, http.StatusOK,
				`{"pages":[{"page":1,"text":"Resumed","md":"Resumed","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
		default:
			t.Fatalf("unexpected route %s", request.URL.Path)
			return nil, errors.New("unexpected route")
		}
	})
	client := fixture.client(t, transport)
	now := time.Now().UTC()
	result, err := client.RenderResumable(t.Context(), nil, fixture.authorization,
		&document.RenditionResumeHandle{Value: testResumeHandle(
			t, fixture.authorization, now.Add(-time.Second), now)}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{statusPath(testJobID), jsonResultPath(testJobID)}, paths)
	assert.Equal(t, "Resumed", result.Evidence.Units[0].Text)
}

func TestClientRejectsInvalidResumeFactsWithoutEgress(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\ninvalid resume\n%%EOF\n"))
	authorizedAt, err := time.Parse(timeForm, fixture.authorization.AuthorizedAt)
	require.NoError(t, err)
	expiresAt, err := time.Parse(timeForm, fixture.authorization.ExpiresAt)
	require.NoError(t, err)
	validSubmittedAt := authorizedAt.Add(time.Nanosecond)
	validCheckpointedAt := validSubmittedAt.Add(time.Nanosecond)
	authorizationFingerprint, err := fixture.authorization.Fingerprint()
	require.NoError(t, err)
	tests := []struct {
		name   string
		handle string
		want   document.RenditionErrorCode
	}{
		{
			name: "submitted before authorization",
			handle: testResumeHandle(t, fixture.authorization,
				authorizedAt.Add(-time.Nanosecond), validCheckpointedAt),
			want: document.RenditionErrorPolicyRejected,
		},
		{
			name: "checkpoint after expiry",
			handle: testResumeHandle(t, fixture.authorization,
				validSubmittedAt, expiresAt.Add(time.Nanosecond)),
			want: document.RenditionErrorPolicyRejected,
		},
		{
			name: "noncanonical timestamp",
			handle: fmt.Sprintf("lp2.%s.%s.0%d.%d", testJobID, authorizationFingerprint,
				validSubmittedAt.UnixNano(), validCheckpointedAt.UnixNano()),
			want: document.RenditionErrorUnknownJob,
		},
		{
			name: "unparseable timestamp",
			handle: fmt.Sprintf("lp2.%s.%s.not-a-time.%d", testJobID,
				authorizationFingerprint, validCheckpointedAt.UnixNano()),
			want: document.RenditionErrorUnknownJob,
		},
		{
			name: "wrong version",
			handle: strings.Replace(testResumeHandle(t, fixture.authorization,
				validSubmittedAt, validCheckpointedAt), "lp2.", "lp3.", 1),
			want: document.RenditionErrorUnknownJob,
		},
		{name: "legacy bare job ID", handle: testJobID, want: document.RenditionErrorUnknownJob},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid resume handle reached egress")
				return nil, errors.New("unexpected egress")
			}))

			_, err := client.RenderResumable(t.Context(), nil, fixture.authorization,
				&document.RenditionResumeHandle{Value: testCase.handle}, nil)

			assertCode(t, err, testCase.want)
			assert.Zero(t, fixture.secrets.calls())
		})
	}
}

func TestClientRejectsResumeHandleFromDifferentAuthorizationWithoutEgress(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nbound resume\n%%EOF\n"))
	now := time.Now().UTC()
	handle := testResumeHandle(t, fixture.authorization, now.Add(-time.Second), now)
	tests := []struct {
		name   string
		mutate func(*document.RenditionAuthorization)
	}{
		{
			name: "source",
			mutate: func(authorization *document.RenditionAuthorization) {
				authorization.SourceSHA256 = strings.Repeat("9", 64)
			},
		},
		{
			name: "request",
			mutate: func(authorization *document.RenditionAuthorization) {
				authorization.RenditionRequestFingerprint = strings.Repeat("8", 64)
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			authorization := fixture.authorization
			testCase.mutate(&authorization)
			client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("mismatched resume authority reached egress")
				return nil, errors.New("unexpected egress")
			}))

			_, err := client.RenderResumable(t.Context(), nil, authorization,
				&document.RenditionResumeHandle{Value: handle}, nil)

			assertCode(t, err, document.RenditionErrorPolicyRejected)
			assert.Zero(t, fixture.secrets.calls())
		})
	}
}

func TestClientRejectsMultipartFilenameNewlinesBeforeSubmission(t *testing.T) {
	for _, filename := range []string{"report\r.pdf", "report\n.pdf"} {
		t.Run(strconv.Quote(filename), func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nfilename\n%%EOF\n"))
			fixture.metadata.Filename = filename
			client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsafe filename reached egress")
				return nil, errors.New("unexpected egress")
			}))

			_, err := document.RenderRenditionWithResume(t.Context(), client, fixture.upload(),
				fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, document.RenditionErrorPolicyRejected)
			assert.Zero(t, fixture.secrets.calls())
		})
	}
}

func TestClientUsesSyntheticFilenameWhenDisclosureIsWithheld(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nwithheld filename\n%%EOF\n"))
	fixture.authorization.DiscloseFilename = false
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath:
			_, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			require.NoError(t, err)
			part, err := multipart.NewReader(request.Body, parameters["boundary"]).NextPart()
			require.NoError(t, err)
			assert.Equal(t, "document.pdf", part.FileName())
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case statusPath(testJobID):
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case jsonResultPath(testJobID):
			return response(request, http.StatusOK,
				`{"pages":[{"page":1,"md":"Page","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
		default:
			t.Fatalf("unexpected route %s", request.URL.Path)
			return nil, errors.New("unexpected route")
		}
	})

	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

	require.NoError(t, err)
}

func TestClientPreservesExplicitMiddleBlankPage(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nblank page\n%%EOF\n"))
	transport := routeTransport(t, map[string]routeResponse{
		uploadPath:            {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		statusPath(testJobID): {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		jsonResultPath(testJobID): {body: `{"pages":[
			{"page":1,"md":"First","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false},
			{"page":2,"md":"","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":true,"triggeredAutoMode":false},
			{"page":3,"md":"Third","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}
		],"job_metadata":{"job_pages":3}}`},
	})

	result, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 3)
	assert.Empty(t, result.Evidence.Units[1].Text)
	assert.Equal(t, int64(2), result.Evidence.Units[1].Locator.Start)
}

func TestClientUsesExplicitDegradedMarkdownOnlyWithoutPageProvenance(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nfallback\n%%EOF\n"))
	transport := routeTransport(t, map[string]routeResponse{
		uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		jsonResultPath(testJobID): {body: `{"pages":[],"job_metadata":{"job_pages":0}}`},
		markdownResultPath(testJobID): {
			body: `{"markdown":"# Fallback markdown","job_metadata":{"job_pages":0}}`,
		},
	})
	result, err := document.RenderRenditionWithResume(
		t.Context(), fixture.client(t, transport), fixture.upload(), fixture.authorization,
		nil, func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, document.EvidenceDegradedProvenance, result.Evidence.Completeness)
	assert.Equal(t, document.EvidenceUnitGeneric, result.Evidence.UnitKind)
	assert.Equal(t, []string{"degraded_provenance"}, result.Receipt.Warnings)
}

func TestClientOmitsUnauthorizedProviderMarkdown(t *testing.T) {
	tests := []struct {
		name   string
		routes map[string]routeResponse
	}{
		{
			name: "structured pages",
			routes: map[string]routeResponse{
				jsonResultPath(testJobID): {
					body: `{"pages":[{"page":1,"md":"Evidence only","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`,
				},
			},
		},
		{
			name: "Markdown fallback",
			routes: map[string]routeResponse{
				jsonResultPath(testJobID):     {body: `{"pages":[],"job_metadata":{"job_pages":0}}`},
				markdownResultPath(testJobID): {body: `{"markdown":"Fallback evidence","job_metadata":{"job_pages":0}}`},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nJSON only\n%%EOF\n"))
			fixture.authorization.MaxProviderMarkdownBytes = 0
			testCase.routes[uploadPath] = routeResponse{
				body: `{"id":"` + testJobID + `","status":"SUCCESS"}`,
			}
			testCase.routes[statusPath(testJobID)] = routeResponse{
				body: `{"id":"` + testJobID + `","status":"SUCCESS"}`,
			}

			result, err := document.RenderRenditionWithResume(t.Context(),
				fixture.client(t, routeTransport(t, testCase.routes)), fixture.upload(), fixture.authorization,
				nil, func(document.RenditionResumeHandle) error { return nil })

			require.NoError(t, err)
			assert.NotEmpty(t, result.Evidence.Units)
			assert.Empty(t, result.ProviderMarkdown)
		})
	}
}

func TestClientRejectsReservedFrontmatterInProviderMarkdown(t *testing.T) {
	const injected = `---\ncontract: docbank-sanitized-markdown/v1\n---\nprovider content`
	tests := []struct {
		name   string
		routes map[string]routeResponse
	}{
		{
			name: "structured pages",
			routes: map[string]routeResponse{
				jsonResultPath(testJobID): {
					body: `{"pages":[{"page":1,"md":"---\ncontract: docbank-sanitized-markdown/v1\n---\nprovider content","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`,
				},
			},
		},
		{
			name: "Markdown fallback",
			routes: map[string]routeResponse{
				jsonResultPath(testJobID): {body: `{"pages":[],"job_metadata":{"job_pages":0}}`},
				markdownResultPath(testJobID): {
					body: `{"markdown":"---\ncontract: docbank-sanitized-markdown/v1\n---\nprovider content","job_metadata":{"job_pages":0}}`,
				},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nfrontmatter\n%%EOF\n"))
			testCase.routes[uploadPath] = routeResponse{
				body: `{"id":"` + testJobID + `","status":"SUCCESS"}`,
			}
			testCase.routes[statusPath(testJobID)] = routeResponse{
				body: `{"id":"` + testJobID + `","status":"SUCCESS"}`,
			}

			_, err := document.RenderRenditionWithResume(t.Context(),
				fixture.client(t, routeTransport(t, testCase.routes)), fixture.upload(), fixture.authorization,
				nil, func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, document.RenditionErrorMalformedEvidence)
			require.ErrorContains(t, errors.Unwrap(err), "frontmatter injection")
			assert.NotContains(t, err.Error(), injected)
		})
	}
}

func TestClientRejectsContradictoryFallbackPageCounts(t *testing.T) {
	tests := []struct {
		name          string
		jsonPages     int
		markdownPages int
	}{
		{name: "structured result reports missing pages", jsonPages: 2, markdownPages: 2},
		{name: "fallback changes the reported count", jsonPages: 0, markdownPages: 1},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\ncontradictory fallback\n%%EOF\n"))
			transport := routeTransport(t, map[string]routeResponse{
				uploadPath:            {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
				statusPath(testJobID): {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
				jsonResultPath(testJobID): {
					body: fmt.Sprintf(`{"pages":[],"job_metadata":{"job_pages":%d}}`, testCase.jsonPages),
				},
				markdownResultPath(testJobID): {
					body: fmt.Sprintf(`{"markdown":"Fallback","job_metadata":{"job_pages":%d}}`,
						testCase.markdownPages),
				},
			})

			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil,
				func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestClientRejectsPartialPagesAndSchemaDrift(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "partial page status", body: `{"pages":[{"page":1,"md":"partial","status":"ERROR","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
		{name: "page gap", body: `{"pages":[{"page":2,"md":"gap","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
		{name: "unknown page field", body: `{"pages":[{"page":1,"md":"drift","provider_url":"https://evil.example/result","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
		{name: "provider page count exceeds result", body: `{"pages":[{"page":1,"md":"partial","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":2}}`},
		{name: "missing explicit page index", body: `{"pages":[{"md":"invented zero","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
		{name: "missing provider page count", body: `{"pages":[{"page":1,"md":"unproven","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{}}`},
		{name: "unexplained empty page", body: `{"pages":[{"page":1,"md":"","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\npartial\n%%EOF\n"))
			transport := routeTransport(t, map[string]routeResponse{
				uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
				statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
				jsonResultPath(testJobID): {body: testCase.body},
			})
			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil,
				func(document.RenditionResumeHandle) error { return nil })
			assertCode(t, err, document.RenditionErrorMalformedEvidence)
			assert.NotContains(t, err.Error(), "evil.example")
		})
	}
}

func TestClientRejectsReportedModelDrift(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nmodel drift\n%%EOF\n"))
	transport := routeTransport(t, map[string]routeResponse{
		uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		jsonResultPath(testJobID): {body: `{"pages":[{"page":1,"md":"drift","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1,"model":"parse-model-next","preset":"document-v1"}}`},
	})
	_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	assertCode(t, err, document.RenditionErrorPolicyRejected)
}

func TestClientClassifiesHostedFailuresWithoutLeakingProviderBodies(t *testing.T) {
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
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nerrors\n%%EOF\n"))
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := response(request, testCase.status,
					`{"detail":"provider body secret synthetic-source"}`)
				response.Header.Set("Location", "https://evil.example/stolen")
				return response, nil
			})
			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil, nil)
			assertCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "provider body")
			assert.NotContains(t, err.Error(), "evil.example")
		})
	}
}

func TestClientClassifiesProviderJobErrorsByCode(t *testing.T) {
	tests := []struct {
		name string
		code *string
		want document.RenditionErrorCode
	}{
		{name: "unsupported file", code: new("UNSUPPORTED_FILE_TYPE"), want: document.RenditionErrorUnsupportedInput},
		{name: "document too large", code: new("DOCUMENT_TOO_LARGE"), want: document.RenditionErrorPolicyRejected},
		{name: "processing failure", code: new("ERROR_DURING_PROCESSING"), want: document.RenditionErrorMalformedEvidence},
		{name: "reconstruction failure", code: new("RECONSTRUCTION_ERROR"), want: document.RenditionErrorMalformedEvidence},
		{name: "Markdown failure", code: new("MARKDOWN_EXTRACTION_FAILED"), want: document.RenditionErrorMalformedEvidence},
		{name: "unknown code", code: new("NEW_PROVIDER_ERROR"), want: document.RenditionErrorMalformedEvidence},
		{name: "missing code", want: document.RenditionErrorMalformedEvidence},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\njob error\n%%EOF\n"))
			status := map[string]any{
				"id": testJobID, "status": "ERROR", "error_message": "private provider detail",
			}
			if testCase.code != nil {
				status["error_code"] = *testCase.code
			}
			statusBody, err := json.Marshal(status)
			require.NoError(t, err)
			transport := routeTransport(t, map[string]routeResponse{
				uploadPath:            {body: `{"id":"` + testJobID + `","status":"PENDING"}`},
				statusPath(testJobID): {body: string(statusBody)},
			})

			_, err = document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil,
				func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, testCase.want)
			assert.NotContains(t, err.Error(), "private provider detail")
		})
	}
}

func TestClientClassifiesAmbiguousSubmissionAndUnknownJobs(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nambiguous\n%%EOF\n"))
	client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("private transport failure")
	}))
	_, err := document.RenderRenditionWithResume(t.Context(), client, fixture.upload(),
		fixture.authorization, nil, nil)
	assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	assert.NotContains(t, err.Error(), "private transport")

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		client = fixture.client(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return response(request, status, `{"detail":"private unknown detail"}`), nil
		}))
		now := time.Now().UTC()
		_, err = client.RenderResumable(t.Context(), nil, fixture.authorization,
			&document.RenditionResumeHandle{Value: testResumeHandle(
				t, fixture.authorization, now.Add(-time.Second), now)}, nil)
		assertCode(t, err, document.RenditionErrorUnknownJob)
	}
}

func TestClientTreatsRetryableKnownJobRequestFailuresAsAmbiguous(t *testing.T) {
	tests := []struct {
		name           string
		failPath       string
		transportError bool
		fallback       bool
		image          bool
	}{
		{name: "poll transport", failPath: statusPath(testJobID), transportError: true},
		{name: "JSON result HTTP", failPath: jsonResultPath(testJobID)},
		{name: "Markdown fallback HTTP", failPath: markdownResultPath(testJobID), fallback: true},
		{name: "image HTTP", failPath: imageResultPath(testJobID, "figure-1.png"), image: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nknown job\n%%EOF\n"))
			if testCase.image {
				fixture.profile.RetainImages = true
				fixture.profile.MaxArtifacts = 1
				fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{
					document.EvidenceArtifactImage,
				}
				fixture.authorization.MaxArtifacts = 1
				fixture.authorization.MaxArtifactBytes = 1024
			}
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == testCase.failPath {
					if testCase.transportError {
						return nil, errors.New("private transport failure")
					}
					return response(request, http.StatusServiceUnavailable, `{"detail":"private failure"}`), nil
				}
				switch request.URL.Path {
				case uploadPath, statusPath(testJobID):
					return response(request, http.StatusOK,
						`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
				case jsonResultPath(testJobID):
					if testCase.fallback {
						return response(request, http.StatusOK,
							`{"pages":[],"job_metadata":{"job_pages":0}}`), nil
					}
					return response(request, http.StatusOK,
						`{"pages":[{"page":1,"md":"Known job","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
				default:
					t.Fatalf("unexpected route %s", request.URL.String())
					return nil, errors.New("unexpected route")
				}
			})

			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil,
				func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
			assert.NotContains(t, err.Error(), "private")
		})
	}
}

func TestClientPreservesPreEgressCredentialFailureClassification(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nauth failure\n%%EOF\n"))
	fixture.secrets.value = ""
	client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("credential failure reached egress")
		return nil, errors.New("unexpected egress")
	}))

	_, err := document.RenderRenditionWithResume(t.Context(), client, fixture.upload(),
		fixture.authorization, nil, nil)

	assertCode(t, err, document.RenditionErrorAuthentication)
}

func TestClientPreservesCredentialFailureClassificationAcrossResultStages(t *testing.T) {
	t.Run("poll", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\npoll credential\n%%EOF\n"))
		fixture.secrets.failAt = 2
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != uploadPath {
				t.Fatalf("credential failure reached %s egress", request.URL.Path)
			}
			return response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorAuthentication)
	})

	t.Run("result", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nresult credential\n%%EOF\n"))
		fixture.secrets.failAt = 3
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case uploadPath, statusPath(testJobID):
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
			default:
				t.Fatalf("credential failure reached %s egress", request.URL.Path)
				return nil, errors.New("unexpected egress")
			}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorAuthentication)
	})

	t.Run("image", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nimage credential\n%%EOF\n"))
		fixture.profile.RetainImages = true
		fixture.profile.MaxArtifacts = 1
		fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
		fixture.authorization.MaxArtifacts = 1
		fixture.authorization.MaxArtifactBytes = 1024
		fixture.secrets.failAt = 4
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case uploadPath, statusPath(testJobID):
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
			case jsonResultPath(testJobID):
				return response(request, http.StatusOK, `{"pages":[{"page":1,"md":"Image page","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
			default:
				t.Fatalf("credential failure reached %s egress", request.URL.Path)
				return nil, errors.New("unexpected egress")
			}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorAuthentication)
	})
}

func TestClientRefusesProviderAuthoredArtifactURLsAndUsesFixedImageRoute(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nimage\n%%EOF\n"))
	fixture.profile.RetainImages = true
	fixture.profile.MaxArtifacts = 1
	fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
	fixture.authorization.MaxArtifacts = 1
	fixture.authorization.MaxArtifactBytes = 128
	var paths []string
	var transport http.RoundTripper = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case uploadPath:
			return response(request, http.StatusOK, `{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case statusPath(testJobID):
			return response(request, http.StatusOK, `{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case jsonResultPath(testJobID):
			return response(request, http.StatusOK, `{"pages":[{"page":1,"md":"Image page","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
		case imageResultPath(testJobID, "figure-1.png"):
			result := bytesResponse(request, http.StatusOK, mediatest.PNG(4, 3, nil))
			result.Header.Set("Content-Type", "image/png")
			return result, nil
		default:
			t.Fatalf("unexpected route %s", request.URL.String())
			return nil, errors.New("unexpected route")
		}
	})
	result, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	require.NoError(t, err)
	require.Len(t, result.Artifacts, 1)
	assert.Equal(t, mediatest.PNG(4, 3, nil), result.Artifacts[0].Payload)
	assert.Equal(t, imageResultPath(testJobID, "figure-1.png"), paths[len(paths)-1])

	for _, name := range []string{"https://evil.example/a.png", ".", ".."} {
		paths = nil
		transport = routeTransport(t, map[string]routeResponse{
			uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			jsonResultPath(testJobID): {body: `{"pages":[{"page":1,"md":"unsafe","images":[{"name":"` + name + `"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`},
		})
		_, err = document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	}
}

func TestClientRejectsMalformedOrMismatchedRetainedImages(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mediaType string
		payload   []byte
	}{
		{name: "malformed", mediaType: "image/png", payload: []byte("not a PNG")},
		{name: "mismatched", mediaType: "image/jpeg", payload: mediatest.PNG(4, 3, nil)},
		{name: "animated", mediaType: "image/gif", payload: mediatest.GIF(2, 2, 2)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nimage validation\n%%EOF\n"))
			fixture.profile.RetainImages = true
			fixture.profile.MaxArtifacts = 1
			fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
			fixture.authorization.MaxArtifacts = 1
			fixture.authorization.MaxArtifactBytes = 1024
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case uploadPath, statusPath(testJobID):
					return response(request, http.StatusOK, `{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
				case jsonResultPath(testJobID):
					return response(request, http.StatusOK, `{"pages":[{"page":1,"md":"Image page","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
				case imageResultPath(testJobID, "figure-1.png"):
					result := bytesResponse(request, http.StatusOK, testCase.payload)
					result.Header.Set("Content-Type", testCase.mediaType)
					return result, nil
				default:
					return nil, errors.New("unexpected route")
				}
			})

			_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
				fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

			assertCode(t, err, document.RenditionErrorMalformedEvidence)
		})
	}
}

func TestClientEnforcesUploadPollResultAndArtifactBounds(t *testing.T) {
	t.Run("upload identity", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nidentity\n%%EOF\n"))
		upload := &testUpload{Reader: bytes.NewReader([]byte("different")), metadata: fixture.metadata}
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t,
			roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("mismatched source reached egress")
				return nil, errors.New("mismatched source reached egress")
			})), upload, fixture.authorization, nil, nil)
		assertCode(t, err, document.RenditionErrorPolicyRejected)
	})

	t.Run("poll limit", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\npoll\n%%EOF\n"))
		fixture.profile.MaxPolls = 2
		transport := routeTransport(t, map[string]routeResponse{
			uploadPath:            {body: `{"id":"` + testJobID + `","status":"PENDING"}`},
			statusPath(testJobID): {body: `{"id":"` + testJobID + `","status":"PENDING"}`},
		})
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	})

	t.Run("result bytes", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nresult\n%%EOF\n"))
		fixture.profile.MaxResultBytes = 64
		transport := routeTransport(t, map[string]routeResponse{
			uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			jsonResultPath(testJobID): {body: strings.Repeat("x", 65)},
		})
		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })
		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("fallback after exact result budget", func(t *testing.T) {
		const resultBody = `{"pages":[],"job_metadata":{"job_pages":0}}`
		fixture := newFixture(t, []byte("%PDF-1.7\nno fallback budget\n%%EOF\n"))
		fixture.authorization.MaxProviderMarkdownBytes = 0
		fixture.authorization.MaxTotalResultBytes = len(resultBody)
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case uploadPath, statusPath(testJobID):
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
			case jsonResultPath(testJobID):
				return response(request, http.StatusOK, resultBody), nil
			case markdownResultPath(testJobID):
				t.Fatal("exhausted result budget reached fallback egress")
				return nil, errors.New("unexpected egress")
			default:
				t.Fatalf("unexpected route %s", request.URL.Path)
				return nil, errors.New("unexpected egress")
			}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("image after exact result budget", func(t *testing.T) {
		const resultBody = `{"pages":[{"page":1,"md":"Image page","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`
		fixture := newFixture(t, []byte("%PDF-1.7\nno image budget\n%%EOF\n"))
		fixture.profile.RetainImages = true
		fixture.profile.MaxArtifacts = 1
		fixture.authorization.MaxProviderMarkdownBytes = 0
		fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
		fixture.authorization.MaxArtifacts = 1
		fixture.authorization.MaxArtifactBytes = 128
		fixture.authorization.MaxTotalResultBytes = len(resultBody)
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case uploadPath, statusPath(testJobID):
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
			case jsonResultPath(testJobID):
				return response(request, http.StatusOK, resultBody), nil
			case imageResultPath(testJobID, "figure-1.png"):
				t.Fatal("exhausted result budget reached image egress")
				return nil, errors.New("unexpected egress")
			default:
				t.Fatalf("unexpected route %s", request.URL.Path)
				return nil, errors.New("unexpected egress")
			}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorMalformedEvidence)
	})

	t.Run("image with zero artifact byte limit", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nno artifact budget\n%%EOF\n"))
		fixture.profile.RetainImages = true
		fixture.profile.MaxArtifacts = 1
		fixture.authorization.MaxProviderMarkdownBytes = 0
		fixture.authorization.AllowedArtifactRoles = []document.EvidenceArtifactRole{document.EvidenceArtifactImage}
		fixture.authorization.MaxArtifacts = 1
		fixture.authorization.MaxArtifactBytes = 0
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case uploadPath, statusPath(testJobID):
				return response(request, http.StatusOK,
					`{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
			case jsonResultPath(testJobID):
				return response(request, http.StatusOK,
					`{"pages":[{"page":1,"md":"Image page","images":[{"name":"figure-1.png"}],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`), nil
			case imageResultPath(testJobID, "figure-1.png"):
				t.Fatal("zero artifact byte limit reached image egress")
				return nil, errors.New("unexpected egress")
			default:
				t.Fatalf("unexpected route %s", request.URL.Path)
				return nil, errors.New("unexpected egress")
			}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil,
			func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorPolicyRejected)
	})
}

func TestClientClassifiesExactUploadReadLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*fixture)
		duringRead func(context.CancelFunc)
		wantCode   document.RenditionErrorCode
		wantError  error
	}{
		{
			name: "caller cancellation",
			duringRead: func(cancel context.CancelFunc) {
				cancel()
			},
			wantError: context.Canceled,
		},
		{
			name: "authorization expiry",
			configure: func(fixture *fixture) {
				fixture.authorization.ExpiresAt = time.Now().UTC().Add(20 * time.Millisecond).Format(timeForm)
			},
			duringRead: func(context.CancelFunc) {
				time.Sleep(40 * time.Millisecond)
			},
			wantError: document.ErrRenditionAuthorizationExpired,
		},
		{
			name: "wall timeout",
			configure: func(fixture *fixture) {
				fixture.profile.MaxWallTime = 20 * time.Millisecond
			},
			duringRead: func(context.CancelFunc) {
				time.Sleep(40 * time.Millisecond)
			},
			wantCode: document.RenditionErrorCapacity,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newFixture(t, []byte("%PDF-1.7\nslow upload\n%%EOF\n"))
			if testCase.configure != nil {
				testCase.configure(fixture)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			upload := &testUpload{
				Reader: &callbackReadCloser{
					reader: bytes.NewReader(fixture.source),
					before: func() { testCase.duringRead(cancel) },
				},
				metadata: fixture.metadata,
			}
			client := fixture.client(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("expired upload reached egress")
				return nil, errors.New("unexpected egress")
			}))

			_, err := document.RenderRenditionWithResume(ctx, client, upload,
				fixture.authorization, nil, nil)

			if testCase.wantError != nil {
				assert.ErrorIs(t, err, testCase.wantError)
			} else {
				assertCode(t, err, testCase.wantCode)
			}
		})
	}
}

func TestClientClassifiesCancellationAndAuthorizationExpiry(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\ncancel\n%%EOF\n"))
	ctx, cancel := context.WithCancel(t.Context())
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == uploadPath {
			return response(request, http.StatusOK, `{"id":"`+testJobID+`","status":"PENDING"}`), nil
		}
		cancel()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	_, err := document.RenderRenditionWithResume(ctx, fixture.client(t, transport),
		fixture.upload(), fixture.authorization, nil,
		func(document.RenditionResumeHandle) error { return nil })
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClientResumesHistoricalSealedAuthorizationWithRecordedReceiptTimes(t *testing.T) {
	fixture := newFixture(t, []byte("%PDF-1.7\nhistorical resume\n%%EOF\n"))
	transport := routeTransport(t, map[string]routeResponse{
		statusPath(testJobID): {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
		jsonResultPath(testJobID): {
			body: `{"pages":[{"page":1,"md":"Historical","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`,
		},
	})
	client := fixture.client(t, transport)
	historical := time.Now().UTC().Add(-time.Hour)
	fixture.authorization.AuthorizedAt = historical.Format(timeForm)
	fixture.authorization.ExpiresAt = historical.Add(time.Minute).Format(timeForm)
	submittedAt := historical.Add(10 * time.Second)
	checkpointedAt := historical.Add(11 * time.Second)
	evidencePolicy, err := document.NewEvidencePolicy(100_000)
	require.NoError(t, err)
	renditionPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{
		MaxDocumentChars: 100_000, MaxUnitRunes: 1_000_000, MaxSegmentRunes: 1_000,
	})
	require.NoError(t, err)
	snapshot, err := document.SealRenditionExecutionAt(historical.Add(time.Second), client,
		fixture.upload(), fixture.authorization, evidencePolicy, renditionPolicy)
	require.NoError(t, err)

	result, err := document.ResumeRendition(t.Context(), client, snapshot,
		document.RenditionResumeHandle{Value: testResumeHandle(
			t, fixture.authorization, submittedAt, checkpointedAt)}, nil)

	require.NoError(t, err)
	assert.Equal(t, submittedAt.Format(timeForm), result.Receipt.StartedAt)
	assert.Equal(t, checkpointedAt.Format(timeForm), result.Receipt.CompletedAt)
}

func TestClientRechecksLifecycleAfterResponseBodiesAndBeforeReturn(t *testing.T) {
	const resultBody = `{"pages":[{"page":1,"md":"Complete","images":[],"charts":[],"tables":[],"layout":[],"items":[],"links":[],"parsingMode":"parse_page","noStructuredContent":true,"noTextContent":false,"triggeredAutoMode":false}],"job_metadata":{"job_pages":1}}`

	t.Run("caller cancellation after complete body", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nbody cancel\n%%EOF\n"))
		ctx, cancel := context.WithCancel(t.Context())
		transport := resultBodyTransport(t, resultBody, func(request *http.Request) io.ReadCloser {
			return &callbackReadCloser{reader: strings.NewReader(resultBody), before: cancel}
		})

		_, err := document.RenderRenditionWithResume(ctx, fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("wall timeout after complete body", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nbody timeout\n%%EOF\n"))
		fixture.profile.MaxWallTime = 20 * time.Millisecond
		transport := resultBodyTransport(t, resultBody, func(request *http.Request) io.ReadCloser {
			return &callbackReadCloser{reader: strings.NewReader(resultBody), before: func() { time.Sleep(40 * time.Millisecond) }}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
	})

	t.Run("authorization expiry after complete body", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nbody expiry\n%%EOF\n"))
		expiresAt := time.Now().UTC().Add(20 * time.Millisecond)
		fixture.authorization.ExpiresAt = expiresAt.Format(timeForm)
		transport := resultBodyTransport(t, resultBody, func(request *http.Request) io.ReadCloser {
			return &callbackReadCloser{reader: strings.NewReader(resultBody), before: func() {
				time.Sleep(time.Until(expiresAt) + 10*time.Millisecond)
			}}
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

		assert.ErrorIs(t, err, document.ErrRenditionAuthorizationExpired)
	})

	t.Run("caller cancellation at final acceptance", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\nfinal cancel\n%%EOF\n"))
		ctx := newCancelWhenCheckedContext(t.Context())
		defer ctx.cancel()
		transport := routeTransport(t, map[string]routeResponse{
			uploadPath:                {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			statusPath(testJobID):     {body: `{"id":"` + testJobID + `","status":"SUCCESS"}`},
			jsonResultPath(testJobID): {body: resultBody},
		})

		_, err := fixture.client(t, transport).RenderResumable(ctx, fixture.upload(),
			fixture.authorization, nil, func(document.RenditionResumeHandle) error { return nil })

		assertCode(t, err, document.RenditionErrorCanceled)
	})

	t.Run("accepted job is checkpointed before cancellation", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\ncheckpoint before cancel\n%%EOF\n"))
		ctx, cancel := context.WithCancel(t.Context())
		checkpointed := false
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != uploadPath {
				t.Fatalf("canceled accepted job reached %s", request.URL.Path)
			}
			result := response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"PENDING"}`)
			result.Body = &callbackReadCloser{
				reader: result.Body,
				afterClose: func() {
					cancel()
				},
			}
			return result, nil
		})

		_, err := document.RenderRenditionWithResume(ctx, fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error {
				checkpointed = true
				return nil
			})

		require.ErrorIs(t, err, context.Canceled)
		assert.True(t, checkpointed)
	})

	t.Run("accepted job is checkpointed before wall timeout", func(t *testing.T) {
		fixture := newFixture(t, []byte("%PDF-1.7\ncheckpoint before timeout\n%%EOF\n"))
		fixture.profile.MaxWallTime = 20 * time.Millisecond
		checkpointed := false
		transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != uploadPath {
				t.Fatalf("timed-out accepted job reached %s", request.URL.Path)
			}
			result := response(request, http.StatusOK,
				`{"id":"`+testJobID+`","status":"PENDING"}`)
			result.Body = &callbackReadCloser{
				reader: result.Body,
				afterClose: func() {
					<-request.Context().Done()
				},
			}
			return result, nil
		})

		_, err := document.RenderRenditionWithResume(t.Context(), fixture.client(t, transport),
			fixture.upload(), fixture.authorization, nil, func(document.RenditionResumeHandle) error {
				checkpointed = true
				return nil
			})

		assertCode(t, err, document.RenditionErrorAmbiguousSubmission)
		assert.True(t, checkpointed)
	})
}

type fixture struct {
	profile       Profile
	secrets       *testSecrets
	metadata      document.AuthorizedUploadMetadata
	source        []byte
	authorization document.RenditionAuthorization
}

func newFixture(t *testing.T, source []byte) *fixture {
	t.Helper()
	profile := Profile{
		Model: "parse-model-v1", Preset: "document-v1", SecretBinding: "llamaparse-production",
		MaxUploadBytes: 1024, MaxRequestBytes: 4096, MaxControlBytes: 1024,
		MaxPolls: 3, PollInterval: time.Millisecond, RequestTimeout: time.Second,
		MaxResultBytes: 32 << 10, MaxArtifactBytes: 1024, MaxArtifacts: 0,
		MaxWallTime: time.Second,
	}
	secrets := &testSecrets{value: "synthetic-secret"}
	client, err := NewProvider(profile, secrets, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused fixture transport")
	}))
	require.NoError(t, err)
	descriptor := client.Descriptor()
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "synthetic.pdf", MediaFamily: "pdf", MediaType: "application/pdf",
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("1", 64),
		ProviderMetadataChecksum: strings.Repeat("2", 64),
		InputKind:                document.RenditionInputOriginalFile,
	}
	now := time.Now().UTC()
	return &fixture{
		profile: profile, secrets: secrets, metadata: metadata, source: bytes.Clone(source),
		authorization: document.RenditionAuthorization{
			ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
			PolicyFingerprint:           descriptor.PolicyFingerprint,
			RenditionRequestFingerprint: strings.Repeat("3", 64),
			SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
			CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
			ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
			MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
			InputKind: metadata.InputKind, DiscloseFilename: true, MaxProviderMarkdownBytes: 16 << 10,
			MaxTotalResultBytes: 64 << 10,
			AuthorizedAt:        now.Add(-time.Second).Format(timeForm),
			ExpiresAt:           now.Add(time.Minute).Format(timeForm),
		},
	}
}

func (fixture *fixture) client(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := NewProvider(fixture.profile, fixture.secrets, transport)
	require.NoError(t, err)
	descriptor := client.Descriptor()
	fixture.authorization.ProviderID = descriptor.ID
	fixture.authorization.DescriptorFingerprint = descriptor.Fingerprint
	fixture.authorization.PolicyFingerprint = descriptor.PolicyFingerprint
	return client
}

func (fixture *fixture) upload() document.AuthorizedUpload {
	return &testUpload{Reader: bytes.NewReader(fixture.source), metadata: fixture.metadata}
}

type testUpload struct {
	io.Reader

	metadata document.AuthorizedUploadMetadata
}

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }
func (upload *testUpload) Close() error                                { return nil }

type testSecrets struct {
	mu     sync.Mutex
	value  string
	count  int
	failAt int
}

type callbackReadCloser struct {
	reader     io.Reader
	once       sync.Once
	closeOnce  sync.Once
	before     func()
	afterClose func()
}

func (body *callbackReadCloser) Read(buffer []byte) (int, error) {
	if body.before != nil {
		body.once.Do(body.before)
	}
	return body.reader.Read(buffer)
}

func (body *callbackReadCloser) Close() error {
	if body.afterClose != nil {
		body.closeOnce.Do(body.afterClose)
	}
	return nil
}

type cancelWhenCheckedContext struct {
	context.Context

	done chan struct{}
	once sync.Once
}

func newCancelWhenCheckedContext(parent context.Context) *cancelWhenCheckedContext {
	return &cancelWhenCheckedContext{Context: parent, done: make(chan struct{})}
}

func (ctx *cancelWhenCheckedContext) Done() <-chan struct{} { return ctx.done }

func (ctx *cancelWhenCheckedContext) Err() error {
	ctx.cancel()
	return context.Canceled
}

func (ctx *cancelWhenCheckedContext) cancel() {
	ctx.once.Do(func() { close(ctx.done) })
}

func (secrets *testSecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	secrets.count++
	if secrets.count == secrets.failAt {
		return "", errors.New("synthetic credential failure")
	}
	return secrets.value, nil
}

func (secrets *testSecrets) calls() int {
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	return secrets.count
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

type routeResponse struct {
	status int
	body   string
}

func routeTransport(t *testing.T, routes map[string]routeResponse) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		route, ok := routes[request.URL.Path]
		if !ok {
			t.Fatalf("unexpected route %s", request.URL.String())
		}
		status := route.status
		if status == 0 {
			status = http.StatusOK
		}
		return response(request, status, route.body), nil
	})
}

func resultBodyTransport(
	t *testing.T, resultBody string, body func(*http.Request) io.ReadCloser,
) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case uploadPath, statusPath(testJobID):
			return response(request, http.StatusOK, `{"id":"`+testJobID+`","status":"SUCCESS"}`), nil
		case jsonResultPath(testJobID):
			result := response(request, http.StatusOK, resultBody)
			result.Body = body(request)
			return result, nil
		default:
			t.Fatalf("unexpected route %s", request.URL.String())
			return nil, errors.New("unexpected route")
		}
	})
}

func response(request *http.Request, status int, body string) *http.Response {
	return bytesResponse(request, status, []byte(body))
}

func bytesResponse(request *http.Request, status int, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", providerutil.JSONMediaType)
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Header: header,
		Body: io.NopCloser(bytes.NewReader(body)), Request: request,
	}
}

func testResumeHandle(
	t *testing.T, authorization document.RenditionAuthorization, submittedAt, checkpointedAt time.Time,
) string {
	t.Helper()
	authorizationFingerprint, err := authorization.Fingerprint()
	require.NoError(t, err)
	return fmt.Sprintf("lp2.%s.%s.%d.%d", testJobID, authorizationFingerprint,
		submittedAt.UnixNano(), checkpointedAt.UnixNano())
}

func assertCode(t *testing.T, err error, want document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	providerError, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "expected classified provider error, got %T: %v", err, err)
	assert.Equal(t, want, providerError.Code())
}
