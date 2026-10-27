package answercard

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

const (
	maxCachedVersions     = 4096
	maxCacheBytes         = 8 << 20
	MaxMatchResponseBytes = 512 << 10
)

var ErrCardLimit = errors.New("card_limit")

type CardIdentity struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Path             string `json:"path"`
}
type MatchItem struct {
	Card            CardIdentity `json:"card"`
	Question        string       `json:"question"`
	MatchedPhrasing string       `json:"matched_phrasing"`
	Score           float64      `json:"score"`
	Answer          string       `json:"answer"`
	Citations       []Citation   `json:"citations"`
}
type Excluded struct {
	Stale    int `json:"stale"`
	Draft    int `json:"draft"`
	Invalid  int `json:"invalid"`
	NotACard int `json:"not_a_card"`
}
type StaleCard struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type MatchResult struct {
	Status     string      `json:"status"`
	Matches    []MatchItem `json:"matches"`
	Excluded   Excluded    `json:"excluded"`
	StaleCards []StaleCard `json:"stale_cards"`
}

type cachedCard struct {
	key  string
	data []byte
}

// Matcher caches only verified immutable bytes, scoped by vault and version.
// Lifecycle, root membership, source identity and quotations are read live.
type Matcher struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	lru     list.List
	bytes   int
}

func NewMatcher() *Matcher { return &Matcher{entries: map[string]*list.Element{}} }
func (m *Matcher) cached(key string) []byte {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[key]; e != nil {
		m.lru.MoveToFront(e)
		return cachedCardValue(e).data
	}
	return nil
}
func (m *Matcher) remember(key string, data []byte) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[string]*list.Element{}
	}
	if e := m.entries[key]; e != nil {
		m.lru.MoveToFront(e)
		return
	}
	m.entries[key] = m.lru.PushFront(cachedCard{key: key, data: data})
	m.bytes += len(data)
	for len(m.entries) > maxCachedVersions || m.bytes > maxCacheBytes {
		e := m.lru.Back()
		entry := cachedCardValue(e)
		delete(m.entries, entry.key)
		m.bytes -= len(entry.data)
		m.lru.Remove(e)
	}
}

// The LRU contains only cachedCard values. Preserve a failed invariant as a
// hard failure rather than treating corrupt accounting as a cache miss.
func cachedCardValue(element *list.Element) cachedCard {
	entry, ok := element.Value.(cachedCard)
	if !ok {
		panic("invalid answer card cache entry")
	}
	return entry
}

// Match discovers the complete bounded catalog before returning any answers.
// Reads are sequential observations, not an atomic snapshot of the vault.
func (m *Matcher) Match(ctx context.Context, c *daemonconn.Connection, question, root string, limit int) (MatchResult, error) {
	result := MatchResult{Status: "no_match", Matches: []MatchItem{}, StaleCards: []StaleCard{}}
	if utf8.RuneCountInString(question) > 300 || !utf8.ValidString(question) || strings.TrimSpace(question) == "" {
		return MatchResult{}, errors.New("invalid question")
	}
	if limit == 0 {
		limit = 3
	}
	if limit < 1 || limit > 10 {
		return MatchResult{}, errors.New("invalid match limit")
	}
	if root == "" {
		root = "/answer-cards"
	}
	normalized, err := store.NormalizeDocumentCatalogQuery(store.DocumentCatalogQuery{PathPrefix: root})
	if err != nil {
		return MatchResult{}, fmt.Errorf("invalid card root: %w", err)
	}
	root = normalized.PathPrefix
	info, err := c.API().VaultInfo(ctx)
	if err != nil {
		return MatchResult{}, err
	}
	type candidate struct {
		item   api.DocumentSummary
		card   Card
		score  float64
		phrase string
		count  int
		hash   string
	}
	var candidates []candidate
	query := api.DocumentQuery{PathPrefix: root, PageSize: 250}
	count, total := 0, int64(0)
	cursors := map[string]bool{}
	for {
		page, err := c.ListDocuments(ctx, query)
		if err != nil {
			return MatchResult{}, err
		}
		if len(page.Items) == 0 && page.NextCursor != "" {
			return MatchResult{}, errors.New("empty document page with continuation")
		}
		for _, item := range page.Items {
			count++
			if count > MaxFiles {
				return MatchResult{}, fmt.Errorf("%w: too many files", ErrCardLimit)
			}
			if !underRoot(item.Path, root) {
				return MatchResult{}, errors.New("card discovery outside root")
			}
			mediaType, _, mediaErr := mime.ParseMediaType(item.MediaType)
			if mediaErr != nil || (mediaType != "text/markdown" && mediaType != "text/plain") {
				result.Excluded.NotACard++
				continue
			}
			if item.Size < 0 {
				return MatchResult{}, errors.New("invalid card size")
			}
			if item.Size > MaxBytes {
				result.Excluded.Invalid++
				continue
			}
			total += item.Size
			if total > MaxDownloadBytes {
				return MatchResult{}, fmt.Errorf("%w: download limit exceeded", ErrCardLimit)
			}
			key := info.VaultID + "/" + item.ContentVersionID
			data := m.cached(key)
			if data == nil {
				stream, err := c.VersionContent(ctx, item.ContentVersionID)
				if err != nil {
					return MatchResult{}, err
				}
				if stream.Size != item.Size || stream.VersionID != item.ContentVersionID {
					_ = stream.Close()
					return MatchResult{}, errors.New("card content identity changed")
				}
				var buf bytes.Buffer
				_, err = stream.CopyVerified(&buf)
				_ = stream.Close()
				if err != nil {
					return MatchResult{}, err
				}
				data = buf.Bytes()
				m.remember(key, data)
			}
			if int64(len(data)) != item.Size {
				return MatchResult{}, errors.New("cached card size mismatch")
			}
			card, err := Parse(data)
			if errors.Is(err, ErrNotCard) {
				result.Excluded.NotACard++
				continue
			}
			if err != nil {
				result.Excluded.Invalid++
				continue
			}
			valid := true
			for _, cite := range card.Citations {
				if cite.VaultUID.String() != info.VaultID {
					valid = false
				}
			}
			if !valid {
				result.Excluded.Invalid++
				continue
			}
			score, phrase, n := card.score(question)
			state := card.Effective(nil)
			switch state.State {
			case Draft:
				result.Excluded.Draft++
				continue
			case Stale:
				result.Excluded.Stale++
				if score >= MatchThreshold && len(result.StaleCards) < 10 {
					result.StaleCards = append(result.StaleCards, StaleCard{item.Path, state.Reason})
				}
				continue
			case Approved:
			default:
				result.Excluded.Invalid++
				continue
			}
			if score >= MatchThreshold {
				sum := sha256.Sum256(data)
				candidates = append(candidates, candidate{item, card, score, phrase, n, hex.EncodeToString(sum[:])})
			}
		}
		if page.NextCursor == "" {
			break
		}
		if cursors[page.NextCursor] {
			return MatchResult{}, errors.New("repeated document cursor")
		}
		cursors[page.NextCursor] = true
		query.Cursor = page.NextCursor
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		if a.count != b.count {
			return b.count - a.count
		}
		return strings.Compare(a.item.Path, b.item.Path)
	})
	for _, candidate := range candidates {
		observations, err := observe(ctx, c, candidate.card, true)
		if err != nil {
			return MatchResult{}, err
		}
		// Retained quotations can resolve after replacement. Re-read the live
		// sources after resolving them so a replacement during that read is
		// excluded before the card identity is checked.
		if len(observations) == 0 {
			observations, err = observe(ctx, c, candidate.card, false)
			if err != nil {
				return MatchResult{}, err
			}
		}
		state := candidate.card.Effective(observations)
		if state.State != Approved {
			result.Excluded.Stale++
			if len(result.StaleCards) < 10 {
				result.StaleCards = append(result.StaleCards, StaleCard{candidate.item.Path, state.Reason})
			}
			continue
		}
		node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: candidate.item.NodeID}})
		if err != nil {
			if code, _ := daemonconn.ProblemCode(err); code == "not_found" {
				result.Excluded.Draft++
				continue
			}
			return MatchResult{}, err
		}
		if node.ID != candidate.item.NodeID {
			return MatchResult{}, errors.New("card node identity mismatch")
		}
		if node.Kind != "file" || node.TrashedAt != "" || node.CurrentVersionID != candidate.item.ContentVersionID || !underRoot(node.Path, root) || node.Path != candidate.item.Path {
			result.Excluded.Draft++
			continue
		}
		if node.BlobHash != candidate.hash || node.Size != candidate.item.Size {
			return MatchResult{}, errors.New("card content integrity mismatch")
		}
		if len(result.Matches) < limit {
			result.Matches = append(result.Matches, MatchItem{Card: CardIdentity{node.ID, node.CurrentVersionID, node.Path}, Question: candidate.card.Question, MatchedPhrasing: candidate.phrase, Score: candidate.score, Answer: candidate.card.Answer, Citations: candidate.card.Citations})
		}
	}
	if len(result.Matches) > 0 {
		result.Status = "matched"
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return MatchResult{}, err
	}
	if len(encoded) > MaxMatchResponseBytes {
		return MatchResult{}, fmt.Errorf("%w: response limit exceeded", ErrCardLimit)
	}
	return result, nil
}
func underRoot(p, root string) bool {
	return strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/") && path.Clean(p) == p
}
