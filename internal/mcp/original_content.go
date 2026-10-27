package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"maps"
	"uuid"

	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

const (
	maxOriginalBytes = 32 << 20
	maxOriginalChunk = 256 << 10
)

var originalContentTool = toolDefinition{
	name: "read_document_content", title: "Read original document bytes",
	description: "Read a base64 byte window from one selected immutable document version, up to 32 MiB. " +
		"Requires its vault, node, version, size and SHA-256 from list_document_versions. " +
		"Clients must verify the assembled bytes against the selected size and hash before use. " +
		"Does not create an export or write a local file.",
	schemas: originalContentSchemas,
}

type originalContentInput struct {
	VaultID          uuid.UUID `json:"vault_id"`
	NodeID           int64     `json:"node_id"`
	ContentVersionID uuid.UUID `json:"content_version_id"`
	BlobHash         string    `json:"blob_hash"`
	Size             int64     `json:"size"`
	Offset           int64     `json:"offset"`
	MaxBytes         int64     `json:"max_bytes"`
}

type originalContentOutput struct {
	privateCache

	VaultID          uuid.UUID `json:"vault_id"`
	NodeID           int64     `json:"node_id"`
	ContentVersionID uuid.UUID `json:"content_version_id"`
	BlobHash         string    `json:"blob_hash"`
	Size             int64     `json:"size"`
	Offset           int64     `json:"offset"`
	NextOffset       int64     `json:"next_offset"`
	EOF              bool      `json:"eof"`
	DataBase64       string    `json:"data_base64"`
}

func readDocumentContent(ctx context.Context, lease *daemonLease, raw []byte) (originalContentOutput, error) {
	var in originalContentInput
	if err := decodeReadArguments(raw, &in); err != nil {
		return originalContentOutput{}, err
	}
	if in.Offset > in.Size {
		return originalContentOutput{}, invalidToolArgumentsError()
	}
	if in.MaxBytes == 0 {
		in.MaxBytes = 64 << 10
	}
	return daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (originalContentOutput, error) {
		if err := in.checkSelection(ctx, c); err != nil {
			return originalContentOutput{}, err
		}
		stream, err := c.VersionContent(ctx, in.ContentVersionID.String())
		if err != nil {
			return originalContentOutput{}, err
		}
		data, err := in.readChunk(stream)
		if err != nil {
			return originalContentOutput{}, err
		}
		if err = in.checkSelection(ctx, c); err != nil {
			return originalContentOutput{}, err
		}
		next := in.Offset + int64(len(data))
		return originalContentOutput{privateCache: newPrivateCache(), VaultID: in.VaultID,
			NodeID: in.NodeID, ContentVersionID: in.ContentVersionID, BlobHash: in.BlobHash,
			Size: in.Size, Offset: in.Offset, NextOffset: next, EOF: next == in.Size,
			DataBase64: base64.StdEncoding.EncodeToString(data)}, nil
	})
}

func (in originalContentInput) checkSelection(ctx context.Context, c *daemonconn.Connection) error {
	info, err := c.API().VaultInfo(ctx)
	if err != nil {
		return err
	}
	if info.VaultID != in.VaultID.String() {
		return errors.New("original content vault does not match the selection")
	}
	node, err := c.API().GetNode(ctx, &apiclient.GetNodeRequestOptions{PathParams: &apiclient.GetNodePath{ID: in.NodeID}})
	if err != nil {
		return err
	}
	if node.ID != in.NodeID || node.Kind != "file" || node.TrashedAt != "" || node.Path == "" {
		return errors.New("original content node is unavailable")
	}
	version, err := c.API().GetContentVersion(ctx, &apiclient.GetContentVersionRequestOptions{
		PathParams: &apiclient.GetContentVersionPath{VersionID: in.ContentVersionID.String()},
	})
	if err != nil {
		return err
	}
	if version.ID != in.ContentVersionID.String() || version.NodeID != in.NodeID ||
		version.BlobHash != in.BlobHash || version.Size != in.Size {
		return errors.New("original content version does not match the selection")
	}
	return nil
}

func (in originalContentInput) readChunk(stream *daemonconn.ContentStream) (data []byte, err error) {
	defer func() { err = errors.Join(err, stream.Close()) }()
	if stream.BlobHash != in.BlobHash || stream.Size != in.Size {
		return nil, errors.New("original content stream does not match the selection")
	}
	// shortcut: The daemon serves whole originals, so each window reads the file.
	// Keep this bounded; add daemon range reads if large-file use warrants it.
	if _, err = io.CopyN(io.Discard, stream, in.Offset); err != nil {
		return nil, err
	}
	data = make([]byte, min(in.MaxBytes, in.Size-in.Offset))
	if _, err = io.ReadFull(stream, data); err != nil {
		return nil, err
	}
	// Consume the bounded remainder before closing so the ownership-proven
	// connection survives for the selection recheck and subsequent requests.
	if _, err = io.CopyN(io.Discard, stream, in.Size-in.Offset-int64(len(data))); err != nil {
		return nil, err
	}
	var extra [1]byte
	if _, err = io.ReadFull(stream, extra[:]); !errors.Is(err, io.EOF) {
		return nil, errors.New("original content stream exceeds the selected size")
	}
	return data, nil
}

func originalContentSchemas() (schema, schema) {
	const nodeField, sizeField = "node_id", "size"
	selection := schema{
		"vault_id": uuidSchema(), nodeField: integerSchema(1, 0),
		versionField: uuidSchema(), "blob_hash": sha256Schema(),
		sizeField: integerSchema(0, maxOriginalBytes), "offset": integerSchema(0, maxOriginalBytes),
	}
	inputProperties := maps.Clone(selection)
	inputProperties["max_bytes"] = integerSchema(1, maxOriginalChunk)
	input := rootObjectSchema(inputProperties, "vault_id", nodeField, versionField, "blob_hash", sizeField)
	selection["next_offset"] = integerSchema(0, maxOriginalBytes)
	selection["eof"] = booleanSchema()
	selection["data_base64"] = stringSchema(base64.StdEncoding.EncodedLen(maxOriginalChunk))
	return input, rootObjectSchema(withPrivateCache(selection), cacheRequired(
		"vault_id", nodeField, versionField, "blob_hash", sizeField, "offset", "next_offset", "eof", "data_base64")...)
}
