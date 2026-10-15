package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
)

// searchAgentValue follows the OK/key=value convention used by Kata and Roborev.
func searchAgentValue(value string) string {
	value = searchAgentText(value)
	if value == "" || strings.ContainsAny(value, "\"\\") || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return strconv.Quote(value)
	}
	return value
}

func searchAgentText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ReplaceAll(ansi.Strip(value), "\r\n", "\n"))
}

func searchAgentExcerpt(value string) string {
	runes := []rune(strings.Join(strings.Fields(searchAgentText(value)), " "))
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return strings.TrimSpace(string(runes))
}

type searchAgentHeader struct {
	query, mode, degraded, degradedError string
	count                                int
	truncated                            bool
	coverage                             *api.DocumentSearchCoverage
	details                              []string
}

func writeSearchAgentHeader(cmd *cobra.Command, header searchAgentHeader) error {
	degraded := header.degraded
	if degraded == "" {
		degraded = "false"
	}
	line := fmt.Sprintf("OK search count=%d query=%s mode=%s degraded=%s truncated=%t", header.count,
		searchAgentValue(header.query), searchAgentValue(header.mode), searchAgentValue(degraded), header.truncated)
	if header.degradedError != "" {
		line += " degraded_error=" + searchAgentValue(header.degradedError)
	}
	if coverage := header.coverage; coverage != nil {
		line += fmt.Sprintf(" coverage=%s complete=%d scoped=%d", searchAgentValue(coverage.State),
			coverage.CompleteDocuments, coverage.ScopedDocuments)
	}
	if len(header.details) > 0 {
		line += " " + strings.Join(header.details, " ")
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("writing search agent header: %w", err)
	}
	return nil
}

// searchAgentHit is one result line. Ranked hits come from processing search
// and carry its score and excerpt; ordinary lexical hits have neither.
type searchAgentHit struct {
	nodeID      int64
	match, path string
	ranked      bool
	score       float64
	excerpt     string
}

func writeSearchAgentHit(cmd *cobra.Command, hit searchAgentHit) error {
	line := fmt.Sprintf("- selector=%s match=%s", formatNodeSelector(hit.nodeID), searchAgentValue(hit.match))
	if hit.ranked {
		line += fmt.Sprintf(" score=%g", hit.score)
	}
	line += " path=" + searchAgentValue(hit.path)
	if hit.ranked {
		line += " excerpt=" + searchAgentValue(searchAgentExcerpt(hit.excerpt))
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("writing search agent hit: %w", err)
	}
	return nil
}

func writeLexicalSearchHits(cmd *cobra.Command, hits []api.SearchHit) error {
	for _, hit := range hits {
		if err := writeSearchAgentHit(cmd, searchAgentHit{nodeID: hit.Node.ID, match: hit.Match, path: hit.Path}); err != nil {
			return err
		}
	}
	return nil
}

func writeLexicalSearchAgent(cmd *cobra.Command, query string, report api.SearchReport,
	automatic automaticSearchOutcome,
) error {
	header := searchAgentHeader{query: query, mode: "lexical", degraded: automatic.reason,
		count: len(report.Hits), truncated: report.Truncated}
	if automatic.err != nil {
		header.degradedError = automatic.err.Error()
	}
	if fuzzy := report.Fuzzy; fuzzy != nil {
		header.details = append(header.details, "fuzzy="+searchAgentValue(fuzzy.Status))
		if len(fuzzy.Reasons) > 0 {
			header.details = append(header.details, "fuzzy_reasons="+searchAgentValue(strings.Join(fuzzy.Reasons, ",")))
		}
	}
	if err := writeSearchAgentHeader(cmd, header); err != nil {
		return err
	}
	return writeLexicalSearchHits(cmd, report.Hits)
}

// writeDocumentSearchAgent prints processing hits, then any folder name
// matches that automatic search adds from ordinary lexical search.
func writeDocumentSearchAgent(cmd *cobra.Command, query string, report api.DocumentSearchReport,
	folders []api.SearchHit,
) error {
	header := searchAgentHeader{query: query, mode: report.ActualMode, degraded: strings.Join(report.Degradations, ","),
		count: len(report.Results) + len(folders), truncated: report.Truncated, coverage: &report.Coverage}
	if len(report.Trace) > 0 {
		stages := make([]string, len(report.Trace))
		for i, event := range report.Trace {
			stages[i] = fmt.Sprintf("%s:%d", event.Code, event.Count)
		}
		header.details = append(header.details, "trace="+searchAgentValue(strings.Join(stages, ",")))
	}
	if r := report.Reranking; r != nil {
		header.details = append(header.details,
			fmt.Sprintf("rerank=%s rerank_candidates=%d", searchAgentValue(r.Outcome), r.CandidateCount))
		if r.Cause != "" {
			header.details = append(header.details, "rerank_cause="+searchAgentValue(r.Cause))
		}
	}
	if err := writeSearchAgentHeader(cmd, header); err != nil {
		return err
	}
	for _, hit := range report.Results {
		match := "lexical"
		if hit.SemanticRank > 0 {
			match = "semantic"
			if hit.LexicalRank > 0 {
				match = "hybrid"
			}
		}
		if err := writeSearchAgentHit(cmd, searchAgentHit{nodeID: hit.NodeID, match: match, path: hit.Path,
			ranked: true, score: hit.Score, excerpt: hit.Excerpt}); err != nil {
			return err
		}
	}
	return writeLexicalSearchHits(cmd, folders)
}

func writeSimilarSearchAgent(cmd *cobra.Command, report api.DocumentSimilarReport) error {
	header := searchAgentHeader{query: formatNodeSelector(report.Source.NodeID), mode: "semantic",
		count: len(report.Results), truncated: report.Truncated, coverage: &report.Coverage}
	if report.State == "unavailable" {
		header.degraded = "embedding_unavailable"
	}
	if err := writeSearchAgentHeader(cmd, header); err != nil {
		return err
	}
	for _, hit := range report.Results {
		if err := writeSearchAgentHit(cmd, searchAgentHit{nodeID: hit.NodeID, match: "semantic", path: hit.Path,
			ranked: true, score: hit.Score}); err != nil {
			return err
		}
	}
	return nil
}
