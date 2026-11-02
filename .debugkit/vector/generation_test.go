package vector_test

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/vector"
)

func TestGenerationFingerprintIsStableAndOrderIndependent(t *testing.T) {
	assert := assert.New(t)
	a := vector.Generation{
		Model:      "text-embedding-3-small",
		Dimensions: 1536,
		Params:     map[string]string{"pooling": "mean", "prompt": "search"},
	}
	b := vector.Generation{
		Model:      "text-embedding-3-small",
		Dimensions: 1536,
		Params:     map[string]string{"prompt": "search", "pooling": "mean"},
	}

	fp := a.Fingerprint()
	assert.Equal(fp, a.Fingerprint(), "same value fingerprints identically")
	assert.Equal(fp, b.Fingerprint(), "map order does not change fingerprint")
}

func TestGenerationFingerprintIsNotAmbiguousAcrossParams(t *testing.T) {
	assert := assert.New(t)
	// Two params vs a single param whose value embeds what used to be the
	// key/value separator. A naive "key=value\n" join hashes both the
	// same; the JSON encoding keeps them distinct.
	two := vector.Generation{Model: "m", Dimensions: 3, Params: map[string]string{"pooling": "mean", "prompt": "x"}}
	one := vector.Generation{Model: "m", Dimensions: 3, Params: map[string]string{"pooling": "mean\nprompt=x"}}

	assert.NotEqual(two.Fingerprint(), one.Fingerprint())
}

func TestGenerationFingerprintChangesWithSpace(t *testing.T) {
	assert := assert.New(t)
	base := vector.Generation{Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "mean"}}

	cases := map[string]vector.Generation{
		"model":       {Model: "other", Dimensions: 768, Params: map[string]string{"pooling": "mean"}},
		"dimensions":  {Model: "m", Dimensions: 1024, Params: map[string]string{"pooling": "mean"}},
		"param value": {Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "cls"}},
		"extra param": {Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "mean", "prompt": "x"}},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			assert.NotEqual(base.Fingerprint(), g.Fingerprint())
		})
	}
}

// TestGenerationFingerprintPinsCanonicalEncoding locks the exact hash
// preimage. If the canonical form ever changes — sorting, omit behavior,
// number formatting, or a field added to the struct's encoding — this
// fails, forcing a conscious decision rather than a silent shift of every
// persisted fingerprint.
func TestGenerationFingerprintPinsCanonicalEncoding(t *testing.T) {
	g := vector.Generation{Model: "m", Dimensions: 3, Params: map[string]string{"b": "2", "a": "1"}}

	// Keys sorted at every level, zero fields omitted, numbers verbatim.
	const canonical = `{"dimensions":3,"model":"m","params":{"a":"1","b":"2"}}`
	sum := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(sum[:8])

	assert.Equal(t, want, g.Fingerprint())
}

// TestGenerationFieldsAreTracked is a tripwire: adding, removing, or
// renaming a Generation field changes this set. When it fails, decide
// whether the new field affects the vector space. If it does, Fingerprint
// already includes it (it encodes the whole struct); if it must not, tag
// the field json:"-". Then update this expectation and the pinned
// encoding above.
func TestGenerationFieldsAreTracked(t *testing.T) {
	want := []string{"Dimensions", "Model", "Params"}

	fields := reflect.VisibleFields(reflect.TypeFor[vector.Generation]())
	got := make([]string, 0, len(fields))
	for _, f := range fields {
		got = append(got, f.Name)
	}
	sort.Strings(got)

	assert.Equal(t, want, got, "Generation fields changed: review fingerprint impact before updating this tripwire")
}
