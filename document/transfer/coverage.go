package transfer

import (
	"errors"
	"slices"
)

// SourceQualification describes one acquisition route whose end-to-end
// behavior must be qualified independently.
type SourceQualification struct {
	SourceType   SourceType
	Route        string
	Capabilities []CapabilityEntryV1
}

var sourceQualificationRows = []struct {
	sourceType SourceType
	route      string
}{
	{SourceTypeGmail, "gmail_api"},
	{SourceTypeIMAP, "imap"},
	{SourceTypeMBOX, "mbox"},
	{SourceTypeEMLX, "emlx"},
	{SourceTypePST, "pst"},
	{SourceTypeTeams, "teams"},
	{SourceTypeSlack, "slack"},
	{SourceTypeDiscord, "discord"},
	{SourceTypeBeeper, "beeper"},
	{SourceTypeIMessage, "imessage"},
	{SourceTypeWhatsApp, "whatsapp"},
	{SourceTypeMessenger, "messenger"},
	{SourceTypeGoogleVoice, "google_voice"},
	{SourceTypeSyncTech, "synctech_local"},
	{SourceTypeSyncTech, "synctech_drive"},
	{SourceTypeGoogleCalendar, "google_calendar"},
	{SourceTypeGranola, "granola"},
	{SourceTypeCircleback, "circleback"},
}

// SourceQualificationMatrix returns a fresh 18-route inventory. Its entries
// remain unavailable until route-specific producer and consumer evidence is
// recorded; the inventory alone never claims support.
func SourceQualificationMatrix() []SourceQualification {
	result := make([]SourceQualification, len(sourceQualificationRows))
	for index, row := range sourceQualificationRows {
		result[index] = SourceQualification{
			SourceType:   row.sourceType,
			Route:        row.route,
			Capabilities: unqualifiedCapabilities(),
		}
	}
	return result
}

func ValidateCapabilityEntries(entries []CapabilityEntryV1) error {
	if len(entries) != len(capabilities) {
		return errors.New("transfer: invalid capability coverage")
	}
	seen := make([]Capability, 0, len(entries))
	for _, entry := range entries {
		if !ValidCapability(entry.Capability) || !ValidCapabilityState(entry.State) ||
			slices.Contains(seen, entry.Capability) {
			return errors.New("transfer: invalid capability coverage")
		}
		if entry.State == CapabilityStateAvailable {
			if entry.Reason != "" {
				return errors.New("transfer: invalid capability coverage")
			}
		} else if !ValidReasonCode(entry.Reason) {
			return errors.New("transfer: invalid capability coverage")
		}
		seen = append(seen, entry.Capability)
	}
	return nil
}

func unqualifiedCapabilities() []CapabilityEntryV1 {
	result := make([]CapabilityEntryV1, len(capabilities))
	for index, capability := range capabilities {
		result[index] = CapabilityEntryV1{
			Capability: capability,
			State:      CapabilityStateUnavailable,
			Reason:     ReasonNotQualified,
		}
	}
	return result
}
