package geminiembed

// UploadPolicy is the immutable local authority used to inspect direct-file
// inputs before Gemini receives them.
type UploadPolicy struct {
	CapabilityProfileFingerprint string
	DisclosureFingerprint        string
	MaxInputBytes                int64
	MaxSourceFrames              int64
}

// UploadPolicy returns the client's normalized direct-file authority without
// exposing credentials or transport state.
func (client *Client) UploadPolicy() UploadPolicy {
	if client == nil {
		return UploadPolicy{}
	}
	return UploadPolicy{
		CapabilityProfileFingerprint: client.profile.CapabilityProfileFingerprint,
		DisclosureFingerprint:        client.profile.DisclosureFingerprint,
		MaxInputBytes:                client.profile.MaxInputBytes,
		MaxSourceFrames:              10_000,
	}
}
