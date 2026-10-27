// Package answercard implements the client-side answer card format.
package answercard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

const MaxBytes = 64 << 10

var ErrNotCard = errors.New("not an answer card")

type State string

const (
	Draft    State = "draft"
	Approved State = "approved"
	Stale    State = "stale"
	Invalid  State = "invalid"
)

type Citation struct {
	Version               int       `yaml:"version" json:"version"`
	VaultUID              uuid.UUID `yaml:"vault_uid" json:"vault_uid"`
	NodeID                int64     `yaml:"node_id" json:"node_id"`
	ContentVersionID      uuid.UUID `yaml:"content_version_id" json:"content_version_id"`
	ContentSHA256         string    `yaml:"content_sha256" json:"content_sha256"`
	RenditionAttachmentID string    `yaml:"rendition_attachment_id" json:"rendition_attachment_id"`
	BuildID               string    `yaml:"build_id" json:"build_id"`
	RenditionSHA256       string    `yaml:"rendition_sha256" json:"rendition_sha256"`
	Start                 int       `yaml:"start" json:"start"`
	End                   int       `yaml:"end" json:"end"`
	Quote                 string    `yaml:"quote" json:"quote"`
}

func (c Citation) TextCitation() document.TextCitation {
	return document.TextCitation{Version: c.Version, VaultUID: c.VaultUID, NodeID: c.NodeID, ContentVersionID: c.ContentVersionID, ContentSHA256: c.ContentSHA256, RenditionAttachmentID: c.RenditionAttachmentID, BuildID: c.BuildID, RenditionSHA256: c.RenditionSHA256, Start: c.Start, End: c.End}
}

type StaleInfo struct {
	Reason     string `yaml:"reason"`
	Citation   int    `yaml:"citation"`
	ObservedAt string `yaml:"observed_at"`
}
type Card struct {
	Marker       int        `yaml:"docbank_answer_card"`
	Question     string     `yaml:"question"`
	Alternates   []string   `yaml:"alternates"`
	Citations    []Citation `yaml:"citations"`
	State        State      `yaml:"state"`
	ApprovedHash string     `yaml:"approved_hash"`
	ApprovedAt   string     `yaml:"approved_at"`
	Stale        *StaleInfo `yaml:"stale"`
	Answer       string     `yaml:"-"`
}
type Observation struct {
	Reason   string `json:"reason"`
	Citation int    `json:"citation"`
}
type Result struct {
	State    State  `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Citation int    `json:"citation,omitempty"`
}

func Parse(data []byte) (Card, error) {
	var c Card
	if len(data) > MaxBytes {
		return c, errors.New("invalid or oversized card")
	}
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return c, ErrNotCard
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		if hasMarker(s[4:]) {
			return c, errors.New("unterminated card frontmatter")
		}
		return c, ErrNotCard
	}
	front := s[4 : 4+end]
	// Inspect the marker before strict decoding, so unrelated frontmatter is ignored.
	var frontmatter yaml.Node
	if err := yaml.Unmarshal([]byte(front), &frontmatter); err != nil {
		if !hasMarker(front) {
			return c, ErrNotCard
		}
		return c, fmt.Errorf("invalid frontmatter: %w", err)
	}
	var marker struct {
		Marker yaml.Node `yaml:"docbank_answer_card"`
	}
	if err := frontmatter.Decode(&marker); err != nil {
		if !hasMarker(front) {
			return c, ErrNotCard
		}
		return c, fmt.Errorf("invalid frontmatter: %w", err)
	}
	value := yamlValue(&marker.Marker)
	if value.IsZero() && !hasMarker(front) {
		return c, ErrNotCard
	}
	if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" {
		return c, errors.New("docbank_answer_card must be a YAML integer")
	}
	var version int
	if err := value.Decode(&version); err != nil {
		return c, fmt.Errorf("invalid card marker: %w", err)
	}
	if version != 1 {
		return c, fmt.Errorf("unsupported answer card version: %d", version)
	}
	if !utf8.Valid(data) {
		return c, errors.New("invalid or oversized card")
	}
	if err := validateIntegerFields(&frontmatter); err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewBufferString(front))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid card: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return c, errors.New("multiple YAML documents")
	}
	if !validPhrase(c.Question) {
		return Card{}, errors.New("invalid question")
	}
	seen := map[string]bool{normalize(c.Question): true}
	alternates := make([]string, 0, len(c.Alternates))
	for _, a := range c.Alternates {
		if !validPhrase(a) {
			return Card{}, errors.New("invalid alternate")
		}
		key := normalize(a)
		if !seen[key] {
			alternates = append(alternates, a)
			seen[key] = true
		}
	}
	if len(c.Alternates) > 20 {
		return Card{}, errors.New("too many alternates")
	}
	c.Alternates = alternates
	c.Answer = strings.TrimRightFunc(s[4+end+5:], unicode.IsSpace)
	if err := c.Validate(); err != nil {
		return Card{}, err
	}
	return c, nil
}
func hasMarker(front string) bool {
	var node yaml.Node
	if yaml.Unmarshal([]byte(front), &node) == nil && len(node.Content) == 1 && node.Content[0].Kind == yaml.MappingNode {
		fields := node.Content[0].Content
		for i := 0; i < len(fields); i += 2 {
			if yamlValue(fields[i]).Value == "docbank_answer_card" {
				return true
			}
		}
		return false
	}
	for line := range strings.SplitSeq(front, "\n") {
		keyText, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		var key string
		if yaml.Unmarshal([]byte(keyText), &key) == nil && key == "docbank_answer_card" {
			return true
		}
	}
	return false
}

func yamlValue(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return node.Alias
	}
	return node
}

func validateIntegerFields(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := yamlValue(node.Content[i]).Value
			switch key {
			case "version", "node_id", "start", "end", "citation":
				value := yamlValue(node.Content[i+1])
				if value.Kind != yaml.ScalarNode || value.ShortTag() != "!!int" {
					return errors.New("invalid card integer field")
				}
			}
		}
	}
	for _, child := range node.Content {
		if err := validateIntegerFields(child); err != nil {
			return err
		}
	}
	return nil
}

func (c Card) Validate() error {
	if c.Marker != 1 || !validPhrase(c.Question) || len(c.Alternates) > 20 || len(c.Citations) < 1 || len(c.Citations) > 16 || !utf8.ValidString(c.Answer) || utf8.RuneCountInString(c.Answer) < 1 || utf8.RuneCountInString(c.Answer) > 4000 {
		return errors.New("invalid card content")
	}
	seen := map[string]bool{normalize(c.Question): true}
	for _, a := range c.Alternates {
		if !validPhrase(a) {
			return errors.New("invalid or duplicate alternate")
		}
		key := normalize(a)
		if seen[key] {
			return errors.New("invalid or duplicate alternate")
		}
		seen[key] = true
	}
	for _, cite := range c.Citations {
		if document.ValidateTextCitation(cite.TextCitation()) != nil || !utf8.ValidString(cite.Quote) || (cite.Quote != "" && utf8.RuneCountInString(cite.Quote) != cite.End-cite.Start) {
			return errors.New("invalid citation")
		}
	}
	if !utf8.ValidString(c.ApprovedHash) || !utf8.ValidString(c.ApprovedAt) || (c.Stale != nil && (!utf8.ValidString(c.Stale.Reason) || !utf8.ValidString(c.Stale.ObservedAt))) {
		return errors.New("invalid lifecycle text")
	}
	if c.State != "" && c.State != Draft && c.State != Approved && c.State != Stale {
		return errors.New("invalid card state")
	}
	if c.State == Stale && (c.Stale == nil || c.Stale.Reason == "" || c.Stale.Citation < 0 || c.Stale.Citation >= len(c.Citations)) {
		return errors.New("invalid stale record")
	}
	return nil
}
func validPhrase(s string) bool {
	n := utf8.RuneCountInString(s)
	if !utf8.ValidString(s) || n < 1 || n > 300 {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
func normalize(s string) string {
	var b strings.Builder
	for _, r := range cases.Fold().String(norm.NFKC.String(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Hash returns the canonical content digest, or an empty string if encoding fails.
func (c Card) Hash() string {
	if c.Alternates == nil {
		c.Alternates = []string{}
	}
	data, err := canonical.Marshal(struct {
		Question   string     `json:"question"`
		Alternates []string   `json:"alternates"`
		Answer     string     `json:"answer"`
		Citations  []Citation `json:"citations"`
	}{c.Question, c.Alternates, strings.TrimRightFunc(strings.ReplaceAll(c.Answer, "\r\n", "\n"), unicode.IsSpace), c.Citations})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func CanTransition(from, to State) bool {
	return from == Draft && to == Approved || from == Approved && to == Stale || from == Stale && to == Approved || from == Approved && to == Draft
}
func (c Card) Effective(observations []Observation) Result {
	if err := c.Validate(); err != nil {
		return Result{State: Invalid, Reason: err.Error()}
	}
	if c.State == Stale {
		return Result{State: Stale, Reason: c.Stale.Reason, Citation: c.Stale.Citation}
	}
	if c.State != Approved {
		return Result{State: Draft}
	}
	hash := c.Hash()
	if hash == "" {
		return Result{State: Invalid, Reason: "card cannot be canonically encoded"}
	}
	if c.ApprovedHash != hash {
		return Result{State: Draft, Reason: "edited_since_approval"}
	}
	for _, cite := range c.Citations {
		if cite.Quote == "" {
			return Result{State: Invalid, Reason: "unresolved quotation"}
		}
	}
	for _, o := range observations {
		if o.Reason != "" {
			return Result{State: Stale, Reason: o.Reason, Citation: o.Citation}
		}
	}
	return Result{State: Approved}
}
