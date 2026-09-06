package sqlite_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	docsqlite "go.kenn.io/docbank/sqlite"
)

type emptyNameDriver struct{}

func (emptyNameDriver) Name() string { return "" }

func (emptyNameDriver) Open(string, docsqlite.OpenOptions) (*sql.DB, error) {
	return nil, errors.New("empty-name test driver is not openable")
}

func (emptyNameDriver) IsBusy(error) bool            { return false }
func (emptyNameDriver) IsUniqueViolation(error) bool { return false }

func TestValidateRejectsMissingDriver(t *testing.T) {
	require.Error(t, docsqlite.Validate(nil))
}

func TestValidateRejectsEmptyName(t *testing.T) {
	require.Error(t, docsqlite.Validate(emptyNameDriver{}))
}
