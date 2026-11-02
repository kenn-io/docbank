package sqlcheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestScanReportsEveryCheckConstraint(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		col  string
	}{
		{"in list", `status TEXT NOT NULL CHECK(status IN ('queued','running','done')) DEFAULT 'queued'`, "status"},
		{"not in", "CHECK (kind NOT IN ('a', 'b'))", "kind"},
		{"or chain", "CHECK (mode = 'fast' OR mode = 'slow')", "mode"},
		{"quoted column", `CHECK ("state" IN ('x'))`, `"state"`},
		{"lower wrapped", "CHECK (lower(role) IN ('admin', 'member'))", "role"},
		{"multiline", "CONSTRAINT valid_kind\n    CHECK (\n      kind IN (\n        'one',\n        'two'\n      )\n    )", "kind"},
		{"escaped quote", "CHECK (name IN ('it''s', 'ok'))", "name"},
		{"nullable prefix", "CHECK (error_code IS NULL OR error_code IN ('timeout', 'refused'))", "error_code"},
		{"per-branch states", "CHECK ((state = 'running' AND started_at IS NOT NULL) OR (state = 'done' AND finished_at IS NOT NULL))", "state"},
		{"postgres any array", "CHECK (status = ANY (ARRAY['queued'::text, 'done'::text]))", "status"},
		{"length", "CHECK (length(name) > 0)", "name"},
		{"range", "CHECK (retries >= 0 AND retries <= 10)", "retries"},
		{"ordering", "CHECK (started_at <= finished_at)", "started_at"},
		{"single literal invariant", "CHECK ((state = 'done' AND finished_at IS NOT NULL) OR (state <> 'done' AND finished_at IS NULL))", "state"},
		{"substr", "CHECK (SUBSTR(local_date, 6, 2) IN ('01', '02'))", "local_date"},
		{"subquery", "CHECK (status IN (SELECT name FROM statuses))", "status"},
		{"json", "CHECK (json_valid(payload))", "payload"},
		{"not null leading", "CHECK (NOT (deleted AND active))", "deleted"},
		{"table constraint", "CREATE TABLE t (a INT, b INT, CHECK (a < b))", "a"},
		{"parenthesis in comment", "CHECK (amount > 0 /* see normalize(value */)", "amount"},
		{"quoted identifier comment marker", `CREATE TABLE t ("--status" TEXT CHECK ("--status" <> ''))`, `"--status"`},
		{"no column", "CHECK (1 = 1)", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			findings := Scan(tc.sql)
			require.Len(t, findings, 1, "expected one finding")
			assert.Equal(CheckConstraint, findings[0].Kind)
			assert.Equal(tc.col, findings[0].Name)
			assert.Contains(findings[0].Message(), "CHECK constraint")
			if tc.col != "" {
				assert.Contains(findings[0].Message(), "on "+tc.col)
			}
		})
	}
}

func TestScanIgnoresCommentsStringsAndOtherText(t *testing.T) {
	assert := assert.New(t)
	for _, sql := range []string{
		"-- CHECK the docs",
		"-- CHECK (kind IN ('a', 'b'))\nCREATE TABLE t (kind TEXT)",
		"/* CHECK (kind IN ('a')) */ CREATE TABLE t (kind TEXT)",
		"/* multi\n CHECK (kind IN ('a'))\n line */",
		"INSERT INTO notes (body) VALUES ('CHECK (kind IN (''a''))')",
		"SELECT $$CHECK (example)$$",
		"SELECT $body$CHECK (example)$body$",
		"/* outer /* CHECK (kind IN ('a')) */ still comment */",
		"-- CREATE TYPE s AS ENUM ('x')",
		"CREATE TABLE t (kind TEXT) -- was: CHECK (kind IN ('a'))",
		"CREATE TABLE checks (id INT); SELECT * FROM checks WHERE checked = 1",
		"CREATE TYPE money AS (amount INT, currency TEXT); CREATE TYPE t AS RANGE (subtype = int)",
		"CHECK (unterminated",
		"",
	} {
		assert.Empty(Scan(sql), sql)
	}
}

func TestScanReportsPositions(t *testing.T) {
	assert := assert.New(t)
	src := "CREATE TABLE t ( -- CHECK (id IN (1))\n  id INTEGER, /* 'CHECK' */\n  status TEXT CHECK (status IN ('a')),\n  other TEXT CHECK (length(other) > 0)\n);"
	findings := Scan(src)
	require.Len(t, findings, 2)
	assert.Equal(3, findings[0].Line)
	assert.Equal(15, findings[0].Column)
	assert.Equal(4, findings[1].Line)
	assert.Equal("other", findings[1].Name)
	assert.Equal("length(other) > 0", findings[1].Expr)
}

func TestScanReportsEnumTypes(t *testing.T) {
	assert := assert.New(t)
	src := "CREATE TYPE job_status AS ENUM ('queued', 'done');\nCREATE TYPE \"public\".\"kind\" AS\n  ENUM ('a');\nCREATE TABLE t (id INT);"
	findings := Scan(src)
	require.Len(t, findings, 2)
	assert.Equal(EnumType, findings[0].Kind)
	assert.Equal("job_status", findings[0].Name)
	assert.Equal("'queued', 'done'", findings[0].Expr)
	assert.Equal(1, findings[0].Line)
	assert.Equal(`"public"."kind"`, findings[1].Name)
	assert.Equal(2, findings[1].Line)
	assert.Contains(findings[0].Message(), "enum type job_status")
}

func TestScanOrdersFindingsByOffset(t *testing.T) {
	src := "CREATE TABLE t (kind TEXT CHECK (kind IN ('a')));\nCREATE TYPE s AS ENUM ('x');\nCREATE TABLE u (n INT CHECK (n > 0));"
	findings := Scan(src)
	require.Len(t, findings, 3)
	assert.Equal(t, []Kind{CheckConstraint, EnumType, CheckConstraint}, []Kind{findings[0].Kind, findings[1].Kind, findings[2].Kind})
}

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}
