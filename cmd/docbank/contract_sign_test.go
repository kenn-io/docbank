package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/daemonconn"
)

func TestContractSignDaemonWalk(t *testing.T) {
	configured := true
	templateBody := `{"templates":[{"id":7,"name":"Lease","roles":["Buyer, Seller","Buyer","Seller"]}],"next":0}`
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !configured {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":503,"title":"Unconfigured","detail":"Enable DocuSeal","code":"docuseal_unconfigured"}`)
			return
		}
		switch r.URL.Path {
		case "/api/v1/integrations/docuseal/status":
			fmt.Fprint(w, `{"endpoint":"https://example.test"}`)
		case "/api/v1/integrations/docuseal/templates":
			if r.URL.Query().Get("after") != "42" {
				t.Errorf("cursor: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, templateBody)
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
		{[]string{"contract", "sign", "templates", "--after", "42"}, `"Buyer, Seller", "Buyer", "Seller"`},
		{[]string{"contract", "sign", "templates", "--after", "42", "--json"}, `"roles"`},
	} {
		out, err := runCLI(t, tc.args...)
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("%v: %q: %v", tc.args, out, err)
		}
	}
	for _, malformed := range []string{
		`{"templates":[{"id":7,"roles":[]}],"next":0}`,
		`{"templates":[{"id":7,"name":null,"roles":[]}],"next":0}`,
		`{"templates":[{"name":"Lease","roles":[]}],"next":0}`,
		`{"templates":[{"id":null,"name":"Lease","roles":[]}],"next":0}`,
		`{"templates":[{"id":7,"name":"Lease","roles":[null]}],"next":0}`,
	} {
		templateBody = malformed
		if _, err := runCLI(t, "contract", "sign", "templates", "--after", "42"); err == nil {
			t.Fatalf("accepted malformed template: %s", malformed)
		}
	}
	configured = false
	_, err := runCLI(t, "contract", "sign", "check")
	code, _ := daemonconn.ProblemCode(err)
	if err == nil || code != "docuseal_unconfigured" {
		t.Fatalf("unconfigured: %v", err)
	}
}
