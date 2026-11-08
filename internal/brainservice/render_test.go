package brainservice

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func renderSpec(goos string) Spec {
	if goos == "windows" {
		return Spec{Name: "synthetic", Executable: `C:\Program Files\Docbank\docbank.exe`, ConfigRoot: `C:\Test Files\café $&%\brain`, UserID: "S-1-5-21-100-200-300-1001", UserHome: `C:\Users\Synthetic`, UserConfigRoot: `C:\Users\Synthetic\AppData\Roaming`}
	}
	exe := `/opt/Test Files/café "quote" \ $%&<>/docbank`
	if goos == "linux" {
		exe = `/opt/Test Files/café $%&<>/docbank`
	}
	return Spec{Name: "synthetic", Executable: exe, ConfigRoot: `/tmp/Synthetic/café "quote" \ $%&<>/brain`, UserID: "1001", UserHome: "/tmp/Synthetic", UserConfigRoot: "/tmp/Synthetic/.config"}
}

// The decoder is deliberately separate from Render: plist/XML use generic
// parsers and systemd quoting is interpreted as tokens, not reversed replaces.
func decodedArgv(t testing.TB, goos string, data []byte) []string {
	t.Helper()
	if goos == "linux" {
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "ExecStart=") {
				return decodeUnit(t, strings.TrimPrefix(line, "ExecStart="))
			}
		}
		t.Fatal("missing ExecStart")
		return nil
	}
	if goos == "windows" {
		var task struct {
			Actions struct {
				Exec struct{ Command, Arguments string }
			}
		}
		require.NoError(t, xml.Unmarshal(data, &task))
		return append([]string{task.Actions.Exec.Command}, decodeWindows(t, task.Actions.Exec.Arguments)...)
	}
	var plist struct {
		Dict struct {
			Keys   []string `xml:"key"`
			Arrays []struct {
				Strings []string `xml:"string"`
			} `xml:"array"`
		} `xml:"dict"`
	}
	require.NoError(t, xml.Unmarshal(data, &plist))
	require.Len(t, plist.Dict.Arrays, 1)
	return plist.Dict.Arrays[0].Strings
}

func decodeUnit(t testing.TB, s string) []string {
	t.Helper()
	var args []string
	literal := strings.HasPrefix(s, ":")
	s = strings.TrimPrefix(s, ":")
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			break
		}
		require.True(t, strings.HasPrefix(s, `"`))
		s = s[1:]
		var b strings.Builder
		for len(s) > 0 {
			c := s[0]
			s = s[1:]
			if c == '"' {
				break
			}
			if c == '\\' {
				require.NotEmpty(t, s)
				c = s[0]
				s = s[1:]
			} else if c == '%' || (c == '$' && !literal) {
				require.NotEmpty(t, s)
				require.Equal(t, c, s[0])
				s = s[1:]
			}
			b.WriteByte(c)
		}
		args = append(args, b.String())
	}
	return args
}

func decodeWindows(t testing.TB, s string) []string {
	t.Helper()
	var args []string
	for len(strings.TrimSpace(s)) > 0 {
		s = strings.TrimLeft(s, " \t")
		var b strings.Builder
		quoted := false
		for len(s) > 0 {
			if !quoted && (s[0] == ' ' || s[0] == '\t') {
				break
			}
			slashes := 0
			for len(s) > 0 && s[0] == '\\' {
				slashes++
				s = s[1:]
			}
			if len(s) > 0 && s[0] == '"' {
				b.WriteString(strings.Repeat(`\`, slashes/2))
				if slashes%2 != 0 {
					b.WriteByte('"')
				} else {
					quoted = !quoted
				}
				s = s[1:]
			} else {
				b.WriteString(strings.Repeat(`\`, slashes))
				if len(s) > 0 {
					b.WriteByte(s[0])
					s = s[1:]
				}
			}
		}
		require.False(t, quoted)
		args = append(args, b.String())
	}
	return args
}

func TestBrainServiceRenderArgv(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			spec := renderSpec(goos)
			reg, err := Render(goos, spec)
			require.NoError(t, err)
			want := []string{spec.Executable, "brain", "--config-root", spec.ConfigRoot, "run", spec.Name}
			require.Equal(t, want, reg.Argv)
			require.Equal(t, want, decodedArgv(t, goos, reg.Definition))
			require.NotContains(t, string(reg.Definition), "Restart=always")
			require.NotContains(t, string(reg.Definition), "KeepAlive")
			if goos == "windows" {
				var task struct {
					Principals struct {
						Principal struct {
							UserID              string `xml:"UserId"`
							LogonType, RunLevel string
						}
					}
					Settings struct{ MultipleInstancesPolicy, ExecutionTimeLimit string }
					Triggers struct {
						LogonTrigger struct {
							UserID string `xml:"UserId"`
						}
					}
				}
				require.NoError(t, xml.Unmarshal(reg.Definition, &task))
				require.Equal(t, spec.UserID, task.Principals.Principal.UserID)
				require.Equal(t, spec.UserID, task.Triggers.LogonTrigger.UserID)
				require.Equal(t, "InteractiveToken", task.Principals.Principal.LogonType)
				require.Equal(t, "LeastPrivilege", task.Principals.Principal.RunLevel)
				require.Equal(t, "IgnoreNew", task.Settings.MultipleInstancesPolicy)
				require.Equal(t, "PT0S", task.Settings.ExecutionTimeLimit)
			}
			second := spec
			second.ConfigRoot += "-other"
			other, err := Render(goos, second)
			require.NoError(t, err)
			require.NotEqual(t, reg.ID, other.ID)
		})
	}
}

func TestBrainServiceRenderUsesPublisherNameRules(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, name := range []string{"_notes", ".notes", "-notes"} {
			spec := renderSpec(goos)
			spec.Name = name
			reg, err := Render(goos, spec)
			require.NoError(t, err)
			require.Equal(t, name, reg.Argv[len(reg.Argv)-1])
		}
		spec := renderSpec(goos)
		spec.Name = "Uppercase"
		_, err := Render(goos, spec)
		require.ErrorIs(t, err, ErrInvalidSpec)
	}
}

func TestBrainServiceRenderRejectsUnrepresentable(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, bad := range []string{"\x00", "\n", "\r", "\t"} {
			t.Run(goos+fmt.Sprintf("-%x", bad), func(t *testing.T) {
				spec := renderSpec(goos)
				spec.ConfigRoot += bad
				_, err := Render(goos, spec)
				require.Error(t, err)
			})
		}
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, field := range []string{"Name", "Executable", "ConfigRoot", "UserID", "UserHome", "UserConfigRoot"} {
			t.Run(goos+field, func(t *testing.T) {
				spec := renderSpec(goos)
				reflect.ValueOf(&spec).Elem().FieldByName(field).SetString("")
				_, err := Render(goos, spec)
				require.Error(t, err)
			})
		}
	}
	spec := renderSpec("windows")
	spec.ConfigRoot = `C:\test\%USERPROFILE%`
	_, err := Render("windows", spec)
	require.Error(t, err, "Task Scheduler expands paired percent variables")
}

func TestBrainServiceDryRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nonexistent")
	spec := renderSpec(runtime.GOOS)
	spec.ConfigRoot = root
	if runtime.GOOS == "windows" {
		spec.ConfigRoot = filepath.Join(t.TempDir(), "nonexistent")
	}
	_, err := Render(runtime.GOOS, spec)
	require.NoError(t, err)
	_, err = os.Stat(spec.ConfigRoot)
	require.True(t, os.IsNotExist(err), "pure rendering must not create directories")
}

func TestMain(m *testing.M) {
	if os.Getenv("DOCBANK_TEST_BRAIN_ARGV_CHILD") == "1" {
		for _, arg := range os.Args {
			fmt.Printf("%q\n", arg)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func nativeArgvChild(t *testing.T) {
	t.Helper()
	spec := renderSpec(runtime.GOOS)
	source, err := os.Executable()
	require.NoError(t, err)
	exe := filepath.Join(t.TempDir(), "Docbank café $&%", "docbank")
	require.NoError(t, os.MkdirAll(filepath.Dir(exe), 0o700))
	binary, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(exe, binary, 0o700))
	spec.Executable = exe
	reg, err := Render(runtime.GOOS, spec)
	require.NoError(t, err)
	args := decodedArgv(t, runtime.GOOS, reg.Definition)
	cmd := exec.CommandContext(t.Context(), args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "DOCBANK_TEST_BRAIN_ARGV_CHILD=1")
	out, err := cmd.Output()
	require.NoError(t, err)
	var expected strings.Builder
	for _, arg := range []string{exe, "brain", "--config-root", spec.ConfigRoot, "run", spec.Name} {
		fmt.Fprintf(&expected, "%q\n", arg)
	}
	require.Equal(t, expected.String(), string(out))
}

func TestBrainServiceSystemdLiteralExecutable(t *testing.T) {
	spec := renderSpec("linux")
	spec.Executable = "/opt/$PUBLISHER/docbank"
	reg, err := Render("linux", spec)
	require.NoError(t, err)
	// systemd resolves its executable separately, before environment expansion
	// of argv. Disable expansion rather than doubling $ in the executable path.
	require.Contains(t, string(reg.Definition), `ExecStart=:"`)
	require.Equal(t, reg.Argv, decodedArgv(t, "linux", reg.Definition))
}
func TestBrainServiceWindowsPairedPercentRejected(t *testing.T) {
	for _, value := range []string{`C:\test\%USERPROFILE%`, `C:\test\one%two%three`, `C:\test\%%`} {
		spec := renderSpec("windows")
		spec.ConfigRoot = value
		_, err := Render("windows", spec)
		require.ErrorIs(t, err, ErrPercentExpansion)
	}
}

func TestBrainServiceSystemdRejectsUnsupportedExecutable(t *testing.T) {
	for _, ch := range []string{`"`, `'`, `\`, `*`, `?`, `[`, `]`} {
		spec := renderSpec("linux")
		spec.Executable = "/opt/synthetic" + ch + "/docbank"
		_, err := Render("linux", spec)
		require.ErrorIs(t, err, ErrInvalidSpec)
	}
}
func TestBrainServiceRejectsLossyXML(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		spec := renderSpec(goos)
		spec.ConfigRoot += "\ufffe"
		_, err := Render(goos, spec)
		require.ErrorIs(t, err, ErrInvalidSpec)
	}
}
