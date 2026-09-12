// Package bundle defines the portable docbank-bundle-v1 archive contract.
package bundle

import (
	"encoding/json/jsontext"
	"errors"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/query"
)

const (
	Format                  = "docbank-bundle-v1"
	MaxMembers              = 100000
	ChunkMembers            = 1000
	MaxRoles                = 300000
	MaxRoleBytes      int64 = 50 << 30
	MaxMetadataBytes  int64 = 512 << 20
	MaxMemberBytes          = 64 << 10
	MaxDirectoryBytes int64 = 64 << 20
	MaxArchiveBytes   int64 = 52 << 30
	BufferSize              = 256 << 10
)

var (
	ErrConflict       = errors.New("export request or retained authority conflicts")
	ErrLimit          = errors.New("export exceeds its declared resource bounds")
	ErrExpired        = errors.New("export admission or retention expired")
	ErrRetained       = errors.New("export_retained: an export temporarily retains this content")
	ErrUnavailable    = errors.New("required export role is unavailable")
	ErrFenced         = errors.New("export worker claim is obsolete")
	ErrInvalidArchive = errors.New("invalid docbank bundle archive")
)

type Member struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Revision  int64  `json:"revision,omitzero"`
}

// SourceRequest selects exactly one source kind. Revision is a precondition
// only when the caller explicitly supplies it.
type SourceRequest struct {
	OperationID        string       `json:"operation_id"`
	Kind               string       `json:"kind"`
	Members            []Member     `json:"members,omitzero"`
	NodeIDs            []int64      `json:"node_ids,omitzero"`
	Query              *query.Query `json:"query,omitzero"`
	SavedQueryID       string       `json:"saved_query_id,omitzero"`
	SavedQueryRevision int64        `json:"saved_query_revision,omitzero"`
	SnapshotID         string       `json:"snapshot_id,omitzero"`
	MemberHash         string       `json:"member_hash,omitzero"`
	Total              int          `json:"total,omitzero"`
}

type Source struct {
	ID                 string `json:"id"`
	RequestSHA256      string `json:"request_sha256"`
	Kind               string `json:"kind"`
	State              string `json:"state"`
	MemberHash         string `json:"member_hash"`
	Total              int    `json:"total"`
	SourceBytes        int64  `json:"source_bytes"`
	CreatedAt          string `json:"created_at"`
	ExpiresAt          string `json:"expires_at"`
	SavedQueryID       string `json:"saved_query_id,omitzero"`
	SavedQueryRevision int64  `json:"saved_query_revision,omitzero"`
	QueryFingerprint   string `json:"query_fingerprint,omitzero"`
}

type RolePolicy struct {
	Role               string `json:"role"`
	AllowUnavailable   bool   `json:"allow_unavailable,omitzero"`
	ProfileFingerprint string `json:"profile_fingerprint,omitzero"`
	RecipeSHA256       string `json:"recipe_sha256,omitzero"`
}

type PlanRequest struct {
	OperationID string       `json:"operation_id"`
	SourceID    string       `json:"source_id"`
	MemberHash  string       `json:"member_hash"`
	Roles       []RolePolicy `json:"roles"`
}

type Role struct {
	Role      string                `json:"role"`
	Status    string                `json:"status"`
	Path      string                `json:"path,omitzero"`
	SHA256    string                `json:"sha256,omitzero"`
	Size      int64                 `json:"size"`
	MediaType string                `json:"media_type,omitzero"`
	Recipe    jsontext.Value        `json:"recipe,omitzero"`
	Page      *document.PageImageV1 `json:"page,omitzero"`
}

type Document struct {
	Member

	Name      string                 `json:"name"`
	Path      string                 `json:"path"`
	MediaType string                 `json:"media_type"`
	Roles     []Role                 `json:"roles"`
	Frames    []document.PageFrameV1 `json:"frames,omitzero"`
}

// Plan is the bounded header. Documents are streamed separately in identity
// order; Fingerprint covers the header with Fingerprint empty plus those rows.
type Plan struct {
	Format        string       `json:"format"`
	ID            string       `json:"id"`
	VaultID       string       `json:"vault_id"`
	Toolchain     string       `json:"toolchain"`
	Source        Source       `json:"source"`
	Roles         []RolePolicy `json:"roles"`
	Fingerprint   string       `json:"fingerprint"`
	Total         int          `json:"total"`
	RoleEntries   int          `json:"role_entries"`
	RoleBytes     int64        `json:"role_bytes"`
	MetadataBytes int64        `json:"metadata_bytes"`
	CreatedAt     string       `json:"created_at"`
	ExpiresAt     string       `json:"expires_at"`
}

type JobRequest struct {
	OperationID string `json:"operation_id"`
	PlanID      string `json:"plan_id"`
	Fingerprint string `json:"fingerprint"`
}

// PlanPreview is a bounded projection of frozen document receipts. It is not
// part of the plan fingerprint or the portable archive format.
type PlanPreview struct {
	PlanID      string        `json:"plan_id"`
	Fingerprint string        `json:"fingerprint"`
	MemberHash  string        `json:"member_hash"`
	Total       int           `json:"total"`
	Roles       []RoleSummary `json:"roles"`
}

type RoleSummary struct {
	Role               string `json:"role"`
	AvailableMembers   int    `json:"available_members"`
	UnavailableMembers int    `json:"unavailable_members"`
	Files              int    `json:"files"`
	Bytes              int64  `json:"bytes"`
	UnavailableReason  string `json:"unavailable_reason,omitzero"`
}

type Receipt struct {
	Format          string `json:"format"`
	PlanFingerprint string `json:"plan_fingerprint"`
	SHA256          string `json:"sha256"`
	Size            int64  `json:"size"`
	Entries         int    `json:"entries"`
}

type ExportJob struct {
	ID             string   `json:"id"`
	PlanID         string   `json:"plan_id"`
	Fingerprint    string   `json:"fingerprint"`
	State          string   `json:"state"`
	Sequence       int64    `json:"sequence"`
	CompletedRoles int      `json:"completed_roles"`
	CompletedBytes int64    `json:"completed_bytes"`
	Attempt        int64    `json:"attempt"`
	Failure        string   `json:"failure,omitzero"`
	CreatedAt      string   `json:"created_at"`
	Deadline       string   `json:"deadline"`
	ExpiresAt      string   `json:"expires_at"`
	Receipt        *Receipt `json:"receipt,omitzero"`
}

type Job = ExportJob
