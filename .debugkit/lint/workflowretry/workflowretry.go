// Package workflowretry checks workflow and composite-action downloads for retries.
package workflowretry

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kit/lint/internal/yamlnode"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// Finding identifies a download. Line and Column locate the command in a
// literal block or single-line plain run value, and the run key otherwise.
type Finding struct {
	Line, Column       int
	Job, Step, Message string
}

// Scan checks run steps using Bash or sh, including steps with no explicit shell.
func Scan(src []byte) ([]Finding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, nil
	}
	root := doc.Content[0]
	lines := strings.Split(string(src), "\n")
	var findings []Finding
	check := func(steps *yaml.Node, job, shell string) {
		if steps == nil {
			return
		}
		for i, step := range steps.Content {
			for step.Kind == yaml.AliasNode {
				step = step.Alias
			}
			run := value(step, "run")
			if run == nil {
				continue
			}
			selected := text(step, "shell")
			if selected == "" {
				selected = shell
			}
			if skipShell(selected) {
				continue
			}
			name := text(step, "name")
			if name == "" {
				name = strconv.Itoa(i + 1)
			}
			key := run
			for k := 0; k+1 < len(step.Content); k += 2 {
				if step.Content[k].Value == "run" {
					key = step.Content[k]
					break
				}
			}
			script := strings.Split(run.Value, "\n")
			for _, hit := range scanScript(run.Value) {
				line, column := key.Line, key.Column
				switch {
				case run.Style == yaml.LiteralStyle && run.Line+hit.line <= len(lines):
					// A literal block keeps each line verbatim after its indentation.
					line = run.Line + hit.line
					indent := len(strings.TrimSuffix(lines[line-1], "\r")) - len(script[hit.line-1])
					column = indent + hit.column
				case run.Style == 0 && len(script) == 1:
					line, column = run.Line, run.Column+hit.column-1
				}
				findings = append(findings, Finding{line, column, job, name, hit.message})
			}
		}
	}
	if jobs := value(root, "jobs"); jobs != nil {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			job := jobs.Content[i+1]
			shell := text(value(value(job, "defaults"), "run"), "shell")
			if shell == "" {
				shell = text(value(value(root, "defaults"), "run"), "shell")
			}
			check(value(job, "steps"), jobs.Content[i].Value, shell)
		}
	}
	if runs := value(root, "runs"); text(runs, "using") == "composite" {
		check(value(runs, "steps"), "composite", "")
	}
	return findings, nil
}

func value(node *yaml.Node, key string) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	result := yamlnode.MappingValue(node, key)
	for result != nil && result.Kind == yaml.AliasNode {
		result = result.Alias
	}
	return result
}

func text(node *yaml.Node, key string) string {
	if v := value(node, key); v != nil {
		return v.Value
	}
	return ""
}

// skipShell reports a shell known not to be Bash or sh; an Actions expression may resolve to Bash.
func skipShell(shell string) bool {
	fields := strings.Fields(shell)
	if len(fields) == 0 || strings.Contains(shell, "${{") {
		return false
	}
	return executable(fields[0]) != "bash" && executable(fields[0]) != "sh"
}

func executable(name string) string {
	return strings.TrimSuffix(path.Base(strings.ReplaceAll(name, `\`, "/")), ".exe")
}

var (
	expressions = regexp.MustCompile(`(?s)\$\{\{.*?\}\}`)
	// installers matches the command words that must run under kit-retry.
	installers = regexp.MustCompile(`^(?:(?:apt|apt-get) (?:install|update|upgrade|dist-upgrade)|go (?:install|get|mod download)|(?:npm|pnpm|yarn|bun) (?:install|ci|i|add)|(?:pip|pip3) install|docker pull|apk (?:add|update)|(?:brew|cargo|pipx|dnf) install)(?: |$)`)
)

// hit is a reported call, positioned by script line and byte column.
type hit struct {
	line, column int
	message      string
}

// scanScript applies the download and installer rules to executable shell calls.
func scanScript(source string) []hit {
	// Filler keeps the parser away from Actions expressions and keeps byte positions.
	masked := expressions.ReplaceAllStringFunc(source, func(span string) string {
		filler := []byte(span)
		for i, c := range filler {
			if c != '\n' {
				filler[i] = 'x'
			}
		}
		return string(filler)
	})
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(strings.NewReader(masked), "")
	if err != nil {
		return nil
	}
	allowed := map[uint]bool{}
	lines := strings.Split(masked, "\n")
	syntax.Walk(file, func(node syntax.Node) bool {
		if c, ok := node.(*syntax.Comment); ok {
			if reason, ok := strings.CutPrefix(c.Text, " workflowretry: allow "); ok && strings.TrimSpace(reason) != "" {
				line := c.Pos().Line()
				allowed[line] = true
				if strings.TrimSpace(lines[line-1][:c.Pos().Col()-1]) == "" {
					allowed[line+1] = true
				}
			}
		}
		return true
	})
	var findings []hit
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || allowed[call.Pos().Line()] {
			return true
		}
		var args []string
		for _, word := range call.Args {
			args = append(args, literal(word))
		}
		args = unprefix(args)
		wrapped := len(args) >= 4 && args[0] == "kit-retry"
		if wrapped {
			args = unprefix(args[3:])
		}
		if len(args) >= 3 && (args[0] == "python" || args[0] == "python3") && args[1] == "-m" && args[2] == "pip" {
			args = args[2:]
		}
		var message string
		switch {
		case len(args) > 0 && (args[0] == "curl" || args[0] == "wget"):
			message = args[0] + ": use kit-download"
		case !wrapped && installer(args):
			message = args[0] + ": wrap in kit-retry"
		default:
			return true
		}
		findings = append(findings, hit{int(call.Pos().Line()), int(call.Pos().Col()), message})
		return true
	})
	return findings
}

// installer stops at the first subcommand, allowing flags and their values
// before it. Yarn installs when no subcommand follows.
func installer(args []string) bool {
	for i := 1; i < len(args); i++ {
		if installers.MatchString(args[0] + " " + strings.Join(args[i:], " ")) {
			return true
		}
		if !strings.HasPrefix(args[i], "-") && (!strings.HasPrefix(args[i-1], "-") || strings.Contains(args[i-1], "=")) {
			return false
		}
	}
	return len(args) > 0 && args[0] == "yarn" && !slices.ContainsFunc(args[1:], func(arg string) bool {
		return arg == "-v" || arg == "--version" || arg == "-h" || arg == "--help"
	})
}

// literal decodes a word's literal and quoted parts; any expansion makes it non-literal.
func literal(word *syntax.Word) string {
	for _, part := range word.Parts {
		switch part := part.(type) {
		case *syntax.Lit, *syntax.SglQuoted:
		case *syntax.DblQuoted:
			if literal(&syntax.Word{Parts: part.Parts}) == "\x00" {
				return "\x00"
			}
		default:
			return "\x00"
		}
	}
	fields, err := expand.Fields(nil, word)
	if err != nil || len(fields) != 1 {
		return "\x00"
	}
	return fields[0]
}

// unprefix normalizes executable names and skips command prefixes.
func unprefix(args []string) []string {
	for len(args) > 0 {
		args[0] = executable(args[0])
		if args[0] == "command" && len(args) > 1 && (args[1] == "-v" || args[1] == "-V") {
			return args
		}
		if args[0] != "sudo" && args[0] != "env" && args[0] != "command" && args[0] != "exec" {
			return args
		}
		args = args[1:]
		for len(args) > 0 && (strings.HasPrefix(args[0], "-") || strings.Contains(args[0], "=")) {
			args = args[1:]
		}
	}
	return args
}
