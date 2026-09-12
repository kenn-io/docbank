package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"go.kenn.io/docbank/internal/canonical"
	"regexp"
	"strings"
)

// EmailDocumentIdentity names immutable file bytes, independently of its head.
type EmailDocumentIdentity struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
}
type EmailDocumentReuse struct {
	PartPath string                `json:"part_path"`
	Child    EmailDocumentIdentity `json:"child"`
	Revision int64                 `json:"revision"`
}

// EmailDocumentPublicationRequest publishes the entire retained attachment
// inventory. Reuse is explicit; omitted occurrences get independent documents.
type EmailDocumentPublicationRequest struct {
	OperationID         string                `json:"operation_id"`
	Parent              EmailDocumentIdentity `json:"parent"`
	GenerationID        string                `json:"generation_id"`
	AttachmentID        string                `json:"attachment_id"`
	DestinationID       int64                 `json:"destination_id"`
	DestinationRevision int64                 `json:"destination_revision"`
	Reuse               []EmailDocumentReuse  `json:"reuse"`
}

// EmailDocumentRelation is an occurrence, not a content-deduplication group.
// Outcome is immutable MIME publication state; Processing is read separately.
type EmailDocumentRelation struct {
	OperationID  string                 `json:"operation_id"`
	Order        int                    `json:"order"`
	Parent       EmailDocumentIdentity  `json:"parent"`
	GenerationID string                 `json:"generation_id"`
	AttachmentID string                 `json:"attachment_id"`
	PartPath     string                 `json:"part_path"`
	SiblingOrder int                    `json:"sibling_order"`
	Filename     string                 `json:"filename"`
	Outcome      string                 `json:"outcome"`
	Child        *EmailDocumentIdentity `json:"child"`
}
type EmailDocumentPublicationReceipt struct {
	OperationID    string                  `json:"operation_id"`
	RequestDigest  string                  `json:"request_digest"`
	CreatedAt      string                  `json:"created_at"`
	InventoryState string                  `json:"inventory_state"`
	Relations      []EmailDocumentRelation `json:"relations"`
}
type EmailDocumentRelationStatus struct {
	Relation EmailDocumentRelation `json:"relation"`
	State    string                `json:"state"`
	Reason   string                `json:"reason"`
}

// EmailDocumentRelationQuery selects either incoming exact child relations or
// outgoing exact parent relations. Continuation is the last occurrence key.
type EmailDocumentRelationQuery struct {
	ParentVersionID  string `json:"parent_version_id"`
	ChildVersionID   string `json:"child_version_id"`
	AfterOperationID string `json:"after_operation_id"`
	AfterOrder       int    `json:"after_order"`
	Limit            int    `json:"limit"`
}
type EmailDocumentRelationPage struct {
	Items           []EmailDocumentRelationStatus `json:"items"`
	Total           int64                         `json:"total"`
	NextOperationID string                        `json:"next_operation_id"`
	NextOrder       int                           `json:"next_order"`
}

// EmailDocumentProcessingRequest submits one exact published occurrence to
// the ordinary rendition scheduler under an explicit profile and consent.
type EmailDocumentProcessingRequest struct {
	OperationID             string                       `json:"operation_id"`
	RequestDigest           string                       `json:"request_digest"`
	Order                   int                          `json:"order"`
	Profile                 ProcessingProfileV1          `json:"profile"`
	ExecutionIdentity       RenditionExecutionIdentityV1 `json:"execution_identity"`
	CapturedArtifactPolicy  jsontext.Value               `json:"captured_artifact_policy"`
	Principal               string                       `json:"principal"`
	Scope                   string                       `json:"scope"`
	InputClasses            []string                     `json:"input_classes"`
	RetainedArtifactClasses []string                     `json:"retained_artifact_classes"`
}
type EmailDocumentProcessingReceipt struct {
	JobID    string                `json:"job_id"`
	WaiterID string                `json:"waiter_id"`
	Child    EmailDocumentIdentity `json:"child"`
	State    string                `json:"state"`
}

const EmailDocumentMaxParts = 1000
const EmailDocumentMaxJSONBytes = 2 << 20
const emailDocumentMaxSafeInteger = 1<<53 - 1

var emailDocumentOperationPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var emailDocumentVersionPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidateEmailDocumentIdentity(v EmailDocumentIdentity) error {
	if v.NodeID < 1 || v.NodeID > emailDocumentMaxSafeInteger || !emailDocumentVersionPattern.MatchString(v.VersionID) || !emailDocumentHash(v.SHA256) || v.Size < 0 || v.Size > 128<<20 {
		return errors.New("invalid exact email document identity")
	}
	return nil
}
func emailDocumentHash(v string) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == 32 && strings.ToLower(v) == v
}
func ValidateEmailDocumentOperationID(v string) error {
	if !emailDocumentOperationPattern.MatchString(v) {
		return errors.New("invalid email publication operation ID")
	}
	return nil
}
func EmailDocumentRequestDigest(r EmailDocumentPublicationRequest) (string, error) {
	if err := ValidateEmailDocumentOperationID(r.OperationID); err != nil {
		return "", err
	}
	if err := ValidateEmailDocumentIdentity(r.Parent); err != nil {
		return "", err
	}
	if !emailDocumentHash(r.GenerationID) || !emailDocumentHash(r.AttachmentID) || r.DestinationID < 1 || r.DestinationID > emailDocumentMaxSafeInteger || r.DestinationRevision < 1 || r.DestinationRevision > emailDocumentMaxSafeInteger || len(r.Reuse) > EmailDocumentMaxParts {
		return "", errors.New("invalid email publication request")
	}
	seen := map[string]bool{}
	for _, v := range r.Reuse {
		if err := ValidateEmailPartPath(v.PartPath); err != nil {
			return "", err
		}
		if err := ValidateEmailDocumentIdentity(v.Child); err != nil {
			return "", err
		}
		if seen[v.PartPath] || v.Revision < 1 || v.Revision > emailDocumentMaxSafeInteger {
			return "", errors.New("invalid or repeated email reuse selection")
		}
		seen[v.PartPath] = true
	}
	b, err := canonical.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("docbank-email-document-publication/v1\x00"), b...))
	return hex.EncodeToString(sum[:]), nil
}
func NormalizeEmailDocumentRelationQuery(q EmailDocumentRelationQuery) (EmailDocumentRelationQuery, error) {
	if (q.ParentVersionID == "") == (q.ChildVersionID == "") {
		return q, errors.New("select exactly one parent or child version")
	}
	for _, id := range []string{q.ParentVersionID, q.ChildVersionID} {
		if id != "" && !emailDocumentVersionPattern.MatchString(id) {
			return q, errors.New("invalid relation version")
		}
	}
	if q.AfterOperationID != "" {
		if err := ValidateEmailDocumentOperationID(q.AfterOperationID); err != nil {
			return q, err
		}
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > 250 || q.AfterOrder < 0 || q.AfterOrder > EmailDocumentMaxParts {
		return q, errors.New("invalid relation page bounds")
	}
	return q, nil
}

// EmailAttachmentParts uses retained MIME structure. Bodies and multipart
// containers are excluded; explicit attachments and inline resources remain
// distinct. Descendants of an attached message are owned by that child email.
func EmailAttachmentParts(e EmailV1) []EmailPartV1 {
	parts := []EmailPartV1{}
	if e.Inventory == nil {
		return parts
	}
	bodies := map[string]bool{}
	for _, m := range e.Inventory.Messages {
		for _, a := range m.Alternatives {
			bodies[a.PartPath] = true
		}
	}
	for _, p := range e.Inventory.Parts {
		if p.Path == "1" || p.MessagePath != "1" {
			continue
		}
		media := ""
		if p.Media.Declared != nil {
			media = strings.ToLower(*p.Media.Declared)
		}
		if strings.HasPrefix(media, "multipart/") {
			continue
		}
		explicit := p.Disposition != nil && strings.EqualFold(*p.Disposition, "attachment")
		if explicit || !bodies[p.Path] {
			parts = append(parts, p)
		}
	}
	return parts
}

func EmailDocumentPartOutcome(p EmailPartV1) string {
	if p.Protection == EmailProtectionEncrypted {
		return "encrypted"
	}
	if p.DecodeState == EmailDecodeUnsupported {
		return "unsupported"
	}
	if p.DecodeState != EmailDecodeDecoded {
		return "failed"
	}
	if p.Payload == nil {
		return "unavailable"
	}
	return "decoded"
}
