package geminiembed

import "time"

// FilesPolicy is the configured Gemini Files-object lifecycle policy. It is
// not execution evidence or a claim about other provider-side retention.
type FilesPolicy struct {
	Transport        Transport
	RetentionCeiling time.Duration
	Cleanup          string
}

// FilesPolicy returns the client's immutable configured Files-object policy.
func (client *Client) FilesPolicy() FilesPolicy {
	if client == nil {
		return FilesPolicy{}
	}
	value := FilesPolicy{Transport: client.profile.Transport,
		RetentionCeiling: profileRetention(client.profile.Transport),
		Cleanup:          "not_applicable"}
	if value.Transport == TransportFilesAPI {
		value.Cleanup = "attempted_delete_or_unconfirmed_retention_error"
	}
	return value
}
