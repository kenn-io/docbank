package grounding

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		name, query, a, b, status, kind, unit string
		values                                []string
		compared                              []bool
	}{
		{"duration conflict", "cooling period", "The cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"figure first", "cooling period", "7 days is the cooling period.", "15 days is the cooling period.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"duration agreement", "cooling period", "The cooling period is 7 days.", "The cooling period is 7 days.", "NO_CONFLICT", "", "", nil, []bool{true, true}},
		{"money agreement", "service fee", "The service fee is USD 5.", "The service fee is $5.", "NO_CONFLICT", "", "", nil, []bool{true, true}},
		{"product variants", "service fee", "The premium service fee is $5.", "The standard service fee is $6.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"qualifiers", "warranty", "The warranty is 12 months for parts.", "The warranty is 24 months for labor.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"escaped decimals", "service fee", `The service fee is $5\.50\.`, `The service fee is $6\.50\.`, "CONFLICTING", "money", "USD", []string{"5.5", "6.5"}, []bool{true, true}},
		{"percent", "service fee", "The service fee is 5%.", "The service fee is 7 percent.", "CONFLICTING", "percent", "%", []string{"5", "7"}, []bool{true, true}},
		{"unit mismatch", "cooling period", "The cooling period is 7 days.", "The cooling period is 2 weeks.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"date plus amount", "service fee", "The service fee on 1 July 2026 is $5.", "The service fee on 1 July 2026 is $7.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"no matching query term", "refund", "The cooling period is 7 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"FAQ question", "how long is the cooling period", "How long is the cooling period? The cooling period is 7 days.", "How long is the cooling period? The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"hard wrapped leading qualifiers", "cooling period", "For annual plans bought in store,\nthe cooling period is 30 days.", "For monthly plans bought online,\nthe cooling period is 14 days.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"capitalised clipped fragment", "cooling period", " … Company terms set the cooling period at 7 days.", " … Company terms set the cooling period at 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"quoted sentence", "cooling period", `"The cooling period is 7 days."`, `"The cooling period is 15 days."`, "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"closing bracket", "cooling period", "[The cooling period is 7 days.]", "[The cooling period is 15 days.]", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"clipped start", "cooling period", " … cooling period is 7 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"clipped end", "cooling period", "The cooling period is 7 days … ", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"clipped decimal", "service fee", "The service fee is $7. … ", "The service fee is $7. … ", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"clipped final paragraph", "service fee", "Overview.\n\nThe service fee is $7. … ", "The service fee is $7.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"whole sentence after clipped fragment", "cooling period", " … Company terms apply. The cooling period is 7 days. Contact support … ", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"lowercase start", "cooling period", "the cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"unterminated", "cooling period", "The cooling period is 7 days", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"contradictory passage", "cooling period", "The cooling period is 7 days. The cooling period is 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"punctuation distinguishes assertions", "cooling period", "Cooling period: 7 days.", "Cooling period - 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"query prefix", "cool per", "The cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"wrapped trailing qualifiers", "cooling period", "The cooling period is 7 days\nfor premium accounts.", "The cooling period is 15 days\nfor standard accounts.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"accounting parentheses", "account balance", "The account balance is ($5).", "The account balance is $5.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"wrapped quoted sentence", "cooling period", "\"The cooling period is 7 days\nfor premium accounts.\"", "\"The cooling period is 15 days\nfor standard accounts.\"", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"wrapped FAQ answer", "how long is the cooling period", "How long is the cooling period? The cooling period is 7 days\nfor premium accounts.", "How long is the cooling period? The cooling period is 15 days\nfor standard accounts.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"heading without end mark", "cooling period", "Cooling period: 7 days\n\nContact support.", "Cooling period: 15 days\n\nContact support.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"bullet without end mark", "cooling period", "- Cooling period: 7 days\n", "- Cooling period: 15 days\n", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"different headings", "cooling period", "Premium policy\n\nThe cooling period is 7 days.", "Standard policy\n\nThe cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"multiple figures", "cooling period", "The cooling period is 7 to 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"unsupported count", "device limit", "The device limit is 3 devices.", "The device limit is 5 devices.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(tc.query, []string{tc.a, tc.b})
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
			for i, figure := range conflict.Figures {
				require.Equal(t, tc.values[i], figure.Value)
				require.Equal(t, i+1, figure.Passage)
				require.NotEmpty(t, figure.Text)
				require.NotEmpty(t, figure.Quote)
			}
		})
	}
}
