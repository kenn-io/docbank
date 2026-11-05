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
		{"lowercase variants", "service fee", "the premium service fee is $5.", "the standard service fee is $6.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"qualifiers", "warranty", "The warranty is 12 months for parts.", "The warranty is 24 months for labor.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"escaped decimals", "service fee", `The service fee is $5\.50\.`, `The service fee is $6\.50\.`, "CONFLICTING", "money", "USD", []string{"5.5", "6.5"}, []bool{true, true}},
		{"percent", "service fee", "The service fee is 5%.", "The service fee is 7 percent.", "CONFLICTING", "percent", "%", []string{"5", "7"}, []bool{true, true}},
		{"unit mismatch", "cooling period", "The cooling period is 7 days.", "The cooling period is 2 weeks.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"date plus amount", "service fee", "The service fee on 1 July 2026 is $5.", "The service fee on 1 July 2026 is $7.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"missing query term", "cooling period refund", "Refunds apply. The cooling period is 7 days.", "Refunds apply. The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"clipped start", "cooling period", " … cooling period is 7 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"clipped end", "cooling period", "The cooling period is 7 days … ", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"whole sentence after marker", "cooling period", " … The cooling period is 7 days. … ", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"lowercase start", "cooling period", "the cooling period is 7 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"unterminated", "cooling period", "The cooling period is 7 days", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"contradictory passage", "cooling period", "The cooling period is 7 days. The cooling period is 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"punctuation independent subject", "cooling period", "Cooling period: 7 days.", "Cooling period – 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"query prefix", "cool per", "The cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"line terminator", "cooling period", "The cooling period is 7 days\n", "The cooling period is 15 days\n", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
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
