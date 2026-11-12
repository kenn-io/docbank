package store

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/docbank/document"
)

type metadataParty struct {
	Type              string  `json:"type"`
	PartyID           string  `json:"party_id" db:"party_id"`
	Kind              string  `json:"kind" db:"kind"`
	DisplayName       string  `json:"display_name" db:"display_name"`
	DisplayNameFolded string  `json:"display_name_folded" db:"display_name_folded"`
	LegalName         string  `json:"legal_name" db:"legal_name"`
	Country           *string `json:"country" db:"country"`
	Own               int     `json:"own" db:"own"`
	PersonID          *string `json:"person_id" db:"person_id"`
	Revision          int64   `json:"revision" db:"revision"`
	CreatedAt         string  `json:"created_at" db:"created_at"`
	UpdatedAt         string  `json:"updated_at" db:"updated_at"`
	RetiredAt         *string `json:"retired_at" db:"retired_at"`
}

var partyMetadata = newMetadataTable(metadataTable[metadataParty]{
	record: metadataParty{Type: "party"}, table: "parties", suffix: "ORDER BY party_id", validate: validateMetadataParty,
})

func validateMetadataParty(p metadataParty) error {
	if p.Type != "party" || validateUUIDv4(p.PartyID) != nil || p.Revision < 1 || (p.Own != 0 && p.Own != 1) {
		return errors.New("invalid party metadata")
	}
	if err := validatePartyInput(PartyInput{Kind: p.Kind, DisplayName: p.DisplayName, LegalName: p.LegalName, Country: p.Country, PersonID: p.PersonID, Own: p.Own == 1}); err != nil {
		return err
	}
	if p.DisplayNameFolded != document.FoldPersonName(p.DisplayName) {
		return errors.New("invalid party folded name")
	}
	if err := validateMetadataTime("party created_at", p.CreatedAt); err != nil {
		return err
	}
	if err := validateMetadataTime("party updated_at", p.UpdatedAt); err != nil {
		return err
	}
	if p.RetiredAt != nil {
		if err := validateMetadataTime("party retired_at", *p.RetiredAt); err != nil {
			return err
		}
	}
	return nil
}

func validatePartyMetadataState(ctx context.Context, q metadataQuerier) error {
	var invalid bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM parties WHERE retired_at IS NULL GROUP BY display_name_folded HAVING COUNT(*)>1)`).Scan(&invalid); err != nil {
		return fmt.Errorf("checking party names: %w", err)
	}
	if invalid {
		return errors.New("duplicate active party name")
	}
	return partyMetadata.validateRows(ctx, q)
}
