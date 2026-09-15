package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/docbank/internal/canonical"
)

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

const personAxisLayout = "2006-01-02T15:04:05.000000000"

var personUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func validPersonAxis(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != len(personAxisLayout) {
		return false
	}
	parsed, err := time.Parse(personAxisLayout, value)
	return err == nil && parsed.Year() >= 1 && parsed.Year() <= 9999 && parsed.Format(personAxisLayout) == value
}

func personEdgeKey(edge DocumentPersonEdgeV1) string {
	return strings.Join([]string{edge.PersonID, edge.Role, edge.ActorKey, edge.EvidenceKind, edge.EvidenceID}, "\x00")
}

func encodeDocumentPeople(value DocumentPeopleV1) ([]byte, error) {
	if value.ContractVersion != PersonContractV1 || !personUUID.MatchString(value.ContentVersionID) || len(value.Edges) > MaxPersonEdgesPerVersion {
		return nil, errors.New("invalid people contract or person edge limit")
	}
	previous := ""
	for _, edge := range value.Edges {
		key := personEdgeKey(edge)
		if !personUUID.MatchString(edge.PersonID) ||
			!slices.Contains(PersonRoles(), PersonRole(edge.Role)) ||
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
	return canonical.Marshal(value)
}

func MarshalDocumentPeopleV1(value DocumentPeopleV1) ([]byte, string, error) {
	raw, err := encodeDocumentPeople(value)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

func DecodeDocumentPeopleV1(raw []byte) (DocumentPeopleV1, string, error) {
	if len(raw) > 8<<20 {
		return DocumentPeopleV1{}, "", errors.New("people encoded byte limit")
	}
	value, err := canonical.DecodeWith(raw, encodeDocumentPeople)
	if err != nil {
		return DocumentPeopleV1{}, "", err
	}
	sum := sha256.Sum256(raw)
	return value, hex.EncodeToString(sum[:]), nil
}
