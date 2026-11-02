package b

import (
	"errors"
	"strings"
	"testing"
)

func TestMessagesAreReportedWhenIncluded(t *testing.T) {
	err := errors.New("boom")
	if !strings.Contains(err.Error(), "boom") { // want "matching on err.Error\\(\\) text"
		t.Skip()
	}
}
