package transfer

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceQualificationMatrixHasEveryHonestRoute(t *testing.T) {
	wantRoutes := []string{
		"gmail_api", "imap", "mbox", "emlx", "pst", "teams", "slack", "discord", "beeper",
		"imessage", "whatsapp", "messenger", "google_voice", "synctech_local", "synctech_drive",
		"google_calendar", "granola", "circleback",
	}
	rows := SourceQualificationMatrix()
	require.Len(t, rows, len(wantRoutes))

	seenRoutes := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		require.Contains(t, wantRoutes, row.Route)
		_, duplicateRoute := seenRoutes[row.Route]
		require.False(t, duplicateRoute, row.Route)
		seenRoutes[row.Route] = struct{}{}
		require.True(t, ValidSourceType(row.SourceType))

		require.Len(t, row.Capabilities, len(AllCapabilities()))
		seenCapabilities := make(map[Capability]struct{}, len(row.Capabilities))
		for _, entry := range row.Capabilities {
			require.True(t, ValidCapability(entry.Capability))
			_, duplicateCapability := seenCapabilities[entry.Capability]
			require.False(t, duplicateCapability, "%s: %s", row.Route, entry.Capability)
			seenCapabilities[entry.Capability] = struct{}{}
			require.Equal(t, CapabilityStateUnavailable, entry.State)
			require.Equal(t, ReasonNotQualified, entry.Reason)
		}
	}
	require.ElementsMatch(t, wantRoutes, mapKeys(seenRoutes))

	rows[0].Route = "mutated"
	rows[0].Capabilities[0].State = CapabilityStateAvailable
	fresh := SourceQualificationMatrix()
	require.Equal(t, "gmail_api", fresh[0].Route)
	require.Equal(t, CapabilityStateUnavailable, fresh[0].Capabilities[0].State)
}

func TestValidateCapabilityEntriesRejectsDuplicatesAndDishonestReasons(t *testing.T) {
	valid := unqualifiedCapabilities()
	require.NoError(t, ValidateCapabilityEntries(valid))

	duplicate := append([]CapabilityEntryV1(nil), valid...)
	duplicate[len(duplicate)-1].Capability = duplicate[0].Capability
	require.Error(t, ValidateCapabilityEntries(duplicate))

	missingReason := append([]CapabilityEntryV1(nil), valid...)
	missingReason[0].Reason = ""
	require.Error(t, ValidateCapabilityEntries(missingReason))

	availableWithReason := append([]CapabilityEntryV1(nil), valid...)
	availableWithReason[0].State = CapabilityStateAvailable
	require.Error(t, ValidateCapabilityEntries(availableWithReason))
}

func mapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}
