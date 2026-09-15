package document

import (
	"errors"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

const (
	PersonContractV1              = "document-people/v1"
	MaxPersonEdgesPerVersion      = 4096
	maxDocumentPeopleEncodedBytes = 8 << 20
)

func PersonResolverFingerprint() string {
	return sha256Hex([]byte("document-people-resolver/v1;event-actor-evidence;person-identity-normalization=v1"))
}

type DocumentPersonEdgeV1 struct {
	PersonID     string `json:"person_id"`
	Role         string `json:"role"`
	ActorKey     string `json:"actor_key"`
	EvidenceKind string `json:"evidence_kind"`
	EvidenceID   string `json:"evidence_id"`
	Confidence   string `json:"confidence"`
	Basis        string `json:"basis"`
	RawLabel     string `json:"raw_label"`
	ClaimCount   int    `json:"claim_count"`
	FirstAxisKey string `json:"first_axis_key"`
	LastAxisKey  string `json:"last_axis_key"`
	Sensitive    bool   `json:"sensitive"`
}

type DocumentPeopleV1 struct {
	ContractVersion   string                 `json:"contract_version"`
	ContentVersionID  string                 `json:"content_version_id"`
	EventGenerationID string                 `json:"event_generation_id"`
	Edges             []DocumentPersonEdgeV1 `json:"edges"`
}

func encodeDocumentPeople(value DocumentPeopleV1) ([]byte, error) {
	if value.ContractVersion != PersonContractV1 || !emailUUIDv4Pattern.MatchString(value.ContentVersionID) || len(value.Edges) > MaxPersonEdgesPerVersion {
		return nil, errors.New("invalid people contract or person edge limit")
	}
	previous := ""
	for _, edge := range value.Edges {
		key := strings.Join([]string{edge.PersonID, edge.Role, edge.ActorKey, edge.EvidenceKind, edge.EvidenceID}, "\x00")
		if !emailUUIDv4Pattern.MatchString(edge.PersonID) || !ValidPersonRole(edge.Role) ||
			!slices.Contains(PersonEvidenceKinds(), PersonEvidenceKind(edge.EvidenceKind)) ||
			edge.EvidenceID == "" || edge.ClaimCount < 1 || key <= previous ||
			!validPersonAxis(edge.FirstAxisKey) || !validPersonAxis(edge.LastAxisKey) ||
			(edge.FirstAxisKey == "") != (edge.LastAxisKey == "") || edge.FirstAxisKey > edge.LastAxisKey ||
			len(edge.RawLabel) > MaxPersonDisplayNameBytes || len(edge.ActorKey) > MaxActorKeyBytes ||
			len(edge.EvidenceID) > MaxPersonEvidenceIDBytes ||
			!slices.Contains([]string{"identifier_match", "external_uid", "operator_assigned", "package_column", "transfer_participant", "transfer_record"}, edge.Basis) ||
			!slices.Contains([]string{"exact_identifier", "operator_asserted", "supplied_identity", "name_candidate"}, edge.Confidence) {
			return nil, errors.New("invalid or unordered person edge")
		}
		previous = key
	}
	raw, err := canonical.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxDocumentPeopleEncodedBytes {
		return nil, errors.New("people encoded byte limit")
	}
	return raw, nil
}

func validPersonAxis(value string) bool {
	if value == "" {
		return true
	}
	const layout = "2006-01-02T15:04:05.000000000"
	parsed, err := time.Parse(layout, value)
	return err == nil && parsed.Year() >= 1 && parsed.Year() <= 9999 && parsed.Format(layout) == value
}

func MarshalDocumentPeopleV1(value DocumentPeopleV1) ([]byte, string, error) {
	raw, err := encodeDocumentPeople(value)
	if err != nil {
		return nil, "", err
	}
	return raw, sha256Hex(raw), nil
}

func DecodeDocumentPeopleV1(raw []byte) (DocumentPeopleV1, string, error) {
	if len(raw) > maxDocumentPeopleEncodedBytes {
		return DocumentPeopleV1{}, "", errors.New("people encoded byte limit")
	}
	value, err := canonical.DecodeWith(raw, encodeDocumentPeople)
	if err != nil {
		return DocumentPeopleV1{}, "", err
	}
	return value, sha256Hex(raw), nil
}
