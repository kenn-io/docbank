package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateHelpGolden = flag.Bool("update", false, "update CLI help golden files")
var helpPlaceholder = regexp.MustCompile(`<[^<>\s]+>`)
var helpEnvAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*=`)

// Parents whose RunE performs work, rather than just cmd.Help().
var workParents = map[string]bool{
	"docbank jobs": true, "docbank mv": true, "docbank package import": true,
}

var helpAllCapsArgument = regexp.MustCompile(`^[A-Z]+$`)

func TestHelpTreeLint(t *testing.T) {
	defer resetFlags(rootCmd)
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
			return
		}
		t.Run(cmd.CommandPath(), func(t *testing.T) {
			require.NotEmpty(t, cmd.Short)
			assert.LessOrEqual(t, utf8.RuneCountInString(cmd.Short), 80, "Short must fit on one line")
			assert.NotContains(t, cmd.Short, "\n")
			assert.False(t, strings.HasSuffix(cmd.Short, "."), "Short has a trailing period")
			for _, prefix := range []string{"Status a ", "Receipts a ", "Watch a "} {
				assert.False(t, strings.HasPrefix(cmd.Short, prefix), "use an imperative Short")
			}
			if cmd.HasSubCommands() {
				assert.NotEmpty(t, cmd.Long, "parents must explain their workflow")
			}
			for line := range strings.SplitSeq(cmd.Long, "\n") {
				assert.LessOrEqual(t, utf8.RuneCountInString(line), 80, "wrap Long at 80 columns: %s", line)
			}
			if cmd != rootCmd && cmd.Name() != "tui" && cmd.Long != "" {
				lineLimit := 12
				if cmd.HasSubCommands() {
					lineLimit = 20
				}
				assert.LessOrEqual(t, len(strings.Split(cmd.Long, "\n")), lineLimit, "keep Long focused")
			}
			if cmd.Parent() == rootCmd {
				assert.NotEmpty(t, cmd.GroupID, "top-level command needs a group")
			}
			for _, old := range []string{"<vault-path-or-id>", "<path-or-node-id>", "node-selector", "<path|id:N>", "<node-id>"} {
				assert.NotContains(t, cmd.Use, old, "use the common selector vocabulary")
			}
			for _, word := range strings.Fields(cmd.Use)[1:] {
				assert.False(t, helpAllCapsArgument.MatchString(strings.Trim(word, "[]")), "lowercase positional placeholder: %s", word)
			}
			flags := pflag.NewFlagSet("help", pflag.ContinueOnError)
			flags.AddFlagSet(cmd.Flags())
			flags.AddFlagSet(cmd.PersistentFlags())
			flags.AddFlagSet(cmd.InheritedFlags())
			flags.VisitAll(func(f *pflag.Flag) {
				assert.NotEmpty(t, f.Usage, "--%s needs a description", f.Name)
				assert.False(t, strings.HasSuffix(f.Usage, "."), "--%s has a trailing period", f.Name)
				first, _ := utf8.DecodeRuneInString(f.Usage)
				acronym := false
				for _, allowed := range []string{"URL", "MIME", "UUID", "JSON", "RFC3339", "EML", "MBOX", "A4"} {
					acronym = acronym || f.Usage == allowed || strings.HasPrefix(f.Usage, allowed+" ")
				}
				assert.True(t, unicode.IsLower(first) || !unicode.IsLetter(first) || acronym, "--%s starts uppercase: %s", f.Name, f.Usage)
				if f.Name == "json" {
					assert.True(t, strings.HasPrefix(f.Usage, "print JSON"), "standardize --json: %s", f.Usage)
				}
			})
			if (cmd.Runnable() && !cmd.HasSubCommands()) || workParents[cmd.CommandPath()] {
				require.NotEmpty(t, cmd.Example, "runnable commands must have examples")
			}
			if cmd.Example != "" {
				own := false
				lines := 0
				for line := range strings.SplitSeq(strings.ReplaceAll(cmd.Example, "\\\n", " "), "\n") {
					if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
						continue
					}
					lines++
					resolved, err := checkHelpExample(rootCmd, line)
					require.NoError(t, err, "example: %s", line)
					own = own || resolved == cmd
				}
				if (cmd.Runnable() && !cmd.HasSubCommands()) || workParents[cmd.CommandPath()] {
					assert.True(t, own, "examples must include this command (prerequisite commands are allowed)")
				}
				assert.LessOrEqual(t, lines, 4, "keep examples short")
			}
			out, err := captureHelp(rootCmd, cmd)
			require.NoError(t, err)
			budget := 2500
			if cmd == rootCmd {
				budget = 4550
			}
			if cmd == rootCmd || (!cmd.HasSubCommands() && cmd.Name() != "tui") {
				assert.LessOrEqual(t, len(out), budget, "help byte budget")
			}
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)
}

// Capture through the root so children keep inheriting its output. Restoring a
// child's effective writer would accidentally pin it to a previous test's buffer.
func captureHelp(root, cmd *cobra.Command) (string, error) {
	var out bytes.Buffer
	oldOut, oldErr := root.OutOrStdout(), root.ErrOrStderr()
	root.SetOut(&out)
	root.SetErr(&out)
	defer func() { root.SetOut(oldOut); root.SetErr(oldErr) }()
	err := cmd.Help()
	return out.String(), err
}

func TestHelpCapturePreservesInheritedOutput(t *testing.T) {
	root := &cobra.Command{Use: "docbank"}
	child := &cobra.Command{Use: "probe", Short: "Print a probe", Run: func(cmd *cobra.Command, _ []string) { _, _ = fmt.Fprint(cmd.OutOrStdout(), "probe") }}
	root.AddCommand(child)
	var first, second bytes.Buffer
	root.SetOut(&first)
	help, err := captureHelp(root, child)
	require.NoError(t, err)
	assert.Contains(t, help, "Print a probe")
	assert.Empty(t, first.String())
	root.SetOut(&second)
	root.SetArgs([]string{"probe"})
	require.NoError(t, root.Execute())
	assert.Equal(t, "probe", second.String())
	assert.Empty(t, first.String())
}

func TestRootHelpGolden(t *testing.T) {
	out, err := runCLI(t, "--help")
	require.NoError(t, err)
	const path = "testdata/help/root.golden"
	if *updateHelpGolden {
		require.NoError(t, os.MkdirAll("testdata/help", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(out), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "regenerate with -run TestRootHelpGolden -update")
	wantText := strings.ReplaceAll(string(want), "\r\n", "\n")
	gotText := strings.ReplaceAll(out, "\r\n", "\n")
	assert.Equal(t, wantText, gotText)
}

// exampleWords accepts the small shell subset used in CLI examples. It never
// expands variables, invokes a shell, or executes a command.
func exampleWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for _, r := range line {
		if escaped {
			if r != '\n' {
				word.WriteRune(r)
				started = true
			}
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '#' && !started:
			flush()
			return words, nil
		case r == ';' || r == '|' || r == '&':
			return nil, errors.New("unsupported shell operator")
		case unicode.IsSpace(r):
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape")
	}
	flush()
	return words, nil
}

func checkHelpExample(root *cobra.Command, line string) (*cobra.Command, error) {
	words, err := exampleWords(line)
	if err != nil {
		return nil, err
	}
	for len(words) > 0 && helpEnvAssignment.MatchString(words[0]) {
		words = words[1:]
	}
	if len(words) < 2 || words[0] != "docbank" {
		return nil, errors.New("example must start with docbank and a command")
	}
	for i, w := range words {
		if w == ">" || w == ">>" {
			if i+2 != len(words) {
				return nil, errors.New("only a trailing output redirect is supported")
			}
			words = words[:i]
			break
		}
		words[i] = helpPlaceholder.ReplaceAllString(w, "x")
		if strings.ContainsAny(words[i], "<>") {
			return nil, errors.New("unsupported redirect or malformed placeholder")
		}
	}
	cmd, args, err := root.Find(words[1:])
	if err != nil {
		return nil, fmt.Errorf("resolving help example: %w", err)
	}
	if cmd == root || cmd.Hidden || cmd.Name() == "help" || cmd.Name() == "completion" {
		return nil, errors.New("example must resolve to a public command")
	}
	flags, err := cloneHelpFlags(cmd)
	if err != nil {
		return nil, err
	}
	if err = flags.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing help example flags: %w", err)
	}
	// This validator also closes over a production flag variable. Validate its
	// arity and required target against the isolated copy instead.
	if cmd == backupRestoreCmd {
		if err = cobra.MaximumNArgs(1)(cmd, flags.Args()); err != nil {
			return nil, fmt.Errorf("validating backup restore example: %w", err)
		}
		target, _ := flags.GetString("target")
		if target == "" {
			return nil, errors.New("backup restore example needs --target")
		}
	} else if cmd.Args != nil {
		if err = cmd.Args(cmd, flags.Args()); err != nil {
			return nil, fmt.Errorf("validating help example arguments: %w", err)
		}
	}
	return cmd, nil
}

// Register fresh typed values: copying pflag.Flag pointers or Values still
// writes package globals, including hidden slice/map changed state.
func cloneHelpFlags(cmd *cobra.Command) (*pflag.FlagSet, error) {
	result := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	result.SetOutput(io.Discard)
	source := pflag.NewFlagSet("source", pflag.ContinueOnError)
	source.AddFlagSet(cmd.Flags())
	source.AddFlagSet(cmd.PersistentFlags())
	source.AddFlagSet(cmd.InheritedFlags())
	var err error
	source.VisitAll(func(f *pflag.Flag) {
		switch f.Value.Type() {
		case "bool":
			result.BoolP(f.Name, f.Shorthand, false, f.Usage)
		case "string":
			result.StringP(f.Name, f.Shorthand, "", f.Usage)
		case "int":
			result.IntP(f.Name, f.Shorthand, 0, f.Usage)
		case "int64":
			result.Int64P(f.Name, f.Shorthand, 0, f.Usage)
		case "duration":
			result.DurationP(f.Name, f.Shorthand, 0, f.Usage)
		case "stringSlice":
			result.StringSliceP(f.Name, f.Shorthand, nil, f.Usage)
		case "stringArray":
			result.StringArrayP(f.Name, f.Shorthand, nil, f.Usage)
		case "stringToString":
			result.StringToStringP(f.Name, f.Shorthand, nil, f.Usage)
		default:
			err = fmt.Errorf("add an isolated clone for flag type %q", f.Value.Type())
			return
		}
		clone := result.Lookup(f.Name)
		clone.NoOptDefVal = f.NoOptDefVal
	})
	return result, err
}

func TestHelpWords(t *testing.T) {
	cases := []struct {
		line string
		want []string
		bad  bool
	}{
		{`docbank search 'merger plan' --json # result`, []string{"docbank", "search", "merger plan", "--json"}, false},
		{`EDITOR="my editor" docbank edit id:12`, []string{"EDITOR=my editor", "docbank", "edit", "id:12"}, false},
		{`docbank search "a\"b" 'a#b'`, []string{"docbank", "search", `a"b`, "a#b"}, false},
		{`docbank cat <path-or-id> > out.txt`, []string{"docbank", "cat", "<path-or-id>", ">", "out.txt"}, false},
		{"docbank search merger\\\n plan", []string{"docbank", "search", "merger", "plan"}, false},
		{`docbank search "unfinished`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			got, err := exampleWords(c.line)
			if c.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, c.want, got)
			}
		})
	}
}

func TestHelpExampleParsing(t *testing.T) {
	tests := []struct {
		line string
		path string
	}{
		{`docbank cat id:12 > invoice.pdf`, "docbank cat"},
		{`docbank search "merger plan" --json`, "docbank search"},
		{`DOCBANK_HOME=/tmp/scratch docbank info --json`, "docbank info"},
		{`docbank rendition window id:12 --version <version-uuid> --profile <profile>`, "docbank rendition window"},
		{`docbank backup restore --repo ./bk --target ./restored`, "docbank backup restore"},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			cmd, err := checkHelpExample(rootCmd, tt.line)
			require.NoError(t, err)
			assert.Equal(t, tt.path, cmd.CommandPath())
		})
	}
	for _, line := range []string{`docbank not-real`, `docbank cat id:12 --not-real`, `docbank get id:12`, `docbank backup restore --repo ./bk`, `docbank cat id:12 > out extra`} {
		t.Run(line, func(t *testing.T) { _, err := checkHelpExample(rootCmd, line); require.Error(t, err) })
	}
}

func TestHelpExampleFlagsAreIsolated(t *testing.T) {
	cmd := &cobra.Command{Use: "probe"}
	flags := cmd.Flags()
	s := "before"
	v := []string{"before"}
	m := map[string]string{"before": "value"}
	arr := []string{"before"}
	enabled := false
	flags.StringArrayVar(&arr, "array", arr, "")
	flags.BoolVarP(&enabled, "enabled", "e", false, "")
	flags.StringVar(&s, "string", "before", "")
	flags.StringSliceVar(&v, "slice", v, "")
	flags.StringToStringVar(&m, "map", m, "")
	cloned, err := cloneHelpFlags(cmd)
	require.NoError(t, err)
	require.NoError(t, cloned.Parse([]string{"--string", "after", "--slice", "after", "--map", "after=value", "--array", "after", "-e"}))
	assert.Equal(t, []string{"before"}, arr)
	assert.False(t, enabled)
	assert.Equal(t, "before", s)
	assert.Equal(t, []string{"before"}, v)
	assert.Equal(t, map[string]string{"before": "value"}, m)
	flags.VisitAll(func(f *pflag.Flag) { assert.False(t, f.Changed) })
	got, err := cloned.GetBool("enabled")
	require.NoError(t, err)
	assert.True(t, got)
	require.NoError(t, flags.Parse([]string{"--slice", "first", "--map", "first=value"}))
	assert.Equal(t, []string{"first"}, v)
	assert.Equal(t, map[string]string{"first": "value"}, m)
}

// The shell subset is parser-only; arbitrary input must never panic and quoted
// synthetic words must round-trip without command or variable expansion.
func FuzzHelpWords(f *testing.F) {
	for _, word := range []string{"merger plan", "invoice.pdf", "a#b", `a"b`, "", "id:12"} {
		f.Add(word)
	}
	f.Fuzz(func(t *testing.T, word string) {
		if !utf8.ValidString(word) || strings.ContainsAny(word, "\x00\r\n") {
			t.Skip()
		}
		quoted := "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
		got, err := exampleWords("docbank search " + quoted)
		require.NoError(t, err)
		assert.Equal(t, []string{"docbank", "search", word}, got)
	})
}

func TestHelpExampleInheritedFlagsAreIsolated(t *testing.T) {
	root := &cobra.Command{Use: "docbank"}
	child := &cobra.Command{Use: "probe", Args: cobra.NoArgs}
	value := false
	root.PersistentFlags().BoolVarP(&value, "json", "j", false, "")
	root.AddCommand(child)
	resolved, err := checkHelpExample(root, "docbank probe -j")
	require.NoError(t, err)
	assert.Same(t, child, resolved)
	assert.False(t, value)
	assert.False(t, root.PersistentFlags().Lookup("json").Changed)
}
