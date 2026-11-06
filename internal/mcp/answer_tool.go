package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/answer"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

// answerContentExcerpt admits only an excerpt from the lexical content lane.
// A name hit can be fused with embedding evidence, so an excerpt alone is not
// sufficient evidence that its text came from document content.
func answerContentExcerpt(result api.DocumentSearchResult) bool {
	if result.Excerpt == "" {
		return false
	}
	for _, evidence := range result.Evidence {
		if evidence.Kind == "rendition_segment" || evidence.Kind == "content_blob" {
			return true
		}
	}
	return false
}

type answerInput struct {
	Question          string                          `json:"question"`
	ContentVersionIDs []string                        `json:"content_version_ids"`
	Filters           *api.DocumentSourceFenceFilters `json:"filters"`
	Profile           string                          `json:"profile"`
	BindingID         string                          `json:"binding_id"`
	Limit             int                             `json:"limit"`
	Rerank            bool                            `json:"rerank"`
	MaxPassageBytes   int                             `json:"max_passage_bytes"`
}

type answerProfile struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
}
type answerCitation struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
}
type answerReceipt struct {
	QuestionSHA256      string                              `json:"question_sha256"`
	ExecutedQuerySHA256 string                              `json:"executed_query_sha256,omitempty"`
	SearchExecuted      bool                                `json:"search_executed"`
	ObservedScopeCount  int                                 `json:"observed_scope_count"`
	VaultID             string                              `json:"vault_id,omitempty"`
	FenceFingerprint    string                              `json:"fence_fingerprint,omitempty"`
	Profile             *answerProfile                      `json:"profile,omitempty"`
	BindingID           string                              `json:"binding_id,omitempty"`
	VectorSpaceIDs      []string                            `json:"vector_space_ids"`
	RequestedMode       string                              `json:"requested_mode,omitempty"`
	ActualMode          string                              `json:"actual_mode,omitempty"`
	Degradations        []string                            `json:"degradations"`
	Coverage            *api.DocumentSearchCoverage         `json:"coverage,omitempty"`
	Reranking           *api.DocumentSearchRerankingReceipt `json:"reranking,omitempty"`
	CitedVersions       []answerCitation                    `json:"cited_versions"`
}
type answerOutput struct {
	privateCache

	Status           answer.Status    `json:"status"`
	CoverageComplete bool             `json:"coverage_complete"`
	CoverageReasons  []string         `json:"coverage_reasons"`
	SemanticFailure  string           `json:"semantic_failure,omitempty"`
	Passages         []answer.Passage `json:"passages"`
	PassageBytes     int              `json:"passage_bytes"`
	OmittedPassages  int              `json:"omitted_passages"`
	Leads            []answer.Lead    `json:"leads"`
	OmittedLeads     int              `json:"omitted_leads"`
	OmittedLinks     int              `json:"omitted_links"`
	Receipt          answerReceipt    `json:"receipt"`
}

type answerReadSession func(context.Context, func(context.Context, documentClient) error) error

func daemonAnswerSession(lease *daemonLease) answerReadSession {
	return daemonClientSession(lease, func(c *daemonconn.Connection) documentClient { return daemonDocumentClient{c} })
}

var answerErrorMessages = map[string]string{
	"processing_unavailable":         "The requested processing profile is unavailable.",
	"processing_profile_unavailable": "The requested processing profile is unavailable.",
	"reranking_unavailable":          "The processing profile has no configured reranking provider; ask the operator to configure it.",
	"reranking_failed":               "Document reranking failed; check the daemon log before retrying.",
	"stale_index":                    "The selected index needs rebuilding.",
}

func selectAnswerSearch(profiles []api.ProcessingProfileSummary, input answerInput) (api.ProcessingProfileSummary, []daemonconn.SearchCandidate, error) {
	if len(profiles) == 0 {
		return api.ProcessingProfileSummary{}, nil, api.NewError(http.StatusServiceUnavailable, "processing_unavailable", "no processing profiles available")
	}
	if input.Profile == "" && input.Rerank {
		profiles = slices.DeleteFunc(slices.Clone(profiles), func(p api.ProcessingProfileSummary) bool { return !p.RerankingAvailable })
		if len(profiles) == 0 {
			return api.ProcessingProfileSummary{}, nil, api.NewError(http.StatusServiceUnavailable, "reranking_unavailable", "no reranking profiles available")
		}
	}
	selected := profiles[0]
	candidates := daemonconn.AutomaticSearchCandidates(profiles)
	if input.Profile != "" {
		found := false
		for _, p := range profiles {
			if p.Name == input.Profile {
				selected = p
				found = true
				break
			}
		}
		if !found {
			return selected, nil, api.NewError(http.StatusUnprocessableEntity, "processing_profile_unavailable", "processing profile is not configured")
		}
		if input.BindingID != "" {
			candidates = []daemonconn.SearchCandidate{{Profile: input.Profile, Binding: input.BindingID}}
		} else {
			candidates = slices.DeleteFunc(candidates, func(candidate daemonconn.SearchCandidate) bool {
				return candidate.Profile != selected.Name
			})
			if len(candidates) > 1 {
				candidates = candidates[:1]
			}
		}
	}
	return selected, candidates, nil
}

func answerQuestion(ctx context.Context, session answerReadSession, raw []byte) (answerOutput, []*sdkmcp.ResourceLink, error) {
	var input answerInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return answerOutput{}, nil, err
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.MaxPassageBytes == 0 {
		input.MaxPassageBytes = 12288
	}
	hash := sha256.Sum256([]byte(input.Question))
	out := answerOutput{privateCache: newPrivateCache(), CoverageReasons: []string{}, Passages: []answer.Passage{}, Leads: []answer.Lead{}, Receipt: answerReceipt{QuestionSHA256: hex.EncodeToString(hash[:]), VectorSpaceIDs: []string{}, Degradations: []string{}, CitedVersions: []answerCitation{}}}
	type response struct {
		fence     api.DocumentSourceFenceResolution
		profile   api.ProcessingProfileSummary
		binding   string
		report    api.DocumentSearchReport
		failure   string
		executed  bool
		documents []api.DocumentSummary
	}
	result, err := documentRead(ctx, session, func(ctx context.Context, c documentClient) (response, error) {
		filters := input.Filters
		if filters == nil && len(input.ContentVersionIDs) == 0 {
			filters = &api.DocumentSourceFenceFilters{}
		}
		fence, err := c.ResolveDocumentSourceFence(ctx, api.DocumentSourceFenceResolveRequest{ContentVersionIDs: input.ContentVersionIDs, Filters: filters})
		if err != nil {
			return response{}, err
		}
		profiles, err := c.ListDocumentProcessingProfiles(ctx)
		if err != nil {
			return response{}, err
		}
		selected, candidates, err := selectAnswerSearch(profiles, input)
		if err != nil {
			return response{}, err
		}
		run := func(profile, binding, mode string) (api.DocumentSearchReport, error) {
			return searchResolvedDocuments(ctx, c, api.DocumentSearchRequest{Query: input.Question, Mode: mode, Profile: profile, BindingID: binding, Limit: input.Limit, Rerank: input.Rerank, ContentFirst: true, Fence: api.DocumentSourceFence{VaultUID: fence.Fence.VaultUID, ContentVersionIDs: fence.Fence.ContentVersionIDs}})
		}
		attempt, err := tryProfileSearch(ctx, profiles, selected, candidates, func(code string) bool {
			return input.Profile == "" && daemonconn.BindingSearchFailure(code)
		}, func(profile, binding string) (api.DocumentSearchReport, error) {
			return run(profile, binding, "hybrid")
		})
		if err != nil {
			facts, _ := toolProblemFacts(err)
			if input.Profile != "" || !daemonconn.BindingSearchFailure(facts.Code) {
				return response{}, err
			}
		}
		report, selected, binding, failure := attempt.value, attempt.profile, attempt.binding, attempt.failure
		if binding == "" {
			report, err = run(selected.Name, "", "lexical")
			if err != nil {
				return response{}, err
			}
		}
		docs, err := resolveSearchSummaries(ctx, c, report.Results)
		if err != nil {
			return response{}, err
		}
		return response{fence: fence, profile: selected, binding: binding, report: report, failure: failure, executed: len(fence.Fence.ContentVersionIDs) > 0, documents: docs}, nil
	})
	if err != nil {
		if code, count := stableDomainError(err); code == "scope_too_large" {
			out.Status = answer.IncompleteCoverage
			out.CoverageReasons = append(out.CoverageReasons, "scope_too_large")
			out.Receipt.ObservedScopeCount = count
			return out, nil, nil
		}
		return answerOutput{}, nil, err
	}
	out.Receipt.ObservedScopeCount = result.fence.ObservedScopeCount
	out.Receipt.VaultID = result.fence.Fence.VaultUID
	out.Receipt.FenceFingerprint = result.fence.FenceFingerprint
	out.Receipt.Profile = &answerProfile{Name: result.profile.Name, Fingerprint: result.profile.Fingerprint}
	out.Receipt.BindingID = result.binding
	out.Receipt.SearchExecuted = result.executed
	if result.executed {
		executedHash := sha256.Sum256([]byte(strings.TrimSpace(input.Question)))
		out.Receipt.ExecutedQuerySHA256 = hex.EncodeToString(executedHash[:])
	}
	out.Receipt.RequestedMode = result.report.RequestedMode
	out.Receipt.ActualMode = result.report.ActualMode
	out.Receipt.Degradations = result.report.Degradations
	if out.Receipt.Degradations == nil {
		out.Receipt.Degradations = []string{}
	}
	coverage := result.report.Coverage
	out.Receipt.Coverage = &coverage
	out.Receipt.Reranking = result.report.Reranking
	out.CoverageComplete = (result.report.ActualMode == "semantic" || result.report.ActualMode == "hybrid") && coverage.State == "complete"
	if result.binding == "" || (result.report.ActualMode != "semantic" && result.report.ActualMode != "hybrid") {
		if result.executed {
			out.CoverageReasons = append(out.CoverageReasons, "semantic_unavailable")
			out.SemanticFailure = result.failure
		}
	}
	// An empty resolved scope was validated without searching. There are no
	// documents whose embeddings could be missing, even without a binding.
	if !result.executed {
		out.CoverageComplete = true
	}
	if coverage.State != "complete" {
		out.CoverageReasons = append(out.CoverageReasons, "embeddings_incomplete")
	}
	if result.report.Truncated {
		out.CoverageReasons = append(out.CoverageReasons, "retrieval_truncated")
	}
	for index, item := range result.report.Results {
		ids, err := encodeSearchEvidenceIDs(item.Evidence)
		if err != nil {
			return answerOutput{}, nil, err
		}
		relevance := searchRerankRelevance(item, index, result.report.Reranking)
		if answerContentExcerpt(item) {
			out.Passages = append(out.Passages, answer.Passage{NodeID: item.NodeID, ContentVersionID: item.ContentVersionID, Path: item.Path, Rank: item.Rank, Text: item.Excerpt, TextBytes: len(item.Excerpt), EvidenceIDs: ids, Relevance: relevance})
		} else {
			out.Leads = append(out.Leads, answer.Lead{NodeID: item.NodeID, ContentVersionID: item.ContentVersionID, Path: item.Path, Rank: item.Rank, EvidenceIDs: ids, Relevance: relevance})
		}
	}
	out.Passages, out.PassageBytes, out.OmittedPassages = answer.Budget(out.Passages, input.MaxPassageBytes)
	if len(out.Leads) > 20 {
		out.OmittedLeads = len(out.Leads) - 20
		out.Leads = out.Leads[:20]
	}
	if len(out.Leads) > 0 || out.OmittedLeads > 0 {
		out.CoverageReasons = append(out.CoverageReasons, "passage_text_unavailable")
	}
	linksFor := func(retained map[answerCitation]bool) []*sdkmcp.ResourceLink {
		docs := make([]api.DocumentSummary, 0, len(retained))
		for _, d := range result.documents {
			if retained[answerCitation{d.NodeID, d.ContentVersionID}] {
				docs = append(docs, d)
			}
		}
		return documentResourceLinks(result.fence.Fence.VaultUID, docs)
	}
	linkLimit := -1
	for {
		if (out.OmittedPassages > 0 || out.OmittedLeads > 0) && !slices.Contains(out.CoverageReasons, "retrieval_truncated") {
			out.CoverageReasons = append(out.CoverageReasons, "retrieval_truncated")
		}
		out.Receipt.VectorSpaceIDs = out.Receipt.VectorSpaceIDs[:0]
		spaces := map[string]bool{}
		retained := map[answerCitation]bool{}
		for _, p := range out.Passages {
			retained[answerCitation{p.NodeID, p.ContentVersionID}] = true
		}
		for _, l := range out.Leads {
			retained[answerCitation{l.NodeID, l.ContentVersionID}] = true
		}
		for _, item := range result.report.Results {
			if !retained[answerCitation{item.NodeID, item.ContentVersionID}] {
				continue
			}
			for _, evidence := range item.Evidence {
				if evidence.VectorSpaceID != "" {
					spaces[evidence.VectorSpaceID] = true
				}
			}
		}
		for space := range spaces {
			out.Receipt.VectorSpaceIDs = append(out.Receipt.VectorSpaceIDs, space)
		}
		slices.Sort(out.Receipt.VectorSpaceIDs)
		out.Receipt.CitedVersions = out.Receipt.CitedVersions[:0]
		for _, p := range out.Passages {
			out.Receipt.CitedVersions = append(out.Receipt.CitedVersions, answerCitation{p.NodeID, p.ContentVersionID})
		}
		slices.SortFunc(out.Receipt.CitedVersions, func(a, b answerCitation) int {
			if n := cmp.Compare(a.NodeID, b.NodeID); n != 0 {
				return n
			}
			return cmp.Compare(a.ContentVersionID, b.ContentVersionID)
		})
		omitted := out.OmittedPassages > 0 || out.OmittedLeads > 0 || result.report.Truncated
		out.Status = answer.Classify(answer.Evidence{Passages: out.Passages, Leads: out.Leads}, out.CoverageComplete, omitted)
		links := linksFor(retained)
		if linkLimit < 0 {
			linkLimit = len(links)
		}
		if linkLimit < len(links) {
			out.OmittedLinks = len(links) - linkLimit
			links = links[:linkLimit]
		} else {
			out.OmittedLinks = 0
		}
		envelope, err := toolEnvelopeBytes(out, links)
		if err != nil {
			return answerOutput{}, nil, err
		}
		if envelope <= maxToolResponseBytes-(4<<10) {
			return out, links, nil
		}
		if len(links) > 0 {
			linkLimit = len(links) / 2
			continue
		}
		if len(out.Leads) > 0 {
			out.Leads = out.Leads[:len(out.Leads)-1]
			out.OmittedLeads++
			linkLimit = -1
			continue
		}
		if len(out.Passages) > 0 {
			p := out.Passages[len(out.Passages)-1]
			out.PassageBytes -= len(p.Text)
			out.Passages = out.Passages[:len(out.Passages)-1]
			out.OmittedPassages++
			linkLimit = -1
			continue
		}
		return answerOutput{}, nil, fmt.Errorf("%w: answer receipt", errToolResultTooLarge)
	}
}

type profileSearchAttempt[T any] struct {
	value            T
	profile          api.ProcessingProfileSummary
	binding, failure string
}

// tryProfileSearch shares candidate order while callers decide retry and fallback policy.
func tryProfileSearch[T any](ctx context.Context, profiles []api.ProcessingProfileSummary, selected api.ProcessingProfileSummary, candidates []daemonconn.SearchCandidate, retry func(string) bool, run func(string, string) (T, error)) (profileSearchAttempt[T], error) {
	attempt := profileSearchAttempt[T]{profile: selected}
	var err error
	for _, candidate := range candidates {
		attempt.value, err = run(candidate.Profile, candidate.Binding)
		if err == nil {
			for _, profile := range profiles {
				if profile.Name == candidate.Profile {
					attempt.profile = profile
					break
				}
			}
			attempt.binding = candidate.Binding
			return attempt, nil
		}
		if ctx.Err() != nil {
			return attempt, ctx.Err()
		}
		facts, _ := toolProblemFacts(err)
		if !retry(facts.Code) {
			return attempt, err
		}
		if attempt.failure == "" {
			attempt.failure = facts.Code
		}
	}
	return attempt, err
}
