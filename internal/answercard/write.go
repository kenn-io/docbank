package answercard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"gopkg.in/yaml.v3"
)

var errUnsafeRewrite = errors.New("invalid_card: YAML aliases or merges cannot be rewritten safely")

func unsafeYAML(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Tag == "!!merge" {
		return true
	}
	return slices.ContainsFunc(n.Content, unsafeYAML)
}

// rewriteLifecycle refuses unknown fields through Parse and preserves the body.
func rewriteLifecycle(data []byte, state State, hash, at string, stale *StaleInfo) ([]byte, error) {
	return rewriteCard(data, state, hash, at, stale, nil)
}

// rewriteCard also fills quotation fields that were resolved for owner review.
func rewriteCard(data []byte, state State, hash, at string, stale *StaleInfo, citations []Citation) ([]byte, error) {
	if _, err := Parse(data); err != nil {
		return nil, err
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	end := strings.Index(s[4:], "\n---\n") + 4
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s[4:end]), &doc); err != nil {
		return nil, fmt.Errorf("decoding card frontmatter: %w", err)
	}
	mapping := doc.Content[0]
	// Aliases and merge keys make a targeted rewrite ambiguous.
	if unsafeYAML(mapping) {
		return nil, errUnsafeRewrite
	}
	set := func(key string, value any) error {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value == key {
				mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
				break
			}
		}
		if value == nil {
			return nil
		}
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return fmt.Errorf("encoding lifecycle field: %w", err)
		}
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
		return nil
	}
	for _, v := range []struct {
		key   string
		value any
	}{{"state", state}, {"approved_hash", hash}, {"approved_at", at}, {"stale", nil}} {
		if err := set(v.key, v.value); err != nil {
			return nil, err
		}
	}
	if stale != nil {
		if err := set("stale", stale); err != nil {
			return nil, err
		}
	}
	if citations != nil {
		for i := 0; i < len(mapping.Content); i += 2 {
			if mapping.Content[i].Value != "citations" {
				continue
			}
			for j, citation := range mapping.Content[i+1].Content {
				var quote *yaml.Node
				for k := 0; k < len(citation.Content); k += 2 {
					if citation.Content[k].Value == "quote" {
						quote = citation.Content[k+1]
						break
					}
				}
				if quote == nil {
					quote = &yaml.Node{Kind: yaml.ScalarNode}
					citation.Content = append(citation.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "quote"}, quote)
				}
				quote.Tag = "!!str"
				quote.Value = citations[j].Quote
			}
		}
	}
	var front bytes.Buffer
	enc := yaml.NewEncoder(&front)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("encoding card frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("finishing card frontmatter: %w", err)
	}
	out := []byte("---\n" + strings.TrimSuffix(front.String(), "\n") + s[end:])
	if _, err := Parse(out); err != nil {
		return nil, err
	}
	return out, nil
}

func writeCard(ctx context.Context, c *daemonconn.Connection, node api.Node, data []byte) error {
	sum := sha256.Sum256(data)
	_, err := c.ReplaceContent(ctx, node.ID, node.Revision, node.MimeType, hex.EncodeToString(sum[:]), int64(len(data)), bytes.NewReader(data))
	if errors.Is(err, store.ErrStaleRevision) {
		return fmt.Errorf("stale_revision: %w", err)
	}
	return err
}

func loadCard(ctx context.Context, c *daemonconn.Connection, node api.Node) (Card, []byte, error) {
	mediaType, _, mediaErr := mime.ParseMediaType(node.MimeType)
	if node.Kind != "file" || node.TrashedAt != "" || mediaErr != nil || (mediaType != "text/markdown" && mediaType != "text/plain") || node.Size > MaxBytes {
		return Card{}, nil, errors.New("invalid_card: not a live text card")
	}
	stream, err := c.VersionContent(ctx, node.CurrentVersionID)
	if err != nil {
		return Card{}, nil, err
	}
	defer func() { _ = stream.Close() }()
	if stream.VersionID != node.CurrentVersionID || stream.Size != node.Size || stream.BlobHash != node.BlobHash {
		return Card{}, nil, errors.New("invalid_card: content identity changed")
	}
	var buf bytes.Buffer
	if _, err := stream.CopyVerified(&buf); err != nil {
		return Card{}, nil, err
	}
	card, err := Parse(buf.Bytes())
	if err != nil {
		return Card{}, nil, fmt.Errorf("invalid_card: %w", err)
	}
	s := strings.ReplaceAll(buf.String(), "\r\n", "\n")
	end := strings.Index(s[4:], "\n---\n") + 4
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s[4:end]), &doc); err != nil {
		return Card{}, nil, fmt.Errorf("decoding card frontmatter: %w", err)
	}
	if unsafeYAML(&doc) {
		return Card{}, nil, errUnsafeRewrite
	}
	return card, buf.Bytes(), nil
}

// Approve validates and displays the card through confirm, then repeats all
// source and quotation reads after confirmation before attempting the write.
func Approve(ctx context.Context, c *daemonconn.Connection, node api.Node, confirm func(Card, []string) (bool, error)) error {
	card, data, err := loadCard(ctx, c, node)
	if err != nil {
		return err
	}
	info, err := c.API().VaultInfo(ctx)
	if err != nil {
		return err
	}
	for _, cite := range card.Citations {
		if cite.VaultUID.String() != info.VaultID {
			return errors.New("invalid_card: citation belongs to another vault")
		}
	}
	check := func(fill bool) ([]string, error) {
		paths := make([]string, 0, len(card.Citations))
		for i, cite := range card.Citations {
			n, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: cite.NodeID}})
			if err != nil {
				if code, _ := daemonconn.ProblemCode(err); code == "not_found" {
					return nil, fmt.Errorf("citation_not_current: citation %d (deleted)", i)
				}
				return nil, err
			}
			paths = append(paths, n.Path)
		}
		obs, err := observe(ctx, c, card, false)
		if err != nil {
			return nil, err
		}
		for _, o := range obs {
			return nil, fmt.Errorf("citation_not_current: citation %d (%s)", o.Citation, o.Reason)
		}
		for i, cite := range card.Citations {
			resolved, err := c.ResolveTextCitation(ctx, cite.TextCitation())
			if err != nil {
				if code, _ := daemonconn.ProblemCode(err); code == "citation_unavailable" {
					return nil, fmt.Errorf("citation_unavailable: citation %d", i)
				}
				return nil, err
			}
			if fill && cite.Quote == "" {
				card.Citations[i].Quote = resolved.Text
			} else if resolved.Text != cite.Quote {
				return nil, fmt.Errorf("quote_mismatch: citation %d", i)
			}
		}
		return paths, nil
	}
	paths, err := check(true)
	if err != nil {
		return err
	}
	ok, err := confirm(card, paths)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("approval cancelled")
	}
	if _, err := check(false); err != nil {
		return err
	}
	out, err := rewriteCard(data, Approved, card.Hash(), time.Now().UTC().Format(time.RFC3339), nil, card.Citations)
	if err != nil {
		return err
	}
	return writeCard(ctx, c, node, out)
}
