package store

import (
	"context"
	"errors"
)

const contractsStorageSchemaVersion = partiesStorageSchemaVersion
const contractDocumentsStorageSchemaVersion = contractsStorageSchemaVersion

type metadataContract struct {
	Type          string  `json:"type"`
	ContractID    string  `json:"contract_id" db:"contract_id"`
	Title         string  `json:"title" db:"title"`
	ContractType  string  `json:"contract_type" db:"contract_type"`
	Status        string  `json:"status" db:"status"`
	StartsOn      *string `json:"starts_on" db:"starts_on"`
	ExpiresOn     *string `json:"expires_on" db:"expires_on"`
	SignedOn      *string `json:"signed_on" db:"signed_on"`
	Summary       string  `json:"summary" db:"summary"`
	Purpose       string  `json:"purpose" db:"purpose"`
	Consideration string  `json:"consideration" db:"consideration"`
	Termination   string  `json:"termination" db:"termination"`
	Revision      int64   `json:"revision" db:"revision"`
	CreatedAt     string  `json:"created_at" db:"created_at"`
	UpdatedAt     string  `json:"updated_at" db:"updated_at"`
}
type metadataContractParty struct {
	Type       string `json:"type"`
	ContractID string `json:"contract_id" db:"contract_id"`
	PartyID    string `json:"party_id" db:"party_id"`
	Role       string `json:"role" db:"role"`
}

var contractMetadata = newMetadataTable(metadataTable[metadataContract]{record: metadataContract{Type: "contract"}, table: "contracts", suffix: "ORDER BY contract_id", validate: validateMetadataContract, checkExport: true})
var contractPartyMetadata = newMetadataTable(metadataTable[metadataContractParty]{record: metadataContractParty{Type: "contract_party"}, table: "contract_parties", suffix: "ORDER BY contract_id,party_id,role", validate: validateMetadataContractParty, checkExport: true})

func validateMetadataContract(c metadataContract) error {
	if c.Type != "contract" || validateUUIDv4(c.ContractID) != nil || c.Revision < 1 {
		return errors.New("invalid contract metadata")
	}
	if err := validateContractInput(ContractInput{Title: c.Title, ContractType: c.ContractType, Status: c.Status, StartsOn: c.StartsOn, ExpiresOn: c.ExpiresOn, SignedOn: c.SignedOn, Summary: c.Summary, Purpose: c.Purpose, Consideration: c.Consideration, Termination: c.Termination}); err != nil {
		return err
	}
	if err := validateMetadataTime("contract created_at", c.CreatedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("contract updated_at", c.UpdatedAt); err != nil {
		return err
	}
	return nil
}
func validateMetadataContractParty(p metadataContractParty) error {
	if p.Type != "contract_party" || validateUUIDv4(p.ContractID) != nil || validateUUIDv4(p.PartyID) != nil || (p.Role != "own" && p.Role != "counterparty") {
		return errors.New("invalid contract party metadata")
	}
	return nil
}
func validateContractMetadataState(ctx context.Context, q metadataQuerier) error {
	if err := contractMetadata.validateRows(ctx, q); err != nil {
		return err
	}
	if err := contractPartyMetadata.validateRows(ctx, q); err != nil {
		return err
	}
	if err := contractDocumentMetadata.validateRows(ctx, q); err != nil {
		return err
	}
	var mismatch bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contract_documents cd JOIN content_versions v ON v.version_id=cd.content_version_id WHERE v.node_id<>cd.node_id OR v.blob_hash<>cd.blob_sha256 OR v.size<>cd.size)`).Scan(&mismatch); err != nil {
		return err
	}
	if mismatch {
		return errors.New("contract document does not match retained version")
	}
	return nil
}

type metadataContractDocument struct {
	Type             string `json:"type"`
	ContractID       string `json:"contract_id" db:"contract_id"`
	NodeID           int64  `json:"node_id" db:"node_id"`
	ContentVersionID string `json:"content_version_id" db:"content_version_id"`
	BlobSHA256       string `json:"blob_sha256" db:"blob_sha256"`
	Size             int64  `json:"size" db:"size"`
	Role             string `json:"role" db:"role"`
	AddedAt          string `json:"added_at" db:"added_at"`
}

var contractDocumentMetadata = newMetadataTable(metadataTable[metadataContractDocument]{record: metadataContractDocument{Type: "contract_document"}, table: "contract_documents", suffix: "ORDER BY contract_id,content_version_id,role", validate: validateMetadataContractDocument, checkExport: true})

func validateMetadataContractDocument(d metadataContractDocument) error {
	if d.Type != "contract_document" || validateUUIDv4(d.ContractID) != nil || d.NodeID < 1 || validateUUIDv4(d.ContentVersionID) != nil || validateCatalogSHA256(d.BlobSHA256, "contract document hash") != nil || d.Size < 0 || !validContractDocumentRole(d.Role) {
		return errors.New("invalid contract document metadata")
	}
	return validateMetadataTime("contract document added_at", d.AddedAt)
}
