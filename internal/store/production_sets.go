package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
)

var (
	// ErrInvalidProduction means production-set input or retained authority is malformed.
	ErrInvalidProduction = errors.New("invalid production-set authority")
	// ErrProductionOperationConflict means an operation ID names different canonical input.
	ErrProductionOperationConflict = errors.New("production operation conflicts with existing receipt")
	// ErrProductionRevisionConflict means a draft ETag or mutable-state fence is stale.
	ErrProductionRevisionConflict = errors.New("production revision conflict")
)

type productionCreateReceiptV1 struct {
	Version int               `json:"version"`
	Set     redaction.Set     `json:"set"`
	Draft   redaction.Draft   `json:"draft"`
	Receipt redaction.Receipt `json:"receipt"`
}

type productionCatalogSelection struct {
	recipeID, recipeSHA256                       string
	profileID, profileSHA256                     string
	disclosureProfileID, disclosureProfileSHA256 string
	numberingRecipeID, numberingRecipeSHA256     string
	recipeJSON, profileJSON                      []byte
	disclosureProfileJSON, numberingRecipeJSON   []byte
}

type productionDisclosureProfileV1 struct {
	Contract            string `json:"contract"`
	ID                  string `json:"id"`
	GeneratedFieldsOnly bool   `json:"generated_fields_only"`
}

type productionNumberingRecipeV1 struct {
	Contract string `json:"contract"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
}

// CreateProductionSet creates revision one and records its replay receipt in
// the same transaction. Actor is trusted server context and is deliberately
// absent from the public request DTO.
func (s *Store) CreateProductionSet(ctx context.Context, actor string, request redaction.CreateRequest) (redaction.Set, redaction.Draft, error) {
	if err := redaction.ValidateCreateRequest(request); err != nil || !validProductionActor(actor) {
		return redaction.Set{}, redaction.Draft{}, errors.Join(ErrInvalidProduction, err)
	}
	requestJSON, err := canonicalProductionCreateRequest(request)
	if err != nil || len(requestJSON) > redaction.MaxCommandBytes {
		return redaction.Set{}, redaction.Draft{}, errors.Join(ErrInvalidProduction, err)
	}
	requestSHA256 := productionSHA256(requestJSON)
	var set redaction.Set
	var draft redaction.Draft
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		storedSet, storedDraft, found, loadErr := loadProductionCreateOperationTx(ctx, tx, request, requestSHA256)
		if loadErr != nil {
			return loadErr
		}
		if found {
			set, draft = storedSet, storedDraft
			return nil
		}
		selection, err := resolveProductionCatalog(request.RecipeID, request.ProfileID, request.DisclosureProfileID, request.NumberingRecipeID)
		if err != nil {
			return err
		}
		policy, policySelection, err := resolveProductionCreatePolicyTx(ctx, tx, request)
		if err != nil {
			return err
		}
		setID, err := newUUIDv4()
		if err != nil {
			return err
		}
		createdAt := nowRFC3339()
		emptyMemberHash, err := productionMemberHash(nil)
		if err != nil {
			return err
		}
		emptyDecisionsSHA256, err := productionDecisionHash(nil)
		if err != nil {
			return err
		}
		set = redaction.Set{ID: setID, Name: request.Name, Creator: actor, CreatedAt: createdAt, HeadRevision: 1}
		draft = redaction.Draft{SetID: setID, Revision: 1, ETag: 1,
			InstructionsSHA256: productionSHA256([]byte(request.Instructions)), MemberHash: emptyMemberHash, DecisionsSHA256: emptyDecisionsSHA256,
			RecipeID: selection.recipeID, RecipeSHA256: selection.recipeSHA256,
			ProfileID: selection.profileID, ProfileSHA256: selection.profileSHA256,
			DisclosureProfileID: selection.disclosureProfileID, DisclosureProfileSHA256: selection.disclosureProfileSHA256,
			NumberingRecipeID: selection.numberingRecipeID, NumberingRecipeSHA256: selection.numberingRecipeSHA256,
			Policy: policySelection, State: "draft"}
		receipt := redaction.Receipt{OperationID: request.OperationID, SetID: setID, Revision: 1, ETag: 1, RequestSHA256: requestSHA256}
		if err := ensureProductionCatalogTx(ctx, tx, selection); err != nil {
			return err
		}
		if policy.ID != draft.Policy.PolicyID || policy.Version != draft.Policy.Version || policy.SHA256 != draft.Policy.PolicySHA256 {
			return ErrInvalidProduction
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_sets(id,name,creator,head_revision,created_at) VALUES(?,?,?,?,?)`, set.ID, set.Name, set.Creator, set.HeadRevision, set.CreatedAt); err != nil {
			return fmt.Errorf("creating production set: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_revisions(
			set_id,revision,predecessor_revision,state,etag,instructions,instructions_sha256,member_hash,decisions_sha256,membership_sealed,
			recipe_kind,recipe_id,recipe_sha256,profile_kind,profile_id,profile_sha256,
			disclosure_profile_kind,disclosure_profile_id,disclosure_profile_sha256,
			numbering_recipe_kind,numbering_recipe_id,numbering_recipe_sha256,
			policy_id,policy_version,policy_sha256,created_at) VALUES(?,?,NULL,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			draft.SetID, draft.Revision, draft.State, draft.ETag, []byte(request.Instructions), draft.InstructionsSHA256,
			draft.MemberHash, draft.DecisionsSHA256, draft.MembershipSealed,
			"recipe", draft.RecipeID, draft.RecipeSHA256, "profile", draft.ProfileID, draft.ProfileSHA256,
			"disclosure", draft.DisclosureProfileID, draft.DisclosureProfileSHA256,
			"numbering", draft.NumberingRecipeID, draft.NumberingRecipeSHA256,
			draft.Policy.PolicyID, draft.Policy.Version, draft.Policy.PolicySHA256, createdAt); err != nil {
			return fmt.Errorf("creating production revision: %w", err)
		}
		value := productionCreateReceiptV1{Version: 1, Set: set, Draft: draft, Receipt: receipt}
		receiptJSON, err := canonical.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_operations(operation_id,set_id,actor,kind,request_sha256,receipt_json,created_at) VALUES(?,?,?,?,?,?,?)`, request.OperationID, set.ID, actor, "create", requestSHA256, receiptJSON, createdAt); err != nil {
			return fmt.Errorf("recording production operation: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_audit_evidence(operation_id,set_id,revision,actor,kind,request_sha256,receipt_sha256,created_at) VALUES(?,?,?,?,?,?,?,?)`, request.OperationID, set.ID, 1, actor, "create", requestSHA256, productionSHA256(receiptJSON), createdAt); err != nil {
			return fmt.Errorf("recording production audit evidence: %w", err)
		}
		return nil
	})
	if err != nil {
		return redaction.Set{}, redaction.Draft{}, err
	}
	return set, draft, nil
}

func (s *Store) ProductionSet(ctx context.Context, setID string) (redaction.Set, error) {
	if validateUUIDv4(setID) != nil {
		return redaction.Set{}, ErrNotFound
	}
	return scanProductionSet(s.db.QueryRowContext(ctx, `SELECT id,name,creator,created_at,head_revision FROM production_sets WHERE id=?`, setID))
}

func (s *Store) ProductionDraft(ctx context.Context, setID string, revision int64) (redaction.Draft, error) {
	if validateUUIDv4(setID) != nil || revision < 1 {
		return redaction.Draft{}, ErrNotFound
	}
	return scanProductionDraft(s.db.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision))
}

const productionDraftSelect = `SELECT set_id,revision,etag,instructions_sha256,member_hash,decisions_sha256,recipe_id,recipe_sha256,
	profile_id,profile_sha256,disclosure_profile_id,disclosure_profile_sha256,numbering_recipe_id,numbering_recipe_sha256,
	policy_id,policy_version,policy_sha256,state,membership_sealed
	FROM production_revisions`

func scanProductionSet(row interface{ Scan(dest ...any) error }) (redaction.Set, error) {
	var value redaction.Set
	if err := row.Scan(&value.ID, &value.Name, &value.Creator, &value.CreatedAt, &value.HeadRevision); errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	} else if err != nil {
		return value, err
	}
	if redaction.ValidateSet(value) != nil {
		return redaction.Set{}, ErrInvalidProduction
	}
	return value, nil
}

func scanProductionDraft(row interface{ Scan(dest ...any) error }) (redaction.Draft, error) {
	var value redaction.Draft
	if err := row.Scan(&value.SetID, &value.Revision, &value.ETag, &value.InstructionsSHA256, &value.MemberHash, &value.DecisionsSHA256,
		&value.RecipeID, &value.RecipeSHA256, &value.ProfileID, &value.ProfileSHA256,
		&value.DisclosureProfileID, &value.DisclosureProfileSHA256, &value.NumberingRecipeID,
		&value.NumberingRecipeSHA256, &value.Policy.PolicyID, &value.Policy.Version, &value.Policy.PolicySHA256,
		&value.State, &value.MembershipSealed); errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	} else if err != nil {
		return value, err
	}
	if redaction.ValidateDraft(value) != nil {
		return redaction.Draft{}, ErrInvalidProduction
	}
	return value, nil
}

func resolveProductionCreatePolicyTx(
	ctx context.Context, tx *sql.Tx, request redaction.CreateRequest,
) (documentproduction.PolicyVersion, redaction.PolicySelection, error) {
	if request.PolicyID == "" {
		policy, err := documentproduction.GenericPolicyVersion()
		if err != nil {
			return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, errors.Join(ErrInvalidProduction, err)
		}
		encoded, digest, err := documentproduction.CanonicalPolicyVersion(policy)
		if err != nil || digest != policy.SHA256 {
			return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, errors.Join(ErrInvalidProduction, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_policy_versions(policy_id,version,sha256,canonical_json)
			VALUES(?,?,?,?) ON CONFLICT(policy_id,version) DO NOTHING`, policy.ID, policy.Version, digest, encoded); err != nil {
			return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, err
		}
	}

	policyID, policyVersion := request.PolicyID, request.PolicyVersion
	if policyID == "" {
		policyID, policyVersion = documentproduction.GenericPolicyID, documentproduction.GenericPolicyVersionNumber
	}
	return loadProductionPolicySelectionTx(ctx, tx, policyID, policyVersion)
}

func loadProductionPolicySelectionTx(
	ctx context.Context, tx *sql.Tx, policyID string, policyVersion int64,
) (documentproduction.PolicyVersion, redaction.PolicySelection, error) {
	var raw []byte
	var digest string
	if err := tx.QueryRowContext(ctx, `SELECT canonical_json,sha256 FROM production_policy_versions
		WHERE policy_id=? AND version=?`, policyID, policyVersion).Scan(&raw, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, ErrNotFound
		}
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, err
	}
	policy, err := decodeProductionPolicy(raw, digest)
	if err != nil || policy.ID != policyID || policy.Version != policyVersion {
		return documentproduction.PolicyVersion{}, redaction.PolicySelection{}, errors.Join(ErrInvalidProduction, err)
	}
	selection := redaction.PolicySelection{PolicyID: policy.ID, Version: policy.Version, PolicySHA256: policy.SHA256}
	return policy, selection, nil
}

func loadProductionCreateOperationTx(ctx context.Context, tx *sql.Tx, request redaction.CreateRequest, requestSHA256 string) (redaction.Set, redaction.Draft, bool, error) {
	var storedSetID, actor, kind, storedSHA256 string
	var receiptJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT set_id,actor,kind,request_sha256,receipt_json FROM production_operations WHERE operation_id=?`, request.OperationID).Scan(&storedSetID, &actor, &kind, &storedSHA256, &receiptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return redaction.Set{}, redaction.Draft{}, false, nil
	}
	if err != nil {
		return redaction.Set{}, redaction.Draft{}, false, err
	}
	if kind != "create" || storedSHA256 != requestSHA256 {
		return redaction.Set{}, redaction.Draft{}, false, ErrProductionOperationConflict
	}
	value, decodeErr := canonical.Decode[productionCreateReceiptV1](receiptJSON)
	if decodeErr != nil || value.Version != 1 ||
		redaction.ValidateSet(value.Set) != nil || redaction.ValidateDraft(value.Draft) != nil || redaction.ValidateReceipt(value.Receipt) != nil ||
		value.Receipt.OperationID != request.OperationID || value.Receipt.RequestSHA256 != requestSHA256 ||
		storedSetID != value.Set.ID || value.Set.ID != value.Draft.SetID || value.Receipt.SetID != value.Set.ID ||
		value.Receipt.Revision != value.Draft.Revision || value.Receipt.ETag != value.Draft.ETag ||
		value.Set.Name != request.Name || value.Set.HeadRevision != 1 || value.Draft.Revision != 1 || value.Draft.ETag != 1 ||
		value.Draft.State != "draft" || value.Draft.MembershipSealed || value.Draft.InstructionsSHA256 != productionSHA256([]byte(request.Instructions)) {
		return redaction.Set{}, redaction.Draft{}, false, ErrInvalidProduction
	}
	if err := validateProductionOperationAuditTx(ctx, tx, request.OperationID, storedSetID, value.Receipt.Revision, actor, kind, storedSHA256, receiptJSON); err != nil {
		return redaction.Set{}, redaction.Draft{}, false, err
	}
	emptyMembers, _ := productionMemberHash(nil)
	emptyDecisions, _ := productionDecisionHash(nil)
	if value.Draft.MemberHash != emptyMembers || value.Draft.DecisionsSHA256 != emptyDecisions ||
		validateProductionDraftCatalogTx(ctx, tx, value.Draft) != nil {
		return redaction.Set{}, redaction.Draft{}, false, ErrInvalidProduction
	}
	storedSet, storedErr := scanProductionSet(tx.QueryRowContext(ctx, `SELECT id,name,creator,created_at,head_revision FROM production_sets WHERE id=?`, value.Set.ID))
	if storedErr != nil || storedSet.ID != value.Set.ID || storedSet.Name != value.Set.Name || storedSet.Creator != value.Set.Creator ||
		storedSet.CreatedAt != value.Set.CreatedAt || storedSet.HeadRevision < value.Set.HeadRevision {
		return redaction.Set{}, redaction.Draft{}, false, ErrInvalidProduction
	}
	return value.Set, value.Draft, true, nil
}

func resolveProductionCatalog(recipeID, profileID, disclosureID, numberingID string) (productionCatalogSelection, error) {
	if recipeID == "" {
		recipeID = redaction.DefaultRecipeID
	}
	var dpi int
	switch recipeID {
	case redaction.RecipeID300DPI:
		dpi = 300
	case redaction.RecipeID600DPI:
		dpi = 600
	default:
		return productionCatalogSelection{}, ErrInvalidProduction
	}
	recipe, err := pdfproduction.QualifiedRecipeForDPI(dpi)
	if err != nil {
		return productionCatalogSelection{}, errors.Join(ErrInvalidProduction, err)
	}
	recipeJSON, err := canonical.Marshal(recipe)
	if err != nil {
		return productionCatalogSelection{}, err
	}
	if profileID == "" {
		profileID = redaction.DefaultOutputProfileID
	}
	if disclosureID == "" {
		disclosureID = redaction.DefaultDisclosureProfileID
	}
	if profileID != redaction.DefaultOutputProfileID || disclosureID != redaction.DefaultDisclosureProfileID || numberingID != "" && numberingID != redaction.BatesNumberingRecipeID {
		return productionCatalogSelection{}, ErrInvalidProduction
	}
	profileJSON, err := canonical.Marshal(productionOutputProfileDescriptor(profileID))
	if err != nil {
		return productionCatalogSelection{}, err
	}
	disclosureJSON, err := canonical.Marshal(productionDisclosureProfileV1{"production-disclosure-profile/v1", disclosureID, true})
	if err != nil {
		return productionCatalogSelection{}, err
	}
	numberingJSON, err := canonical.Marshal(productionNumberingRecipeV1{"production-numbering-recipe/v1", numberingID, map[bool]string{true: "unstamped", false: "sequential-bates"}[numberingID == ""]})
	if err != nil {
		return productionCatalogSelection{}, err
	}
	return productionCatalogSelection{recipeID: recipeID, recipeSHA256: productionSHA256(recipeJSON),
		profileID: profileID, profileSHA256: productionSHA256(profileJSON), disclosureProfileID: disclosureID,
		disclosureProfileSHA256: productionSHA256(disclosureJSON), numberingRecipeID: numberingID,
		numberingRecipeSHA256: productionSHA256(numberingJSON), recipeJSON: recipeJSON, profileJSON: profileJSON,
		disclosureProfileJSON: disclosureJSON, numberingRecipeJSON: numberingJSON}, nil
}

func ensureProductionCatalogTx(ctx context.Context, tx *sql.Tx, selection productionCatalogSelection) error {
	entries := []struct {
		kind, id, sha256 string
		raw              []byte
	}{
		{"recipe", selection.recipeID, selection.recipeSHA256, selection.recipeJSON},
		{"profile", selection.profileID, selection.profileSHA256, selection.profileJSON},
		{"disclosure", selection.disclosureProfileID, selection.disclosureProfileSHA256, selection.disclosureProfileJSON},
		{"numbering", selection.numberingRecipeID, selection.numberingRecipeSHA256, selection.numberingRecipeJSON},
	}
	for _, entry := range entries {
		if err := validateProductionCatalogEntry(entry.kind, entry.id, entry.sha256, entry.raw); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO production_catalog_entries(kind,id,sha256,canonical_json,created_at)
			VALUES(?,?,?,?,?) ON CONFLICT(kind,id,sha256) DO NOTHING`, entry.kind, entry.id, entry.sha256, entry.raw, nowRFC3339()); err != nil {
			return errors.Join(ErrInvalidProduction, err)
		}
		var retainedSHA256 string
		var retained []byte
		if err := tx.QueryRowContext(ctx, `SELECT sha256,canonical_json FROM production_catalog_entries WHERE kind=? AND id=?`, entry.kind, entry.id).Scan(&retainedSHA256, &retained); err != nil || retainedSHA256 != entry.sha256 || !bytes.Equal(retained, entry.raw) {
			return ErrInvalidProduction
		}
	}
	return nil
}

func validateProductionDraftCatalogTx(ctx context.Context, q metadataQuerier, draft redaction.Draft) error {
	entries := []struct{ kind, id, sha256 string }{
		{"recipe", draft.RecipeID, draft.RecipeSHA256}, {"profile", draft.ProfileID, draft.ProfileSHA256},
		{"disclosure", draft.DisclosureProfileID, draft.DisclosureProfileSHA256}, {"numbering", draft.NumberingRecipeID, draft.NumberingRecipeSHA256},
	}
	for _, entry := range entries {
		var raw []byte
		if err := q.QueryRowContext(ctx, `SELECT canonical_json FROM production_catalog_entries WHERE kind=? AND id=? AND sha256=?`, entry.kind, entry.id, entry.sha256).Scan(&raw); err != nil || validateProductionCatalogEntry(entry.kind, entry.id, entry.sha256, raw) != nil {
			return ErrInvalidProduction
		}
	}
	policy, err := loadProductionPolicyByDigest(ctx, q, draft.Policy.PolicySHA256)
	if err != nil || policy.ID != draft.Policy.PolicyID || policy.Version != draft.Policy.Version {
		return ErrInvalidProduction
	}
	return nil
}

func validateProductionCatalogEntry(kind, id, sha256 string, raw []byte) error {
	if !canonical.IsSHA256Hex(sha256) || productionSHA256(raw) != sha256 {
		return ErrInvalidProduction
	}
	switch kind {
	case "recipe":
		value, err := canonical.Decode[redaction.Recipe](raw)
		if err != nil || id != redaction.RecipeID300DPI && id != redaction.RecipeID600DPI ||
			value.Contract != "raster-redaction/v1" || value.DPI != 300 && value.DPI != 600 ||
			id == redaction.RecipeID300DPI && value.DPI != 300 || id == redaction.RecipeID600DPI && value.DPI != 600 ||
			!canonical.IsSHA256Hex(value.RendererSHA256) || !canonical.IsSHA256Hex(value.FontSHA256) || value.WriterVersion == "" {
			return ErrInvalidProduction
		}
	case "profile":
		value, err := canonical.Decode[productionOutputProfileV1](raw)
		if err != nil || value.ID != id || value.Contract != "production-output-profile/v1" ||
			len(value.ArtifactRoles) != 2 || value.ArtifactRoles[0] != "redacted_pdf" || value.ArtifactRoles[1] != "redacted_text" ||
			value.LoadfileKind != "dat_pdf" || value.AllowOriginal || value.AllowNativeTextFallback || !value.SearchablePDF {
			return ErrInvalidProduction
		}
	case "disclosure":
		value, err := canonical.Decode[productionDisclosureProfileV1](raw)
		if err != nil || value.ID != id || value.Contract != "production-disclosure-profile/v1" || !value.GeneratedFieldsOnly {
			return ErrInvalidProduction
		}
	case "numbering":
		value, err := canonical.Decode[productionNumberingRecipeV1](raw)
		if err != nil || value.ID != id || value.Contract != "production-numbering-recipe/v1" ||
			id == "" && value.Kind != "unstamped" || id != "" && value.Kind != "sequential-bates" {
			return ErrInvalidProduction
		}
	default:
		return ErrInvalidProduction
	}
	return nil
}

type productionOutputProfileV1 struct {
	Contract                string   `json:"contract"`
	ID                      string   `json:"id"`
	ArtifactRoles           []string `json:"artifact_roles"`
	LoadfileKind            string   `json:"loadfile_kind"`
	AllowOriginal           bool     `json:"allow_original"`
	AllowNativeTextFallback bool     `json:"allow_native_text_fallback"`
	SearchablePDF           bool     `json:"searchable_pdf"`
}

func productionOutputProfileDescriptor(id string) productionOutputProfileV1 {
	return productionOutputProfileV1{
		Contract: "production-output-profile/v1", ID: id,
		ArtifactRoles: []string{"redacted_pdf", "redacted_text"}, LoadfileKind: "dat_pdf",
		AllowOriginal: false, AllowNativeTextFallback: false, SearchablePDF: true,
	}
}

func canonicalProductionCreateRequest(value redaction.CreateRequest) ([]byte, error) {
	return canonical.Marshal(struct {
		OperationID, Name, Instructions, RecipeID, ProfileID, DisclosureProfileID, NumberingRecipeID, PolicyID string
		PolicyVersion                                                                                          int64
	}{value.OperationID, value.Name, value.Instructions,
		emptyProductionRequestReference(value.RecipeID, "@default-recipe"),
		emptyProductionRequestReference(value.ProfileID, "@default-output"),
		emptyProductionRequestReference(value.DisclosureProfileID, "@default-disclosure"),
		emptyProductionRequestReference(value.NumberingRecipeID, "@unstamped"),
		emptyProductionRequestReference(value.PolicyID, "@generic-policy"), value.PolicyVersion})
}

func emptyProductionRequestReference(value, marker string) string {
	if value == "" {
		return marker
	}
	return value
}

func productionSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validProductionActor(value string) bool {
	return len(value) > 0 && len(value) <= 256 && utf8.ValidString(value) && !containsProductionControl(value)
}

func containsProductionControl(value string) bool {
	for _, char := range value {
		if unicode.IsControl(char) {
			return true
		}
	}
	return false
}
