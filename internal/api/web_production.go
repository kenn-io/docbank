package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Browser sessions may request only the verified, one-use package handoff.
func productionPackageBrowserRequestAllowed(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		return false
	}
	after, ok := strings.CutPrefix(r.URL.Path, "/api/v1/productions/jobs/")
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	return len(parts) == 4 && validBatesBrowserID(parts[0]) &&
		parts[1] == "packages" && validBatesBrowserID(parts[2]) && parts[3] == "download"
}

func productionSetBrowserRequestAllowed(r *http.Request) bool {
	const base = "/api/v1/productions/sets"
	if r.URL.Path == base {
		return r.Method == http.MethodPost && r.URL.RawQuery == ""
	}
	after, ok := strings.CutPrefix(r.URL.Path, base+"/")
	if !ok {
		return false
	}
	parts := strings.Split(after, "/")
	if !validPageJobPathID(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return r.Method == http.MethodGet && r.URL.RawQuery == ""
	}
	if len(parts) < 3 || parts[1] != "revisions" {
		return false
	}
	revision, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != parts[2] {
		return false
	}
	if len(parts) == 3 {
		return r.Method == http.MethodGet && r.URL.RawQuery == ""
	}
	if len(parts) == 4 && r.URL.RawQuery == "" {
		if parts[3] == "instructions" && r.Method == http.MethodPut ||
			parts[3] == "changes" && r.Method == http.MethodPost {
			return true
		}
	}
	if r.Method != http.MethodGet || len(parts) != 4 ||
		parts[3] != "members" && parts[3] != "decisions" {
		return false
	}
	if r.URL.RawQuery == "" {
		return true
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values) > 2 {
		return false
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return false
		}
		switch key {
		case "cursor":
			if len(entries[0]) > 4096 {
				return false
			}
		case packageLimitParameter:
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > 200 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
