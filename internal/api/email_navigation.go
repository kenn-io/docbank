package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
)

func emailNavigationBrowserReadAllowed(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/versions/"); ok {
		_, err := document.NormalizeEmailDocumentRelationQuery(document.EmailDocumentRelationQuery{ParentVersionID: id})
		return r.URL.RawQuery == "" && err == nil
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/email-document-publications/"); ok {
		return r.URL.RawQuery == "" && document.ValidateEmailDocumentOperationID(id) == nil
	}
	if r.URL.Path != "/api/v1/email-document-relations" {
		return false
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	q := document.EmailDocumentRelationQuery{}
	for key, entries := range values {
		if len(entries) != 1 || entries[0] == "" {
			return false
		}
		switch key {
		case "parent_version_id":
			q.ParentVersionID = entries[0]
		case "child_version_id":
			q.ChildVersionID = entries[0]
		case "after_operation_id":
			q.AfterOperationID = entries[0]
		case "after_order", "limit":
			n, err := strconv.Atoi(entries[0])
			if err != nil || n < 1 {
				return false
			}
			if key == "limit" {
				q.Limit = n
			} else {
				q.AfterOrder = n
			}
		default:
			return false
		}
	}
	if (q.AfterOperationID == "") != (q.AfterOrder == 0) {
		return false
	}
	_, err = document.NormalizeEmailDocumentRelationQuery(q)
	return err == nil
}
