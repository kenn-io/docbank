package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type PersonIdentityKind string
type PersonRole string
type PersonEvidenceKind string
type PersonBasis string
type PersonConfidence string

type NormalizedIdentity struct {
	Kind             PersonIdentityKind
	ValueNormalized  string
	ValueDisplay     string
	Normalization    string
	ScopeKind        string
	ScopeValue       string
	AutoLinkEligible bool
}

const (
	PersonContractV1                  = "document-people/v1"
	PersonNormalizationV1             = "person-identity-normalization/v1"
	PersonResolverDescriptor          = "document-people-resolver/v1;normalization=v1;evidence-pair;scoped-identities;consent-before-binding"
	MaxPersonDisplayNameBytes         = 200
	MaxPersonIdentityValueBytes       = 320
	MaxPersonIdentitiesPerPerson      = 200
	MaxPersonExternalIdentities       = 64
	MaxPersonEdgesPerVersion          = 4096
	MaxPersonAssertionNoteBytes       = 1000
	MaxCustodianSourceRefBytes        = 512
	MaxPersonRoleCountsBytes          = 4096
	MaxOpenPersonCandidates           = 10000
	MaxPersonCandidateEvidence        = 16384
	MaxPersonMergeMovedBytes          = 262144
	MaxPersonDirtyVersionsPerScan     = 10000
	MaxPersonArchiveIDBytes           = 128
	MaxPersonExternalUIDBytes         = 256
	MaxPersonDisplayNameSnapshotBytes = 200
	MaxPersonEvidenceKindBytes        = 64
	MaxPersonEvidenceIDBytes          = 256
)

func PersonResolverFingerprint() string {
	sum := sha256.Sum256([]byte(PersonResolverDescriptor))
	return hex.EncodeToString(sum[:])
}

func FoldPersonName(value string) string {
	return strings.Join(strings.Fields(cases.Fold().String(norm.NFKC.String(value))), " ")
}

func NormalizePersonIdentity(kind PersonIdentityKind, raw string) (NormalizedIdentity, error) {
	return NormalizeScopedPersonIdentity(kind, raw, "", "")
}

func NormalizeScopedPersonIdentity(kind PersonIdentityKind, raw, scopeKind, scopeValue string) (NormalizedIdentity, error) {
	bad := func() (NormalizedIdentity, error) {
		return NormalizedIdentity{}, errors.New("invalid person identity")
	}
	if !validPersonIdentityText(raw) || len(raw) > MaxPersonIdentityValueBytes || strings.TrimSpace(raw) == "" {
		return bad()
	}
	if (scopeKind == "") != (scopeValue == "") || (!validPersonIdentityText(scopeKind) && scopeKind != "") || (!validPersonIdentityText(scopeValue) && scopeValue != "") {
		return bad()
	}

	value := strings.TrimSpace(raw)
	out := NormalizedIdentity{
		Kind: kind, ValueDisplay: raw, ScopeKind: scopeKind, ScopeValue: scopeValue,
	}
	switch kind {
	case "email":
		address, err := mail.ParseAddress(value)
		if err != nil {
			return bad()
		}
		at := strings.LastIndexByte(address.Address, '@')
		if at <= 0 || at == len(address.Address)-1 {
			return bad()
		}
		local, domain := address.Address[:at], address.Address[at+1:]
		ascii, err := idna.Lookup.ToASCII(domain)
		if err != nil || ascii == "" {
			return bad()
		}
		out.ValueNormalized = local + "@" + strings.ToLower(ascii)
		out.Normalization = "email_v1"
		out.AutoLinkEligible = true
	case "phone":
		var digits strings.Builder
		for index, r := range value {
			switch {
			case r >= '0' && r <= '9':
				digits.WriteRune(r)
			case r == '+' && index == 0:
			case r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			default:
				return bad()
			}
		}
		if digits.Len() < 1 || digits.Len() > 15 {
			return bad()
		}
		out.ValueNormalized = digits.String()
		out.Normalization = "phone_digits"
		if strings.HasPrefix(value, "+") {
			if digits.Len() < 7 || out.ValueNormalized[0] == '0' {
				return bad()
			}
			out.ValueNormalized = "+" + out.ValueNormalized
			out.Normalization = "phone_e164"
			out.AutoLinkEligible = true
		}
	case "handle":
		service, handle, found := strings.Cut(value, "/")
		if !found || service == "" || handle == "" {
			return bad()
		}
		key, err := personTupleDigest([]string{strings.ToLower(service), scopeKind, scopeValue, handle})
		if err != nil {
			return NormalizedIdentity{}, err
		}
		out.ValueNormalized = key
		out.Normalization = "none"
		out.AutoLinkEligible = scopeKind != "" && scopeValue != ""
	case "name_alias":
		out.ValueNormalized = FoldPersonName(value)
		out.Normalization = "casefold"
	default:
		return bad()
	}
	if out.ValueNormalized == "" || len(out.ValueNormalized) > MaxPersonIdentityValueBytes {
		return bad()
	}
	return out, nil
}

func validPersonIdentityText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

func personTupleDigest(values []string) (string, error) {
	raw, err := canonical.Marshal(values)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ActorKey(identity NormalizedIdentity) (string, error) {
	key := string(identity.Kind) + ":" + identity.ValueNormalized
	if identity.ValueNormalized == "" || len(key) > MaxActorKeyBytes {
		return "", errors.New("invalid actor key")
	}
	switch identity.Kind {
	case "email", "phone", "handle", "name_alias":
		return key, nil
	default:
		return "", errors.New("unknown actor kind")
	}
}

func ExternalPersonActorKey(system, archiveID, uid string) (string, error) {
	if system != "msgvault" || archiveID == "" || uid == "" || len(archiveID) > MaxPersonArchiveIDBytes || len(uid) > MaxPersonExternalUIDBytes ||
		!validPersonIdentityText(archiveID) || !validPersonIdentityText(uid) {
		return "", errors.New("invalid external person tuple")
	}
	key, err := personTupleDigest([]string{system, archiveID, uid})
	return "external_uid:" + key, err
}

func PersonRoles() []PersonRole {
	return []PersonRole{"author", "last_saved_by", "custodian", "sender", "recipient", "copied", "blind_copy", "organizer", "attendee", "participant", "speaker"}
}

func PersonEvidenceKinds() []PersonEvidenceKind {
	return []PersonEvidenceKind{"source_metadata", "email_generation", "provenance_binding", "content_version", "package_row", "output_receipt", "transfer_record", "custodian_assignment", "operator_assertion"}
}

func PersonRoleGroup(role PersonRole) string {
	switch role {
	case "sender":
		return "sent"
	case "recipient", "copied", "blind_copy":
		return "received"
	case "author", "last_saved_by":
		return "authored"
	case "custodian":
		return "custodian_of"
	default:
		return "other"
	}
}
