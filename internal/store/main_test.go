package store

import (
	"flag"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"go.kenn.io/docbank/internal/packagetest"
	"go.kenn.io/docbank/internal/pgstoretest"
	"go.kenn.io/docbank/internal/sqlitetest"
)

// slowPostgresScenarios marks the PostgreSQL contract scenarios that take
// several seconds, most of them because their subtests run one at a time in
// a child process. The list only orders the run; a scenario missing from it
// still runs.
var slowPostgresScenarios = map[string]bool{
	"lexical-search/LexicalSearchIndexes":                                      true,
	"sealed-delivery/SealCollectionSnapshotStreamValidatesFamilyAcrossBatches": true,
	"exports/ExportPlanOptionalPagesRespectMetadataBounds":                     true,
	"photo-browse/PhotoFacetsCountSharedValuesOnce":                            true,
	"photo-edit/PhotoXMPInvalidation":                                          true,
	"retention/RetainedCleanupFixtures":                                        true,
	"people-curation/BrowserMergeRequiresHiddenUnlock":                         true,
	"enrollment/AuditEnrollmentCatalogs":                                       true,
	"metadata-export/MetadataRetainedCatalogs":                                 true,
	"photo-review/PhotoStackTimeRules":                                         true,
	"trash/PhotoTrashRestore":                                                  true,
	"physical-storage/PackCatalogContract":                                     true,
	"metadata-import/RetainedStringNativeWrites":                               true,
}

func TestMain(m *testing.M) {
	flag.Parse()
	if *reportBackendCoverage {
		printBackendCoverage()
		return
	}
	sqlitetest.DisableSync.Store(true)
	// The backend contracts hold a large share of the package's tests, so
	// they spread over the child processes like separate tests. A pattern
	// that matches no subtest makes go test report that the package ran no
	// tests, so each backend lists only what it runs. SQLite suites run fast
	// and go whole. Every PostgreSQL scenario waits on the server, and a
	// whole suite in one child took longer than the rest of the package, so
	// its scenarios go one at a time.
	var sqliteSuites, postgresScenarios []string
	for _, suite := range backendSuites {
		if slices.ContainsFunc(suite.scenarios, func(s backendScenario) bool { return s.sqlite != nil }) {
			sqliteSuites = append(sqliteSuites, suite.name)
		}
		for _, scenario := range suite.scenarios {
			if scenario.postgres == nil {
				continue
			}
			name := suite.name + "/" + scenario.name
			if name != "metadata-import/MetadataImportCatalogs" {
				postgresScenarios = append(postgresScenarios, name)
				continue
			}
			// Its fifty parallel subtests would run one at a time in one child.
			for _, subtest := range slices.Sorted(maps.Keys(metadataImportCatalogSeeds())) {
				// go test names a subtest with its spaces replaced.
				postgresScenarios = append(postgresScenarios, name+"/"+strings.ReplaceAll(subtest, " ", "_"))
			}
		}
	}
	// Children take scenarios in order, so the longest start first and none
	// runs alone at the end.
	slices.SortStableFunc(postgresScenarios, func(a, b string) int {
		return compareBool(slowPostgresScenarios[b], slowPostgresScenarios[a])
	})
	os.Exit(pgstoretest.Finish(packagetest.RunShards(m,
		packagetest.Split{Test: "TestBackendContractsSQLite", Subtests: sqliteSuites},
		packagetest.Split{Test: "TestBackendContractsPostgres", Subtests: postgresScenarios})))
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	default:
		return 1
	}
}
