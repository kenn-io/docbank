package rerankeval_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embedding"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSyntheticComparison(t *testing.T) {
	corpus := loadCorpus(t)
	fixture := newComparisonFixture(t, corpus)
	runner := &comparisonRunner{fixture: fixture}
	systems := comparisonSystems(fixture.vectorSpace, map[string]string{})
	report, err := embeddingeval.Evaluate(t.Context(), corpus, systems, 1, runner)
	require.NoError(t, err)
	require.Len(t, report.Systems, len(systems))
	assert.Positive(t, fixture.lexicalCalls)
	assert.Positive(t, fixture.semanticCalls)
	assert.Positive(t, fixture.fusionCalls)
	assert.Equal(t, 18, runner.rerankCalls)
	for _, system := range report.Systems {
		require.Len(t, system.Trials, 1)
		require.Len(t, system.Trials[0].Queries, len(corpus.Queries))
	}
	for _, excerpt := range runner.providerSeen {
		assert.LessOrEqual(t, len([]byte(excerpt)), 4<<10)
		assert.NotContains(t, excerpt, "judgment-grade")
		assert.NotContains(t, excerpt, ".txt")
	}
	assert.Len(t, runner.providerQueries, runner.rerankCalls)
	for _, query := range runner.providerQueries {
		assert.Contains(t, []string{"lumen archive lantern", "lumen archive meadow"}, query)
	}
	t.Log(renderComparisonTable(report))
	t.Log(renderComparisonJSON(report))
}

func TestCorpusIsSyntheticAndJudgedDocumentsAvoidQueryTerms(t *testing.T) {
	corpus := loadCorpus(t)
	fixture := newComparisonFixture(t, corpus)
	byID := make(map[string]embeddingeval.Document, len(corpus.Documents))
	for _, doc := range corpus.Documents {
		byID[doc.ID] = doc
		assert.NotContains(t, doc.ID, "\\")
		assert.NotContains(t, doc.ID, "/")
		assert.NotContains(t, doc.Text, "@")
	}
	for _, query := range corpus.Queries {
		for _, judgment := range query.Judgments {
			doc := byID[judgment.DocumentID]
			nodeID, ok := fixture.documentToNode[judgment.DocumentID]
			require.True(t, ok)
			view, err := fixture.store.NodeViewByID(t.Context(), nodeID)
			require.NoError(t, err)
			filename := filepath.Base(view.Path)
			assert.Equal(t, view.Node.Name, filename)
			for term := range strings.FieldsSeq(strings.ToLower(query.Text)) {
				assert.NotContains(t, strings.ToLower(doc.Text), term)
				assert.NotContains(t, strings.ToLower(filename), term)
				assert.NotContains(t, strings.ToLower(view.Path), term)
			}
		}
	}
}

func TestComparisonReport(t *testing.T) {
	report := reportForRendering(t)
	table := renderComparisonTable(report)
	jsonReport := renderComparisonJSON(report)
	assert.Contains(t, table, "| corpus | version | judgment provenance | mode | reranker | request shape | status | top-1 | top-10 | nDCG@10 | p95 latency | requests/query | rerank requests/query | tokens/query | rerank tokens/query | cost/query micros | cost basis | reason |")
	assert.Contains(t, table, "| synthetic-rerank-public | 1 | fixture:synthetic-public-graded-v1 | lexical | none | n/a | synthetic | 1.000 | 1.000 | 1.000 | ")
	assert.Contains(t, table, "| synthetic-rerank-public | 1 | fixture:synthetic-public-graded-v1 | lexical | none | n/a | synthetic | 1.000 | 1.000 | 1.000 | 0s | 0.000 | 0.000 | 0.000 | 0.000 | 0 | 2026-09-21:local-zero | ")
	assert.Contains(t, table, "| synthetic-rerank-public | 1 | fixture:synthetic-public-graded-v1 | semantic | jev | batched | synthetic | ")
	assert.Contains(t, table, "| synthetic-rerank-public | 1 | fixture:synthetic-public-graded-v1 | semantic | jev | batched | synthetic | 1.000 | 1.000 | 1.000 | 0s | 1.000 | 1.000 | 2.000 | 2.000 | 17 | 2026-09-21:jev-token-rate,2026-09-21:local-zero | ")
	assert.Contains(t, table, "| synthetic-rerank-public | 1 | fixture:synthetic-public-graded-v1 | all | zeroentropy | n/a | not-run | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable | unavailable | not part of target |")
	assert.Contains(t, jsonReport, `"top_1":"unavailable"`)
	assert.Contains(t, jsonReport, `"cost_per_query_micros":"unavailable"`)
	assert.Contains(t, jsonReport, `"status":"not-run"`)
	assert.Contains(t, jsonReport, `"reason":"not part of target"`)
}

func TestLiveEnvironmentGate(t *testing.T) {
	for _, test := range []struct {
		name     string
		cohere   string
		typesafe string
		want     bool
	}{
		{name: "missing", want: false},
		{name: "cohere-only", cohere: "synthetic-key", want: false},
		{name: "typesafe-only", typesafe: "synthetic-key", want: false},
		{name: "both", cohere: "synthetic-key", typesafe: "synthetic-key", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("COHERE_API_KEY", test.cohere)
			t.Setenv("TYPESAFE_API_KEY", test.typesafe)
			assert.Equal(t, test.want, liveKeysPresent())
		})
	}
}

const syntheticComparisonCaveat = "synthetic provider rows and query vectors validate wiring only; unavailable usage and cost cells have no provider measurement; no quality recommendation is implied"

func reportForRendering(t *testing.T) embeddingeval.Report {
	t.Helper()
	corpus := embeddingeval.Corpus{
		ID: "synthetic-rerank-public", Version: "1",
		Documents: []embeddingeval.Document{{ID: "hit", Text: "synthetic hit"}, {ID: "miss", Text: "synthetic miss"}},
		Queries:   []embeddingeval.Query{{ID: "q", Text: "synthetic query", Judgments: []embeddingeval.Judgment{{DocumentID: "hit", Grade: 2}}}},
	}
	systems := []embeddingeval.System{
		{ID: "lexical/none", RecipeFingerprint: "recipe"},
		{ID: "semantic/jev-batched", RecipeFingerprint: "recipe", Reranker: "jev-batched", RerankerFingerprint: "policy", RerankTopN: 2},
		{ID: "hybrid/cohere", RecipeFingerprint: "recipe", Reranker: "cohere", RerankerFingerprint: "policy", RerankTopN: 2},
	}
	report, err := embeddingeval.Evaluate(t.Context(), corpus, systems, 1, reportRunner{})
	require.NoError(t, err)
	return report
}

type reportRunner struct{}

func (reportRunner) Search(context.Context, embeddingeval.System, embeddingeval.Corpus, embeddingeval.Query) (embeddingeval.SearchResult, error) {
	zero := float64(0)
	return embeddingeval.SearchResult{DocumentIDs: []string{"hit", "miss"}, Usage: embeddingeval.Usage{
		TokenUsage: &zero, Cost: &embeddingeval.CostObservation{Basis: "2026-09-21:local-zero"},
	}}, nil
}

func (reportRunner) Rerank(_ context.Context, system embeddingeval.System, _ string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	usage := embeddingeval.Usage{ProviderCalls: 1}
	if system.Reranker == "jev-batched" {
		tokens := 2.0
		usage.TokenUsage = &tokens
		usage.Cost = &embeddingeval.CostObservation{Micros: 17, Basis: "2026-09-21:jev-token-rate"}
	}
	return embeddingeval.RerankResult{Scores: []float64{0.9, 0.1}, Usage: usage}, nil
}

type comparisonCorpus struct {
	ID        string           `json:"id"`
	Version   string           `json:"version"`
	Documents []corpusDocument `json:"documents"`
	Queries   []corpusQuery    `json:"queries"`
}

type corpusDocument struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type corpusQuery struct {
	ID        string           `json:"id"`
	Text      string           `json:"text"`
	Judgments []corpusJudgment `json:"judgments"`
}

type corpusJudgment struct {
	DocumentID string `json:"document_id"`
	Grade      int    `json:"grade"`
	Critical   bool   `json:"critical"`
}

func loadCorpus(t *testing.T) embeddingeval.Corpus {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "corpus.json"))
	require.NoError(t, err)
	var source comparisonCorpus
	require.NoError(t, json.Unmarshal(data, &source))
	corpus := embeddingeval.Corpus{ID: source.ID, Version: source.Version}
	for _, doc := range source.Documents {
		corpus.Documents = append(corpus.Documents, embeddingeval.Document{ID: doc.ID, Text: doc.Text})
	}
	for _, query := range source.Queries {
		converted := embeddingeval.Query{ID: query.ID, Text: query.Text}
		for _, judgment := range query.Judgments {
			converted.Judgments = append(converted.Judgments, embeddingeval.Judgment{
				DocumentID: judgment.DocumentID, Grade: judgment.Grade, Critical: judgment.Critical,
			})
		}
		corpus.Queries = append(corpus.Queries, converted)
	}
	return corpus
}

type comparisonFixture struct {
	store          *store.Store
	nodeToDocument map[int64]string
	documentToNode map[string]int64
	generation     *vectorindex.Generation
	vectorSpace    string
	// The target scope has no query embedding owner, so output labels this fixture as wiring-only.
	syntheticQueryVectors map[string][]float32
	lexicalCalls          int
	semanticCalls         int
	fusionCalls           int
}

func newComparisonFixture(t *testing.T, corpus embeddingeval.Corpus) *comparisonFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-vault.db")
	vault, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	fixture := &comparisonFixture{store: vault, vectorSpace: strings.Repeat("a", 64), syntheticQueryVectors: map[string][]float32{
		"lumen archive lantern": {1, 0}, "lumen archive meadow": {0, 1},
	}}
	fixture.nodeToDocument = make(map[int64]string, len(corpus.Documents))
	fixture.documentToNode = make(map[string]int64, len(corpus.Documents))
	for index, doc := range corpus.Documents {
		bytes := []byte(doc.Text)
		hash := sha256.Sum256(append([]byte("blob:"), []byte(doc.ID)...))
		blobHash := hex.EncodeToString(hash[:])
		name := fmt.Sprintf("synthetic-document-%03d.txt", index)
		node, err := vault.CreateFile(t.Context(), vault.RootID(), name, blobHash,
			int64(len(bytes)), "text/plain", store.BlobPhysical{Encoding: "raw", StoredBytes: int64(len(bytes)), Created: true})
		require.NoError(t, err)
		require.NoError(t, vault.RecordExtraction(t.Context(), store.ExtractionResult{
			BlobHash: blobHash, Extractor: "synthetic-eval", ExtractorVersion: 1,
			Status: store.ExtractionOK, Text: doc.Text,
		}))
		fixture.nodeToDocument[node.ID] = doc.ID
		fixture.documentToNode[doc.ID] = node.ID
	}
	set := makeVectorSet(t, corpus, fixture.vectorSpace)
	encoded, setID, err := document.EncodeVectorSetV1(set)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)
	manifest, err := vectorindex.NewManifest([]string{setID})
	require.NoError(t, err)
	fixture.generation, err = vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
	require.NoError(t, err)
	return fixture
}

func makeVectorSet(t *testing.T, corpus embeddingeval.Corpus, vectorSpace string) document.VectorSetV1 {
	t.Helper()
	values := make([][]float64, 0, len(corpus.Documents))
	keys := make([]string, 0, len(corpus.Documents))
	checksums := make([]string, 0, len(corpus.Documents))
	for _, doc := range corpus.Documents {
		keys = append(keys, doc.ID)
		checksums = append(checksums, sha256String("input:"+doc.ID))
		value := []float64{-1, 0}
		switch doc.ID {
		case "relevant-lantern":
			value = []float64{1, 0}
		case "relevant-meadow":
			value = []float64{0, 1}
		case "near-miss":
			value = []float64{0.70710677, 0.70710677}
		}
		values = append(values, value)
	}
	set, err := document.NewVectorSetV1(document.VectorSetV1Input{
		VectorSpaceFingerprint: vectorSpace, Metric: document.VectorMetricCosine,
		Normalization: document.VectorNormalizationUnitLength, Dimension: 2,
		InputKeys: keys, InputChecksums: checksums, Values: values,
	})
	require.NoError(t, err)
	return set
}

func sha256String(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

type comparisonRunner struct {
	fixture         *comparisonFixture
	rerankCalls     int
	providerSeen    []string
	providerQueries []string
}

func (runner *comparisonRunner) Search(ctx context.Context, system embeddingeval.System, _ embeddingeval.Corpus, query embeddingeval.Query) (embeddingeval.SearchResult, error) {
	started := time.Now()
	var ranking []string
	mode, _, _ := strings.Cut(system.ID, "/")
	switch mode {
	case "lexical":
		runner.fixture.lexicalCalls++
		hits, _, err := runner.fixture.store.SearchExplainedLexicalCandidates(ctx, query.Text, 4, store.SearchOptions{})
		if err != nil {
			return embeddingeval.SearchResult{}, err
		}
		for _, hit := range hits {
			if id := runner.fixture.nodeToDocument[hit.Node.ID]; id != "" {
				ranking = append(ranking, id)
			}
		}
	case "semantic":
		runner.fixture.semanticCalls++
		neighbors, err := runner.fixture.generation.Search(runner.fixture.syntheticQueryVectors[query.Text], 4)
		if err != nil {
			return embeddingeval.SearchResult{}, err
		}
		for _, neighbor := range neighbors {
			ranking = append(ranking, neighbor.InputKey)
		}
	case "hybrid":
		runner.fixture.fusionCalls++
		lexical, err := runner.lexicalCandidates(ctx, query)
		if err != nil {
			return embeddingeval.SearchResult{}, err
		}
		neighbors, err := runner.fixture.generation.Search(runner.fixture.syntheticQueryVectors[query.Text], 4)
		if err != nil {
			return embeddingeval.SearchResult{}, err
		}
		semantic := embedding.ScopedCandidates{Candidates: make([]embedding.RankedCandidate, len(neighbors))}
		for index, neighbor := range neighbors {
			semantic.Candidates[index] = embedding.RankedCandidate{Key: neighbor.InputKey, Rank: index + 1, Score: neighbor.Score}
		}
		fused, err := embedding.FuseReciprocalRank(embedding.FusionInput{Lexical: lexical, Semantic: semantic}, 4)
		if err != nil {
			return embeddingeval.SearchResult{}, err
		}
		for _, candidate := range fused.Candidates {
			ranking = append(ranking, candidate.Key)
		}
	default:
		return embeddingeval.SearchResult{}, fmt.Errorf("unknown comparison mode %q", mode)
	}
	zero := float64(0)
	return embeddingeval.SearchResult{DocumentIDs: ranking, Usage: embeddingeval.Usage{
		Latency: time.Since(started), TokenUsage: &zero,
		Cost: &embeddingeval.CostObservation{Basis: "measured-zero:local-retrieval"},
	}}, nil
}

func (runner *comparisonRunner) lexicalCandidates(ctx context.Context, query embeddingeval.Query) (embedding.ScopedCandidates, error) {
	hits, _, err := runner.fixture.store.SearchExplainedLexicalCandidates(ctx, query.Text, 4, store.SearchOptions{})
	if err != nil {
		return embedding.ScopedCandidates{}, err
	}
	candidates := make([]embedding.RankedCandidate, 0, len(hits))
	for index, hit := range hits {
		if id := runner.fixture.nodeToDocument[hit.Node.ID]; id != "" {
			candidates = append(candidates, embedding.RankedCandidate{Key: id, Rank: index + 1, Score: float64(len(hits) - index)})
		}
	}
	return embedding.ScopedCandidates{Candidates: candidates}, nil
}

func (runner *comparisonRunner) recordProviderInput(query string, candidates []embeddingeval.Document) {
	runner.rerankCalls++
	for _, candidate := range candidates {
		runner.providerSeen = append(runner.providerSeen, candidate.Text)
	}
	runner.providerQueries = append(runner.providerQueries, query)
}

func (runner *comparisonRunner) Rerank(_ context.Context, system embeddingeval.System, query string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	runner.recordProviderInput(query, candidates)
	scores := make([]float64, len(candidates))
	for index := range candidates {
		scores[index] = float64(len(candidates) - index)
	}
	usage := embeddingeval.Usage{}
	switch system.Reranker {
	case "jev-per-candidate":
		usage.ProviderCalls = len(candidates)
	case "jev-batched":
		usage.ProviderCalls = 1
	case "cohere":
		usage.ProviderCalls = 1
	}
	return embeddingeval.RerankResult{Scores: scores, Usage: usage}, nil
}

type comparisonReranker interface {
	Rerank(ctx context.Context, system embeddingeval.System, query string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error)
	PolicyFingerprint() string
}

type providerComparisonRunner struct {
	*comparisonRunner

	adapters map[string]comparisonReranker
}

func (runner *providerComparisonRunner) Rerank(ctx context.Context, system embeddingeval.System, query string, candidates []embeddingeval.Document) (embeddingeval.RerankResult, error) {
	runner.recordProviderInput(query, candidates)
	adapter := runner.adapters[system.Reranker]
	if adapter == nil {
		return embeddingeval.RerankResult{}, fmt.Errorf("reranker %q is not configured", system.Reranker)
	}
	if adapter.PolicyFingerprint() != system.RerankerFingerprint {
		return embeddingeval.RerankResult{}, fmt.Errorf("reranker %q fingerprint does not match the system", system.Reranker)
	}
	return adapter.Rerank(ctx, system, query, candidates)
}

func comparisonSystems(vectorSpace string, fingerprints map[string]string) []embeddingeval.System {
	systems := make([]embeddingeval.System, 0, 12)
	for _, mode := range []string{"lexical", "semantic", "hybrid"} {
		for _, arm := range []string{"none", "cohere", "jev-per-candidate", "jev-batched"} {
			system := embeddingeval.System{
				ID: mode + "/" + arm, RecipeFingerprint: "synthetic-recipe-" + mode,
				VectorSpaceFingerprint: vectorSpace,
			}
			if arm != "none" {
				system.Reranker = arm
				system.RerankerFingerprint = fingerprints[arm]
				if system.RerankerFingerprint == "" {
					system.RerankerFingerprint = "synthetic-policy"
				}
				system.RerankTopN = 4
			}
			systems = append(systems, system)
		}
	}
	return systems
}

type comparisonRow struct {
	CorpusID           string `json:"corpus_id"`
	CorpusVersion      string `json:"corpus_version"`
	JudgmentProvenance string `json:"judgment_provenance"`
	Mode               string `json:"mode"`
	Reranker           string `json:"reranker"`
	RequestShape       string `json:"request_shape"`
	Status             string `json:"status"`
	Top1               string `json:"top_1"`
	Top10              string `json:"top_10"`
	NDCG               string `json:"ndcg_at_10"`
	P95                string `json:"p95_latency"`
	Requests           string `json:"requests_per_query"`
	RerankRequests     string `json:"rerank_requests_per_query"`
	Tokens             string `json:"tokens_per_query"`
	RerankTokens       string `json:"rerank_tokens_per_query"`
	CostMicros         string `json:"cost_per_query_micros"`
	CostBasis          string `json:"cost_basis"`
	Reason             string `json:"reason"`
}

func renderComparisonTable(report embeddingeval.Report) string {
	return renderComparisonTableForStatus(report, "synthetic", syntheticComparisonCaveat)
}

func renderComparisonTableForStatus(report embeddingeval.Report, status, reason string) string {
	return renderRows(comparisonRows(report, status, reason))
}

func comparisonRows(report embeddingeval.Report, status, reason string) []comparisonRow {
	rows := make([]comparisonRow, 0, len(report.Systems)+1)
	for _, system := range report.Systems {
		mode, arm, _ := strings.Cut(system.System.ID, "/")
		reranker, requestShape := comparisonArm(arm)
		performance := system.Performance
		row := comparisonRow{
			CorpusID: report.CorpusID, CorpusVersion: report.CorpusVersion,
			JudgmentProvenance: "fixture:synthetic-public-graded-v1", Mode: mode,
			Reranker: reranker, RequestShape: requestShape, Status: status,
			Top1: "unavailable", Top10: "unavailable", NDCG: "unavailable",
			P95: "unavailable", Requests: "unavailable", RerankRequests: "unavailable",
			Tokens: "unavailable", RerankTokens: "unavailable",
			CostMicros: "unavailable", CostBasis: "unavailable", Reason: reason,
		}
		row.Top1 = fmt.Sprintf("%.3f", system.Aggregate.HitAt1.Mean)
		row.Top10 = fmt.Sprintf("%.3f", system.Aggregate.HitAt10.Mean)
		row.NDCG = fmt.Sprintf("%.3f", system.Aggregate.NDCGAt10.Mean)
		row.P95 = performance.P95Latency.String()
		row.Requests = fmt.Sprintf("%.3f", performance.RequestsPerQuery)
		row.RerankRequests = fmt.Sprintf("%.3f", performance.RerankRequestsPerQuery)
		if performance.TokensPerQuery != nil {
			row.Tokens = fmt.Sprintf("%.3f", *performance.TokensPerQuery)
		}
		if performance.RerankTokensPerQuery != nil {
			row.RerankTokens = fmt.Sprintf("%.3f", *performance.RerankTokensPerQuery)
		}
		if performance.CostPerQuery != nil {
			row.CostMicros = strconv.FormatInt(performance.CostPerQuery.Micros, 10)
			row.CostBasis = performance.CostPerQuery.Basis
		}
		if status != "not-run" {
			missing := make([]string, 0, 2)
			if row.Tokens == "unavailable" || row.RerankTokens == "unavailable" {
				missing = append(missing, "token evidence is unavailable where the provider receipt does not report it")
			}
			if row.CostMicros == "unavailable" {
				missing = append(missing, "cost is unavailable without complete evidence and a caller-supplied dated rate")
			}
			if len(missing) > 0 {
				row.Reason = strings.Join(append([]string{row.Reason}, missing...), "; ")
			}
		}
		rows = append(rows, row)
	}
	rows = append(rows, comparisonRow{
		CorpusID: report.CorpusID, CorpusVersion: report.CorpusVersion,
		JudgmentProvenance: "fixture:synthetic-public-graded-v1", Mode: "all",
		Reranker: "zeroentropy", RequestShape: "n/a", Status: "not-run",
		Top1: "unavailable", Top10: "unavailable", NDCG: "unavailable",
		P95: "unavailable", Requests: "unavailable", RerankRequests: "unavailable",
		Tokens: "unavailable", RerankTokens: "unavailable",
		CostMicros: "unavailable", CostBasis: "unavailable", Reason: "not part of target",
	})
	return rows
}

func renderRows(rows []comparisonRow) string {
	var builder strings.Builder
	builder.WriteString("| corpus | version | judgment provenance | mode | reranker | request shape | status | top-1 | top-10 | nDCG@10 | p95 latency | requests/query | rerank requests/query | tokens/query | rerank tokens/query | cost/query micros | cost basis | reason |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, row := range rows {
		values := []string{row.CorpusID, row.CorpusVersion, row.JudgmentProvenance, row.Mode, row.Reranker, row.RequestShape, row.Status, row.Top1, row.Top10, row.NDCG, row.P95, row.Requests, row.RerankRequests, row.Tokens, row.RerankTokens, row.CostMicros, row.CostBasis, row.Reason}
		for index := range values {
			if values[index] == "" {
				values[index] = "unavailable"
			}
		}
		fmt.Fprintf(&builder, "| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7], values[8], values[9], values[10], values[11], values[12], values[13], values[14], values[15], values[16], values[17])
	}
	return builder.String()
}

func renderComparisonJSON(report embeddingeval.Report) string {
	return renderComparisonJSONForStatus(report, "synthetic", syntheticComparisonCaveat)
}

func renderComparisonJSONForStatus(report embeddingeval.Report, status, reason string) string {
	rows := comparisonRows(report, status, reason)
	encoded, err := json.Marshal(rows)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func comparisonArm(arm string) (string, string) {
	switch arm {
	case "none":
		return "none", "n/a"
	case "cohere":
		return "cohere", "n/a"
	case "jev-per-candidate":
		return "jev", "per_candidate"
	case "jev-batched":
		return "jev", "batched"
	default:
		return arm, "unknown"
	}
}

var _ embeddingeval.RerankingRunner = (*comparisonRunner)(nil)
var _ embeddingeval.RerankingRunner = (*providerComparisonRunner)(nil)
