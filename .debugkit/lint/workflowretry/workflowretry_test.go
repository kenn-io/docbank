package workflowretry

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScan(t *testing.T) {
	for _, tt := range []struct {
		script   string
		messages []string
	}{
		{"curl URL", []string{"5:11: curl: use kit-download"}},
		{`c\url https://example.com/x`, []string{"5:11: curl: use kit-download"}},
		{"wget URL", []string{"5:11: wget: use kit-download"}},
		{"kit-retry 3 5 curl URL", []string{"5:11: curl: use kit-download"}},
		{">log curl URL", []string{"5:16: curl: use kit-download"}},
		{"wget -qO- URL | tar xz", []string{"5:11: wget: use kit-download"}},
		{"x=$(curl URL)", []string{"5:15: curl: use kit-download"}},
		{"sudo curl URL", []string{"5:11: curl: use kit-download"}},
		{"/usr/bin/curl URL", []string{"5:11: curl: use kit-download"}},
		{"command curl URL", []string{"5:11: curl: use kit-download"}},
		{"kit-retry 3 5 exec curl URL", []string{"5:11: curl: use kit-download"}},
		{"if ! curl URL; then exit 1; fi", []string{"5:16: curl: use kit-download"}},
		{"apt-get update", []string{"5:11: apt-get: wrap in kit-retry"}},
		{"sudo apt-get -y install jq", []string{"5:11: apt-get: wrap in kit-retry"}},
		{"apt-get -qq update", []string{"5:11: apt-get: wrap in kit-retry"}},
		{"sudo apt-get -o APT::Update::Error-Mode=any update", []string{"5:11: apt-get: wrap in kit-retry"}},
		{"go install x", []string{"5:11: go: wrap in kit-retry"}},
		{"go mod download", []string{"5:11: go: wrap in kit-retry"}},
		{"npm ci", []string{"5:11: npm: wrap in kit-retry"}},
		{"npm --prefix web ci", []string{"5:11: npm: wrap in kit-retry"}},
		{"pip3 install x", []string{"5:11: pip3: wrap in kit-retry"}},
		{"python -m pip install x", []string{"5:11: pip: wrap in kit-retry"}},
		{"python3 -m pip install x", []string{"5:11: pip: wrap in kit-retry"}},
		{"docker pull x", []string{"5:11: docker: wrap in kit-retry"}},
		{"apk update", []string{"5:11: apk: wrap in kit-retry"}},
		{"brew install x", []string{"5:11: brew: wrap in kit-retry"}},
		{"env X=1 apt-get install x", []string{"5:11: apt-get: wrap in kit-retry"}},
		{"# workflowretry: allow diagnostics\ncurl URL 2>&1", nil},
		{"kit-download URL dest", nil},
		{"kit-retry 3 5 sudo apt-get -y install x", nil},
		{"FOO=1 kit-retry 3 5 npm ci", nil},
		{"echo curl; command -v curl", nil},
		{"go test -run install", nil},
		{"curl URL # workflowretry: allow health probe", nil},
		{"curl ${{ runner.temp }}/x", []string{"5:11: curl: use kit-download"}},
		{"if ($env:CI) { Write-Host 'CI' }", nil},
		{"echo curl\ncurl URL", []string{"6:11: curl: use kit-download"}},
		{"'cu'\"rl\" URL", []string{"5:11: curl: use kit-download"}},
		{"# workflowretry: allow \ncurl URL", []string{"6:11: curl: use kit-download"}},
		{"apt-get update\napt-get install -y jq", []string{"5:11: apt-get: wrap in kit-retry", "6:11: apt-get: wrap in kit-retry"}},
		{"curl URL\nwget URL", []string{"5:11: curl: use kit-download", "6:11: wget: use kit-download"}},
		{"apt-get -y update && apt-get -y upgrade", []string{"5:11: apt-get: wrap in kit-retry", "5:32: apt-get: wrap in kit-retry"}},
		{"echo ${{ 'é' }}; curl URL", []string{"5:29: curl: use kit-download"}},
		{"yarn", []string{"5:11: yarn: wrap in kit-retry"}},
		{"yarn --frozen-lockfile", []string{"5:11: yarn: wrap in kit-retry"}},
		{"yarn --cwd web", []string{"5:11: yarn: wrap in kit-retry"}},
		{"yarn --version", nil},
		{"yarn --cwd web build", nil},
	} {
		t.Run(tt.script, func(t *testing.T) {
			src := "jobs:\n  test:\n    steps:\n      - run: |\n          " + strings.ReplaceAll(tt.script, "\n", "\n          ") + "\n"
			findings, err := Scan([]byte(src))
			require.NoError(t, err)
			var got []string
			for _, finding := range findings {
				got = append(got, fmt.Sprintf("%d:%d: %s", finding.Line, finding.Column, finding.Message))
			}
			assert.Equal(t, tt.messages, got)
		})
	}
}

func TestScanShellsAndComposite(t *testing.T) {
	for _, src := range []string{
		"defaults: {run: {shell: pwsh}}\njobs: {test: {steps: [{run: curl URL}]}}",
		"jobs: {test: {defaults: {run: {shell: pwsh}}, steps: [{run: curl URL}]}}",
	} {
		findings, err := Scan([]byte(src))
		require.NoError(t, err)
		assert.Empty(t, findings)
	}
	findings, err := Scan([]byte("runs: {using: composite, steps: [{shell: bash, run: 'curl URL'}]}"))
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "composite", findings[0].Job)
}

func TestScanPositions(t *testing.T) {
	for _, tt := range []struct {
		name, src    string
		line, column int
	}{
		{"plain", "jobs: {test: {steps: [{run: true && curl URL}]}}", 1, 37},
		{"quoted uses run key", "jobs: {test: {steps: [{run: 'curl URL'}]}}", 1, 24},
		{"folded uses run key", "jobs:\n  test:\n    steps:\n      - run: >\n          echo x;\n          curl URL\n", 4, 9},
		{"literal with CRLF", "jobs:\r\n  test:\r\n    steps:\r\n      - run: |\r\n          echo\r\n            curl URL\r\n", 6, 13},
	} {
		t.Run(tt.name, func(t *testing.T) {
			findings, err := Scan([]byte(tt.src))
			require.NoError(t, err)
			require.Len(t, findings, 1)
			assert.Equal(t, tt.line, findings[0].Line)
			assert.Equal(t, tt.column, findings[0].Column)
		})
	}
}
