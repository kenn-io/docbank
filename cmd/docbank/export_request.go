package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/query"
)

type exportPreviewRequest struct {
	PhotoRender       *bundle.PhotoRenderProfile   `json:"photo_render,omitzero"`
	Photos            *bundle.PhotoExportSelection `json:"photos,omitzero"`
	SourceOperationID string                       `json:"source_operation_id"`
	PlanOperationID   string                       `json:"plan_operation_id"`
	Members           []bundle.Member              `json:"members"`
}

func validateExportID(field, value string) error {
	if !daemonconn.IsCanonicalUUIDv4(value) {
		return usageError(fmt.Errorf("%s must be a canonical UUIDv4", field))
	}
	return nil
}

func readExportRequest(path string) (exportPreviewRequest, error) {
	var request exportPreviewRequest
	if path == "" {
		return request, usageError(errors.New("export preview requires --request"))
	}
	f, err := os.Open(path)
	if err != nil {
		return request, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return request, err
	}
	if len(raw) > 1<<20 {
		return request, usageError(errors.New("request JSON exceeds 1 MiB"))
	}
	if err := json.Unmarshal(raw, &request, json.RejectUnknownMembers(true)); err != nil {
		return request, usageError(fmt.Errorf("invalid export request JSON: %w", err))
	}
	return request, validateExportRequest(request)
}

func validateExportRequest(request exportPreviewRequest) error {
	if err := validateExportID("source_operation_id", request.SourceOperationID); err != nil {
		return err
	}
	if err := validateExportID("plan_operation_id", request.PlanOperationID); err != nil {
		return err
	}
	if request.PhotoRender != nil {
		if err := request.PhotoRender.Validate(); err != nil {
			return usageError(err)
		}
	}
	if request.Photos != nil {
		if len(request.Members) != 0 || request.PhotoRender == nil {
			return usageError(errors.New("photos requires photo_render and excludes members"))
		}
		if _, err := query.Canonical(request.Photos.Query); err != nil {
			return usageError(err)
		}
		if len(request.Photos.AssetIDs) > bundle.MaxMembers {
			return usageError(bundle.ErrLimit)
		}
		for _, id := range request.Photos.AssetIDs {
			if err := validateExportID("photo asset ID", id); err != nil {
				return err
			}
		}
		return nil
	}
	if len(request.Members) == 0 || len(request.Members) > bundle.ChunkMembers {
		return usageError(errors.New("members must contain 1 to 1,000 document versions"))
	}
	seen := make(map[struct {
		node    int64
		version string
	}]bool, len(request.Members))
	var total int64
	for i, member := range request.Members {
		field := fmt.Sprintf("members[%d]", i)
		if member.NodeID < 1 {
			return usageError(fmt.Errorf("%s.node_id must be positive", field))
		}
		if err := validateExportID(field+".version_id", member.VersionID); err != nil {
			return err
		}
		if !canonical.IsSHA256Hex(member.SHA256) {
			return usageError(fmt.Errorf(
				"%s.sha256 must be 64 lowercase hexadecimal characters", field))
		}
		if member.Size < 0 {
			return usageError(fmt.Errorf("%s.size must be nonnegative", field))
		}
		if member.Revision < 0 {
			return usageError(fmt.Errorf("%s.revision must be nonnegative", field))
		}
		if member.Size > bundle.MaxRoleBytes-total {
			return usageError(fmt.Errorf(
				"%s.size exceeds the 50 GiB combined original bytes limit", field))
		}
		total += member.Size
		key := struct {
			node    int64
			version string
		}{member.NodeID, member.VersionID}
		if seen[key] {
			return usageError(fmt.Errorf("%s duplicates a node/version pair", field))
		}
		seen[key] = true
	}
	return nil
}
