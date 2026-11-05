package grounding

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		name, a, b, status, kind, unit string
		values                         []string
		compared                       []bool
	}{
		{"duration conflict", "The cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"duration agreement", "The cooling period is 7 days.", "The cooling period is 7 days.", "NO_CONFLICT", "", "", nil, []bool{true, true}},
		{"product variants", "The premium service fee is $5.", "The standard service fee is $6.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"qualifiers", "The warranty is 12 months for parts.", "The warranty is 24 months for labor.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"escaped decimals", `The service fee is $5\.50\.`, `The service fee is $6\.50\.`, "CONFLICTING", "money", "USD", []string{"5.5", "6.5"}, []bool{true, true}},
		{"percent", "The service fee is 5%.", "The service fee is 7 percent.", "CONFLICTING", "percent", "%", []string{"5", "7"}, []bool{true, true}},
		{"unit mismatch", "The cooling period is 7 days.", "The cooling period is 2 weeks.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"hard wrapped leading qualifiers", "For annual plans bought in store,\nthe cooling period is 30 days.", "For monthly plans bought online,\nthe cooling period is 14 days.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"hard wrapped trailing qualifiers", "The cooling period is 7 days\nfor premium accounts.", "The cooling period is 15 days\nfor standard accounts.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"unterminated", "The cooling period is 7 days", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"contradictory passage", "The cooling period is 7 days. The cooling period is 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"accounting parentheses", "The account balance is ($5).", "The account balance is $5.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"multiple figures", "The cooling period is 7 to 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"unsupported count", "The device limit is 3 devices.", "The device limit is 5 devices.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare([]Source{{tc.a, 0, len(tc.a)}, {tc.b, 0, len(tc.b)}})
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.compared, got.Compared)
			if tc.status != "CONFLICTING" {
				require.Empty(t, got.Conflicts)
				return
			}
			require.Len(t, got.Conflicts, 1)
			conflict := got.Conflicts[0]
			require.Equal(t, tc.kind, conflict.Kind)
			require.Equal(t, tc.unit, conflict.Unit)
			require.Len(t, conflict.Figures, 2)
			quotes := []string{tc.a, tc.b}
			if tc.name == "escaped decimals" {
				quotes = []string{"The service fee is $5.50.", "The service fee is $6.50."}
			}
			for i, figure := range conflict.Figures {
				require.Equal(t, tc.values[i], figure.Value)
				require.Equal(t, i+1, figure.Passage)
				require.NotEmpty(t, figure.Text)
				require.Equal(t, quotes[i], figure.Quote)
			}
		})
	}
}

func TestCompareSourceSpans(t *testing.T) {
	for _, tc := range []struct {
		name, text, needle, other, quote, value, status string
		compared                                        []bool
	}{
		{"decimal cut", "The fee is $7.50.", "The fee is $7.", "The fee is $7.60.", "The fee is $7.50.", "7.5", "CONFLICTING", []bool{true, true}},
		{"email opening fence", "```\nThe cooling period is 30 days. Contact support.\n```", "The cooling period is 30 days.", "The cooling period is 15 days.", "The cooling period is 30 days.", "30", "CONFLICTING", []bool{true, true}},
		{"long fence", "````\nThe cooling period is 30 days.\n````", "The cooling period is 30 days.", "The cooling period is 15 days.", "The cooling period is 30 days.", "30", "CONFLICTING", []bool{true, true}},
		{"tilde fence", "~~~\r\nThe cooling period is 30 days.\r\n~~~", "The cooling period is 30 days.", "The cooling period is 15 days.", "The cooling period is 30 days.", "30", "CONFLICTING", []bool{true, true}},
		{"fence separates incomplete sentence", "The cooling period is 30\n```\ndays.", "The cooling period is 30", "The cooling period is 15 days.", "", "", "NOT_COMPARABLE", []bool{false, true}},
		{"capitalized segment start", "For premium plans, The fee is $7.50.", "The fee is $7.50.", "For standard plans, The fee is $7.60.", "", "", "NOT_COMPARABLE", []bool{true, true}},
		{"closing quote", `"The cooling period is 7 days."`, `"The cooling period is 7 days."`, `"The cooling period is 15 days."`, `"The cooling period is 7 days."`, "7", "CONFLICTING", []bool{true, true}},
		{"heading", "The fee is $5\n\nTerms apply.", "The fee is $5", "The fee is $6.", "", "", "NOT_COMPARABLE", []bool{false, true}},
		{"blank line splits sentence", "The cooling period is 7\n\ndays.", "The cooling period is 7", "The cooling period is 15 days.", "", "", "NOT_COMPARABLE", []bool{false, true}},
		{"blank line qualifier", "For premium plans,\n\nThe fee is $5.", "The fee is $5.", "The fee is $6.", "The fee is $5.", "5", "CONFLICTING", []bool{true, true}},
		{"outside span", "The fee is $5. The fee is $6.", "The fee is $5.", "The fee is $5.", "", "", "NO_CONFLICT", []bool{true, true}},
		{"long sentence crop", strings.Repeat("Premium ", 65) + "the fee is $7.50.", "the fee is $7.", "The fee is $7.60.", "", "", "NOT_COMPARABLE", []bool{false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := strings.Index(tc.text, tc.needle)
			require.GreaterOrEqual(t, at, 0)
			got := Compare([]Source{{tc.text, at, at + len(tc.needle)}, {tc.other, 0, len(tc.other)}})
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.compared, got.Compared)
			if tc.status == "CONFLICTING" {
				require.Len(t, got.Conflicts, 1)
				require.Equal(t, tc.quote, got.Conflicts[0].Figures[0].Quote)
				require.Equal(t, tc.value, got.Conflicts[0].Figures[0].Value)
			} else {
				require.Empty(t, got.Conflicts)
			}
		})
	}
	got := Compare([]Source{{}, {Text: "The fee is $5.", Start: 0, End: 14}})
	require.Equal(t, "NOT_COMPARABLE", got.Status)
	require.Equal(t, []bool{false, true}, got.Compared)
}
