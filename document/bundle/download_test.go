package bundle

import (
	"strings"
	"testing"
)

func TestDownloadBasename(t *testing.T) {
	for _, name := range []string{"", "docbank-bundle.zip", "Review 2026.zip", "résumé.zip"} {
		got, err := DownloadBasename(name)
		want := name
		if want == "" {
			want = "docbank-bundle.zip"
		}
		if err != nil || got != want {
			t.Errorf("valid name %q: %q %v", name, got, err)
		}
	}
	for _, name := range []string{"../out.zip", "a/b.zip", `a\b.zip`, "x\n.zip", "x\u007f.zip", "CON.zip", "a:stream.zip", "foo", "a.zip ", ".zip", strings.Repeat("a", 181) + ".zip"} {
		if _, err := DownloadBasename(name); err == nil {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
}
