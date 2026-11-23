package store

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

var releaseTagVersion = regexp.MustCompile(`(?m)^const currentStorageSchemaVersion = ([0-9]+)\b`)

func TestReleaseTagSchemas(t *testing.T) {
	dir := os.Getenv("DOCBANK_RELEASE_SCHEMAS")
	if dir == "" {
		t.Skip("DOCBANK_RELEASE_SCHEMAS is unset")
	}
	fixtures := make(map[int]string, len(releasedSchemaFixtures)+1)
	for version, schema := range releasedSchemaFixtures {
		fixtures[version] = schema
	}
	fixtures[3] = schemaV0140SQL
	checkReleaseTags(t, dir, releasedStorageSchemaVersions, fixtures)
}

func checkReleaseTags(t *testing.T, dir string, versions []int, fixtures map[int]string) {
	t.Helper()
	order, err := os.ReadFile(filepath.Join(dir, "order"))
	require.NoError(t, err)
	tags := strings.Fields(string(order))
	require.NotEmpty(t, tags, "release tag order is empty")
	previous := make(map[int]string)
	for _, tag := range tags {
		t.Run(tag, func(t *testing.T) {
			store, err := os.ReadFile(filepath.Join(dir, tag, "store.go"))
			if os.IsNotExist(err) {
				store = nil
			} else {
				require.NoError(t, err)
			}
			match := releaseTagVersion.FindSubmatch(store)
			if len(match) == 0 {
				require.Truef(t, semver.IsValid(tag) && semver.Compare(tag, "v0.10.0") < 0, "%s has no storage schema version", tag)
				return
			}
			version, err := strconv.Atoi(string(match[1]))
			require.NoError(t, err)
			require.Containsf(t, versions, version, "%s ships unregistered storage schema %d", tag, version)
			if version < 3 {
				return
			}
			baseline, ok := previous[version]
			if !ok {
				baseline, ok = fixtures[version]
				require.Truef(t, ok, "%s storage schema %d has no fixture", tag, version)
			}
			schema, err := os.ReadFile(filepath.Join(dir, tag, "schema.sql"))
			require.NoErrorf(t, err, "%s storage schema %d", tag, version)
			passed := true
			for _, driver := range v090UpgradeDrivers() {
				if !t.Run(driver.name, func(t *testing.T) {
					released := schemaContractDB(t, driver.driver, baseline)
					candidate := schemaContractDB(t, driver.driver, string(schema))
					assertCompatibleReleasedSchema(t, released, candidate, fmt.Sprintf("%s storage schema %d", tag, version))
				}) {
					passed = false
				}
			}
			if passed {
				previous[version] = string(schema)
			}
		})
	}
}

func TestReleaseTagSchemasRules(t *testing.T) {
	if dir := os.Getenv("DOCBANK_RELEASE_RULES_DIR"); dir != "" {
		checkReleaseTags(t, dir, []int{3, 28}, map[int]string{3: schemaV0140SQL, 28: "CREATE TABLE item (id INTEGER); CREATE TRIGGER trig AFTER INSERT ON item BEGIN SELECT 1; END;"})
		return
	}
	const base = "CREATE TABLE item (id INTEGER); CREATE TRIGGER trig AFTER INSERT ON item BEGIN SELECT 1; END;"
	const index = " CREATE INDEX idx ON item(id);"
	cases := []struct {
		name     string
		tags     []string
		schemas  []string
		versions []string
		fail     bool
	}{
		{"first", []string{"v0.15.0"}, []string{base}, nil, false},
		{"first index", []string{"v0.15.0"}, []string{base + index}, nil, false},
		{"comments", []string{"v0.15.0"}, []string{"-- comment\n" + base + "\n-- comment"}, nil, false},
		{"unique", []string{"v0.15.0"}, []string{base + " CREATE UNIQUE INDEX idx ON item(id);"}, nil, true},
		{"table changed", []string{"v0.15.0"}, []string{strings.Replace(base, "id INTEGER", "id TEXT", 1)}, nil, true},
		{"missing object", []string{"v0.15.0"}, []string{"CREATE TABLE item (id INTEGER);"}, nil, true},
		{"later index", []string{"v0.15.0", "v0.15.1"}, []string{base, base + index}, nil, false},
		{"trigger changed", []string{"v0.15.0", "v0.15.1"}, []string{base, strings.Replace(base, "SELECT 1", "SELECT 2", 1)}, nil, true},
		{"trigger in comment", []string{"v0.15.0", "v0.15.1"}, []string{base, strings.Replace(base, "SELECT 1", "SELECT 2", 1) + " -- " + base}, nil, true},
		{"index removed", []string{"v0.15.0", "v0.15.1", "v0.15.2"}, []string{base, base + index, base}, nil, true},
		{"index changed", []string{"v0.15.0", "v0.15.1", "v0.15.2"}, []string{base, base + index, base + " CREATE INDEX idx ON item(id DESC);"}, nil, true},
		{"unregistered", []string{"v0.15.0"}, []string{base}, []string{"99"}, true},
		{"missing version", []string{"v0.10.0"}, []string{base}, []string{""}, true},
		{"old unversioned", []string{"v0.9.0"}, []string{base}, []string{""}, false},
		{"version 3", []string{"v0.12.0"}, []string{schemaV0140SQL}, []string{"3"}, false},
		{"version 3 mismatch", []string{"v0.12.0"}, []string{base}, []string{"3"}, true},
		{"empty", nil, nil, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "order"), []byte(strings.Join(tc.tags, "\n")), 0600))
			for i, tag := range tc.tags {
				tagDir := filepath.Join(dir, tag)
				require.NoError(t, os.Mkdir(tagDir, 0700))
				version := "28"
				if tc.versions != nil {
					version = tc.versions[i]
				}
				if version != "" {
					require.NoError(t, os.WriteFile(filepath.Join(tagDir, "store.go"), []byte("const currentStorageSchemaVersion = "+version+"\n"), 0600))
				}
				require.NoError(t, os.WriteFile(filepath.Join(tagDir, "schema.sql"), []byte(tc.schemas[i]), 0600))
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReleaseTagSchemasRules$")
			cmd.Env = append(slices.Clone(os.Environ()), "DOCBANK_RELEASE_RULES_DIR="+dir)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result: %v\n%s", err, output)
			}
		})
	}
}
