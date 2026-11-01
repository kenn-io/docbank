package answercard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

const MaxFiles = 2000
const MaxDownloadBytes = 32 << 20

type ReportItem struct {
	Path           string        `json:"path"`
	NodeID         int64         `json:"node_id"`
	State          string        `json:"state"`
	Reason         string        `json:"reason,omitempty"`
	Citation       *int          `json:"citation,omitempty"`
	CitationIssues []Observation `json:"citation_issues,omitempty"`
}
type Report struct {
	Items []ReportItem `json:"items"`
}

// Check reports cards under root. Any failed read aborts the report; only a
// daemon's explicit not-found response is treated as deletion or pruning.
func Check(ctx context.Context, c *daemonconn.Connection, root string, deep bool) (Report, error) {
	report := Report{Items: []ReportItem{}}
	info, err := c.API().VaultInfo(ctx)
	if err != nil {
		return Report{}, err
	}
	query := api.DocumentQuery{PathPrefix: root, PageSize: 250}
	count, total := 0, int64(0)
	cursors := map[string]bool{}
	for {
		page, err := c.ListDocuments(ctx, query)
		if err != nil {
			return Report{}, err
		}
		for _, item := range page.Items {
			count++
			if count > MaxFiles {
				return Report{}, errors.New("card_limit: too many files")
			}
			row := ReportItem{Path: item.Path, NodeID: item.NodeID, State: "not_a_card"}
			mediaType, _, err := mime.ParseMediaType(item.MediaType)
			if err != nil || mediaType != "text/markdown" && mediaType != "text/plain" {
				report.Items = append(report.Items, row)
				continue
			}
			if item.Size > MaxBytes {
				row.State = "invalid"
				row.Reason = "oversized"
				report.Items = append(report.Items, row)
				continue
			}
			total += item.Size
			if total > MaxDownloadBytes {
				return Report{}, errors.New("card_limit: download limit exceeded")
			}
			stream, err := c.VersionContent(ctx, item.ContentVersionID)
			if err != nil {
				return Report{}, err
			}
			if stream.Size != item.Size {
				_ = stream.Close()
				return Report{}, errors.New("card content identity changed")
			}
			var buf bytes.Buffer
			_, err = stream.CopyVerified(&buf)
			_ = stream.Close()
			if err != nil {
				return Report{}, err
			}
			card, err := Parse(buf.Bytes())
			if errors.Is(err, ErrNotCard) {
				report.Items = append(report.Items, row)
				continue
			}
			if err != nil {
				row.State = "invalid"
				row.Reason = err.Error()
				report.Items = append(report.Items, row)
				continue
			}
			valid := true
			for _, citation := range card.Citations {
				if citation.VaultUID.String() != info.VaultID {
					valid = false
					break
				}
			}
			if !valid {
				row.State = "invalid"
				row.Reason = "citation belongs to another vault"
				report.Items = append(report.Items, row)
				continue
			}
			state := card.Effective(nil)
			if state.State == Approved || deep {
				observations, err := observe(ctx, c, card, deep)
				if err != nil {
					return Report{}, fmt.Errorf("checking card %s: %w", item.Path, err)
				}
				if deep {
					row.CitationIssues = observations
				}
				state = card.Effective(observations)
			}
			row.State = string(state.State)
			row.Reason = state.Reason
			if state.State == Stale {
				index := state.Citation
				row.Citation = &index
			}
			report.Items = append(report.Items, row)
		}
		if page.NextCursor == "" {
			break
		}
		if cursors[page.NextCursor] {
			return Report{}, errors.New("repeated document cursor")
		}
		cursors[page.NextCursor] = true
		query.Cursor = page.NextCursor
	}
	return report, nil
}
func observe(ctx context.Context, c *daemonconn.Connection, card Card, deep bool) ([]Observation, error) {
	var result []Observation
	for i, citation := range card.Citations {
		node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: citation.NodeID}})
		if err != nil {
			if code, _ := daemonconn.ProblemCode(err); code == "not_found" {
				result = append(result, Observation{Reason: "deleted", Citation: i})
				if deep {
					_, resolveErr := c.ResolveTextCitation(ctx, citation.TextCitation())
					if resolveErr != nil {
						if code, _ := daemonconn.ProblemCode(resolveErr); code != "citation_unavailable" && code != "invalid_citation_range" {
							return nil, resolveErr
						}
						result = append(result, Observation{Reason: "quote_unavailable", Citation: i})
					}
				}
				continue
			}
			return nil, err
		}
		if node.ID != citation.NodeID {
			return nil, errors.New("citation node identity mismatch")
		}
		reason := ""
		if node.TrashedAt != "" {
			reason = "trashed"
		} else if node.Kind != "file" {
			reason = "quote_unavailable"
		} else if node.CurrentVersionID != citation.ContentVersionID.String() {
			version, e := c.API().GetContentVersion(ctx, &apiclient.GetContentVersionRequestOptions{PathParams: &apiclient.GetContentVersionPath{VersionID: citation.ContentVersionID.String()}})
			if e != nil {
				if code, _ := daemonconn.ProblemCode(e); code == "not_found" {
					reason = "version_pruned"
				} else {
					return nil, e
				}
			} else if version.ID != citation.ContentVersionID.String() {
				return nil, errors.New("citation version identity mismatch")
			} else if version.NodeID != citation.NodeID {
				reason = "quote_unavailable"
			} else {
				reason = "new_version"
			}
		} else if node.BlobHash != citation.ContentSHA256 {
			reason = "quote_unavailable"
		}
		if reason != "" {
			result = append(result, Observation{Reason: reason, Citation: i})
		}
		if deep {
			resolved, e := c.ResolveTextCitation(ctx, citation.TextCitation())
			if e != nil {
				if code, _ := daemonconn.ProblemCode(e); code == "citation_unavailable" || code == "invalid_citation_range" {
					result = append(result, Observation{Reason: "quote_unavailable", Citation: i})
				} else {
					return nil, e
				}
			} else if resolved.Text != citation.Quote {
				result = append(result, Observation{Reason: "quote_mismatch", Citation: i})
			}
		}
	}
	return result, nil
}
