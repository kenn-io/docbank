package brainservice

import (
	"path"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"hegel.dev/go/hegel"
)

// Exercise acceptance and rejection together over arbitrary Unicode, not only
// a hand-picked safe alphabet. Bound materialized input, not the draw domain.
func TestBrainServiceArgvProperty(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			hegel.Test(t, func(ht *hegel.T) {
				raw := []rune(hegel.Draw(ht, hegel.Text()))
				if len(raw) > 64 {
					raw = raw[:64]
				}
				value := string(raw)
				spec := renderSpec(goos)
				expectedRoot := path.Clean("/synthetic/" + value + "/registry")
				invalid := strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || r == 0xfffe || r == 0xffff }) >= 0
				if goos == "windows" {
					spec.ConfigRoot = `C:\synthetic\` + value + `\registry`
					invalid = invalid || strings.ContainsAny(value, `:"<>|?*`) || strings.Count(value, "%") > 1
					expectedRoot = `C:` + strings.ReplaceAll(path.Clean("/synthetic/"+strings.ReplaceAll(value, `\`, "/")+"/registry"), "/", `\`)
				} else {
					spec.ConfigRoot = "/synthetic/" + value + "/registry"
				}
				reg, err := Render(goos, spec)
				if invalid {
					if err == nil {
						ht.Fatalf("accepted unrepresentable path %q", value)
					}
					return
				}
				if err != nil {
					ht.Fatalf("rejected representable path %q: %v", value, err)
				}
				want := []string{spec.Executable, "brain", "--config-root", expectedRoot, "run", spec.Name}
				if got := decodedArgv(ht, goos, reg.Definition); !reflect.DeepEqual(want, got) {
					ht.Fatalf("argv %q, want %q", got, want)
				}
			}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
		})
	}
}

func TestBrainServiceIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		goos := hegel.Draw(ht, hegel.SampledFrom([]string{"linux", "darwin", "windows"}))
		spec := renderSpec(goos)
		spec.Name = "source-" + hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789._-").MaxSize(32))
		first, err := Render(goos, spec)
		if err != nil {
			ht.Fatal(err)
		}
		// Equivalent normalized roots must identify one launcher; a distinct name
		// or root must not adopt it. None of the registration IDs exposes the root.
		equivalent := spec
		if goos == "windows" {
			equivalent.ConfigRoot += `\child\..`
		} else {
			equivalent.ConfigRoot += "/child/.."
		}
		same, err := Render(goos, equivalent)
		if err != nil || first.ID != same.ID {
			ht.Fatalf("normalization changed identity: %v", err)
		}
		other := spec
		other.Name += "-other"
		second, err := Render(goos, other)
		if err != nil || first.ID == second.ID {
			ht.Fatalf("different name adopted identity: %v", err)
		}
		other = spec
		other.ConfigRoot += "-other"
		second, err = Render(goos, other)
		if err != nil || first.ID == second.ID {
			ht.Fatalf("different root adopted identity: %v", err)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
