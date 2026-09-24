package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func productionRecipeBrowserRequestAllowed(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/api/v1/productions/recipes" &&
		r.URL.RawQuery == ""
}

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
		return r.Method == http.MethodPost && r.URL.RawQuery == "" ||
			r.Method == http.MethodGet && productionPageQueryAllowed(r.URL.RawQuery, 2048)
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
	if len(parts) == 3 && parts[1] == "jobs" && validPageJobPathID(parts[2]) {
		return r.Method == http.MethodGet && r.URL.RawQuery == ""
	}
	if len(parts) == 4 && parts[1] == "jobs" && validPageJobPathID(parts[2]) && parts[3] == "cancel" {
		return r.Method == http.MethodPost && r.URL.RawQuery == ""
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
			(parts[3] == "changes" || parts[3] == "members" || parts[3] == "seal" || parts[3] == "fork" || parts[3] == "jobs") && r.Method == http.MethodPost {
			return true
		}
	}
	if len(parts) == 6 && r.Method == http.MethodPost && r.URL.RawQuery == "" &&
		parts[3] == "members" && validPageJobPathID(parts[4]) && parts[5] == "review" {
		return true
	}
	if len(parts) == 5 && r.Method == http.MethodGet && parts[3] == "maps" && validPageJobPathID(parts[4]) {
		return productionBoundedQueryAllowed(r.URL.RawQuery, 512, 65536)
	}
	if r.Method != http.MethodGet || len(parts) != 4 {
		return false
	}
	if parts[3] == "decisions" {
		return productionDecisionQueryAllowed(r.URL.RawQuery)
	}
	return parts[3] == "members" && productionPageQueryAllowed(r.URL.RawQuery, 4096)
}

func productionDecisionQueryAllowed(raw string) bool {
	if raw == "" {
		return true
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(values) > 3 {
		return false
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return false
		}
		switch key {
		case "cursor":
			if len(entries[0]) > 2048 {
				return false
			}
		case packageLimitParameter:
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > 500 {
				return false
			}
		case "uncertain":
			if entries[0] != "true" && entries[0] != "false" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func productionPageQueryAllowed(raw string, maxCursor int) bool {
	return productionBoundedQueryAllowed(raw, maxCursor, 200)
}

func productionBoundedQueryAllowed(raw string, maxCursor, maxLimit int) bool {
	if raw == "" {
		return true
	}
	values, err := url.ParseQuery(raw)
	if err != nil || len(values) > 2 {
		return false
	}
	for key, entries := range values {
		if len(entries) != 1 {
			return false
		}
		switch key {
		case "cursor":
			if len(entries[0]) > maxCursor {
				return false
			}
		case packageLimitParameter:
			limit, err := strconv.Atoi(entries[0])
			if err != nil || limit < 1 || limit > maxLimit {
				return false
			}
		default:
			return false
		}
	}
	return true
}
