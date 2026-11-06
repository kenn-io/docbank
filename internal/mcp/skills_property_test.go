//go:build !darwin || arm64

package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"hegel.dev/go/hegel"
)

func TestSkillsUnknownNameProperty(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		if name == "workflow" || name == "search" || name == "processing" {
			return // These are the complete authored allowlist, tested separately.
		}
		response, err := skillsExchange(ht.Context(), server, requestFor("tools/call", map[string]any{
			"name": "docbank.skills", "arguments": map[string]any{"name": name},
		}))
		var wire struct {
			Error *jsonrpc.Error `json:"error"`
		}
		if err != nil {
			ht.Fatalf("guide request failed for %q: %v", name, err)
		}
		if err := json.Unmarshal(response, &wire); err != nil || wire.Error == nil || wire.Error.Code != jsonrpc.CodeInvalidParams {
			ht.Fatalf("unknown guide %q did not reject: %s (%v)", name, response, err)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSkillsDiscoveryAcrossOptionsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		options := ServerOptions{
			AllowProcessing:    hegel.Draw(ht, hegel.Booleans()),
			AllowPackageWrites: hegel.Draw(ht, hegel.Booleans()),
			AllowPhotoEdits:    hegel.Draw(ht, hegel.Booleans()),
			AllowExportWrites:  hegel.Draw(ht, hegel.Booleans()),
			AllowReportWrites:  hegel.Draw(ht, hegel.Booleans()),
			AllowKataWrites:    hegel.Draw(ht, hegel.Booleans()),
		}
		server := skillsTestServer(t, options)
		discovery, err := skillsSuccess(ht.Context(), server, requestFor("server/discover", nil))
		if err != nil {
			ht.Fatalf("discovery failed for %+v: %v", options, err)
		}
		instructions, _ := discovery["instructions"].(string)
		if !strings.Contains(instructions, "docbank.skills") {
			ht.Fatalf("guide discovery missing for %+v", options)
		}
		listed, err := skillsSuccess(ht.Context(), server, requestFor("tools/list", nil))
		if err != nil {
			ht.Fatalf("tool listing failed for %+v: %v", options, err)
		}
		encoded, err := json.Marshal(listed["tools"])
		var tools []struct {
			Name string `json:"name"`
		}
		if err != nil {
			ht.Fatalf("tool list encoding failed: %v", err)
		}
		if err := json.Unmarshal(encoded, &tools); err != nil {
			ht.Fatalf("invalid tool list: %v", err)
		}
		if !slices.ContainsFunc(tools, func(tool struct {
			Name string `json:"name"`
		}) bool {
			return tool.Name == "docbank.skills"
		}) {
			ht.Fatalf("guide tool missing for %+v", options)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestSkillsEscapedSelectorProperty(t *testing.T) {
	server := skillsTestServer(t, ServerOptions{})
	hegel.Test(t, func(ht *hegel.T) {
		spell := func(value string) string {
			var b strings.Builder
			for _, char := range value {
				if hegel.Draw(ht, hegel.Booleans()) {
					fmt.Fprintf(&b, "\\u%04x", char)
				} else {
					b.WriteRune(char)
				}
			}
			return b.String()
		}
		for _, name := range []string{"workflow", "search", "processing"} {
			raw := fmt.Sprintf(`{"%s":"%s"}`, spell("name"), spell(name))
			result, err := skillsSuccess(ht.Context(), server, requestFor("tools/call", map[string]any{
				"name": "docbank.skills", "arguments": json.RawMessage(raw),
			}))
			if err != nil {
				ht.Fatalf("escaped selector %s failed: %v", raw, err)
			}
			encoded, err := json.Marshal(result["structuredContent"])
			if err != nil {
				ht.Fatalf("metadata encoding failed: %v", err)
			}
			var metadata struct {
				Skills []struct {
					Name string `json:"name"`
				} `json:"skills"`
			}
			if err := json.Unmarshal(encoded, &metadata); err != nil || len(metadata.Skills) != 1 || metadata.Skills[0].Name != name {
				ht.Fatalf("escaped selector %s selected wrong guide: %s (%v)", raw, encoded, err)
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}
