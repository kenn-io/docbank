package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	errContractResultTooLarge = errors.New("contract result exceeds MCP response limit")
	errContractOutcomeUnknown = errors.New("contract write outcome unknown")
)

type contractListItem struct {
	ContractID      string  `json:"contract_id"`
	Title           string  `json:"title"`
	ContractType    string  `json:"contract_type"`
	Status          string  `json:"status"`
	EffectiveStatus string  `json:"effective_status"`
	StartsOn        *string `json:"starts_on"`
	ExpiresOn       *string `json:"expires_on"`
	SignedOn        *string `json:"signed_on"`
	Revision        int64   `json:"revision,string"`
}
type contractParty struct {
	PartyID     string  `json:"party_id"`
	Role        string  `json:"role"`
	DisplayName string  `json:"display_name"`
	Kind        string  `json:"kind"`
	Own         bool    `json:"own"`
	RetiredAt   *string `json:"retired_at"`
}
type contractDocument struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	BlobSHA256       string `json:"blob_sha256"`
	Size             int64  `json:"size"`
	Role             string `json:"role"`
	AddedAt          string `json:"added_at"`
	Current          bool   `json:"current"`
	Available        bool   `json:"available"`
}
type contractDetailToolOutput struct {
	privateCache
	contractListItem

	Summary       string             `json:"summary"`
	Purpose       string             `json:"purpose"`
	Consideration string             `json:"consideration"`
	Termination   string             `json:"termination"`
	CreatedAt     string             `json:"created_at"`
	UpdatedAt     string             `json:"updated_at"`
	Parties       []contractParty    `json:"parties"`
	Documents     []contractDocument `json:"documents"`
}
type listContractsToolOutput struct {
	privateCache

	Items  []contractListItem `json:"items"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
	AsOf   string             `json:"as_of"`
}
type contractReceipt struct {
	privateCache

	ContractID string `json:"contract_id"`
	Revision   int64  `json:"revision,string"`
}

func contractItem(c api.Contract) contractListItem {
	return contractListItem{c.ContractID, c.Title, c.ContractType, c.Status, c.EffectiveStatus, c.StartsOn, c.ExpiresOn, c.SignedOn, c.Revision}
}
func contractDetail(c api.Contract) contractDetailToolOutput {
	out := contractDetailToolOutput{privateCache: newPrivateCache(), contractListItem: contractItem(c), Summary: c.Summary, Purpose: c.Purpose, Consideration: c.Consideration, Termination: c.Termination, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Parties: []contractParty{}, Documents: []contractDocument{}}
	for _, p := range c.Parties {
		out.Parties = append(out.Parties, contractParty{p.PartyID, p.Role, p.DisplayName, p.Kind, p.Own, p.RetiredAt})
	}
	for _, d := range c.Documents {
		out.Documents = append(out.Documents, contractDocument{d.NodeID, d.ContentVersionID, d.BlobSHA256, d.Size, d.Role, d.AddedAt, d.Current, d.Available})
	}
	return out
}
func contractResultFits(output any) error {
	size, err := toolEnvelopeBytes(output, nil)
	if err != nil {
		return err
	}
	if size > maxToolResponseBytes-8<<10 {
		return errContractResultTooLarge
	}
	return nil
}
func listContracts(ctx context.Context, lease *daemonLease, raw []byte) (listContractsToolOutput, error) {
	var in struct {
		Status         string `json:"status"`
		ContractType   string `json:"contract_type"`
		PartyID        string `json:"party_id"`
		ExpiringBefore string `json:"expiring_before"`
		Q              string `json:"q"`
		AsOf           string `json:"as_of"`
		Limit          int    `json:"limit"`
		Offset         int    `json:"offset"`
	}
	if err := decodeReadArguments(raw, &in); err != nil {
		return listContractsToolOutput{}, err
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.ContractPage, error) {
		return c.Contracts(ctx, in.Status, in.ContractType, in.PartyID, in.ExpiringBefore, in.Q, in.AsOf, in.Limit, in.Offset)
	})
	if err != nil {
		return listContractsToolOutput{}, err
	}
	out := listContractsToolOutput{privateCache: newPrivateCache(), Items: []contractListItem{}, Limit: page.Limit, Offset: page.Offset, AsOf: page.AsOf}
	for _, c := range page.Items {
		out.Items = append(out.Items, contractItem(c))
	}
	return out, contractResultFits(out)
}
func getContract(ctx context.Context, lease *daemonLease, raw []byte) (contractDetailToolOutput, error) {
	var in struct {
		ContractID string `json:"contract_id"`
		AsOf       string `json:"as_of"`
	}
	if err := decodeReadArguments(raw, &in); err != nil {
		return contractDetailToolOutput{}, err
	}
	c, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.Contract, error) {
		return c.Contract(ctx, in.ContractID, in.AsOf)
	})
	if err != nil {
		return contractDetailToolOutput{}, err
	}
	out := contractDetail(c)
	return out, contractResultFits(out)
}
func contractWriteTool(name string) bool {
	return name == "create_contract" || name == "update_contract" || name == "link_contract_document"
}
func contractWriteToolHandler(lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		result, err := executeContractWriteTool(ctx, lease, name, validator, request.Params.Arguments)
		if err != nil {
			return readToolFailure(logger, name, err)
		}
		return result, nil
	}
}
func executeContractWriteTool(ctx context.Context, lease *daemonLease, name string, validator *jsonschema.Resolved, raw []byte) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	var c api.Contract
	var err error
	switch name {
	case "create_contract":
		var in api.CreateContractRequest
		if err = decodeReadArguments(raw, &in); err == nil {
			c, err = daemonProcessingStart(ctx, lease, func(conn *daemonconn.Connection) (api.Contract, error) { return conn.CreateContract(ctx, in) })
		}
	case "update_contract":
		var in struct {
			ContractID string         `json:"contract_id"`
			Revision   int64          `json:"revision,string"`
			Patch      jsontext.Value `json:"patch"`
		}
		if err = decodeReadArguments(raw, &in); err == nil {
			var patch api.PatchContractRequest
			err = json.Unmarshal(in.Patch, &patch)
			if err == nil {
				c, err = daemonProcessingStart(ctx, lease, func(conn *daemonconn.Connection) (api.Contract, error) {
					return conn.UpdateContract(ctx, in.ContractID, in.Revision, patch)
				})
			}
		}
	case "link_contract_document":
		var in struct {
			ContractID       string `json:"contract_id"`
			Revision         int64  `json:"revision,string"`
			NodeID           int64  `json:"node_id"`
			ContentVersionID string `json:"content_version_id"`
			Role             string `json:"role"`
		}
		if err = decodeReadArguments(raw, &in); err == nil {
			c, err = daemonProcessingStart(ctx, lease, func(conn *daemonconn.Connection) (api.Contract, error) {
				return conn.LinkContractDocument(ctx, in.ContractID, in.Revision, in.NodeID, in.Role, in.ContentVersionID)
			})
		}
	default:
		return nil, errors.New("unknown contract write tool")
	}
	if errors.Is(err, errProcessingOutcomeUnknown) {
		return nil, sanitizedDaemonError(errContractOutcomeUnknown, err)
	}
	if err != nil {
		return nil, err
	}
	result, err := boundedToolSuccess(validator, contractReceipt{newPrivateCache(), c.ContractID, c.Revision}, nil)
	if err != nil {
		return nil, sanitizedDaemonError(errContractOutcomeUnknown, fmt.Errorf("contract receipt failed: %w", err))
	}
	return result, nil
}
