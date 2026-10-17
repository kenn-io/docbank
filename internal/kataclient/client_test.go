package kataclient

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.kenn.io/docbank/internal/config"
)

const projects = `{"projects":[{"id":1,"name":"archived","active":false},{"id":7,"name":"docbank","active":true},{"id":8,"name":"context","active":true}]}`
const shown = `{"issue":{"uid":"01TEST","short_id":"abcd","title":"Synthetic issue","body":"Details","metadata":{"docbank.evidence":[{"uid":"sample"}]}},"comments":[{"body":"A synthetic comment"}]}`

func fakeClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("missing authentication")
		}
		if r.URL.Path == "/api/v1/health" {
			_, _ = io.WriteString(w, `{"api_schema_version":"0.21.0"}`)
			return
		}
		if r.URL.Path == "/api/v1/projects" {
			if r.Method != http.MethodGet {
				t.Errorf("projects method %s", r.Method)
			}
			_, _ = io.WriteString(w, projects)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return New(config.KataConnection{Endpoint: s.URL, APIKey: "synthetic-key", Project: "docbank"})
}

func readRequest(t *testing.T, r *http.Request) map[string]jsontext.Value {
	t.Helper()
	var body map[string]jsontext.Value
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGetReadsQualifiedContextAndComments(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/8/issues/abcd" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		_, _ = io.WriteString(w, shown)
	})
	issue, err := c.Get(t.Context(), "context#abcd")
	if err != nil {
		t.Fatal(err)
	}
	if issue.UID != "01TEST" || issue.QualifiedRef != "context#abcd" || issue.Body != "Details" || len(issue.Comments) != 1 || issue.Comments[0].Body != "A synthetic comment" || string(issue.Metadata["docbank.evidence"]) != `[{"uid":"sample"}]` {
		t.Fatalf("unexpected issue: %+v", issue)
	}
}

func TestUnsupportedKataSchemaRefusesWrites(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = io.WriteString(w, `{"api_schema_version":"0.20.0"}`)
		case "/api/v1/projects":
			_, _ = io.WriteString(w, projects)
		default:
			if r.Method == http.MethodPost {
				writes.Add(1)
			}
			_, _ = io.WriteString(w, shown)
		}
	}))
	defer server.Close()
	c := New(config.KataConnection{Endpoint: server.URL, APIKey: "synthetic-key", Project: "docbank"})
	_, err := c.Patch(t.Context(), "abcd", "docbank.evidence", nil, jsontext.Value(`[]`))
	if err == nil || !strings.Contains(err.Error(), "incompatible_api") {
		t.Errorf("expected unsupported schema refusal, got %v", err)
	}
	if writes.Load() != 0 {
		t.Errorf("unsupported Kata received %d writes", writes.Load())
	}
}

func TestFindMarkerFollowsMovedIssue(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/issues":
			_, _ = io.WriteString(w, `{"issues":[{"uid":"01TEST","short_id":"abcd","project_name":"context","metadata":{"docbank.request":"request"}}]}`)
		case "/api/v1/projects/7/issues":
			_, _ = io.WriteString(w, `{"issues":[]}`)
		case "/api/v1/projects/8/issues/01TEST":
			_, _ = io.WriteString(w, shown)
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
	})
	issue, err := c.FindMarker(t.Context(), "marker")
	if err != nil || issue == nil || issue.QualifiedRef != "context#abcd" {
		t.Fatalf("moved marker result %+v %v", issue, err)
	}
}

func TestCreateUsesNativeWireAndFetchesCreatedIssue(t *testing.T) {
	var posts int
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			if r.URL.Path != "/api/v1/projects/7/issues" || r.Header.Get("Idempotency-Key") != "create-key" {
				t.Errorf("create route or key: %s", r.URL)
			}
			body := readRequest(t, r)
			if string(body["actor"]) != `"docbank"` || string(body["force_new"]) != "true" || string(body["title"]) != `"Synthetic issue"` || string(body["body"]) != `"Details"` || string(body["metadata"]) != `{"docbank.action":"marker"}` {
				t.Errorf("create payload: %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"issue":{"uid":"01TEST"}}`)
			return
		}
		if r.URL.Path != "/api/v1/projects/7/issues/01TEST" {
			t.Errorf("get route %s", r.URL)
		}
		_, _ = io.WriteString(w, shown)
	})
	issue, err := c.Create(t.Context(), "create-key", "Synthetic issue", "Details", map[string]jsontext.Value{"docbank.action": jsontext.Value(`"marker"`)})
	if err != nil {
		t.Fatal(err)
	}
	if posts != 1 || issue.QualifiedRef != "docbank#abcd" || len(issue.Comments) != 1 {
		t.Fatalf("create result %+v; posts %d", issue, posts)
	}
}

func TestPatchEncodesAbsentAndExactJSONGuards(t *testing.T) {
	for _, previous := range []jsontext.Value{nil, jsontext.Value(`[{"uid":"old"}]`)} {
		t.Run(string(previous), func(t *testing.T) {
			c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					if r.URL.Path != "/api/v1/projects/7/issues/abcd/metadata" {
						t.Errorf("patch route %s", r.URL)
					}
					body := readRequest(t, r)
					if string(body["actor"]) != `"docbank"` || string(body["patch"]) != `{"docbank.evidence":[{"uid":"new"}]}` {
						t.Errorf("patch body %s", body)
					}
					want := `{"key":"docbank.evidence","if_absent":true}`
					if previous != nil {
						want = `{"key":"docbank.evidence","if_value":"[{\"uid\":\"old\"}]"}`
					}
					if string(body["guard"]) != want {
						t.Errorf("guard %s want %s", body["guard"], want)
					}
					_, _ = io.WriteString(w, `{"issue":{"uid":"01TEST"}}`)
					return
				}
				_, _ = io.WriteString(w, shown)
			})
			issue, err := c.Patch(t.Context(), "docbank#abcd", "docbank.evidence", previous, jsontext.Value(`[{"uid":"new"}]`))
			if err != nil || issue.UID != "01TEST" {
				t.Fatalf("patch: %+v %v", issue, err)
			}
		})
	}
}

func TestCommentUsesIdempotencyAndDoesNotRetryUnknownOutcome(t *testing.T) {
	var calls int
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/7/issues/abcd/comments" || r.Header.Get("Idempotency-Key") != "comment-key" {
			t.Errorf("comment request %s %s", r.Method, r.URL)
		}
		body := readRequest(t, r)
		if string(body["actor"]) != `"docbank"` || string(body["body"]) != `"Evidence added"` {
			t.Errorf("comment body %s", body)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"code":"remote-secret","detail":"private response text"}`)
	})
	err := c.Comment(t.Context(), "abcd", "comment-key", "Evidence added")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != 502 || calls != 1 || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "remote-secret") {
		t.Fatalf("comment calls %d error %v", calls, err)
	}
}

func TestFindMarkerSearchesConfiguredProjectIncludingClosedOldestFirst(t *testing.T) {
	for _, found := range []bool{false, true} {
		t.Run(strconv.FormatBool(found), func(t *testing.T) {
			c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/issues" {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{}`)
					return
				}
				q := r.URL.Query()
				if r.URL.Path != "/api/v1/projects/7/issues" || q.Get("meta") != "docbank.action=marker &=+" || q.Get("sort") != "oldest" || q.Get("limit") != "1" || q.Get("status") != "" {
					t.Errorf("marker query %s", r.URL)
				}
				if found {
					_, _ = io.WriteString(w, `{"issues":[{"uid":"01TEST","short_id":"abcd","title":"Closed issue","status":"closed","metadata":{"docbank.action":"marker &=+"}}]}`)
				} else {
					_, _ = io.WriteString(w, `{"issues":[]}`)
				}
			})
			issue, err := c.FindMarker(t.Context(), "marker &=+")
			if err != nil || (issue != nil) != found {
				t.Fatalf("marker %+v error %v", issue, err)
			}
			if found && issue.QualifiedRef != "docbank#abcd" {
				t.Fatalf("reference %+v", issue)
			}
		})
	}
}

func TestGuardConflictIsSpecificAndSanitized(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"status":409,"error":{"code":"metadata_guard_failed","message":"private metadata","data":{"private":"metadata"}}}`)
	})
	_, err := c.Patch(t.Context(), "abcd", "docbank.evidence", nil, jsontext.Value(`[]`))
	if !IsGuardConflict(err) || strings.Contains(err.Error(), "private") {
		t.Fatalf("guard error %v", err)
	}
	if IsGuardConflict(&Error{Status: 409, Code: "idempotency_conflict"}) || IsGuardConflict(nil) {
		t.Fatal("unrelated conflict recognized")
	}
}

func TestInvalidReferencesAndCrossProjectWritesDoNotSendRequests(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected request %s", r.URL) })
	for _, ref := range []string{"", "../abcd", "a/b", "a%2fb", "a?x", "a#b#c", "a\\b", "a\x00b", ".", ".."} {
		if _, err := c.Get(t.Context(), ref); err == nil {
			t.Errorf("accepted ref %q", ref)
		}
	}
	if _, err := c.Patch(t.Context(), "context#abcd", "k", nil, jsontext.Value(`[]`)); err == nil {
		t.Error("cross project patch")
	}
	if err := c.Comment(t.Context(), "context#abcd", "key", "body"); err == nil {
		t.Error("cross project comment")
	}
	if _, err := c.Create(t.Context(), "", "title", "body", nil); err == nil {
		t.Error("missing create key")
	}
	if err := c.Comment(t.Context(), "abcd", "", "body"); err == nil {
		t.Error("missing comment key")
	}
}

func TestUnsafeDirectConfigurationIsRejected(t *testing.T) {
	for _, endpoint := range []string{"http://example.com", "http://127.0.0.1/path", "https://user:secret@example.com", "https://example.com?secret=key", "https://example.com/#fragment"} {
		c := New(config.KataConnection{Endpoint: endpoint, APIKey: "synthetic-key", Project: "docbank"})
		var requests atomic.Int32
		c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, errors.New("forbidden network request")
		})
		_, err := c.Get(t.Context(), "abcd")
		failure, ok := errors.AsType[*Error](err)
		if !ok || failure.Code != "invalid_connection" {
			t.Fatalf("expected invalid_connection, got %v", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("endpoint error leaked credentials: %v", err)
		}
		if requests.Load() != 0 {
			t.Fatal("invalid connection reached the network")
		}
	}
}

func TestRedirectNeverForwardsCredential(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/private", http.StatusTemporaryRedirect)
	})
	if _, err := c.Get(t.Context(), "abcd"); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached.Load() != 0 {
		t.Fatal("redirect followed")
	}
}

func TestResponseAndRequestSizesAreBounded(t *testing.T) {
	var posts atomic.Int32
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = io.WriteString(w, `{"issue":{"uid":"01TEST","short_id":"abcd","body":"`+strings.Repeat("x", 2<<20)+`"}}`)
	})
	if _, err := c.Get(t.Context(), "abcd"); err == nil {
		t.Error("oversized response accepted")
	}
	if _, err := c.Create(t.Context(), "key", "title", strings.Repeat("x", 1<<20), nil); err == nil {
		t.Error("oversized request accepted")
	}
	if posts.Load() != 0 {
		t.Fatal("oversized request sent")
	}
}

func TestContextCancellationStopsReadAndSanitizesTransportErrors(t *testing.T) {
	entered := make(chan struct{})
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "abcd"); done <- err }()
	select {
	case <-entered:
		cancel()
	case err := <-done:
		t.Fatalf("request failed before cancellation: %v", err)
	}
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation cause lost: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("private endpoint exposed")
	}
}

func TestWriteDoesNotRetryDroppedConnection(t *testing.T) {
	var calls atomic.Int32
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	err := c.Comment(t.Context(), "abcd", "synthetic-key", "Synthetic comment")
	if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("calls %d error %v", calls.Load(), err)
	}
}

func TestMalformedIssueResponsesFailClosed(t *testing.T) {
	for _, response := range []string{`{`, `{}`, `{"issue":{"uid":"../escape","short_id":"abcd"}}`, `{"issue":{"uid":"01TEST"}}`} {
		t.Run(response, func(t *testing.T) {
			c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, response) })
			if _, err := c.Get(t.Context(), "abcd"); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestInactiveProjectIsUnavailable(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("inactive project request %s", r.URL) })
	if _, err := c.Get(t.Context(), "archived#abcd"); err == nil {
		t.Fatal("inactive project accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportDeadlineRetainsOnlySafeCause(t *testing.T) {
	c := fakeClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected network call") })
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	_, err := c.Get(t.Context(), "abcd")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("transport timeout cause lost: %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("private endpoint exposed")
	}
}
