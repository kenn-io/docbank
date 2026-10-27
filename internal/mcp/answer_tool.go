package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
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
	if result.Excerpt == "" || result.LexicalRank == 0 {
		return false
	}
	for _, evidence := range result.Evidence {
		if evidence.Kind == "rendition_segment" || evidence.Kind == "content_blob" {
			return true
		}
	}
	return false
}

var errAnswerGateFailed = errors.New("answer gate failed")

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
type answerReranking struct {
	Requested      bool   `json:"requested"`
	Outcome        string `json:"outcome"`
	CandidateCount int    `json:"candidate_count"`
}
type answerCitation struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
}
type answerReceipt struct {
	QuestionSHA256      string                      `json:"question_sha256"`
	ExecutedQuerySHA256 string                      `json:"executed_query_sha256,omitempty"`
	SearchExecuted      bool                        `json:"search_executed"`
	ObservedScopeCount  int                         `json:"observed_scope_count"`
	VaultID             string                      `json:"vault_id,omitempty"`
	Fence               *sourceFenceOutput          `json:"fence,omitempty"`
	FenceFingerprint    string                      `json:"fence_fingerprint,omitempty"`
	Profile             *answerProfile              `json:"profile,omitempty"`
	BindingID           string                      `json:"binding_id,omitempty"`
	VectorSpaceIDs      []string                    `json:"vector_space_ids"`
	RequestedMode       string                      `json:"requested_mode,omitempty"`
	ActualMode          string                      `json:"actual_mode,omitempty"`
	Degradations        []string                    `json:"degradations"`
	Coverage            *api.DocumentSearchCoverage `json:"coverage,omitempty"`
	Reranking           *answerReranking            `json:"reranking,omitempty"`
	Gates               []answer.GateReceipt        `json:"gates"`
	CitedVersions       []answerCitation            `json:"cited_versions"`
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

type answerProfileLister interface {
	ListDocumentProcessingProfiles(ctx context.Context) ([]api.ProcessingProfileSummary, error)
}

func answerQuestion(ctx context.Context, session documentReadSession, raw []byte) (answerOutput, []*sdkmcp.ResourceLink, error) {
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
	out := answerOutput{privateCache: newPrivateCache(), CoverageReasons: []string{}, Passages: []answer.Passage{}, Leads: []answer.Lead{}, Receipt: answerReceipt{QuestionSHA256: hex.EncodeToString(hash[:]), VectorSpaceIDs: []string{}, Degradations: []string{}, Gates: []answer.GateReceipt{}, CitedVersions: []answerCitation{}}}
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
		lister, ok := c.(answerProfileLister)
		if !ok {
			return response{}, errors.New("profile listing unavailable")
		}
		profiles, err := lister.ListDocumentProcessingProfiles(ctx)
		if err != nil {
			return response{}, err
		}
		slices.SortFunc(profiles, func(a, b api.ProcessingProfileSummary) int { return cmp.Compare(a.Name, b.Name) })
		if len(profiles) == 0 {
			return response{}, api.NewError(http.StatusServiceUnavailable, "processing_unavailable", "no processing profiles available")
		}
		if input.Profile == "" && input.Rerank {
			profiles = slices.DeleteFunc(profiles, func(p api.ProcessingProfileSummary) bool { return !p.RerankingAvailable })
			if len(profiles) == 0 {
				return response{}, api.NewError(http.StatusServiceUnavailable, "reranking_unavailable", "no reranking profiles available")
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
				return response{}, api.NewError(http.StatusBadRequest, "processing_profile_unavailable", "processing profile unavailable")
			}
			candidates = nil
			bindings := slices.Clone(selected.QueryEmbeddingBindings)
			slices.Sort(bindings)
			for _, binding := range bindings {
				if input.BindingID == "" || input.BindingID == binding {
					candidates = append(candidates, daemonconn.SearchCandidate{Profile: selected.Name, Binding: binding})
				}
			}
			if len(candidates) > 1 {
				candidates = candidates[:1]
			}
			if input.BindingID != "" && len(candidates) == 0 {
				return response{}, api.NewError(http.StatusBadRequest, "processing_profile_unavailable", "query binding unavailable")
			}
		}
		findProfile := func(name string) api.ProcessingProfileSummary {
			for _, p := range profiles {
				if p.Name == name {
					return p
				}
			}
			return selected
		}
		run := func(profile, binding, mode string) (api.DocumentSearchReport, error) {
			if len(fence.Fence.ContentVersionIDs) == 0 {
				err := c.ValidateDocumentSearch(ctx, api.DocumentSearchValidationRequest{Query: input.Question, Mode: mode, Profile: profile, BindingID: binding, Limit: input.Limit, Rerank: input.Rerank, ContentFirst: true})
				return api.DocumentSearchReport{RequestedMode: mode, ActualMode: mode, Coverage: api.DocumentSearchCoverage{State: "complete"}, Degradations: []string{}, Results: []api.DocumentSearchResult{}}, err
			}
			return c.SearchDocuments(ctx, api.DocumentSearchRequest{Query: input.Question, Mode: mode, Profile: profile, BindingID: binding, Limit: input.Limit, Rerank: input.Rerank, ContentFirst: true, Fence: api.DocumentSourceFence{VaultUID: fence.Fence.VaultUID, ContentVersionIDs: fence.Fence.ContentVersionIDs}})
		}
		var report api.DocumentSearchReport
		var binding, failure string
		for _, candidate := range candidates {
			report, err = run(candidate.Profile, candidate.Binding, "hybrid")
			if err == nil {
				selected = findProfile(candidate.Profile)
				binding = candidate.Binding
				break
			}
			code, _ := daemonconn.ProblemCode(err)
			if native, ok := errors.AsType[*api.Error](err); ok {
				code = native.Code
			}
			if ctx.Err() != nil {
				return response{}, ctx.Err()
			}
			// The CLI retries processing_failed, but it can also describe an
			// unrelated or fail-closed provider failure. Preserve it here.
			if input.Profile != "" || code == "processing_failed" || !daemonconn.BindingSearchFailure(code) {
				return response{}, err
			}
			if failure == "" {
				failure = code
			}
		}
		if binding == "" {
			report, err = run(selected.Name, "", "lexical")
			if err != nil {
				return response{}, err
			}
		}
		identities := make([]api.DocumentIdentity, len(report.Results))
		for i, r := range report.Results {
			identities[i] = api.DocumentIdentity{NodeID: r.NodeID, ContentVersionID: r.ContentVersionID, Path: r.Path}
		}
		var docs []api.DocumentSummary
		if len(identities) > 0 {
			docs, err = c.ResolveDocumentSummaries(ctx, api.DocumentSummaryResolveRequest{Identities: identities})
			if err != nil {
				return response{}, err
			}
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
	out.Receipt.Fence = &sourceFenceOutput{VaultID: result.fence.Fence.VaultUID, ContentVersionIDs: result.fence.Fence.ContentVersionIDs}
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
	if result.report.Reranking != nil {
		out.Receipt.Reranking = &answerReranking{Requested: input.Rerank, Outcome: result.report.Reranking.Outcome, CandidateCount: result.report.Reranking.CandidateCount}
	} else {
		outcome := "not_requested"
		if input.Rerank {
			outcome = "not_reported"
		}
		out.Receipt.Reranking = &answerReranking{Requested: input.Rerank, Outcome: outcome}
	}
	out.CoverageComplete = (result.report.ActualMode == "semantic" || result.report.ActualMode == "hybrid") && coverage.State == "complete" && coverage.CompleteDocuments == coverage.ScopedDocuments
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
	if coverage.State != "complete" || coverage.CompleteDocuments != coverage.ScopedDocuments {
		out.CoverageReasons = append(out.CoverageReasons, "embeddings_incomplete")
	}
	if result.report.Truncated {
		out.CoverageReasons = append(out.CoverageReasons, "retrieval_truncated")
	}
	for _, item := range result.report.Results {
		ids, err := encodeSearchEvidenceIDs(item.Evidence)
		if err != nil {
			return answerOutput{}, nil, err
		}
		var relevance *float64
		if result.report.Reranking != nil && result.report.Reranking.Outcome == "applied" && item.Rank <= result.report.Reranking.CandidateCount {
			score := item.Score
			relevance = &score
		}
		if answerContentExcerpt(item) {
			out.Passages = append(out.Passages, answer.Passage{NodeID: item.NodeID, ContentVersionID: item.ContentVersionID, Path: item.Path, Rank: item.Rank, Text: item.Excerpt, TextBytes: len(item.Excerpt), EvidenceIDs: ids, Relevance: relevance})
		} else {
			out.Leads = append(out.Leads, answer.Lead{NodeID: item.NodeID, ContentVersionID: item.ContentVersionID, Path: item.Path, Rank: item.Rank, EvidenceIDs: ids, Relevance: relevance})
		}
	}
	evidence, gates, err := answer.ApplyGates(ctx, answer.Question{Text: input.Question}, answer.Evidence{Passages: out.Passages, Leads: out.Leads}, nil)
	if err != nil {
		return answerOutput{}, nil, fmt.Errorf("%w: %w", errAnswerGateFailed, err)
	}
	out.Passages, out.Leads, out.Receipt.Gates = evidence.Passages, evidence.Leads, gates
	out.Passages, out.PassageBytes, out.OmittedPassages = answer.Budget(out.Passages, input.MaxPassageBytes)
	if len(out.Leads) > 20 {
		out.OmittedLeads = len(out.Leads) - 20
		out.Leads = out.Leads[:20]
	}
	if len(out.Leads) > 0 || out.OmittedLeads > 0 {
		out.CoverageReasons = append(out.CoverageReasons, "passage_text_unavailable")
	}
	linksFor := func() []*sdkmcp.ResourceLink {
		keep := map[answerCitation]bool{}
		for _, p := range out.Passages {
			keep[answerCitation{p.NodeID, p.ContentVersionID}] = true
		}
		for _, l := range out.Leads {
			keep[answerCitation{l.NodeID, l.ContentVersionID}] = true
		}
		docs := make([]api.DocumentSummary, 0, len(keep))
		for _, d := range result.documents {
			if keep[answerCitation{d.NodeID, d.ContentVersionID}] {
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
		addSpaces := func(ids []string) error {
			for _, id := range ids {
				var e api.DocumentEvidenceReference
				if err := json.Unmarshal([]byte(id), &e); err != nil {
					return err
				}
				if e.VectorSpaceID != "" {
					spaces[e.VectorSpaceID] = true
				}
			}
			return nil
		}
		for _, p := range out.Passages {
			if err := addSpaces(p.EvidenceIDs); err != nil {
				return answerOutput{}, nil, err
			}
		}
		for _, l := range out.Leads {
			if err := addSpaces(l.EvidenceIDs); err != nil {
				return answerOutput{}, nil, err
			}
		}
		for space := range spaces {
			out.Receipt.VectorSpaceIDs = append(out.Receipt.VectorSpaceIDs, space)
		}
		slices.Sort(out.Receipt.VectorSpaceIDs)
		out.Receipt.CitedVersions = out.Receipt.CitedVersions[:0]
		seen := map[answerCitation]bool{}
		for _, p := range out.Passages {
			c := answerCitation{p.NodeID, p.ContentVersionID}
			if !seen[c] {
				seen[c] = true
				out.Receipt.CitedVersions = append(out.Receipt.CitedVersions, c)
			}
		}
		slices.SortFunc(out.Receipt.CitedVersions, func(a, b answerCitation) int {
			if n := cmp.Compare(a.NodeID, b.NodeID); n != 0 {
				return n
			}
			return cmp.Compare(a.ContentVersionID, b.ContentVersionID)
		})
		omitted := out.OmittedPassages > 0 || out.OmittedLeads > 0 || result.report.Truncated
		out.Status = answer.Classify(answer.Evidence{Passages: out.Passages, Leads: out.Leads}, out.CoverageComplete, omitted)
		links := linksFor()
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
