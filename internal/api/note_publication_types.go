package api

// PublishedNoteIdentity identifies an accepted original independently of its
// archive location. All spans referring to this original use UTF-8 bytes.
type PublishedNoteIdentity struct {
	VaultID           string `json:"vault_id"`
	Publication       string `json:"publication"`
	SourceRef         string `json:"source_ref" maxLength:"4096"`
	NodeID            int64  `json:"node_id" minimum:"1"`
	ContentVersionID  string `json:"content_version_id"`
	Hash              string `json:"hash" pattern:"^[0-9a-f]{64}$"`
	Size              int64  `json:"size" minimum:"0"`
	SourceRevision    int64  `json:"source_revision" minimum:"1"`
	NamespaceRevision int64  `json:"namespace_revision" minimum:"1"`
}

type NoteArchiveHead struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Hash             string `json:"hash"`
	Size             int64  `json:"size"`
}

type NotePublicationScan struct {
	ScanID            string `json:"scan_id"`
	DatabaseEpoch     string `json:"database_epoch"`
	PolicyFingerprint string `json:"policy_fingerprint"`
	Epoch             int64  `json:"epoch"`
	NamespaceRevision int64  `json:"namespace_revision"`
}

type NotePublicationBeginRequest struct {
	Name              string `json:"name,omitzero" maxLength:"64"`
	Destination       string `json:"destination" maxLength:"4096"`
	PolicyFingerprint string `json:"policy_fingerprint" pattern:"^[0-9a-f]{64}$"`
	OperationID       string `json:"operation_id" maxLength:"36"`
}

type NotePublicationSourcesRequest struct {
	Name   string `json:"name"`
	ScanID string `json:"scan_id"`
	After  string `json:"after,omitzero"`
	Limit  int    `json:"limit"`
}

type NotePublicationRecoveryGrant struct {
	SourceRef             string `json:"source_ref"`
	ExpectedHeadVersionID string `json:"expected_head_version_id"`
	SourceRevision        int64  `json:"source_revision"`
}

type NotePublicationSource struct {
	Identity     PublishedNoteIdentity         `json:"identity"`
	ArchiveHead  *NoteArchiveHead              `json:"archive_head,omitzero"`
	Presence     string                        `json:"presence"`
	Availability string                        `json:"availability"`
	Conflict     string                        `json:"conflict,omitzero"`
	Recovery     *NotePublicationRecoveryGrant `json:"recovery,omitzero"`
}

type NotePublicationSourcesPage struct {
	Scan  NotePublicationScan     `json:"scan"`
	Items []NotePublicationSource `json:"items"`
	Next  string                  `json:"next,omitzero"`
}

type NotePublicationSourceSnapshot struct {
	DatabaseEpoch string                `json:"database_epoch"`
	Source        NotePublicationSource `json:"source"`
}

// NotePublicationUploadRequest describes the multipart query and digest headers.
type NotePublicationUploadRequest struct {
	Name                   string `json:"name"`
	ScanID                 string `json:"scan_id"`
	SourceRef              string `json:"source_ref"`
	FileName               string `json:"file_name"`
	ParentPath             string `json:"parent_path"`
	ModifiedAt             string `json:"modified_at,omitzero"`
	OperationID            string `json:"operation_id"`
	Hash                   string `json:"hash"`
	Size                   int64  `json:"size"`
	ExpectedSourceRevision int64  `json:"expected_source_revision"`
}

type NotePublicationReceipt struct {
	Scan    NotePublicationScan   `json:"scan"`
	Source  NotePublicationSource `json:"source"`
	Outcome string                `json:"outcome"`
}

type NotePublicationSeenRequest struct {
	Name     string                  `json:"name,omitzero" maxLength:"64"`
	ScanID   string                  `json:"scan_id,omitzero" maxLength:"36"`
	Bindings []PublishedNoteIdentity `json:"bindings" maxItems:"200"`
}

type NotePublicationAck struct {
	Scan    NotePublicationScan     `json:"scan"`
	Sources []PublishedNoteIdentity `json:"sources"`
}

type NotePublicationCompleteRequest struct {
	Name              string `json:"name,omitzero" maxLength:"64"`
	ScanID            string `json:"scan_id,omitzero" maxLength:"36"`
	PolicyFingerprint string `json:"policy_fingerprint" pattern:"^[0-9a-f]{64}$"`
	InventoryComplete bool   `json:"inventory_complete"`
}

type NotePublicationStatus struct {
	Name              string               `json:"name"`
	DatabaseEpoch     string               `json:"database_epoch"`
	LastCompleteAt    string               `json:"last_complete_at"`
	LastAcceptedAt    string               `json:"last_accepted_at"`
	NamespaceRevision int64                `json:"namespace_revision"`
	ActiveScan        *NotePublicationScan `json:"active_scan,omitzero"`
	Incomplete        bool                 `json:"incomplete"`
	Present           int64                `json:"present"`
	Absent            int64                `json:"absent"`
	Unknown           int64                `json:"unknown"`
	Conflicts         int64                `json:"conflicts"`
	Unavailable       int64                `json:"unavailable"`
	RecoveryPending   int64                `json:"recovery_pending"`
}

type NotePublicationAcceptLocalRequest struct {
	Name                   string `json:"name,omitzero" maxLength:"64"`
	DatabaseEpoch          string `json:"database_epoch" maxLength:"36"`
	SourceRef              string `json:"source_ref" maxLength:"4096"`
	ExpectedHeadVersionID  string `json:"expected_head_version_id" maxLength:"36"`
	OperationID            string `json:"operation_id" maxLength:"36"`
	ExpectedSourceRevision int64  `json:"expected_source_revision" minimum:"1"`
}
