package a

import (
	"errors"
	"strings"
	"testing"
)

func TestMessagesAreSkippedByDefault(t *testing.T) {
	err := errors.New("boom")
	if !strings.Contains(err.Error(), "boom") {
		t.Skip()
	}
}
