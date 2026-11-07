package answercard

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

// ApproveRebind finds each saved quote exactly once in a current rendition,
// then uses the ordinary approval review, source checks and guarded write.
func ApproveRebind(ctx context.Context, c *daemonconn.Connection, node api.Node, confirm func(Card, []string) (bool, error)) error {
	return approve(ctx, c, node, confirm, true)
}

func rebindCitation(ctx context.Context, c *daemonconn.Connection, citation Citation) (Citation, error) {
	if citation.Quote == "" {
		return Citation{}, errors.New("rebind_refused: empty saved quote; edit the citation manually")
	}
	source, err := rebindSource(ctx, c, citation.NodeID)
	if err != nil {
		return Citation{}, err
	}
	summaries, err := c.ResolveDocumentSummaries(ctx, api.DocumentSummaryResolveRequest{
		Identities: []api.DocumentIdentity{{NodeID: source.ID, ContentVersionID: source.CurrentVersionID, Path: source.Path}},
	})
	if err != nil {
		return Citation{}, err
	}
	renditions := summaries[0].ActiveRenditions
	if len(renditions) == 0 {
		return Citation{}, errors.New("rebind_refused: current rendition unavailable; edit the citation manually")
	}
	// Fixed-width hexadecimal fields give an unambiguous stable ordering.
	selected := slices.MinFunc(renditions, func(a, b api.DocumentRenditionIdentity) int {
		return strings.Compare(a.ProfileFingerprint+a.AttachmentID+a.BuildID, b.ProfileFingerprint+b.AttachmentID+b.BuildID)
	})
	checksum := ""
	start, digest, err := scanExactQuote(citation.Quote, rebindWindowChars, func(offset, maxChars int) (string, bool, error) {
		window, err := c.RenditionTextWindow(ctx, api.RenditionWindowRequest{
			VaultID: citation.VaultUID.String(), NodeID: source.ID, ContentVersionID: source.CurrentVersionID,
			AttachmentID: selected.AttachmentID, Offset: offset, MaxChars: maxChars,
		})
		if err != nil {
			return "", false, err
		}
		if window.BuildID != selected.BuildID || window.ProfileFingerprint != selected.ProfileFingerprint || (checksum != "" && window.Checksum != checksum) {
			return "", false, errors.New("rebind_refused: rendition identity changed while scanning")
		}
		checksum = window.Checksum
		return window.Text, window.EOF, nil
	})
	if err != nil {
		return Citation{}, err
	}
	if digest != checksum {
		return Citation{}, errors.New("rebind_refused: rendition checksum mismatch")
	}
	current, err := rebindSource(ctx, c, source.ID)
	if err != nil {
		return Citation{}, err
	}
	if current.CurrentVersionID != source.CurrentVersionID || current.BlobHash != source.BlobHash {
		return Citation{}, errors.New("citation_not_current: source changed while rebinding")
	}
	version, err := uuid.Parse(source.CurrentVersionID)
	if err != nil {
		return Citation{}, fmt.Errorf("parsing current source version: %w", err)
	}
	citation.ContentVersionID = version
	citation.ContentSHA256 = source.BlobHash
	citation.RenditionAttachmentID = selected.AttachmentID
	citation.BuildID = selected.BuildID
	citation.RenditionSHA256 = checksum
	citation.Start = start
	citation.End = start + utf8.RuneCountInString(citation.Quote)
	if err := document.ValidateTextCitation(citation.TextCitation()); err != nil {
		return Citation{}, err
	}
	return citation, nil
}

func rebindSource(ctx context.Context, c *daemonconn.Connection, id int64) (api.Node, error) {
	node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: id}})
	if err != nil {
		if code, _ := daemonconn.ProblemCode(err); code == "not_found" {
			return api.Node{}, errors.New("citation_not_current: source deleted")
		}
		return api.Node{}, err
	}
	if node.ID != id || node.Kind != "file" || node.TrashedAt != "" {
		return api.Node{}, errors.New("citation_not_current: source is not a live file")
	}
	return *node, nil
}
