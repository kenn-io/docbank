package store

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
)

const firstSupportedReleaseTag = "v0.9.0"

var releaseTagVersion = regexp.MustCompile(`(?m)^const currentStorageSchemaVersion = ([0-9]+)\b`)

type releaseTag struct {
	name, store, schema string
}

func releaseTagFixtures() map[int]string {
	fixtures := maps.Clone(releasedSchemaFixtures)
	fixtures[1], fixtures[2], fixtures[3] = schemaV090SQL, schemaV0100SQL, schemaV0140SQL
	return fixtures
}

func releaseTagNames(output []byte) []string {
	names := strings.Fields(string(output))
	slices.SortFunc(names, semver.Compare)
	return names
}

func TestReleaseTagNamesPrereleaseOrder(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{firstSupportedReleaseTag, "v0.10.0", "v0.20.0-rc.1", "v0.20.0"}, releaseTagNames([]byte("v0.20.0\nv0.20.0-rc.1\nv0.10.0\nv0.9.0\n")))
}

func TestReleaseTagSchemas(t *testing.T) {
	t.Parallel()
	if os.Getenv("DOCBANK_RELEASE_SCHEMAS") == "" {
		t.Skip("DOCBANK_RELEASE_SCHEMAS is unset")
	}
	output, err := exec.CommandContext(t.Context(), "git", "tag", "-l", "v[0-9]*").CombinedOutput()
	require.NoErrorf(t, err, "list release tags: %s", output)
	names := releaseTagNames(output)
	require.Contains(t, names, firstSupportedReleaseTag, "release tags must include the first supported release; fetch full tag history")
	var tags []releaseTag
	for _, name := range names {
		if semver.Compare(name, firstSupportedReleaseTag) < 0 {
			continue
		}
		var files [2]string
		for i, file := range []string{"store.go", "schema.sql"} {
			output, err := exec.CommandContext(t.Context(), "git", "show", name+":internal/store/"+file).CombinedOutput()
			require.NoErrorf(t, err, "%s %s: %s", name, file, output)
			files[i] = string(output)
		}
		tags = append(tags, releaseTag{name, files[0], files[1]})
	}
	assert.Empty(t, releaseTagProblems(t, tags, releasedStorageSchemaVersions, releaseTagFixtures()))
}

func releaseTagProblems(t *testing.T, tags []releaseTag, versions []int, fixtures map[int]string) []string {
	t.Helper()
	var problems []string
	previous := make(map[int]string)
	checked := make(map[int]bool)
	for _, tag := range tags {
		version := 1
		if match := releaseTagVersion.FindStringSubmatch(tag.store); match != nil {
			version, _ = strconv.Atoi(match[1])
		} else if tag.name != firstSupportedReleaseTag {
			problems = append(problems, tag.name+" has no storage schema version")
			continue
		}
		if !slices.Contains(versions, version) {
			problems = append(problems, fmt.Sprintf("%s ships unregistered storage schema %d", tag.name, version))
			continue
		}
		fixture, ok := fixtures[version]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s storage schema %d has no fixture", tag.name, version))
			continue
		}
		baseline, seen := previous[version]
		if !seen {
			baseline = fixture
		}
		start := len(problems)
		if !checked[version] && tag.schema != fixture {
			problems = append(problems, fmt.Sprintf("%s storage schema %d differs from its exact fixture", tag.name, version))
		}
		checked[version] = true
		for _, driver := range v090UpgradeDrivers() {
			released := schemaContractDB(t, driver.driver, baseline)
			candidate := schemaContractDB(t, driver.driver, tag.schema)
			changes, _ := releasedSchemaChanges(t, released, candidate)
			for _, change := range changes {
				problems = append(problems, fmt.Sprintf("%s storage schema %d (%s): %s", tag.name, version, driver.name, change))
			}
		}
		if len(problems) == start {
			previous[version] = tag.schema
		}
	}
	return problems
}

func TestReleaseTagSchemasRules(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE item (id INTEGER); CREATE TRIGGER trig AFTER INSERT ON item BEGIN SELECT 1; END;"
	const index = " CREATE INDEX idx ON item(id);"
	version := func(n int) string { return fmt.Sprintf("const currentStorageSchemaVersion = %d\n", n) }
	tag := func(name string, n int, schema string) releaseTag { return releaseTag{name, version(n), schema} }
	fixtures := map[int]string{1: base, 2: base, 28: base}
	cases := []struct {
		name          string
		tags          []releaseTag
		want, notWant string
	}{
		{"first", []releaseTag{tag("v0.15.0", 28, base)}, "", ""},
		{"first index", []releaseTag{tag("v0.15.0", 28, base+index)}, "exact fixture", ""},
		{"comments", []releaseTag{tag("v0.15.0", 28, "-- comment\n"+base)}, "exact fixture", ""},
		{"later index", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+index)}, "", ""},
		{"unregistered", []releaseTag{tag("v0.15.0", 99, base)}, "unregistered", ""},
		{"missing fixture", []releaseTag{tag("v0.15.0", 29, base)}, "no fixture", ""},
		{"missing version", []releaseTag{{"v0.10.0", "", base}}, "no storage schema version", ""},
		{"v1", []releaseTag{{firstSupportedReleaseTag, "", base}}, "", ""},
		{"baseline retained", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+" CREATE UNIQUE INDEX idx ON item(id);"), tag("v0.15.2", 28, base+index)}, "added unique index idx", "v0.15.2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := strings.Join(releaseTagProblems(t, tc.tags, []int{1, 2, 28, 29}, fixtures), "\n")
			if tc.want == "" {
				assert.Empty(t, problems)
			} else {
				assert.Contains(t, problems, tc.want)
			}
			if tc.notWant != "" {
				assert.NotContains(t, problems, tc.notWant)
			}
		})
	}
	t.Run("catalog change", func(t *testing.T) {
		tags := []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, strings.Replace(base, "id INTEGER", "id TEXT", 1))}
		problems := strings.Join(releaseTagProblems(t, tags, []int{28}, fixtures), "\n")
		assert.Contains(t, problems, "v0.15.1 storage schema 28 (")
		assert.Contains(t, problems, "changed or missing table item")
	})

	t.Run("first mismatch is reported once", func(t *testing.T) {
		tags := []releaseTag{tag("v0.10.0", 2, base+index), tag("v0.10.1", 2, base+index), tag("v0.11.0", 2, base)}
		problems := strings.Join(releaseTagProblems(t, tags, []int{2}, fixtures), "\n")
		assert.Equal(t, 1, strings.Count(problems, "differs from its exact fixture"))
		assert.Contains(t, problems, "v0.11.0")
		assert.NotContains(t, problems, "v0.10.1")
	})
}

func TestReleasedSchemaChanges(t *testing.T) {
	t.Parallel()
	const base = "CREATE TABLE item (id INTEGER); CREATE TRIGGER trig AFTER INSERT ON item BEGIN SELECT 1; END;"
	const index = " CREATE INDEX idx ON item(id);"
	cases := []struct {
		name, before, after, want string
	}{
		{"unique", base, base + " CREATE UNIQUE INDEX idx ON item(id);", "added unique index idx"},
		{"changed table", base, strings.Replace(base, "id INTEGER", "id TEXT", 1), "changed or missing table item"},
		{"removed trigger", base, "CREATE TABLE item (id INTEGER);", "changed or missing trigger trig"},
		{"added table", base, base + " CREATE TABLE extra (id INTEGER);", "added table extra"},
		{"removed index", base + index, base, "changed or missing index idx"},
		{"added index", base, base + index, ""},
		{"shared name", base + " CREATE INDEX trig ON item(id);", base + " CREATE INDEX trig ON item(id DESC);", "changed or missing index trig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, driver := range v090UpgradeDrivers() {
				t.Run(driver.name, func(t *testing.T) {
					before := schemaContractDB(t, driver.driver, tc.before)
					after := schemaContractDB(t, driver.driver, tc.after)
					changes, _ := releasedSchemaChanges(t, before, after)
					if tc.want == "" {
						assert.Empty(t, changes)
					} else {
						assert.Contains(t, changes, tc.want)
					}
				})
			}
		})
	}
}
