package ingest

import (
	"encoding/json/v2"
	"net/url"
	"time"
)

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
	for _, raw := range []string{f.RequestedURL, f.FinalURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil
		}
	}
	at, err := time.Parse(time.RFC3339Nano, f.FetchedAt)
	if err != nil || at.Location() != time.UTC {
		return nil
	}
	return &f
}
