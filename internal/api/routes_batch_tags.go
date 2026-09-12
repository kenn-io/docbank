package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/docbank/internal/store"
)

// BatchTagTarget binds an explicit selected identity to its observed revision.
type BatchTagTarget struct {
	NodeID   int64 `json:"node_id" minimum:"1"`
	Revision int64 `json:"revision" minimum:"1"`
}

type BatchTagRequest struct {
	OperationID string           `json:"operation_id" format:"uuid"`
	TagID       string           `json:"tag_id" format:"uuid"`
	Assign      bool             `json:"assign"`
	Nodes       []BatchTagTarget `json:"nodes" minItems:"1" maxItems:"1000"`
}

type BatchTagNodeResult struct {
	NodeID           int64 `json:"node_id" minimum:"1"`
	ExpectedRevision int64 `json:"expected_revision" minimum:"1"`
	Revision         int64 `json:"revision" minimum:"1"`
	Changed          bool  `json:"changed"`
}

// BatchTagReceipt describes the original completed operation, not current state.
type BatchTagReceipt struct {
	Version         int                  `json:"version"`
	OperationID     string               `json:"operation_id" format:"uuid"`
	RequestDigest   string               `json:"request_digest" pattern:"^[0-9a-f]{64}$"`
	TagID           string               `json:"tag_id" format:"uuid"`
	Assign          bool                 `json:"assign"`
	TagRevision     int64                `json:"tag_revision" minimum:"1"`
	AssignmentCount int                  `json:"assignment_count" minimum:"0"`
	CompletedAt     string               `json:"completed_at"`
	Nodes           []BatchTagNodeResult `json:"nodes" minItems:"1" maxItems:"1000"`
}

type BatchTagPreviewNode struct {
	NodeID   int64 `json:"node_id" minimum:"1"`
	Revision int64 `json:"revision" minimum:"1"`
	Assigned bool  `json:"assigned"`
}

type BatchTagPreview struct {
	TagID       string                `json:"tag_id" format:"uuid"`
	TagRevision int64                 `json:"tag_revision" minimum:"1"`
	Nodes       []BatchTagPreviewNode `json:"nodes" minItems:"1" maxItems:"1000"`
}

func registerBatchTagRoutes(api huma.API, d Deps, g *gate) {
	huma.Register(api, huma.Operation{
		OperationID: "changeBatchTags", Method: http.MethodPost, Path: "/api/v1/batch/tags",
		Summary: "Assign or remove one tag across an atomic selected set", MaxBodyBytes: 1 << 20,
	}, func(ctx context.Context, in *struct{ Body BatchTagRequest }) (*struct{ Body BatchTagReceipt }, error) {
		var receipt store.BatchTagReceiptV1
		err := g.mutate(func() error {
			var err error
			receipt, err = d.Store.BatchTags(ctx, store.BatchTagRequest{
				OperationID: in.Body.OperationID, TagID: in.Body.TagID, Assign: in.Body.Assign,
				Nodes: batchTagStoreTargets(in.Body.Nodes),
			})
			return FromStoreError(err)
		})
		if err != nil {
			return nil, err
		}
		out := BatchTagReceipt{
			Version: receipt.Version, OperationID: receipt.OperationID, RequestDigest: receipt.RequestDigest,
			TagID: receipt.TagID, Assign: receipt.Assign, TagRevision: receipt.TagRevision,
			AssignmentCount: receipt.AssignmentCount, CompletedAt: receipt.CompletedAt,
			Nodes: make([]BatchTagNodeResult, 0, len(receipt.Nodes)),
		}
		for _, node := range receipt.Nodes {
			out.Nodes = append(out.Nodes, BatchTagNodeResult{NodeID: node.NodeID,
				ExpectedRevision: node.ExpectedRevision, Revision: node.Revision, Changed: node.Changed})
		}
		return &struct{ Body BatchTagReceipt }{Body: out}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "previewBatchTags", Method: http.MethodPost, Path: "/api/v1/batch/tags/preview",
		Summary: "Observe exact tag membership for a revision-fenced selected set", MaxBodyBytes: 1 << 20,
	}, func(ctx context.Context, in *struct {
		Body struct {
			TagID string           `json:"tag_id" format:"uuid"`
			Nodes []BatchTagTarget `json:"nodes" minItems:"1" maxItems:"1000"`
		}
	}) (*struct{ Body BatchTagPreview }, error) {
		preview, err := d.Store.PreviewBatchTags(ctx, in.Body.TagID, batchTagStoreTargets(in.Body.Nodes))
		if err != nil {
			return nil, FromStoreError(err)
		}
		out := BatchTagPreview{TagID: preview.TagID, TagRevision: preview.TagRevision,
			Nodes: make([]BatchTagPreviewNode, 0, len(preview.Nodes))}
		for _, node := range preview.Nodes {
			out.Nodes = append(out.Nodes, BatchTagPreviewNode{NodeID: node.NodeID, Revision: node.Revision, Assigned: node.Assigned})
		}
		return &struct{ Body BatchTagPreview }{Body: out}, nil
	})
}

func batchTagStoreTargets(nodes []BatchTagTarget) []store.BatchTagTarget {
	targets := make([]store.BatchTagTarget, len(nodes))
	for i, node := range nodes {
		targets[i] = store.BatchTagTarget{NodeID: node.NodeID, Revision: node.Revision}
	}
	return targets
}
