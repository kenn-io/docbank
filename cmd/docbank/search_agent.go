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

func writeSearchAgentHeader(cmd *cobra.Command, query, mode, reason string, count int, truncated bool, coverage *api.DocumentSearchCoverage, details ...string) error {
	if reason == "" {
		reason = "false"
	}
	header := fmt.Sprintf("OK search count=%d query=%s mode=%s degraded=%s truncated=%t", count, searchAgentValue(query), searchAgentValue(mode), searchAgentValue(reason), truncated)
	if coverage != nil {
		header += fmt.Sprintf(" coverage=%s complete=%d scoped=%d", searchAgentValue(coverage.State), coverage.CompleteDocuments, coverage.ScopedDocuments)
	}
	if len(details) > 0 {
		header += " " + strings.Join(details, " ")
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), header)
	if err != nil {
		return fmt.Errorf("writing search agent header: %w", err)
	}
	return nil
}

func writeSearchAgentHit(cmd *cobra.Command, nodeID int64, match string, score float64, path, excerpt string) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "- selector=%s match=%s score=%g path=%s excerpt=%s\n", formatNodeSelector(nodeID), searchAgentValue(match), score, searchAgentValue(path), searchAgentValue(searchAgentExcerpt(excerpt)))
	if err != nil {
		return fmt.Errorf("writing search agent hit: %w", err)
	}
	return nil
}

func writeLexicalSearchAgent(cmd *cobra.Command, query string, report api.SearchReport, reason string) error {
	if err := writeSearchAgentHeader(cmd, query, "lexical", reason, len(report.Hits), report.Truncated, nil); err != nil {
		return err
	}
	for _, hit := range report.Hits {
		if err := writeSearchAgentHit(cmd, hit.Node.ID, hit.Match, 0, hit.Path, hit.Node.Name); err != nil {
			return err
		}
	}
	return nil
}

func writeDocumentSearchAgent(cmd *cobra.Command, query string, report api.DocumentSearchReport) error {
	var details []string
	if len(report.Trace) > 0 {
		stages := make([]string, len(report.Trace))
		for i, event := range report.Trace {
			stages[i] = fmt.Sprintf("%s:%d", event.Code, event.Count)
		}
		details = append(details, "trace="+searchAgentValue(strings.Join(stages, ",")))
	}
	if r := report.Reranking; r != nil {
		details = append(details, fmt.Sprintf("rerank=%s rerank_candidates=%d", searchAgentValue(r.Outcome), r.CandidateCount))
		if r.Cause != "" {
			details = append(details, "rerank_cause="+searchAgentValue(r.Cause))
		}
	}
	if err := writeSearchAgentHeader(cmd, query, report.ActualMode, strings.Join(report.Degradations, ","), len(report.Results), report.Truncated, &report.Coverage, details...); err != nil {
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
		if err := writeSearchAgentHit(cmd, hit.NodeID, match, hit.Score, hit.Path, hit.Excerpt); err != nil {
			return err
		}
	}
	return nil
}

func writeSimilarSearchAgent(cmd *cobra.Command, report api.DocumentSimilarReport) error {
	reason := ""
	if report.State == "unavailable" {
		reason = "embedding_unavailable"
	}
	if err := writeSearchAgentHeader(cmd, formatNodeSelector(report.Source.NodeID), "semantic", reason, len(report.Results), report.Truncated, &report.Coverage); err != nil {
		return err
	}
	for _, hit := range report.Results {
		if err := writeSearchAgentHit(cmd, hit.NodeID, "semantic", hit.Score, hit.Path, ""); err != nil {
			return err
		}
	}
	return nil
}
