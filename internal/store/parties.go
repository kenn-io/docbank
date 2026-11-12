package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

var ErrInvalidParty = errors.New("invalid party")
var ErrPartyRetired = errors.New("party is retired")

// PartyExistsError identifies the active party with an equivalent name.
type PartyExistsError struct{ PartyID string }

func (e *PartyExistsError) Error() string { return "party already exists: " + e.PartyID }

// Party is a reusable legal entity or individual.
type Party struct {
	PartyID, Kind, DisplayName, DisplayNameFolded, LegalName string
	Country, PersonID                                        *string
	Own                                                      bool
	Revision                                                 int64
	CreatedAt, UpdatedAt                                     string
	RetiredAt                                                *string
}

// PartyInput replaces the editable fields of a party.
type PartyInput struct {
	Kind, DisplayName, LegalName string
	Country, PersonID            *string
	Own                          bool
}

// PartyListOptions selects one bounded page.
type PartyListOptions struct {
	Kind           string
	Own            *bool
	IncludeRetired bool
	Limit, Offset  int
}

// ISO 3166-1 alpha-2 assigned codes, including official territories.
const partyCountries = " AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW "

func validatePartyInput(in PartyInput) error {
	if in.Kind != "person" && in.Kind != "organization" {
		return fmt.Errorf("%w: unknown kind", ErrInvalidParty)
	}
	if !utf8.ValidString(in.DisplayName) || strings.TrimSpace(in.DisplayName) == "" || utf8.RuneCountInString(in.DisplayName) > 200 || document.FoldPersonName(in.DisplayName) == "" {
		return fmt.Errorf("%w: display name", ErrInvalidParty)
	}
	if !utf8.ValidString(in.LegalName) || utf8.RuneCountInString(in.LegalName) > 300 {
		return fmt.Errorf("%w: legal name", ErrInvalidParty)
	}
	if in.Country != nil && (len(*in.Country) != 2 || !strings.Contains(partyCountries, " "+*in.Country+" ")) {
		return fmt.Errorf("%w: country", ErrInvalidParty)
	}
	if in.PersonID != nil && validateUUIDv4(*in.PersonID) != nil {
		return fmt.Errorf("%w: person ID", ErrInvalidParty)
	}
	return nil
}

const partyColumns = `party_id,kind,display_name,display_name_folded,legal_name,country,own,person_id,revision,created_at,updated_at,retired_at`

func scanParty(row interface{ Scan(...any) error }) (Party, error) {
	var p Party
	var own int
	err := row.Scan(&p.PartyID, &p.Kind, &p.DisplayName, &p.DisplayNameFolded, &p.LegalName, &p.Country, &own, &p.PersonID, &p.Revision, &p.CreatedAt, &p.UpdatedAt, &p.RetiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Party{}, ErrNotFound
	}
	if err != nil {
		return Party{}, err
	}
	p.Own = own == 1
	return p, nil
}
func partyOwn(v bool) int {
	if v {
		return 1
	}
	return 0
}
func getPartyQuery(ctx context.Context, q rowQuerier, id string) (Party, error) {
	if validateUUIDv4(id) != nil {
		return Party{}, ErrNotFound
	}
	return scanParty(q.QueryRowContext(ctx, `SELECT `+partyColumns+` FROM parties WHERE party_id=$1`, id))
}
func (s *Store) GetParty(ctx context.Context, id string) (Party, error) {
	return getPartyQuery(ctx, s.db, id)
}

func checkPartyReferences(ctx context.Context, tx *sql.Tx, input PartyInput, exclude string) error {
	if input.PersonID != nil {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE person_id=$1)`, *input.PersonID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: person does not exist", ErrInvalidParty)
		}
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT party_id FROM parties WHERE display_name_folded=$1 AND retired_at IS NULL AND party_id<>$2 ORDER BY party_id LIMIT 1`, document.FoldPersonName(input.DisplayName), exclude).Scan(&id)
	if err == nil {
		return &PartyExistsError{PartyID: id}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (s *Store) CreateParty(ctx context.Context, input PartyInput) (Party, error) {
	if err := validatePartyInput(input); err != nil {
		return Party{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return Party{}, err
	}
	now := nowRFC3339()
	p := Party{PartyID: id, Kind: input.Kind, DisplayName: input.DisplayName, DisplayNameFolded: document.FoldPersonName(input.DisplayName), LegalName: input.LegalName, Country: input.Country, PersonID: input.PersonID, Own: input.Own, Revision: 1, CreatedAt: now, UpdatedAt: now}
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		if err := checkPartyReferences(ctx, tx, input, ""); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO parties (`+partyColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, p.PartyID, p.Kind, p.DisplayName, p.DisplayNameFolded, p.LegalName, p.Country, partyOwn(p.Own), p.PersonID, p.Revision, p.CreatedAt, p.UpdatedAt, p.RetiredAt)
		return err
	})
	if err != nil {
		return Party{}, err
	}
	return p, nil
}

func (s *Store) ListParties(ctx context.Context, options PartyListOptions) ([]Party, error) {
	if options.Limit < 1 || options.Limit > maxPageSize || options.Offset < 0 || options.Kind != "" && options.Kind != "person" && options.Kind != "organization" {
		return nil, ErrInvalidParty
	}
	own := 0
	if options.Own != nil {
		own = partyOwn(*options.Own)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+partyColumns+` FROM parties WHERE ($1='' OR kind=$2) AND ($3 OR own=$4) AND ($5 OR retired_at IS NULL) ORDER BY display_name_folded,party_id LIMIT $6 OFFSET $7`, options.Kind, options.Kind, options.Own == nil, own, options.IncludeRetired, options.Limit, options.Offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []Party{}
	for rows.Next() {
		p, err := scanParty(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) UpdateParty(ctx context.Context, id string, revision int64, input PartyInput) (Party, error) {
	if err := validatePartyInput(input); err != nil {
		return Party{}, err
	}
	var updated Party
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		p, err := getPartyQuery(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Revision != revision {
			return ErrStaleRevision
		}
		if p.RetiredAt != nil {
			return ErrPartyRetired
		}
		if err := checkPartyReferences(ctx, tx, input, id); err != nil {
			return err
		}
		p.Kind = input.Kind
		p.DisplayName = input.DisplayName
		p.DisplayNameFolded = document.FoldPersonName(input.DisplayName)
		p.LegalName = input.LegalName
		p.Country = input.Country
		p.PersonID = input.PersonID
		p.Own = input.Own
		p.Revision++
		p.UpdatedAt = nowRFC3339()
		_, err = tx.ExecContext(ctx, `UPDATE parties SET kind=$1,display_name=$2,display_name_folded=$3,legal_name=$4,country=$5,own=$6,person_id=$7,revision=$8,updated_at=$9 WHERE party_id=$10`, p.Kind, p.DisplayName, p.DisplayNameFolded, p.LegalName, p.Country, partyOwn(p.Own), p.PersonID, p.Revision, p.UpdatedAt, p.PartyID)
		updated = p
		return err
	})
	if err != nil {
		return Party{}, err
	}
	return updated, nil
}
func (s *Store) RetireParty(ctx context.Context, id string, revision int64) (Party, error) {
	var updated Party
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		p, err := getPartyQuery(ctx, tx, id)
		if err != nil {
			return err
		}
		if p.Revision != revision {
			return ErrStaleRevision
		}
		if p.RetiredAt != nil {
			return ErrPartyRetired
		}
		now := nowRFC3339()
		p.RetiredAt = &now
		p.UpdatedAt = now
		p.Revision++
		_, err = tx.ExecContext(ctx, `UPDATE parties SET retired_at=$1,updated_at=$2,revision=$3 WHERE party_id=$4`, p.RetiredAt, p.UpdatedAt, p.Revision, p.PartyID)
		updated = p
		return err
	})
	if err != nil {
		return Party{}, err
	}
	return updated, nil
}
