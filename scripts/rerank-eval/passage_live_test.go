package rerankeval_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tiktoken-go/tokenizer"
	"go.kenn.io/docbank/document"
	embeddingeval "go.kenn.io/docbank/document/embedding/eval"
	"go.kenn.io/docbank/document/providerhttp"
	"go.kenn.io/docbank/document/voyage"
)

func TestLivePassageGate(t *testing.T) {
	if os.Getenv("RERANK_EVAL_LIVE") != "1" {
		t.Skip("set RERANK_EVAL_LIVE=1; preregister the fixed gate before running")
	}
	output := os.Getenv("RERANK_PASSAGE_OUTPUT")
	repo, err := filepath.Abs("../..")
	require.NoError(t, err)
	absolute := output
	require.True(t, filepath.IsAbs(absolute), "output must be absolute")
	if strings.EqualFold(filepath.VolumeName(repo), filepath.VolumeName(absolute)) {
		rel, err := filepath.Rel(repo, absolute)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)), "output must be outside the repository")
	}
	require.NoError(t, os.Mkdir(absolute, 0700), "output directory must not exist")
	data, err := os.ReadFile(os.Getenv("RERANK_PASSAGE_CORPUS"))
	require.NoError(t, err)
	require.Equal(t, "3bf89e1acb201889477235bbc7d587ed514383c853fa2060c00764fade6d8050", sha256String(string(data)), "use the pinned converter output")
	corpus, err := decodeComparisonCorpus(data)
	require.NoError(t, err)
	codec, err := tokenizer.Get(tokenizer.Cl100kBase)
	require.NoError(t, err)
	profile, err := passageEmbeddingProfile()
	require.NoError(t, err)
	provider, err := voyage.NewEmbeddingProvider(profile, envSecrets{}, nil)
	require.NoError(t, err)
	transport, err := providerhttp.NewTransport(passageEgress(), nil)
	require.NoError(t, err)
	defer transport.CloseIdleConnections()
	interrupt, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, 2*time.Hour)
	defer cancel()
	budget := &passageBudget{started: time.Now()}
	runner := &passageVoyageRunner{endpoint: "https://api.voyageai.com/v1/rerank", client: &http.Client{Transport: transport, CheckRedirect: providerhttp.RefuseRedirects}, secret: func() string { return os.Getenv("VOYAGE_API_KEY") }, codec: codec, budget: budget}
	report := passageGateReport{}
	defer writePassageGateReport(t, absolute, budget, &report)
	for _, sample := range []struct {
		name   string
		corpus embeddingeval.Corpus
	}{{"short", loadCorpus(t)}, {"synthetic", passageLongCorpus()}, {"public", corpus}} {
		err := runPassageCorpus(ctx, t.TempDir(), sample.name, sample.corpus, &passageBudgetedEmbeddings{EmbeddingProvider: provider, codec: codec, budget: budget}, runner, &report)
		if err != nil {
			report.Observations = append(report.Observations, passageObservation{Corpus: sample.name, Failure: err.Error()})
			return
		}
	}
}

func writePassageGateReport(t *testing.T, output string, budget *passageBudget, report *passageGateReport) {
	t.Helper()
	budget.Elapsed = time.Since(budget.started)
	report.Summary.Budget = *budget
	report.finish()
	for name, value := range map[string]any{"observations.json": report, "aggregate.json": report.Summary} {
		encoded, e := json.MarshalIndent(value, "", "  ")
		require.NoError(t, e)
		require.NoError(t, os.WriteFile(filepath.Join(output, name), encoded, 0600))
	}
	markdown, e := report.markdown()
	require.NoError(t, e)
	require.NoError(t, os.WriteFile(filepath.Join(output, "report.md"), []byte(markdown), 0600))
	t.Log(markdown)
}

func runPassageCorpus(ctx context.Context, root, label string, corpus embeddingeval.Corpus, provider document.EmbeddingProvider, runner embeddingeval.PassageRunner, report *passageGateReport) (err error) {
	started := time.Now()
	fixture, err := preparePassageCorpus(ctx, root, corpus, provider)
	report.Summary.Preparation += time.Since(started)
	if err != nil {
		return err
	}
	if report.Versions == nil {
		report.Versions = map[string]map[string]string{}
	}
	report.Versions[label] = fixture.versions
	defer func() { err = errors.Join(err, fixture.close()) }()
	texts := map[string]string{}
	for _, doc := range corpus.Documents {
		texts[doc.ID] = doc.Text
	}
	for _, query := range corpus.Queries {
		if err := ctx.Err(); err != nil {
			return err
		}
		started = time.Now()
		prepared, prepareErr := fixture.prepare(ctx, query)
		report.Summary.Preparation += time.Since(started)
		judgments := slices.Clone(query.Judgments)
		proxy := false
		for i, judgment := range judgments {
			version := fixture.versions[judgment.DocumentID]
			judgments[i].DocumentID = version
			excerpt := ""
			if prepared != nil {
				for _, c := range prepared.Candidates {
					if c.DocumentID == version {
						excerpt = c.Excerpt
					}
				}
			}
			if judgment.Grade > 0 && len(texts[judgment.DocumentID]) >= 8192 && !strings.Contains(excerpt, texts[judgment.DocumentID]) {
				proxy = true
			}
		}
		for repeat := range 3 {
			observation := passageObservation{Corpus: label, Query: query.ID, Repeat: repeat, Proxy: proxy}
			runErr := prepareErr
			if runErr == nil {
				observation.Comparison, runErr = embeddingeval.EvaluatePassagesWithOptions(ctx, query.Text, prepared.Candidates, runner, embeddingeval.PassageOptions{Deadline: 5 * time.Second, Rotation: repeat, Select: prepared.Select})
			}
			metrics, scoreErr := embeddingeval.ScorePassageComparison(observation.Comparison, judgments, 5)
			observation.Metrics = metrics
			observation.NDCGAt10 = [4]float64{metrics.Baseline.NDCGAt10, metrics.Current.NDCGAt10, metrics.P1.NDCGAt10, metrics.P3.NDCGAt10}
			observation.HitAt1 = [4]float64{metrics.Baseline.HitAt1, metrics.Current.HitAt1, metrics.P1.HitAt1, metrics.P3.HitAt1}
			if e := errors.Join(runErr, scoreErr); e != nil {
				observation.Failure = e.Error()
			}
			report.Observations = append(report.Observations, observation)
		}
	}
	return nil
}

type passageBudgetedEmbeddings struct {
	document.EmbeddingProvider

	codec  tokenizer.Codec
	budget *passageBudget
}

func (p *passageBudgetedEmbeddings) Embed(ctx context.Context, inputs []document.EmbeddingInput, authorization document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	tokens := 0
	for _, input := range inputs {
		n, err := p.codec.Count(input.Text)
		if err != nil {
			return document.EmbeddingResult{}, fmt.Errorf("count embedding tokens: %w", err)
		}
		tokens += n
	}
	// MaxRetries=1 means one transport attempt; the service classifier is permanent.
	if err := p.budget.reserve(tokens, .12); err != nil {
		return document.EmbeddingResult{}, err
	}
	p.budget.EmbeddingLocalTokens += tokens
	p.budget.EmbeddingReservedMicros += float64(tokens) * .12
	return p.EmbeddingProvider.Embed(ctx, inputs, authorization)
}
