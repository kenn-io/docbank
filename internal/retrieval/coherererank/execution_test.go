package coherererank

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRerankWithReceiptKeepsConcurrentFractionalUsageRequestLocalAndRedacted(t *testing.T) {
	const secret = "SYNTHETIC_CREDENTIAL_SENTINEL"
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCalls := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseCalls)
	client := testClient(t, testProfile(ModelPro), &countingSecrets{value: secret}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var id string
		var inputTokens, searchUnits float64
		switch {
		case strings.Contains(string(body), "usage-one"):
			id, inputTokens, searchUnits = "response-one", 1.25, 2.5
		case strings.Contains(string(body), "usage-two"):
			id, inputTokens, searchUnits = "response-two", 5.5, 8.75
		default:
			return nil, errors.New("unexpected synthetic request")
		}
		select {
		case arrived <- struct{}{}:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		response := fmt.Sprintf(`{"id":%q,"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}],"meta":{"billed_units":{"input_tokens":%g,"search_units":%g},"tokens":{"input_tokens":%g}}}`, id, inputTokens, searchUnits, inputTokens)
		return jsonResponse(request, http.StatusOK, []byte(response)), nil
	}))
	type outcome struct {
		index     int
		execution Execution
		err       error
	}
	outcomes := make(chan outcome, 2)
	for index, query := range []string{"usage-one", "usage-two"} {
		go func(index int, query string) {
			request := rerankingRequest()
			request.Query = query
			execution, err := client.RerankWithReceipt(t.Context(), request)
			outcomes <- outcome{index: index, execution: execution, err: err}
		}(index, query)
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			t.Fatal("concurrent rerank request did not reach transport")
		}
	}
	releaseCalls()
	results := make([]Execution, 2)
	for range 2 {
		select {
		case result := <-outcomes:
			require.NoError(t, result.err)
			results[result.index] = result.execution
		case <-time.After(time.Second):
			t.Fatal("concurrent rerank request did not return")
		}
	}

	for _, execution := range results {
		encoded, err := json.Marshal(execution.Receipt)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "usage-one")
		assert.NotContains(t, string(encoded), "usage-two")
		assert.NotContains(t, string(encoded), secret)
	}
	assert.InDelta(t, 1.25, results[0].Receipt.InputTokens, 0)
	assert.InDelta(t, 2.5, results[0].Receipt.SearchUnits, 0)
	assert.Equal(t, "response-one", results[0].Receipt.ProviderResponseID)
	assert.InDelta(t, 5.5, results[1].Receipt.InputTokens, 0)
	assert.InDelta(t, 8.75, results[1].Receipt.SearchUnits, 0)
	assert.Equal(t, "response-two", results[1].Receipt.ProviderResponseID)
}

func TestRerankWithReceiptRejectsLateSuccessAfterCancellationOrDeadline(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancellation"
		if timeout {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			profile := testProfile(ModelPro)
			if timeout {
				profile.RequestTimeout = 25 * time.Millisecond
			}
			client := testClient(t, profile, &countingSecrets{value: "synthetic-key"}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				close(started)
				<-request.Context().Done()
				return jsonResponse(request, http.StatusOK, []byte(`{"id":"late-success","results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.4}]}`)), nil
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct {
				execution Execution
				err       error
			}, 1)
			go func() {
				execution, err := client.RerankWithReceipt(ctx, rerankingRequest())
				done <- struct {
					execution Execution
					err       error
				}{execution: execution, err: err}
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("late-success request did not reach transport")
			}
			if !timeout {
				cancel()
			}
			var outcome struct {
				execution Execution
				err       error
			}
			select {
			case outcome = <-done:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("late-success rerank did not return")
			}
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			require.ErrorIs(t, outcome.err, want)
			assert.Equal(t, Execution{}, outcome.execution)
		})
	}
}
