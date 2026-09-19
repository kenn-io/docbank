package typesafe

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/document/internal/providerutil"
	"go.kenn.io/docbank/document/providerhttp"
	"golang.org/x/sync/errgroup"
)

const (
	rankingQuestionID  = "matches"
	maximumSecretBytes = 64 << 10
	maximumUsage       = int64(1 << 50)
)

// RerankRequest contains one query and its ordered candidate texts.
type RerankRequest struct {
	Query      string
	Candidates []string
}

// Receipt records provider metadata without retaining request or response text.
type Receipt struct {
	PolicyFingerprint string
	Model             string
	RequestShape      RequestShape
	CandidateCount    int
	InputTokens       int64
	OutputTokens      int64
}

// Result contains one score for each candidate and its opaque receipt.
type Result struct {
	Scores  []float64
	Receipt Receipt
}

type wireRequest struct {
	State     wireState               `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireState struct {
	Query      string   `json:"query"`
	Candidate  *string  `json:"candidate,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

type wireQuestion struct {
	Type         string       `json:"type"`
	Instructions string       `json:"instructions"`
	Criteria     wireCriteria `json:"criteria"`
}

type wireCriteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   *wireUsage            `json:"usage"`
}

type wireAnswer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

type wireUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type preparedCall struct {
	ids     []string
	payload []byte
}

type callUsage struct {
	inputTokens  int64
	outputTokens int64
}

// CheckRequest validates a request against the effective profile and encodes
// every provider payload without resolving a secret or issuing a request.
func CheckRequest(profile Profile, request RerankRequest) error {
	calls, err := encodeCalls(profile, request)
	for index := range calls {
		clear(calls[index].payload)
	}
	return err
}

func encodeCalls(profile Profile, request RerankRequest) ([]preparedCall, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, fmt.Errorf("typesafe rerank: invalid profile: %w", err)
	}
	if strings.TrimSpace(request.Query) == "" || !utf8.ValidString(request.Query) {
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
	if len(request.Query) > normalized.MaxQueryBytes {
		return nil, &ProviderError{Kind: ErrCapacityResponse}
	}
	if len(request.Candidates) == 0 || len(request.Candidates) > normalized.MaxCandidates {
		return nil, &ProviderError{Kind: ErrCapacityResponse}
	}
	for _, candidate := range request.Candidates {
		if candidate == "" {
			return nil, &ProviderError{Kind: ErrPermanentResponse}
		}
		if !utf8.ValidString(candidate) || len(candidate) > normalized.MaxCandidateBytes {
			return nil, &ProviderError{Kind: ErrCapacityResponse}
		}
	}

	switch normalized.RequestShape {
	case RequestShapePerCandidate:
		calls := make([]preparedCall, len(request.Candidates))
		for index, candidate := range request.Candidates {
			payload, encodeErr := encodeRequest(wireRequest{
				State: wireState{Query: request.Query, Candidate: new(candidate)},
				Model: normalized.Model,
				Questions: map[string]wireQuestion{
					rankingQuestionID: rankingQuestion("candidate"),
				},
			})
			if encodeErr != nil {
				clearPreparedCalls(calls)
				return nil, encodeErr
			}
			if int64(len(payload)) > normalized.MaxRequestBytes {
				clear(payload)
				clearPreparedCalls(calls)
				return nil, &ProviderError{Kind: ErrCapacityResponse}
			}
			calls[index] = preparedCall{ids: []string{rankingQuestionID}, payload: payload}
		}
		return calls, nil
	case RequestShapeBatched:
		questions := make(map[string]wireQuestion, len(request.Candidates))
		ids := make([]string, len(request.Candidates))
		for index := range request.Candidates {
			id := fmt.Sprintf("candidate_%d", index)
			ids[index] = id
			questions[id] = rankingQuestion(fmt.Sprintf("candidates[%d]", index))
		}
		payload, encodeErr := encodeRequest(wireRequest{
			State: wireState{Query: request.Query, Candidates: slices.Clone(request.Candidates)},
			Model: normalized.Model, Questions: questions,
		})
		if encodeErr != nil {
			return nil, encodeErr
		}
		if int64(len(payload)) > normalized.MaxRequestBytes {
			clear(payload)
			return nil, &ProviderError{Kind: ErrCapacityResponse}
		}
		return []preparedCall{{ids: ids, payload: payload}}, nil
	default:
		return nil, &ProviderError{Kind: ErrPermanentResponse}
	}
}

func encodeRequest(request wireRequest) ([]byte, error) {
	payload, err := json.Marshal(request, json.Deterministic(true))
	if err != nil {
		return nil, errors.New("typesafe rerank: request encoding failed")
	}
	return payload, nil
}

func rankingQuestion(candidate string) wireQuestion {
	return wireQuestion{
		Type:         "noul",
		Instructions: fmt.Sprintf("Could `%s` be the best answer to `query`?", candidate),
		Criteria: wireCriteria{
			True:  fmt.Sprintf("The %s contains the specific information needed to answer the query.", candidate),
			False: fmt.Sprintf("The %s is only topically similar or does not contain the needed evidence.", candidate),
		},
	}
}

// Rerank evaluates all candidates and returns scores in request order.
func (client *Client) Rerank(ctx context.Context, request RerankRequest) (Result, error) {
	if client == nil || ctx == nil {
		return Result{}, errors.New("typesafe rerank: client and context are required")
	}
	calls, err := encodeCalls(client.profile, request)
	if err != nil {
		return Result{}, err
	}
	defer clearPreparedCalls(calls)

	requestCtx, cancel := context.WithTimeout(ctx, client.profile.RequestTimeout)
	defer cancel()
	secret, err := client.secrets.ResolveSecret(requestCtx, client.profile.SecretBinding)
	if err != nil || !validSecret(secret) {
		if contextErr := ctx.Err(); contextErr != nil {
			return Result{}, fmt.Errorf("typesafe rerank: credential resolution canceled: %w", contextErr)
		}
		if requestCtx.Err() != nil {
			return Result{}, &ProviderError{Kind: ErrTransientResponse}
		}
		return Result{}, errors.New("typesafe rerank: API-key resolution failed")
	}

	scores := make([]float64, len(request.Candidates))
	var totals callUsage
	var totalsMu sync.Mutex
	group, groupCtx := errgroup.WithContext(requestCtx)
	group.SetLimit(client.profile.MaxConcurrentCalls)
	for index := range calls {
		call := calls[index]
		group.Go(func() error {
			values, usage, sendErr := client.send(groupCtx, ctx, requestCtx, call, secret)
			if sendErr != nil {
				return sendErr
			}
			totalsMu.Lock()
			defer totalsMu.Unlock()
			if totals.inputTokens > maximumUsage-usage.inputTokens || totals.outputTokens > maximumUsage-usage.outputTokens {
				return &ProviderError{Kind: ErrPermanentResponse}
			}
			for questionID, score := range values {
				position := questionPosition(call.ids, questionID)
				if position < 0 {
					return &ProviderError{Kind: ErrPermanentResponse}
				}
				if len(calls) == 1 {
					scores[position] = score
				} else {
					scores[index] = score
				}
			}
			totals.inputTokens += usage.inputTokens
			totals.outputTokens += usage.outputTokens
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return Result{}, fmt.Errorf("typesafe rerank: provider calls failed: %w", err)
	}
	return Result{
		Scores: scores,
		Receipt: Receipt{
			PolicyFingerprint: client.fingerprint, Model: client.profile.Model,
			RequestShape: client.profile.RequestShape, CandidateCount: len(request.Candidates),
			InputTokens: totals.inputTokens, OutputTokens: totals.outputTokens,
		},
	}, nil
}

func (client *Client) send(callCtx, callerCtx, requestCtx context.Context, call preparedCall, secret string) (map[string]float64, callUsage, error) {
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, origin+systemOnePath, bytes.NewReader(call.payload))
	if err != nil {
		return nil, callUsage{}, errors.New("typesafe rerank: request construction failed")
	}
	request.Header.Set("Accept", providerutil.JSONMediaType)
	request.Header.Set("Content-Type", providerutil.JSONMediaType)
	request.Header.Set("Authorization", "Bearer "+secret)
	response, err := client.http.Do(request)
	if err != nil {
		return nil, callUsage{}, classifyTransportError(callerCtx, requestCtx, err)
	}
	if response == nil || response.Body == nil {
		return nil, callUsage{}, &ProviderError{Kind: ErrTransientResponse}
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := providerutil.ReadBounded(response.Body, client.profile.MaxResponseBytes)
	defer clear(body)
	if contextErr := callerCtx.Err(); contextErr != nil {
		return nil, callUsage{}, fmt.Errorf("typesafe rerank: response read canceled: %w", contextErr)
	}
	if requestCtx.Err() != nil {
		return nil, callUsage{}, &ProviderError{Kind: ErrTransientResponse}
	}
	if readErr != nil {
		if errors.Is(readErr, providerutil.ErrResponseTooLarge) {
			return nil, callUsage{}, &ProviderError{Kind: ErrCapacityResponse}
		}
		return nil, callUsage{}, &ProviderError{Kind: ErrTransientResponse}
	}
	if response.StatusCode != http.StatusOK {
		return nil, callUsage{}, statusError(response.StatusCode, response.Header)
	}
	if !isJSONContentType(response.Header.Get("Content-Type")) {
		return nil, callUsage{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	var decoded wireResponse
	if err := json.Unmarshal(body, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return nil, callUsage{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	scores, usage, valid := validResponse(decoded, call.ids, client.profile.Model)
	if !valid {
		return nil, callUsage{}, &ProviderError{Kind: ErrPermanentResponse}
	}
	return scores, usage, nil
}

func classifyTransportError(callerCtx, requestCtx context.Context, err error) error {
	if contextErr := callerCtx.Err(); contextErr != nil {
		return fmt.Errorf("typesafe rerank: request canceled: %w", contextErr)
	}
	if requestCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return &ProviderError{Kind: ErrTransientResponse}
	}
	if errors.Is(err, providerhttp.ErrAddressDenied) || errors.Is(err, providerhttp.ErrDestinationDenied) ||
		errors.Is(err, providerhttp.ErrCertificatePin) {
		return &ProviderError{Kind: ErrPermanentResponse}
	}
	return &ProviderError{Kind: ErrTransientResponse}
}

func validResponse(response wireResponse, ids []string, model string) (map[string]float64, callUsage, bool) {
	if response.Model != model || len(response.Answers) != len(ids) || response.Usage == nil ||
		response.Usage.InputTokens == nil || response.Usage.OutputTokens == nil ||
		!validUsage(*response.Usage.InputTokens) || !validUsage(*response.Usage.OutputTokens) {
		return nil, callUsage{}, false
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	scores := make(map[string]float64, len(ids))
	for id, answer := range response.Answers {
		if _, ok := wanted[id]; !ok || answer.Type != "noul" || answer.Noul == nil ||
			math.IsNaN(*answer.Noul) || math.IsInf(*answer.Noul, 0) || *answer.Noul < 0 || *answer.Noul > 1 {
			return nil, callUsage{}, false
		}
		scores[id] = *answer.Noul
	}
	if len(scores) != len(ids) {
		return nil, callUsage{}, false
	}
	return scores, callUsage{inputTokens: *response.Usage.InputTokens, outputTokens: *response.Usage.OutputTokens}, true
}

func validUsage(value int64) bool { return value >= 0 && value <= maximumUsage }

func questionPosition(ids []string, questionID string) int {
	return slices.Index(ids, questionID)
}

func clearPreparedCalls(calls []preparedCall) {
	for index := range calls {
		clear(calls[index].payload)
		calls[index].payload = nil
	}
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (mediaType == providerutil.JSONMediaType || strings.HasSuffix(mediaType, "+json"))
}

func validSecret(value string) bool {
	if value == "" || len(value) > maximumSecretBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, current := range value {
		if unicode.IsControl(current) {
			return false
		}
	}
	return true
}
