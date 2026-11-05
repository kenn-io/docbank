package grounding

import (
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
		{"clipped start", " … cooling period is 7 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"clipped decimal", "The service fee is $7. … ", "The service fee is $7. … ", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
		{"whole sentence after clipped fragment", " … Company terms apply. The cooling period is 7 days. Contact support … ", "The cooling period is 15 days.", "CONFLICTING", "duration", "day", []string{"7", "15"}, []bool{true, true}},
		{"unterminated", "The cooling period is 7 days", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"contradictory passage", "The cooling period is 7 days. The cooling period is 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"accounting parentheses", "The account balance is ($5).", "The account balance is $5.", "NOT_COMPARABLE", "", "", nil, []bool{true, true}},
		{"multiple figures", "The cooling period is 7 to 15 days.", "The cooling period is 15 days.", "NOT_COMPARABLE", "", "", nil, []bool{false, true}},
		{"unsupported count", "The device limit is 3 devices.", "The device limit is 5 devices.", "NOT_COMPARABLE", "", "", nil, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare([]string{tc.a, tc.b})
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
