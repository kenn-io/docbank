package store

import (
	"context"
	"errors"
)

const contractsStorageSchemaVersion = partiesStorageSchemaVersion

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
	return contractPartyMetadata.validateRows(ctx, q)
}
