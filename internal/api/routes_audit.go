package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/internal/store"
)

type auditPreviewOutput struct{ Body AuditEnrollmentPreview }
type auditStatusOutput struct{ Body AuditStatus }

func registerAuditRoutes(
	api huma.API, d Deps, g *gate, previews *auditPreviewRegistry,
) {
	huma.Register(api, huma.Operation{
		OperationID: "previewAuditEnrollment", Method: http.MethodPost,
		Path:    "/api/v1/audit/preview",
		Summary: "Preview the permanent first audit scope without changing the vault",
	}, func(ctx context.Context, in *struct {
		Body struct {
			Path       string `json:"path,omitempty"`
			NodeID     int64  `json:"node_id,omitempty" minimum:"1"`
			AgentLabel string `json:"agent_label,omitempty" maxLength:"200"`
		}
	}) (*auditPreviewOutput, error) {
		if (in.Body.Path == "") == (in.Body.NodeID == 0) {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"audit preview requires exactly one of path or node_id")
		}
		var label *string
		if in.Body.AgentLabel != "" {
			label = &in.Body.AgentLabel
		}
		var out *auditPreviewOutput
		err := g.mutate(func() error {
			var plan *store.AuditEnrollmentPlan
			var err error
			if in.Body.Path != "" {
				plan, err = d.Store.PreviewInitialAuditPath(ctx, in.Body.Path, "api", label)
			} else {
				plan, err = d.Store.PreviewInitialAudit(ctx, in.Body.NodeID, "api", label)
			}
			if err != nil {
				return FromStoreError(err)
			}
			token, expiresAt, err := previews.issue(plan)
			if err != nil {
				return NewError(http.StatusInternalServerError, "internal",
					fmt.Sprintf("issuing audit preview token: %v", err))
			}
			out = &auditPreviewOutput{Body: auditEnrollmentPreview(
				plan.Preview(), token, expiresAt,
			)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "enableAudit", Method: http.MethodPost, Path: "/api/v1/audit/enable",
		Summary: "Permanently enable the exact reviewed first audit scope",
		Description: "Consumes a one-use preview token. The acknowledgment explicitly " +
			"accepts permanent protected history plus names, topology, tags, and provenance " +
			"across the vault, including outside the selected scope; preview again after " +
			"any stale-token response.",
	}, func(ctx context.Context, in *struct {
		Body struct {
			PreviewToken                  string `json:"preview_token" minLength:"43" maxLength:"43"`
			AcknowledgePermanentRetention bool   `json:"acknowledge_permanent_retention"`
		}
	}) (*auditStatusOutput, error) {
		if !in.Body.AcknowledgePermanentRetention {
			return nil, NewError(http.StatusUnprocessableEntity,
				"audit_acknowledgment_required",
				"acknowledge permanent protected history and vault-wide metadata retention")
		}
		var out *auditStatusOutput
		err := g.mutate(func() error {
			plan, err := previews.take(in.Body.PreviewToken)
			if err != nil {
				return NewError(http.StatusConflict, "audit_preview_stale", err.Error())
			}
			status, err := d.Store.EnableInitialAudit(ctx, plan)
			if err != nil {
				return FromStoreError(err)
			}
			out = &auditStatusOutput{Body: auditStatus(status)}
			return nil
		})
		return out, err
	})

	huma.Register(api, huma.Operation{
		OperationID: "auditStatus", Method: http.MethodGet, Path: "/api/v1/audit/status",
		Summary: "Inspect audit authority and optional node protection",
	}, func(ctx context.Context, in *struct {
		Path   string `query:"path"`
		NodeID int64  `query:"node_id" minimum:"1"`
	}) (*auditStatusOutput, error) {
		if in.Path != "" && in.NodeID != 0 {
			return nil, NewError(http.StatusUnprocessableEntity, "validation",
				"audit status accepts at most one of path or node_id")
		}
		var status store.AuditStatus
		var err error
		switch {
		case in.Path != "":
			status, err = d.Store.AuditStatusPath(ctx, in.Path)
		case in.NodeID != 0:
			status, err = d.Store.AuditStatus(ctx, &in.NodeID)
		default:
			status, err = d.Store.AuditStatus(ctx, nil)
		}
		if err != nil {
			return nil, FromStoreError(err)
		}
		return &auditStatusOutput{Body: auditStatus(status)}, nil
	})
}

func auditEnrollmentPreview(
	preview store.AuditEnrollmentPreview, token string, expiresAt time.Time,
) AuditEnrollmentPreview {
	return AuditEnrollmentPreview{
		VaultID: preview.VaultID, ScopeID: preview.ScopeID,
		OperationID: preview.OperationID, TargetNodeID: preview.TargetNodeID,
		TargetPath: preview.TargetPath, BaselineDigest: preview.BaselineDigest,
		MemberCount: preview.MemberCount, FileCount: preview.FileCount,
		DirectoryCount: preview.DirectoryCount, VersionCount: preview.VersionCount,
		LogicalVersionBytes: preview.LogicalVersionBytes,
		UniqueBlobs:         preview.UniqueBlobs, UniqueBlobBytes: preview.UniqueBlobBytes,
		UnresolvedTrashOrigins: preview.UnresolvedTrashOrigins,
		VaultTopologyNodes:     preview.VaultTopologyNodes,
		VaultAttachmentRecords: preview.VaultAttachmentRecords,
		AuthorityJSONBytes:     preview.AuthorityJSONBytes,
		PreviewToken:           token, ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
	}
}

func auditStatus(status store.AuditStatus) AuditStatus {
	out := AuditStatus{
		Enabled: status.Enabled, VaultID: status.VaultID, LineageID: status.LineageID,
		OperationSequenceHighWater: status.OperationSequenceHighWater,
		AllocationEntryCount:       status.AllocationEntryCount,
		AllocationHead:             status.AllocationHead, Scopes: []AuditScopeStatus{},
	}
	for _, scope := range status.Scopes {
		out.Scopes = append(out.Scopes, AuditScopeStatus{
			ID: scope.ID, TargetNodeID: scope.TargetNodeID, TargetPath: scope.TargetPath,
			TargetTrashed: scope.TargetTrashed, EnableOperationID: scope.EnableOperationID,
			BaselineDigest: scope.BaselineDigest, MemberCount: scope.MemberCount,
			EntryCount: scope.EntryCount, ChainHead: scope.ChainHead,
		})
	}
	if status.Membership != nil {
		out.Membership = &AuditMembershipStatus{
			NodeID: status.Membership.NodeID, Path: status.Membership.Path,
			Trashed: status.Membership.Trashed, Protected: status.Membership.Protected,
			ScopeIDs:        status.Membership.ScopeIDs,
			BaselineDigests: status.Membership.BaselineDigests,
		}
	}
	return out
}
