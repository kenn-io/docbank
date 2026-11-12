package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

var ErrInvalidContract = errors.New("invalid contract")
var ErrPartyNotOwn = errors.New("party is not an own entity")

type ContractPartyInput struct{ PartyID, Role string }
type ContractParty struct {
	PartyID, Role, DisplayName, Kind string
	Own                              bool
	RetiredAt                        *string
}
type ContractInput struct {
	Title, ContractType, Status                  string
	StartsOn, ExpiresOn, SignedOn                *string
	Summary, Purpose, Consideration, Termination string
	Parties                                      []ContractPartyInput
}
type Contract struct {
	ContractID, Title, ContractType, Status, EffectiveStatus string
	StartsOn, ExpiresOn, SignedOn                            *string
	Summary, Purpose, Consideration, Termination             string
	Revision                                                 int64
	CreatedAt, UpdatedAt                                     string
	Parties                                                  []ContractParty
	Documents                                                []ContractDocument
}
type ContractListOptions struct {
	Status, ContractType, PartyID, ExpiringBefore, Q, AsOf string
	Limit, Offset                                          int
	NodeID                                                 int64
}

var contractTypePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func validContractDate(d string) bool {
	t, err := time.Parse(time.DateOnly, d)
	return err == nil && t.Format(time.DateOnly) == d
}
func validContractStatus(s string) bool {
	switch s {
	case "draft", "pending_signature", "active", "terminated", "cancelled":
		return true
	}
	return false
}
func validateContractInput(in ContractInput) error {
	if !document.ValidPersonIdentityText(in.Title) || strings.TrimSpace(in.Title) == "" || utf8.RuneCountInString(in.Title) > 300 {
		return fmt.Errorf("%w: title", ErrInvalidContract)
	}
	if in.ContractType != "" && !contractTypePattern.MatchString(in.ContractType) {
		return fmt.Errorf("%w: type", ErrInvalidContract)
	}
	if !validContractStatus(in.Status) {
		return fmt.Errorf("%w: status", ErrInvalidContract)
	}
	for _, d := range []*string{in.StartsOn, in.ExpiresOn, in.SignedOn} {
		if d != nil && !validContractDate(*d) {
			return fmt.Errorf("%w: date", ErrInvalidContract)
		}
	}
	if in.StartsOn != nil && in.ExpiresOn != nil && *in.ExpiresOn < *in.StartsOn {
		return fmt.Errorf("%w: date order", ErrInvalidContract)
	}
	for _, term := range []struct{ name, value string }{{"summary", in.Summary}, {"purpose", in.Purpose}, {"consideration", in.Consideration}, {"termination", in.Termination}} {
		if len(term.value) > 8192 || !utf8.ValidString(term.value) || strings.ContainsRune(term.value, 0) {
			return fmt.Errorf("%w: %s must be valid UTF-8 without NUL and at most 8192 bytes", ErrInvalidContract, term.name)
		}
	}
	return nil
}

const contractColumns = `contract_id,title,contract_type,status,starts_on,expires_on,signed_on,summary,purpose,consideration,termination,revision,created_at,updated_at`

func scanContract(row interface{ Scan(dest ...any) error }, asOf string) (Contract, error) {
	var c Contract
	err := row.Scan(&c.ContractID, &c.Title, &c.ContractType, &c.Status, &c.StartsOn, &c.ExpiresOn, &c.SignedOn, &c.Summary, &c.Purpose, &c.Consideration, &c.Termination, &c.Revision, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Contract{}, ErrNotFound
	}
	if err != nil {
		return Contract{}, err
	}
	c.EffectiveStatus = c.Status
	if c.Status == "active" && c.ExpiresOn != nil && *c.ExpiresOn < asOf {
		c.EffectiveStatus = "expired"
	}
	c.Parties = []ContractParty{}
	c.Documents = []ContractDocument{}
	return c, nil
}
func contractLinks(ctx context.Context, tx *sql.Tx, items []Contract) error {
	if len(items) == 0 {
		return nil
	}
	// A single page query keeps every projection in the same snapshot.
	ids := make(map[string]int, len(items))
	args := make([]any, len(items))
	placeholders := make([]string, len(items))
	for i := range items {
		ids[items[i].ContractID] = i
		args[i] = items[i].ContractID
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	rows, err := tx.QueryContext(ctx, `SELECT cp.contract_id,p.party_id,cp.role,p.display_name,p.kind,p.own,p.retired_at FROM contract_parties cp JOIN parties p ON p.party_id=cp.party_id WHERE cp.contract_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY cp.role,p.display_name_folded,p.party_id`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var p ContractParty
		var own int
		if err := rows.Scan(&id, &p.PartyID, &p.Role, &p.DisplayName, &p.Kind, &own, &p.RetiredAt); err != nil {
			return err
		}
		p.Own = own == 1
		if i, ok := ids[id]; ok {
			items[i].Parties = append(items[i].Parties, p)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT cd.contract_id,cd.node_id,cd.content_version_id,cd.blob_sha256,cd.size,cd.role,cd.added_at,
 CASE WHEN n.kind='file' AND n.trashed_at IS NULL AND v.node_id=cd.node_id AND v.blob_hash=cd.blob_sha256 AND v.size=cd.size THEN 1 ELSE 0 END,
 CASE WHEN n.kind='file' AND n.trashed_at IS NULL AND v.node_id=cd.node_id AND v.blob_hash=cd.blob_sha256 AND v.size=cd.size AND n.current_version_id=cd.content_version_id THEN 1 ELSE 0 END
 FROM contract_documents cd LEFT JOIN nodes n ON n.id=cd.node_id LEFT JOIN content_versions v ON v.version_id=cd.content_version_id
 WHERE cd.contract_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY cd.role,cd.node_id,cd.content_version_id`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d ContractDocument
		var available, current int
		if err := rows.Scan(&d.ContractID, &d.NodeID, &d.ContentVersionID, &d.BlobSHA256, &d.Size, &d.Role, &d.AddedAt, &available, &current); err != nil {
			return err
		}
		d.Available = available == 1
		d.Current = current == 1
		if i, ok := ids[d.ContractID]; ok {
			items[i].Documents = append(items[i].Documents, d)
		}
	}
	return rows.Err()
}
func getContractTx(ctx context.Context, tx *sql.Tx, id, asOf string) (Contract, error) {
	c, err := scanContract(tx.QueryRowContext(ctx, `SELECT `+contractColumns+` FROM contracts WHERE contract_id=$1`, id), asOf)
	if err != nil {
		return Contract{}, err
	}
	list := []Contract{c}
	if err = contractLinks(ctx, tx, list); err != nil {
		return Contract{}, err
	}
	return list[0], nil
}
func (s *Store) GetContract(ctx context.Context, id, asOf string) (Contract, error) {
	if !validContractDate(asOf) {
		return Contract{}, ErrInvalidContract
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if s.postgresSchema != "" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return Contract{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := getContractTx(ctx, tx, id, asOf)
	if err != nil {
		return Contract{}, err
	}
	return c, tx.Commit()
}
func admitContractParty(ctx context.Context, tx *sql.Tx, p ContractPartyInput) error {
	if p.Role != "own" && p.Role != "counterparty" {
		return ErrInvalidContract
	}
	party, err := getPartyQuery(ctx, tx, p.PartyID)
	if errors.Is(err, ErrNotFound) {
		return ErrInvalidContract
	}
	if err != nil {
		return err
	}
	if party.RetiredAt != nil {
		return ErrPartyRetired
	}
	if p.Role == "own" && !party.Own {
		return ErrPartyNotOwn
	}
	return nil
}
func (s *Store) CreateContract(ctx context.Context, in ContractInput) (Contract, error) {
	if in.Status == "" {
		in.Status = "draft"
	}
	if err := validateContractInput(in); err != nil {
		return Contract{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return Contract{}, err
	}
	now := nowRFC3339()
	asOf := time.Now().Format(time.DateOnly)
	var out Contract
	err = s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		seen := map[ContractPartyInput]bool{}
		for _, p := range in.Parties {
			if seen[p] {
				return ErrInvalidContract
			}
			seen[p] = true
			if err := admitContractParty(ctx, tx, p); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO contracts (`+contractColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, id, in.Title, in.ContractType, in.Status, in.StartsOn, in.ExpiresOn, in.SignedOn, in.Summary, in.Purpose, in.Consideration, in.Termination, 1, now, now)
		if err != nil {
			return err
		}
		for _, p := range in.Parties {
			if _, err = tx.ExecContext(ctx, `INSERT INTO contract_parties (contract_id,party_id,role) VALUES ($1,$2,$3)`, id, p.PartyID, p.Role); err != nil {
				return err
			}
		}
		out, err = getContractTx(ctx, tx, id, asOf)
		return err
	})
	return out, err
}
func sameContractDate(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}
func (s *Store) UpdateContract(ctx context.Context, id string, rev int64, in ContractInput) (Contract, error) {
	if err := validateContractInput(in); err != nil {
		return Contract{}, err
	}
	var out Contract
	asOf := time.Now().Format(time.DateOnly)
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		c, err := getContractTx(ctx, tx, id, asOf)
		if err != nil {
			return err
		}
		if c.Revision != rev {
			return ErrStaleRevision
		}
		if c.Title == in.Title && c.ContractType == in.ContractType && c.Status == in.Status && sameContractDate(c.StartsOn, in.StartsOn) && sameContractDate(c.ExpiresOn, in.ExpiresOn) && sameContractDate(c.SignedOn, in.SignedOn) && c.Summary == in.Summary && c.Purpose == in.Purpose && c.Consideration == in.Consideration && c.Termination == in.Termination {
			out = c
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE contracts SET title=$1,contract_type=$2,status=$3,starts_on=$4,expires_on=$5,signed_on=$6,summary=$7,purpose=$8,consideration=$9,termination=$10,revision=$11,updated_at=$12 WHERE contract_id=$13`, in.Title, in.ContractType, in.Status, in.StartsOn, in.ExpiresOn, in.SignedOn, in.Summary, in.Purpose, in.Consideration, in.Termination, rev+1, nowRFC3339(), id)
		if err != nil {
			return err
		}
		out, err = getContractTx(ctx, tx, id, asOf)
		return err
	})
	return out, err
}
func (s *Store) DeleteContract(ctx context.Context, id string, rev int64) error {
	return s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		c, err := getContractTx(ctx, tx, id, time.Now().Format(time.DateOnly))
		if err != nil {
			return err
		}
		if c.Revision != rev {
			return ErrStaleRevision
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM contracts WHERE contract_id=$1`, id)
		return err
	})
}
func (s *Store) ListContracts(ctx context.Context, o ContractListOptions) ([]Contract, error) {
	if !validContractDate(o.AsOf) || o.Limit < 1 || o.Limit > maxPageSize || o.Offset < 0 || o.NodeID < 0 || (o.Status != "" && o.Status != "expired" && !validContractStatus(o.Status)) || (o.ContractType != "" && !contractTypePattern.MatchString(o.ContractType)) || (o.PartyID != "" && validateUUIDv4(o.PartyID) != nil) || (o.ExpiringBefore != "" && !validContractDate(o.ExpiringBefore)) || !utf8.ValidString(o.Q) || strings.ContainsRune(o.Q, 0) {
		return nil, ErrInvalidContract
	}
	contains := `instr(lower(title),lower($9))>0`
	if s.postgresSchema != "" {
		contains = `strpos(lower(title),lower($9 COLLATE "C"))>0`
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if s.postgresSchema != "" {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT `+contractColumns+` FROM contracts WHERE ($1='' OR CASE WHEN status='active' AND expires_on IS NOT NULL AND expires_on<$2 THEN 'expired' ELSE status END=$3) AND ($4='' OR contract_type=$5) AND ($6='' OR EXISTS(SELECT 1 FROM contract_parties cp WHERE cp.contract_id=contracts.contract_id AND cp.party_id=$7)) AND ($8='' OR `+contains+`) AND ($10='' OR expires_on<$11) AND ($14=0 OR EXISTS(SELECT 1 FROM contract_documents cd WHERE cd.contract_id=contracts.contract_id AND cd.node_id=$15)) ORDER BY title,contract_id LIMIT $12 OFFSET $13`, o.Status, o.AsOf, o.Status, o.ContractType, o.ContractType, o.PartyID, o.PartyID, o.Q, o.Q, o.ExpiringBefore, o.ExpiringBefore, o.Limit, o.Offset, o.NodeID, o.NodeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []Contract{}
	for rows.Next() {
		c, e := scanContract(rows, o.AsOf)
		if e != nil {
			return nil, e
		}
		items = append(items, c)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = contractLinks(ctx, tx, items); err != nil {
		return nil, err
	}
	return items, tx.Commit()
}
func (s *Store) changeContractParty(ctx context.Context, id string, rev int64, partyID, role string, attach bool) (Contract, error) {
	if role != "own" && role != "counterparty" {
		return Contract{}, ErrInvalidContract
	}
	var out Contract
	asOf := time.Now().Format(time.DateOnly)
	err := s.withLogicalTx(ctx, func(tx *sql.Tx) error {
		c, err := getContractTx(ctx, tx, id, asOf)
		if err != nil {
			return err
		}
		if c.Revision != rev {
			return ErrStaleRevision
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contract_parties WHERE contract_id=$1 AND party_id=$2 AND role=$3)`, id, partyID, role).Scan(&exists); err != nil {
			return err
		}
		if attach && exists {
			out = c
			return nil
		}
		if !attach && !exists {
			return ErrNotFound
		}
		if attach {
			if err = admitContractParty(ctx, tx, ContractPartyInput{PartyID: partyID, Role: role}); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO contract_parties(contract_id,party_id,role) VALUES ($1,$2,$3)`, id, partyID, role)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM contract_parties WHERE contract_id=$1 AND party_id=$2 AND role=$3`, id, partyID, role)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE contracts SET revision=$1,updated_at=$2 WHERE contract_id=$3`, rev+1, nowRFC3339(), id)
		if err != nil {
			return err
		}
		out, err = getContractTx(ctx, tx, id, asOf)
		return err
	})
	return out, err
}
func (s *Store) AttachContractParty(ctx context.Context, id string, rev int64, partyID, role string) (Contract, error) {
	return s.changeContractParty(ctx, id, rev, partyID, role, true)
}
func (s *Store) DetachContractParty(ctx context.Context, id string, rev int64, partyID, role string) (Contract, error) {
	return s.changeContractParty(ctx, id, rev, partyID, role, false)
}
