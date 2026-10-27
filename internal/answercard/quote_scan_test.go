package answercard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func quoteWindows(text string) func(int, int) (string, bool, error) {
	runes := []rune(text)
	return func(offset, maxChars int) (string, bool, error) {
		end := min(offset+maxChars, len(runes))
		return string(runes[offset:end]), end == len(runes), nil
	}
}

func TestScanExactQuote(t *testing.T) {
	for _, tc := range []struct {
		text, quote   string
		window, start int
		refusal       string
	}{
		{"abc雪🌱déf", "c雪🌱d", 3, 2, ""},
		{"aaaa", "aaa", 2, 0, "ambiguous"},
		{"abcabc", "abc", 3, 0, "ambiguous"},
		{"xxabcxxabc", "abc", 4, 0, "ambiguous"},
		{"xxabc", "abc", 1, 2, ""},
		{"abc", "absent", 2, 0, "missing"},
		{"", "a", 2, 0, "missing"},
		{"abc", "", 2, 0, "empty"},
	} {
		t.Run(fmt.Sprintf("%q/%q/%d", tc.text, tc.quote, tc.window), func(t *testing.T) {
			start, hash, err := scanExactQuote(tc.quote, tc.window, quoteWindows(tc.text))
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("want %s, got %v", tc.refusal, err)
				}
				return
			}
			expectedHash := sha256.Sum256([]byte(tc.text))
			if err != nil || start != tc.start || hash != hex.EncodeToString(expectedHash[:]) {
				t.Fatalf("start=%d hash=%s error=%v", start, hash, err)
			}
		})
	}
}

func TestScanExactQuoteRequiresCompleteBoundedEvidence(t *testing.T) {
	for _, suffix := range []string{"", "z"} {
		text := "quote" + strings.Repeat("x", maxRebindChars-5) + suffix
		_, _, err := scanExactQuote("quote", rebindWindowChars, quoteWindows(text))
		if (err != nil) != (suffix != "") {
			t.Fatalf("limit suffix=%q: %v", suffix, err)
		}
	}
	_, _, err := scanExactQuote("a", 1, func(offset, _ int) (string, bool, error) {
		if offset == 0 {
			return "a", false, nil
		}
		return "", false, errors.New("interrupted scan")
	})
	if err == nil || !strings.Contains(err.Error(), "interrupted scan") {
		t.Fatal(err)
	}
	_, _, err = scanExactQuote("a", 1, func(int, int) (string, bool, error) { return "", false, nil })
	if err == nil {
		t.Fatal("accepted non-progressing window")
	}
	_, _, err = scanExactQuote("a", 1, func(int, int) (string, bool, error) { return "\xff", true, nil })
	if err == nil {
		t.Fatal("accepted invalid UTF-8")
	}
	_, _, err = scanExactQuote("a", 2, func(int, int) (string, bool, error) { return "a", false, nil })
	if err == nil {
		t.Fatal("accepted short window without EOF")
	}
	for _, quote := range []string{"\xff", strings.Repeat("a", rebindWindowChars+1)} {
		if _, _, err := scanExactQuote(quote, 1, quoteWindows("a")); err == nil {
			t.Fatal("accepted invalid or oversized quote")
		}
	}
	for _, size := range []int{0, rebindWindowChars + 1} {
		if _, _, err := scanExactQuote("a", size, quoteWindows("a")); err == nil {
			t.Fatal("accepted invalid window size")
		}
	}
}

func TestScanExactQuoteFourByteLimit(t *testing.T) {
	for _, suffix := range []string{"", "🌱"} {
		text := "🧪" + strings.Repeat("🌱", maxRebindChars-1) + suffix
		_, _, err := scanExactQuote("🧪", rebindWindowChars, quoteWindows(text))
		if (err != nil) != (suffix != "") {
			t.Fatalf("four-byte boundary suffix=%q: %v", suffix, err)
		}
	}
}

// Independent oracle enumerates every possible rune start, including overlaps.
func FuzzScanExactQuote(f *testing.F) {
	f.Add("abc雪🌱déf", "c雪🌱d", uint8(3))
	f.Add("aaaa", "aaa", uint8(2))
	f.Add("abc", "absent", uint8(1))
	f.Add("", "", uint8(1))
	f.Fuzz(func(t *testing.T, text, quote string, size uint8) {
		if !utf8.ValidString(text) || !utf8.ValidString(quote) {
			return
		}
		if utf8.RuneCountInString(text) > maxRebindChars || len(text) > maxRebindBytes {
			return
		}
		runes, needle := []rune(text), []rune(quote)
		count, want := 0, 0
		for i := 0; i+len(needle) <= len(runes) && len(needle) > 0; i++ {
			if string(runes[i:i+len(needle)]) == quote {
				count++
				want = i
			}
		}
		start, _, err := scanExactQuote(quote, int(size)+1, quoteWindows(text))
		valid := len(needle) > 0 && len(needle) <= rebindWindowChars && count == 1
		if (err == nil) != valid || (valid && start != want) {
			t.Fatalf("count=%d want=%d got=%d error=%v", count, want, start, err)
		}
	})
}
