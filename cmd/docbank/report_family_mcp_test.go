package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/mcp"
	"go.kenn.io/docbank/report"
)

type familyMCP struct {
	connection net.Conn
	reader     *bufio.Reader
	nextID     int
	shutdown   func()
}

func startFamilyMCP(t *testing.T) *familyMCP {
	t.Helper()
	serverPipe, clientPipe := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	server := mcp.NewServerWithOptions(mcp.ServerOptions{
		AllowReportWrites: true, AllowExportWrites: true,
	})
	go func() { done <- mcp.ServeStdio(ctx, server, serverPipe, serverPipe, nil) }()
	client := &familyMCP{connection: clientPipe, reader: bufio.NewReader(clientPipe)}
	client.shutdown = sync.OnceFunc(func() {
		cancel()
		require.NoError(t, clientPipe.Close())
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(daemonShutdownTimeout):
			require.Fail(t, "MCP server did not shut down")
		}
	})
	t.Cleanup(client.shutdown)
	return client
}

func (c *familyMCP) stop(t *testing.T) {
	t.Helper()
	c.shutdown()
}

func (c *familyMCP) call(t *testing.T, name string, args any) jsontext.Value {
	t.Helper()
	c.nextID++
	request := map[string]any{
		"jsonrpc": "2.0", "id": c.nextID, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args, "_meta": map[string]any{
			sdkmcp.MetaKeyProtocolVersion:    mcp.ProtocolVersion,
			sdkmcp.MetaKeyClientCapabilities: map[string]any{},
		}},
	}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, c.connection.SetDeadline(time.Now().Add(30*time.Second)))
	_, err = c.connection.Write(append(encoded, '\n'))
	require.NoError(t, err)
	line, err := c.reader.ReadBytes('\n')
	require.NoError(t, err)
	var response struct {
		ID     int            `json:"id"`
		Error  jsontext.Value `json:"error"`
		Result struct {
			Structured jsontext.Value `json:"structuredContent"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(line, &response))
	require.Empty(t, response.Error, string(line))
	require.Equal(t, c.nextID, response.ID)
	require.NotEmpty(t, response.Result.Structured, string(line))
	return response.Result.Structured
}

func (c *familyMCP) captureReport(t *testing.T, request report.Request) familyReportObservation {
	t.Helper()
	raw := c.call(t, "create_report", map[string]any{"request": request})
	var receipt struct {
		ID    string `json:"report_id"`
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(raw, &receipt))
	require.Equal(t, "complete", receipt.State, string(raw))
	var output struct {
		Summary report.Summary `json:"summary"`
	}
	raw = c.call(t, "get_report_summary", map[string]any{"report_id": receipt.ID})
	require.NoError(t, json.Unmarshal(raw, &output))
	packet := filepath.Join(t.TempDir(), "report.zip")
	c.downloadReport(t, receipt.ID, packet)
	return familyReportObservation{summary: output.Summary, packet: packet}
}

func (c *familyMCP) downloadReport(t *testing.T, id, path string) {
	t.Helper()
	raw := c.call(t, "download_report", map[string]any{
		"report_id": id, "destination_path": path,
	})
	var result struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, "published", result.State, string(raw))
}
