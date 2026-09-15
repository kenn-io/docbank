package media

// TranscriptQualification describes the local evidence required to transcribe
// a fixed media container and codec through a bound provider.
type TranscriptQualification struct {
	Container, Codec, MediaType, Evidence string
	Bounded, ProviderBound                bool
}

// TranscriptState reports whether a qualification has enough local and
// provider evidence to support transcription.
func TranscriptState(q TranscriptQualification) string {
	if q.Container == "" || q.Codec == "" || q.Codec == "unknown" || !q.Bounded || q.Evidence == "" {
		return "unqualified"
	}
	if !q.ProviderBound {
		return "provider_required"
	}
	return "qualified"
}
