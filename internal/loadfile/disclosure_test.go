package loadfile

import "testing"

func TestPackageFieldSensitiveKeepsOnlyDeclaredPublicFieldsVisible(t *testing.T) {
	for _, key := range []string{"", "custom.contact", "loadfile.actor.blind_copy", "loadfile.file.native"} {
		if !PackageFieldSensitive(key, false) {
			t.Fatalf("%q must be sensitive by default", key)
		}
	}
	for _, key := range []string{"loadfile.label.begin", "loadfile.label.end", "loadfile.label.set", "loadfile.document.name", "loadfile.date.sent", "loadfile.time.sent"} {
		if PackageFieldSensitive(key, false) {
			t.Fatalf("%q is an approved disclosure", key)
		}
	}
	if !PackageFieldSensitive("loadfile.label.begin", true) {
		t.Fatal("explicit sensitivity must override the public catalog")
	}
}
