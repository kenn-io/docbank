package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestFromMaintenanceErrorPreservesCommittedRetirementBoundary(t *testing.T) {
	t.Parallel()
	err := &packstore.PackRetirementError{PackID: "01kxbz5s5z6b3m8v8m8p0m6h4y", Err: errors.New("sharing violation")}
	mapped := &Error{}
	ok := errors.As(FromMaintenanceError(err), &mapped)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, mapped.Status)
	assert.Equal(t, "pack_retirement_deferred", mapped.Code)
	assert.Contains(t, mapped.Detail, "pack replacement committed")
	assert.Contains(t, mapped.Detail, "docbank storage pack")
}

func TestFromStoreErrorMapsUnsupportedAuditMutation(t *testing.T) {
	t.Parallel()
	mapped := &Error{}
	ok := errors.As(FromStoreError(store.ErrAuditMutationUnsupported), &mapped)
	require.True(t, ok)
	assert.Equal(t, http.StatusConflict, mapped.Status)
	assert.Equal(t, "audit_mutation_unsupported", mapped.Code)
}

func TestFromStoreErrorMapsCustodianConflict(t *testing.T) {
	t.Parallel()
	mapped := &Error{}
	require.ErrorAs(t, FromStoreError(store.ErrCustodianConflict), &mapped)
	assert.Equal(t, http.StatusConflict, mapped.Status)
	assert.Equal(t, "custodian_conflict", mapped.Code)
}

func TestFromStoreErrorMappings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{store.ErrPersonMergeConflict, http.StatusConflict, "person_merge_conflict"},
		{store.ErrPhotoCheckoutConflict, http.StatusConflict, "photo_checkout_conflict"},
		{store.ErrPersonMergeTooLarge, http.StatusUnprocessableEntity, "person_merge_too_large"},
		{store.ErrPersonRetired, http.StatusConflict, "person_retired"},
		{store.ErrPhotoEditReceiptLimit, http.StatusUnprocessableEntity, "photo_edit_receipt_limit"},
	} {
		t.Run(test.code, func(t *testing.T) {
			mapped := &Error{}
			require.ErrorAs(t, FromStoreError(test.err), &mapped)
			assert.Equal(t, test.status, mapped.Status)
			assert.Equal(t, test.code, mapped.Code)
		})
	}
}

func TestProcessingErrorsPreserveRenditionOutcomes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		err    error
		code   string
		detail string
	}{
		{processing.ErrRenditionFailed, "rendition_failed", ""},
		{fmt.Errorf("%w: %w: open /synthetic/private/passwords.txt: permission denied", processing.ErrRenditionOperatorRequired, processing.ErrPDFPasswordsUnreadable), "rendition_operator_required", "PDF passwords file can't be read; check media.pdf_passwords_file"},
		{processing.ErrRenditionOperatorRequired, "rendition_operator_required", "document rendition requires operator intervention"},
		{vectorindex.ErrSimilarSearchScoringBudgetExceeded, "similar_search_too_large", ""},
	} {
		mapped := &Error{}
		require.ErrorAs(t, fromProcessingError(test.err), &mapped)
		assert.Equal(t, test.code, mapped.Code)
		if test.detail != "" {
			assert.Equal(t, test.detail, mapped.Detail)
		}
	}
}
