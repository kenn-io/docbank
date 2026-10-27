package docbank

import (
	"context"
	"errors"
	"uuid"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	internalmcp "go.kenn.io/docbank/internal/mcp"
)

// ErrReadScope means the caller did not supply authority for this whole vault.
var ErrReadScope = errors.New("read scope does not bind this vault")

// CallReadTool executes get_vault_info, list_documents or search_documents on
// this vault using the native MCP semantics and input/output validation. It
// starts no listener, discovers no daemon and reads no owner credentials.
//
// vaultID must identify the entire vault admitted by the host for this call.
// A zero or mismatched ID fails before data access. This initial contract does
// not provide access to selected documents within a shared vault. The host
// authenticates and rechecks its current whole-vault grant on every call and
// holds its admission fence until this method returns, including cursor calls.
// Tool filters only narrow the selected vault; they never confer authority.
// Register the corresponding definition from go.kenn.io/docbank/mcp.ReadTool
// on the host's MCP server and call this method from its handler.
func (v *Vault) CallReadTool(ctx context.Context, vaultID uuid.UUID, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := v.begin(); err != nil {
		return nil, err
	}
	defer v.lifecycle.RUnlock()
	current, err := uuid.Parse(v.metadata.VaultID())
	if err != nil || vaultID == uuid.Nil() || vaultID != current {
		return nil, ErrReadScope
	}
	v.documentReadsOnce.Do(func() {
		v.documentReads = api.NewDocumentReader(v.metadata, v.blobs, v.vaultRoot, v.processing, v.missLog)
	})
	return internalmcp.CallDocumentReadTool(ctx, v.documentReads, req)
}
