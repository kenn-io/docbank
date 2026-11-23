package store

import (
	"fmt"
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

var releaseTagVersion = regexp.MustCompile(`(?m)^const currentStorageSchemaVersion = ([0-9]+)\b`)

type releaseTag struct {
	name, store, schema string
}

func releaseTagFixtures() map[int]string {
	fixtures := make(map[int]string, len(releasedSchemaFixtures)+3)
	for version, schema := range releasedSchemaFixtures {
		fixtures[version] = schema
	}
	fixtures[1], fixtures[2], fixtures[3] = schemaV090SQL, schemaV0100SQL, schemaV0140SQL
	return fixtures
}

func TestReleaseTagSchemas(t *testing.T) {
	if os.Getenv("DOCBANK_RELEASE_SCHEMAS") == "" {
		t.Skip("DOCBANK_RELEASE_SCHEMAS is unset")
	}
	output, err := exec.CommandContext(t.Context(), "git", "tag", "-l", "v[0-9]*", "--sort=v:refname").CombinedOutput()
	require.NoErrorf(t, err, "list release tags: %s", output)
	names := strings.Fields(string(output))
	require.Contains(t, names, "v0.9.0", "release tags must include the first supported release; fetch full tag history")
	var tags []releaseTag
	for _, name := range names {
		if semver.Compare(name, "v0.9.0") < 0 {
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
	if len(tags) == 0 {
		return []string{"release tag list is empty"}
	}
	var problems []string
	previous := make(map[int]string)
	for _, tag := range tags {
		if semver.Compare(tag.name, "v0.9.0") < 0 {
			continue
		}
		match := releaseTagVersion.FindStringSubmatch(tag.store)
		version := 1
		if len(match) == 0 {
			if tag.name != "v0.9.0" {
				problems = append(problems, fmt.Sprintf("%s has no storage schema version", tag.name))
				continue
			}
		} else {
			var err error
			version, err = strconv.Atoi(match[1])
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: invalid storage schema version %s", tag.name, match[1]))
				continue
			}
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
		if !seen && tag.schema != fixture {
			problems = append(problems, fmt.Sprintf("%s storage schema %d differs from its exact fixture", tag.name, version))
		}
		for _, driver := range v090UpgradeDrivers() {
			released := schemaContractDB(t, driver.driver, baseline)
			candidate := schemaContractDB(t, driver.driver, tag.schema)
			for _, change := range releasedSchemaChanges(t, released, candidate) {
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
	const base = "CREATE TABLE item (id INTEGER); CREATE TRIGGER trig AFTER INSERT ON item BEGIN SELECT 1; END;"
	const index = " CREATE INDEX idx ON item(id);"
	version := func(n int) string { return fmt.Sprintf("const currentStorageSchemaVersion = %d\n", n) }
	tag := func(name string, n int, schema string) releaseTag { return releaseTag{name, version(n), schema} }
	fixtures := map[int]string{1: base, 2: base, 3: schemaV0140SQL, 28: base}
	cases := []struct {
		name string
		tags []releaseTag
		want []string
	}{
		{"first", []releaseTag{tag("v0.15.0", 28, base)}, nil},
		{"first index", []releaseTag{tag("v0.15.0", 28, base+index)}, []string{"exact fixture"}},
		{"comments", []releaseTag{tag("v0.15.0", 28, "-- comment\n"+base)}, []string{"exact fixture"}},
		{"unique", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+" CREATE UNIQUE INDEX idx ON item(id);")}, []string{"added unique index idx"}},
		{"table changed", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, strings.Replace(base, "id INTEGER", "id TEXT", 1))}, []string{"changed or missing table item"}},
		{"missing table", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, "")}, []string{"changed or missing table item"}},
		{"missing trigger", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, "CREATE TABLE item (id INTEGER);")}, []string{"changed or missing trigger trig"}},
		{"added table", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+" CREATE TABLE extra (id INTEGER);")}, []string{"added table extra"}},
		{"later index", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+index)}, nil},
		{"trigger changed", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, strings.Replace(base, "SELECT 1", "SELECT 2", 1)+" -- "+base)}, []string{"changed or missing trigger trig"}},
		{"index removed", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+index), tag("v0.15.2", 28, base)}, []string{"v0.15.2", "changed or missing index idx"}},
		{"index changed", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+index), tag("v0.15.2", 28, base+" CREATE INDEX idx ON item(id DESC);")}, []string{"changed or missing index idx"}},
		{"unregistered", []releaseTag{tag("v0.15.0", 99, base)}, []string{"unregistered"}},
		{"missing fixture", []releaseTag{tag("v0.15.0", 29, base)}, []string{"no fixture"}},
		{"missing version", []releaseTag{{"v0.10.0", "", base}}, []string{"no storage schema version"}},
		{"missing version after boundary", []releaseTag{{"v0.9.1", "", base}}, []string{"no storage schema version"}},
		{"old ignored", []releaseTag{{"v0.8.1", "", "garbage"}}, nil},
		{"v1", []releaseTag{{"v0.9.0", "", base}}, nil},
		{"v1 mismatch", []releaseTag{{"v0.9.0", "", "CREATE TABLE other (id INTEGER);"}}, []string{"exact fixture", "changed or missing table item"}},
		{"v2 mismatch", []releaseTag{tag("v0.10.0", 2, base+index)}, []string{"exact fixture"}},
		{"v2 later index", []releaseTag{tag("v0.10.0", 2, base), tag("v0.10.1", 2, base+index)}, nil},
		{"v2 table change", []releaseTag{tag("v0.10.0", 2, base), tag("v0.10.1", 2, strings.Replace(base, "id INTEGER", "id TEXT", 1))}, []string{"v0.10.1", "changed or missing table item"}},
		{"version 3", []releaseTag{tag("v0.12.0", 3, schemaV0140SQL)}, nil},
		{"version 3 mismatch", []releaseTag{tag("v0.12.0", 3, base)}, []string{"exact fixture"}},
		{"baseline retained", []releaseTag{tag("v0.15.0", 28, base), tag("v0.15.1", 28, base+" CREATE UNIQUE INDEX idx ON item(id);"), tag("v0.15.2", 28, base+index)}, []string{"v0.15.1", "added unique index idx"}},
		{"empty", nil, []string{"empty"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := releaseTagProblems(t, tc.tags, []int{1, 2, 3, 28, 29}, fixtures)
			if tc.want == nil {
				require.Empty(t, problems)
				return
			}
			for _, want := range tc.want {
				assert.Contains(t, strings.Join(problems, "\n"), want)
			}
			if tc.name == "baseline retained" {
				assert.NotContains(t, strings.Join(problems, "\n"), "v0.15.2")
			}
		})
	}
	t.Run("shared name", func(t *testing.T) {
		first := base + " CREATE INDEX trig ON item(id);"
		last := base + " CREATE INDEX trig ON item(id DESC);"
		problems := releaseTagProblems(t, []releaseTag{tag("v0.15.0", 28, first), tag("v0.15.1", 28, last)}, []int{28}, map[int]string{28: first})
		assert.Contains(t, strings.Join(problems, "\n"), "changed or missing index trig")
	})
}
