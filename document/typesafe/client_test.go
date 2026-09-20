package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.kenn.io/docbank/document/providerhttp"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

type countingSecrets struct {
	calls atomic.Int32
	value string
}

type blockingSecrets struct {
	started chan struct{}
}

func (secrets *blockingSecrets) ResolveSecret(ctx context.Context, _ string) (string, error) {
	close(secrets.started)
	<-ctx.Done()
	return "", ctx.Err()
}

func (secrets *countingSecrets) ResolveSecret(context.Context, string) (string, error) {
	secrets.calls.Add(1)
	return secrets.value, nil
}

func newTestClient(t *testing.T, response string) *Client {
	t.Helper()
	client, err := New(testProfile(), &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(response, request), nil
	})
	return client
}

func jsonResponse(body string, request *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func TestRerankPerCandidateSendsExactSystemOneBody(t *testing.T) {
	var bodies []string
	client := newTestClient(t, "")
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		return jsonResponse("{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.75}},\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}", request), nil
	})
	result, err := client.Rerank(context.Background(), RerankRequest{Query: "query", Candidates: []string{"candidate"}})
	if err != nil || len(result.Scores) != 1 || result.Scores[0] != 0.75 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	want := "{\"state\":{\"query\":\"query\",\"candidate\":\"candidate\"},\"model\":\"jev-1.13.0\",\"questions\":{\"matches\":{\"type\":\"noul\",\"instructions\":\"Could `candidate` be the best answer to `query`?\",\"criteria\":{\"true\":\"The candidate contains the specific information needed to answer the query.\",\"false\":\"The candidate is only topically similar or does not contain the needed evidence.\"}}}}"
	if !slices.Equal(bodies, []string{want}) {
		t.Fatalf("request body = %q, want %q", bodies, want)
	}
}

func TestRerankBatchedSendsOneCallWithIndexedQuestions(t *testing.T) {
	profile := testProfile()
	profile.RequestShape = RequestShapeBatched
	client, err := New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		value, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		body = string(value)
		return jsonResponse("{\"model\":\"jev-1.13.0\",\"answers\":{\"candidate_1\":{\"type\":\"noul\",\"noul\":0.8},\"candidate_0\":{\"type\":\"noul\",\"noul\":0.2}},\"usage\":{\"input_tokens\":4,\"output_tokens\":2}}", request), nil
	})
	result, err := client.Rerank(context.Background(), RerankRequest{Query: "query", Candidates: []string{"first", "second"}})
	if err != nil || !slices.Equal(result.Scores, []float64{0.2, 0.8}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var decoded wireBatchedRequest
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(decoded.State.Candidates, []string{"first", "second"}) || len(decoded.Questions) != 2 {
		t.Fatalf("request = %+v", decoded)
	}
	for index := range 2 {
		question := decoded.Questions[fmt.Sprintf("candidate_%d", index)]
		candidate := fmt.Sprintf("candidates[%d]", index)
		if question.Type != "noul" || !strings.Contains(question.Instructions, "`"+candidate+"`") ||
			!strings.Contains(question.Criteria.True, candidate) || !strings.Contains(question.Criteria.False, candidate) {
			t.Fatalf("missing question candidate_%d", index)
		}
	}
}

func TestRerankRejectsOverBoundRequestsBeforeSecretsOrEgress(t *testing.T) {
	profile := testProfile()
	profile.MaxCandidates = 1
	secrets := &countingSecrets{value: "synthetic-secret"}
	var requests atomic.Int32
	client, err := New(profile, secrets, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected provider request")
	})
	for name, request := range map[string]RerankRequest{
		"query":      {Query: strings.Repeat("q", 4097), Candidates: []string{"x"}},
		"candidates": {Query: "q", Candidates: []string{"x", "y"}},
		"excerpt":    {Query: "q", Candidates: []string{strings.Repeat("x", 4097)}},
	} {
		if _, err := client.Rerank(context.Background(), request); !errors.Is(err, ErrCapacityResponse) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	profile.MaxRequestBytes = 1
	if err := CheckRequest(profile, RerankRequest{Query: "q", Candidates: []string{"x"}}); !errors.Is(err, ErrCapacityResponse) {
		t.Fatalf("request boundary MaxRequestBytes+1: got %v", err)
	}
	exact := testProfile()
	exact.MaxQueryBytes = 1
	exact.MaxCandidateBytes = 1
	exact.MaxRequestBytes = 4096
	if err := CheckRequest(exact, RerankRequest{Query: "q", Candidates: []string{"x"}}); err != nil {
		t.Fatalf("exact request bounds: %v", err)
	}
	if secrets.calls.Load() != 0 || requests.Load() != 0 {
		t.Fatalf("secret calls=%d provider requests=%d", secrets.calls.Load(), requests.Load())
	}
}

func TestCheckRequestBatchedDefaultsAdmitFullSizeCandidates(t *testing.T) {
	for name, value := range map[string]string{"plain": "x", "escaped": "\x01"} {
		t.Run(name, func(t *testing.T) {
			profile := testProfile()
			profile.RequestShape = RequestShapeBatched
			request := RerankRequest{Query: strings.Repeat(value, 4096), Candidates: make([]string, 100)}
			for index := range request.Candidates {
				request.Candidates[index] = strings.Repeat(value, 4096)
			}
			if err := CheckRequest(profile, request); err != nil {
				t.Fatalf("full-size batched request: %v", err)
			}
			profile.MaxRequestBytes = 256 << 10
			if err := CheckRequest(profile, request); !errors.Is(err, ErrCapacityResponse) {
				t.Fatalf("explicit request limit: got %v, want capacity error", err)
			}
		})
	}
}

func TestRerankPreservesEmptyCandidateBeforeSecretAndProvider(t *testing.T) {
	secrets := &countingSecrets{value: "synthetic-secret"}
	client, err := New(testProfile(), secrets, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var body []byte
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		return jsonResponse("{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}", request), nil
	})
	if _, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{""}}); err != nil {
		t.Fatalf("got %v", err)
	}
	if secrets.calls.Load() != 1 || requests.Load() != 1 || !bytes.Contains(body, []byte(`"candidate":""`)) {
		t.Fatalf("secret calls=%d provider requests=%d", secrets.calls.Load(), requests.Load())
	}
}

func TestValidSecretUses64KiBBound(t *testing.T) {
	if !validSecret(strings.Repeat("x", maximumSecretBytes)) {
		t.Fatal("64 KiB secret was rejected")
	}
	if validSecret(strings.Repeat("x", maximumSecretBytes+1)) || validSecret(" secret") || validSecret("secret\nvalue") {
		t.Fatal("invalid secret was accepted")
	}
}

func TestRerankBoundsResponseRead(t *testing.T) {
	profile := testProfile()
	profile.MaxResponseBytes = 65536
	client, err := New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	var read atomic.Int64
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: &countingReader{reader: strings.NewReader(strings.Repeat("x", 65537)), reads: &read}, Request: request}, nil
	})
	if _, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}}); !errors.Is(err, ErrCapacityResponse) {
		t.Fatalf("got %v", err)
	}
	if read.Load() > 65537 {
		t.Fatalf("read %d bytes", read.Load())
	}
	valid := "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}"
	exact := testProfile()
	exact.MaxResponseBytes = int64(len(valid))
	exactClient, err := New(exact, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	exactClient.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(valid, request), nil
	})
	if _, err := exactClient.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}}); err != nil {
		t.Fatalf("exact response boundary: %v", err)
	}
}

func TestRerankClassifiesStatusBeforeReadingBody(t *testing.T) {
	client := newTestClient(t, "")
	body := &readFailingBody{}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"7"}},
			Body:       body,
			Request:    request,
		}, nil
	})

	_, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}})
	if !errors.Is(err, ErrTransientResponse) {
		t.Fatalf("got %v, want transient response", err)
	}
	failure, ok := errors.AsType[*ProviderError](err)
	if !ok || failure.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("provider error = %+v, %v; want HTTP 429", failure, ok)
	}
	if delay, ok := RetryAfter(err); !ok || delay != 7*time.Second {
		t.Fatalf("RetryAfter = %v, %v; want 7s, true", delay, ok)
	}
	if body.read {
		t.Fatal("response body was read")
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestRerankRejectsContentTypeBeforeReadingBody(t *testing.T) {
	client := newTestClient(t, "")
	body := &readFailingBody{}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       body,
			Request:    request,
		}, nil
	})
	_, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}})
	if !errors.Is(err, ErrPermanentResponse) {
		t.Errorf("got %v, want permanent response", err)
	}
	if body.read {
		t.Error("response body was read")
	}
	if !body.closed {
		t.Error("response body was not closed")
	}
}

type readFailingBody struct {
	read   bool
	closed bool
}

func (body *readFailingBody) Read([]byte) (int, error) {
	body.read = true
	return 0, errors.New("response body read")
}

func (body *readFailingBody) Close() error {
	body.closed = true
	return nil
}

type countingReader struct {
	reader io.Reader
	reads  *atomic.Int64
}

func (reader *countingReader) Read(value []byte) (int, error) {
	count, err := reader.reader.Read(value)
	reader.reads.Add(int64(count))
	return count, err
}

func (reader *countingReader) Close() error { return nil }

func TestRerankRejectsMalformedAnswersWithoutPartialScores(t *testing.T) {
	valid := "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}"
	for name, body := range map[string]string{
		"wrong model":      "{\"model\":\"jev-latest\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"missing id":       "{\"model\":\"jev-1.13.0\",\"answers\":{},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"extra id":         "{\"model\":\"jev-1.13.0\",\"answers\":{\"other\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"duplicate key":    "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5},\"matches\":{\"type\":\"noul\",\"noul\":0.6}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"choice":           "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"choice\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"negative score":   "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":-0.1}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"high score":       "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":1.1}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"infinite score":   "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":1e1000}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}",
		"missing usage":    "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}}}",
		"unknown member":   "{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"extra\":true}",
		"wrong media type": valid,
	} {
		client := newTestClient(t, body)
		if name == "wrong media type" {
			client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/html"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})
		}
		result, err := client.Rerank(context.Background(), RerankRequest{Query: "private-query", Candidates: []string{"private-candidate"}})
		if !errors.Is(err, ErrPermanentResponse) || len(result.Scores) != 0 {
			t.Errorf("%s: result=%+v err=%v", name, result, err)
		}
	}
	client := newTestClient(t, valid)
	result, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}})
	if err != nil || !slices.Equal(result.Scores, []float64{0.5}) {
		t.Fatalf("valid result=%+v err=%v", result, err)
	}
}

func TestRerankErrorsAndReceiptsCarryNoProviderText(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, 529} {
		client := newTestClient(t, "")
		client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("private-body-marker")), Request: request}, nil
		})
		result, err := client.Rerank(context.Background(), RerankRequest{Query: "private-query", Candidates: []string{"private-candidate"}})
		want := ErrPermanentResponse
		if status == 529 {
			want = ErrTransientResponse
		}
		if !errors.Is(err, want) || err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(fmt.Sprintf("%+v", result.Receipt), "private-") {
			t.Fatalf("status %d leaked: result=%+v err=%v", status, result, err)
		}
	}
}

func TestRerankNeverExceedsMaxConcurrentCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		profile := testProfile()
		profile.MaxConcurrentCalls = 3
		client, err := New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		var active, maximum atomic.Int32
		entered := make(chan struct{}, 10)
		release := make(chan struct{})
		client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			current := active.Add(1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			entered <- struct{}{}
			<-release
			active.Add(-1)
			return jsonResponse("{\"model\":\"jev-1.13.0\",\"answers\":{\"matches\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}", request), nil
		})
		done := make(chan error, 1)
		go func() {
			_, callErr := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}})
			done <- callErr
		}()
		for range 3 {
			<-entered
		}
		select {
		case <-entered:
			t.Fatal("fourth provider call started")
		default:
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if maximum.Load() != 3 {
			t.Fatalf("maximum concurrent calls = %d", maximum.Load())
		}
	})
}

func TestRerankDistinguishesClientTimeoutFromCallerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		profile := testProfile()
		profile.RequestTimeout = time.Second
		client, err := New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		caller, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, callErr := client.Rerank(caller, RerankRequest{Query: "q", Candidates: []string{"x"}})
			done <- callErr
		}()
		time.Sleep(500 * time.Millisecond)
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline: %v", err)
		}
		profile.RequestTimeout = 500 * time.Millisecond
		client, err = New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		caller, cancel = context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		done = make(chan error, 1)
		go func() {
			_, callErr := client.Rerank(caller, RerankRequest{Query: "q", Candidates: []string{"x"}})
			done <- callErr
		}()
		time.Sleep(500 * time.Millisecond)
		if err := <-done; !errors.Is(err, ErrTransientResponse) {
			t.Fatalf("client timeout: %v", err)
		}
	})
}

func TestRerankCancelsCredentialResolution(t *testing.T) {
	secrets := &blockingSecrets{started: make(chan struct{})}
	client, err := New(testProfile(), secrets, nil, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := client.Rerank(ctx, RerankRequest{Query: "q", Candidates: []string{"x"}})
		done <- callErr
	}()
	<-secrets.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("credential cancellation: %v", err)
	}
}

type cancelingBody struct {
	started chan struct{}
	release chan struct{}
}

func (body *cancelingBody) Read([]byte) (int, error) {
	close(body.started)
	<-body.release
	return 0, context.Canceled
}

func (body *cancelingBody) Close() error { return nil }

func TestRerankReportsResponseBodyCancellation(t *testing.T) {
	client := newTestClient(t, "")
	body := &cancelingBody{started: make(chan struct{}), release: make(chan struct{})}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		go func() {
			<-request.Context().Done()
			close(body.release)
		}()
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: request}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, callErr := client.Rerank(ctx, RerankRequest{Query: "q", Candidates: []string{"x"}})
		done <- callErr
	}()
	<-body.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("response cancellation: %v", err)
	}
}

func TestRerankClassifiesTransportFailures(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		kind  error
	}{
		{name: "address", cause: providerhttp.ErrAddressDenied, kind: ErrPermanentResponse},
		{name: "destination", cause: providerhttp.ErrDestinationDenied, kind: ErrPermanentResponse},
		{name: "pin", cause: providerhttp.ErrCertificatePin, kind: ErrPermanentResponse},
		{name: "other", cause: errors.New("connection reset"), kind: ErrTransientResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, "")
			client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, test.cause })
			_, err := client.Rerank(context.Background(), RerankRequest{Query: "q", Candidates: []string{"x"}})
			if !errors.Is(err, test.kind) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCaptureReplayMatchesClientEncoding(t *testing.T) {
	for _, test := range []struct {
		name       string
		shape      RequestShape
		candidates []string
		ids        []string
	}{{"per_candidate", RequestShapePerCandidate, []string{"synthetic candidate"}, []string{rankingQuestionID}},
		{"batched", RequestShapeBatched, []string{"synthetic first", "synthetic second"}, []string{"candidate_0", "candidate_1"}}} {
		t.Run(test.name, func(t *testing.T) {
			record, err := os.ReadFile("testdata/capture_" + test.name + ".json")
			if errors.Is(err, os.ErrNotExist) {
				t.Skip("capture is absent")
			}
			if err != nil {
				t.Fatal(err)
			}
			var capture struct {
				Request     json.RawMessage `json:"request"`
				Status      int             `json:"status"`
				ContentType string          `json:"content_type"`
				Response    json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal(record, &capture); err != nil {
				t.Fatal(err)
			}
			var response wireResponse
			if err := json.Unmarshal(capture.Response, &response); err != nil {
				t.Fatal(err)
			}
			expected, usage, valid := validResponse(response, test.ids, ModelJev113)
			if !valid {
				t.Fatal("captured response is not a valid TypeSafe response")
			}
			wantScores := make([]float64, len(test.ids))
			for index, id := range test.ids {
				wantScores[index] = expected[id]
			}
			profile := testProfile()
			profile.RequestShape = test.shape
			client, err := New(profile, &countingSecrets{value: "synthetic-secret"}, nil, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, readErr := io.ReadAll(request.Body)
				if readErr != nil {
					return nil, readErr
				}
				if !bytes.Equal(canonicalJSON(body), canonicalJSON(capture.Request)) {
					return nil, fmt.Errorf("generated request differs from capture: %s != %s", body, capture.Request)
				}
				return &http.Response{StatusCode: capture.Status, Header: http.Header{"Content-Type": []string{capture.ContentType}}, Body: io.NopCloser(bytes.NewReader(capture.Response)), Request: request}, nil
			})
			result, err := client.Rerank(context.Background(), RerankRequest{Query: "synthetic question", Candidates: test.candidates})
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !slices.Equal(result.Scores, wantScores) {
				t.Fatalf("scores = %v, want captured scores %v", result.Scores, wantScores)
			}
			if result.Receipt.InputTokens != usage.inputTokens || result.Receipt.OutputTokens != usage.outputTokens {
				t.Fatalf("usage = %d/%d, want %d/%d", result.Receipt.InputTokens, result.Receipt.OutputTokens, usage.inputTokens, usage.outputTokens)
			}
		})
	}
}

func canonicalJSON(value []byte) []byte {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return value
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return value
	}
	return canonical
}
