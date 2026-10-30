package ingest

import "encoding/json/v2"

// URLFetch records the response observed by the importing client.
type URLFetch struct {
	Version      int    `json:"version"`
	RequestedURL string `json:"requested_url"`
	FinalURL     string `json:"final_url"`
	FetchedAt    string `json:"fetched_at"`
	ETag         []byte `json:"etag,omitempty"`
	LastModified []byte `json:"last_modified,omitempty"`
}

// ParseURLFetch accepts the supported evidence format for both uploads and readers.
func ParseURLFetch(kind, description string) *URLFetch {
	var f URLFetch
	if kind != "url" || json.Unmarshal([]byte(description), &f, json.RejectUnknownMembers(true)) != nil || f.Version != 1 {
		return nil
	}
	return &f
}
