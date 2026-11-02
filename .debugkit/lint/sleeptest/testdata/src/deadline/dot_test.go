package deadline

import (
	"testing"
	"time"

	. "github.com/stretchr/testify/assert"
)

func TestDotImportedAssertion(t *testing.T) {
	Eventually(t, ready, 80*time.Millisecond, time.Millisecond) // want "assert.Eventually with budget 80ms"
}
