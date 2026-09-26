package document

import (
	"bytes"
	"testing"
)

func TestSectionPage(t *testing.T) {
	body := []byte("a😀bc")
	part, next, err := SectionPage(body, 0, len(body), 4)
	if err != nil || string(part) != "a" || next != 1 {
		t.Fatal(string(part), next, err)
	}
	part, next, err = SectionPage(body, next, len(body), 4)
	if err != nil || string(part) != "😀" || next != 5 {
		t.Fatal(string(part), next, err)
	}
}

func TestSectionPageEmptySectionIsComplete(t *testing.T) {
	part, next, err := SectionPage([]byte("beforeafter"), 6, 6, 1)
	if err != nil || len(part) != 0 || next != 6 {
		t.Fatal(part, next, err)
	}
}

func TestSectionPageRejectsInvalidRangesAndTooSmallBudget(t *testing.T) {
	body := []byte("a😀b")
	for _, test := range []struct {
		name               string
		body               []byte
		start, end, budget int
	}{
		{name: "zero budget", body: body, start: 0, end: len(body), budget: 0},
		{name: "split start", body: body, start: 2, end: len(body), budget: 1},
		{name: "split end", body: body, start: 0, end: 2, budget: 1},
		{name: "past end", body: body, start: 0, end: len(body) + 1, budget: 1},
		{name: "invalid UTF-8", body: []byte{0xff}, start: 0, end: 1, budget: 1},
		{name: "codepoint too large", body: []byte("😀"), start: 0, end: 4, budget: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			part, next, err := SectionPage(test.body, test.start, test.end, test.budget)
			if err == nil || part != nil || next != test.start {
				t.Fatal(part, next, err)
			}
		})
	}
}

func TestSectionPageConcatenatesToExactSection(t *testing.T) {
	body := []byte("prefix|alpha 😀\n| a | b |\n|---|---|\n```go\n# not a heading\n```\nomega|suffix")
	start := bytes.Index(body, []byte("alpha"))
	end := bytes.Index(body, []byte("|suffix"))
	var joined []byte
	for next := start; next < end; {
		part, after, err := SectionPage(body, next, end, 13)
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, part...)
		next = after
	}
	if !bytes.Equal(joined, body[start:end]) {
		t.Fatalf("got %q want %q", joined, body[start:end])
	}
}
