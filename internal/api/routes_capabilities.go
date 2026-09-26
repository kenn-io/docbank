package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// RemoteAPIVersion is the remote-daemon contract understood by this client.
const RemoteAPIVersion = "v1"

// Capabilities is the credential-scoped daemon contract used during explicit
// remote selection. It intentionally excludes host paths and credential data.
type Capabilities struct {
	VaultUID   string           `json:"vault_uid" format:"uuid"`
	APIVersion string           `json:"api_version"`
	Operations []string         `json:"operations"`
	Limits     map[string]int64 `json:"limits"`
}

// RegisterCapabilitiesRoute installs the isolated capability route. The
// server constructor owns central registration so schema/client generation can
// remain one coordinated change.
func RegisterCapabilitiesRoute(api huma.API, d Deps) {
	type capabilitiesOutput struct{ Body Capabilities }
	huma.Register(api, huma.Operation{
		OperationID: "readCapabilities", Method: http.MethodGet,
		Path: "/api/v1/capabilities", Summary: "Negotiate remote daemon capabilities",
	}, func(ctx context.Context, _ *struct{}) (*capabilitiesOutput, error) {
		principal, ok := PrincipalFromContext(ctx)
		if !ok {
			return nil, operationHTTPError(ErrOperationDenied, false)
		}
		operations := []string{"admin"}
		if !principal.Local {
			operations = nil
			for _, operation := range []Operation{
				OperationRead, OperationContribute, OperationProcessing, OperationAnalyze,
				OperationMetadataMutation, OperationRelationshipReview, OperationExportShare,
				OperationFederation, OperationAdmin,
			} {
				if _, err := d.OperationPolicy.Authorize(ctx, OperationAuthorizationRequest{
					Principal: principal, Operation: operation,
				}); err == nil {
					operations = append(operations, string(operation))
				}
			}
		}
		return &capabilitiesOutput{Body: Capabilities{
			VaultUID: d.Store.VaultID(), APIVersion: RemoteAPIVersion,
			Operations: operations, Limits: map[string]int64{"max_source_ids": MaxOperationSourceIDs},
		}}, nil
	})
}
