package answercard

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const sample = `---
docbank_answer_card: 1
question: When is renewal?
citations:
  - version: 1
    vault_uid: 00000000-0000-4000-8000-000000000001
    node_id: 42
    content_version_id: 00000000-0000-4000-8000-000000000002
    content_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    rendition_attachment_id: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    build_id: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    rendition_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    start: 0
    end: 4
    quote: test
state: draft
---
Test answer.
`

func TestParseAndState(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Effective(nil); got.State != Draft {
		t.Fatal(got)
	}
	h := c.Hash()
	c.State = Approved
	c.ApprovedHash = h
	if got := c.Effective(nil); got.State != Approved {
		t.Fatal(got)
	}
	if got := c.Effective([]Observation{{Reason: "new_version"}}); got.State != Stale {
		t.Fatal(got)
	}
	c.Answer += "changed"
	if got := c.Effective(nil); got.State != Draft || got.Reason != "edited_since_approval" {
		t.Fatal(got)
	}
}

func TestInvalid(t *testing.T) {
	type invalidCase struct {
		name, input string
		notCard     bool
	}
	tests := []invalidCase{
		{"unsupported marker", strings.Replace(sample, "docbank_answer_card: 1", "docbank_answer_card: 2", 1), false},
		{"null marker", strings.Replace(sample, "docbank_answer_card: 1", "docbank_answer_card: null", 1), false},
		{"absent marker", strings.Replace(sample, "docbank_answer_card: 1\n", "", 1), true},
		{"nested marker only", "---\ntitle:\n  docbank_answer_card: null\n---\nNotes.\n", true},
		{"nonsearchable question", strings.Replace(sample, "question: When is renewal?", `question: "???"`, 1), false},
		{"unknown state", strings.Replace(sample, "state: draft", "state: unknown", 1), false},
		{"duplicate question", strings.Replace(sample, "question: When is renewal?", "question: a\nquestion: b", 1), false},
		{"oversized", sample + strings.Repeat(" ", MaxBytes+1-len(sample)), false},
		{"quoted marker and duplicate question", strings.Replace(strings.Replace(sample, "docbank_answer_card: 1", `"docbank_answer_card": 1`, 1), "question: When is renewal?", "question: a\nquestion: b", 1), false},
		{"unterminated quoted marker", strings.Replace(strings.Replace(sample, "docbank_answer_card: 1", `"docbank_answer_card": 1`, 1), "\n---\n", "\n", 1), false},
		{"unmarked Latin-1 text", "caf\xe9", true},
		{"unmarked frontmatter with Latin-1 body", "---\ntitle: Notes\n---\ncaf\xe9", true},
		{"marked Latin-1 frontmatter", strings.Replace(sample, "question: When is renewal?", "question: caf\xe9", 1), false},
		{"marked Latin-1 body", sample + "caf\xe9", false},
	}
	for _, tc := range []struct{ name, old, replacement string }{
		{"question", "question: When is renewal?", "question: !!binary Zm9v/w=="},
		{"alternate", "question: When is renewal?", "question: When is renewal?\nalternates: [!!binary Zm9v/w==]"},
		{"quote", "quote: test", "quote: !!binary dGVz/w=="},
		{"approved hash", "state: draft", "state: draft\napproved_hash: !!binary Zm9v/w=="},
		{"approved at", "state: draft", "state: draft\napproved_at: !!binary Zm9v/w=="},
		{"stale reason", "state: draft", "state: draft\nstale:\n  reason: !!binary Zm9v/w==\n  observed_at: now"},
		{"stale observed at", "state: draft", "state: draft\nstale:\n  reason: new_version\n  observed_at: !!binary Zm9v/w=="},
	} {
		input := strings.Replace(sample, tc.old, tc.replacement, 1)
		if !utf8.ValidString(input) {
			t.Fatalf("%s fixture must be valid UTF-8", tc.name)
		}
		tests = append(tests, invalidCase{tc.name, input, false})
	}
	stale := strings.Replace(sample, "state: draft", "state: stale\nstale:\n  reason: new_version\n  citation: 0", 1)
	for _, tc := range []struct{ name, input string }{
		{"fractional marker", strings.Replace(sample, "docbank_answer_card: 1", "docbank_answer_card: 1.5", 1)},
		{"fractional citation version", strings.Replace(sample, "  - version: 1", "  - version: 1.5", 1)},
		{"fractional node identity", strings.Replace(sample, "node_id: 42", "node_id: 42.5", 1)},
		{"fractional range start", strings.Replace(sample, "start: 0", "start: 0.5", 1)},
		{"fractional range end", strings.Replace(sample, "end: 4", "end: 4.5", 1)},
		{"fractional stale citation", strings.Replace(stale, "citation: 0", "citation: 0.5", 1)},
		{"fractional aliased citation version", strings.Replace(strings.Replace(sample, "question: When is renewal?", "question: &version 1.5", 1), "  - version: 1", "  - version: *version", 1)},
	} {
		tests = append(tests, invalidCase{tc.name, tc.input, false})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil || errors.Is(err, ErrNotCard) != tc.notCard {
				t.Errorf("wrong classification: not_a_card=%v, error=%v", tc.notCard, err)
			} else if tc.name == "unsupported marker" && !strings.Contains(err.Error(), "unsupported answer card version: 2") {
				t.Errorf("missing unsupported-version error: %v", err)
			}
		})
	}
}

func FuzzParseCard(f *testing.F) {
	f.Add([]byte(sample))
	f.Add([]byte("---\ninvalid"))
	f.Add([]byte("caf\xe9"))
	f.Add([]byte(sample + "caf\xe9"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := Parse(b)
		if err != nil {
			return
		}
		if c.Hash() == "" || c.Effective(nil).State == Invalid {
			t.Fatal("parsed card must have valid canonical content")
		}
		encoded, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		roundtrip, err := Parse([]byte("---\n" + string(encoded) + "---\n" + c.Answer))
		if err != nil {
			t.Fatal(err)
		}
		if roundtrip.Hash() != c.Hash() {
			t.Fatal("round trip changed canonical content")
		}
	})
}

func TestCanonicalAndTransitions(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	h := c.Hash()
	c.Alternates = []string{}
	if c.Hash() != h {
		t.Fatal("empty alternates changed hash")
	}
	c.ApprovedAt = "2026-10-26T12:00:00Z"
	c.ApprovedHash = "anything"
	c.Stale = &StaleInfo{Reason: "new_version", Citation: 0}
	if c.Hash() != h {
		t.Fatal("lifecycle fields changed hash")
	}
	for _, from := range []State{Draft, Approved, Stale} {
		for _, to := range []State{Draft, Approved, Stale} {
			want := from == Draft && to == Approved || from == Approved && (to == Draft || to == Stale) || from == Stale && to == Approved
			if CanTransition(from, to) != want {
				t.Fatalf("%s -> %s", from, to)
			}
		}
	}
	c.State = Stale
	c.Stale = nil
	if c.Effective(nil).State != Invalid {
		t.Fatal("invalid stale record accepted")
	}
}

func TestNormalizedDuplicatesAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, replacement string
		invalid           bool
	}{
		{"Unicode duplicate", "question: Café\nalternates: [ＣＡＦÉ]", false},
		{"punctuation duplicate", "question: hello world\nalternates: [hello-world]", false},
		{"oversized question", "question: \"" + strings.Repeat(" ", 301) + "a\"", true},
		{"oversized duplicate alternate", "question: When is renewal?\nalternates: [\"When is renewal?" + strings.Repeat(" ", 301) + "\"]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(strings.Replace(sample, "question: When is renewal?", tc.replacement, 1)))
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted oversized input")
				}
			} else if err != nil || len(c.Alternates) != 0 {
				t.Fatalf("duplicate not removed: %v %v", c.Alternates, err)
			}
		})
	}
}

func TestYAMLIntegerAliasesAndStringScalars(t *testing.T) {
	input := strings.Replace(sample, "docbank_answer_card: 1", "\"docbank_answer_card\": &version 1", 1)
	input = strings.Replace(input, "  - version: 1", "  - version: *version", 1)
	input = strings.Replace(input, "question: When is renewal?", "question: 2026", 1)
	card, err := Parse([]byte(input))
	if err != nil || card.Question != "2026" || card.Citations[0].Version != 1 {
		t.Fatalf("valid integer aliases or string-valued scalars rejected: %+v %v", card, err)
	}
}

func FuzzRejectNonIntegerYAMLFields(f *testing.F) {
	f.Add(uint8(0), uint64(5))
	f.Add(uint8(5), uint64(0))
	f.Fuzz(func(t *testing.T, field uint8, fraction uint64) {
		fields := []string{"docbank_answer_card: 1", "  - version: 1", "node_id: 42", "start: 0", "end: 4", "citation: 0"}
		input := sample
		index := int(field) % len(fields)
		if index == 5 {
			input = strings.Replace(input, "state: draft", "state: stale\nstale:\n  reason: new_version\n  citation: 0", 1)
		}
		input = strings.Replace(input, fields[index], fmt.Sprintf("%s.%d", fields[index], fraction), 1)
		if _, err := Parse([]byte(input)); err == nil || errors.Is(err, ErrNotCard) {
			t.Fatalf("accepted noninteger field %s.%d", fields[index], fraction)
		}
	})
}

func TestHashBindsContent(t *testing.T) {
	base, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	mutations := []func(*Card){
		func(c *Card) { c.Question += "?" }, func(c *Card) { c.Alternates = []string{"another question"} }, func(c *Card) { c.Answer += " changed" },
		func(c *Card) { c.Citations[0].Version++ }, func(c *Card) { c.Citations[0].VaultUID[15]++ }, func(c *Card) { c.Citations[0].NodeID++ }, func(c *Card) { c.Citations[0].ContentVersionID[15]++ },
		func(c *Card) { c.Citations[0].ContentSHA256 += "a" }, func(c *Card) { c.Citations[0].RenditionAttachmentID += "a" }, func(c *Card) { c.Citations[0].BuildID += "a" }, func(c *Card) { c.Citations[0].RenditionSHA256 += "a" },
		func(c *Card) { c.Citations[0].Start++ }, func(c *Card) { c.Citations[0].End++ }, func(c *Card) { c.Citations[0].Quote += "x" },
	}
	for i, mutate := range mutations {
		c := base
		c.Citations = append([]Citation(nil), base.Citations...)
		mutate(&c)
		if c.Hash() == base.Hash() {
			t.Fatalf("mutation %d did not change hash", i)
		}
	}
}

func TestCanonicalEncodingFailureCannotApprove(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	c.Question = "invalid\xff"
	c.State = Approved
	c.ApprovedHash = ""
	if c.Hash() != "" {
		t.Fatal("invalid UTF-8 was canonically encoded")
	}
	if got := c.Effective(nil); got.State == Approved {
		t.Fatal("failed encoding approved", got)
	}
}

func TestEmptyQuoteNeedsApprovalResolution(t *testing.T) {
	card, err := Parse([]byte(strings.Replace(sample, "quote: test", `quote: ""`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := card.Effective(nil); got.State != Draft {
		t.Fatal(got)
	}
	card.State = Approved
	card.ApprovedHash = card.Hash()
	if got := card.Effective(nil); got.State != Invalid {
		t.Fatalf("unresolved quotation became approved: %+v", got)
	}
}
