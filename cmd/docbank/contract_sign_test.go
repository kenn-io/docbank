package main

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/docusealclient"
)

func TestContractSignDaemonWalk(t *testing.T) {
	configured := true
	templateBody := ""
	queries := make(chan string, 10)
	var items []string
	for id := 200; id >= 101; id-- {
		items = append(items, fmt.Sprintf(`{"id":%d,"name":"Lease","roles":["Buyer, Seller","Buyer","Seller"]}`, id))
	}
	firstPage := `{"templates":[` + strings.Join(items, ",") + `],"next":101}`
	finalPage := `{"templates":[{"id":100,"name":"Final","roles":[]}],"next":0}`
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !configured {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"status":503,"title":"Unconfigured","detail":"Enable DocuSeal","code":"docuseal_unconfigured"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/integrations/docuseal/status":
			_, _ = fmt.Fprint(w, `{"endpoint":"https://example.test"}`)
		case "/api/v1/integrations/docuseal/templates":
			queries <- r.URL.RawQuery
			body := templateBody
			if body == "" {
				if r.URL.Query().Get("after") == "101" {
					body = finalPage
				} else {
					body = firstPage
				}
			}
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	})
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"contract", "sign", "check"}, "Connected to https://example.test"},
		{[]string{"contract", "sign", "check", "--json"}, `"endpoint"`},
	} {
		out, err := runCLI(t, tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%v: %q: %v", tc.args, out, err)
		}
	}
	out, err := runCLI(t, "contract", "sign", "templates")
	if err != nil || !strings.Contains(out, `"Buyer, Seller", "Buyer", "Seller"`) || !strings.Contains(out, "Continue with --after 101") || <-queries != "after=0" {
		t.Fatalf("first page: %q: %v", out, err)
	}
	out, err = runCLI(t, "contract", "sign", "templates", "--json")
	var page docusealclient.TemplatePage
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil || len(page.Templates) != 100 || page.Next != 101 || <-queries != "after=0" {
		t.Fatalf("first JSON page: %q: %v", out, err)
	}
	out, err = runCLI(t, "contract", "sign", "templates", "--after", strconv.FormatInt(page.Next, 10), "--json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil || len(page.Templates) != 1 || page.Templates[0].ID != 100 || page.Templates[0].Roles == nil || page.Next != 0 || <-queries != "after=101" {
		t.Fatalf("final JSON page: %q: %v", out, err)
	}
	out, err = runCLI(t, "contract", "sign", "templates", "--after", "101")
	if err != nil || !strings.Contains(out, "100\tFinal") || strings.Contains(out, "Continue with") || <-queries != "after=101" {
		t.Fatalf("final page: %q: %v", out, err)
	}

	for _, malformed := range []string{
		`{"templates":[{"id":7,"roles":[]}],"next":0}`,
		`{"templates":[{"id":7,"name":null,"roles":[]}],"next":0}`,
		`{"templates":[{"name":"Lease","roles":[]}],"next":0}`,
		`{"templates":[{"id":null,"name":"Lease","roles":[]}],"next":0}`,
		`{"templates":[{"id":7,"name":"Lease","roles":[null]}],"next":0}`,
		`{"templates":[{"id":7,"name":"Lease"}],"next":0}`,
		`{"templates":[{"id":7,"name":"Lease","roles":null}],"next":0}`,
	} {
		templateBody = malformed
		if _, err := runCLI(t, "contract", "sign", "templates", "--after", "101"); err == nil {
			t.Fatalf("accepted malformed template: %s", malformed)
		}
		select {
		case got := <-queries:
			if got != "after=101" {
				t.Fatalf("cursor: %s", got)
			}
		default:
			t.Fatal("template request did not reach daemon")
		}
	}
	configured = false
	_, err = runCLI(t, "contract", "sign", "check")
	code, _ := daemonconn.ProblemCode(err)
	if err == nil || code != "docuseal_unconfigured" {
		t.Fatalf("unconfigured: %v", err)
	}
}
