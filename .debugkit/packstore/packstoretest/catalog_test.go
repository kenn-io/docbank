package packstoretest_test

import (
	"testing"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore/packstoretest"
)

func TestMemoryCatalogConforms(t *testing.T) {
	packstoretest.RunCatalogContract(t, func(*testing.T) packstoretest.CatalogHarness {
		return packstoretest.NewMemoryCatalog()
	}, packstoretest.ContractOptions{
		Now:       time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		NewPackID: pack.NewPackID,
	})
}
