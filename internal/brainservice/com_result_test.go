package brainservice

import (
	"errors"
	"testing"

	ole "github.com/go-ole/go-ole"
	"github.com/stretchr/testify/require"
)

func TestBrainServiceCOMInitializationResult(t *testing.T) {
	require.NoError(t, comInitializationResult(nil))
	require.NoError(t, comInitializationResult(ole.NewError(1)), "S_FALSE is successful COM initialization and requires balanced uninitialize")
	for _, err := range []error{ole.NewError(0x80010106), ole.NewError(0x80070005), errors.New("synthetic initialization failure")} {
		require.ErrorIs(t, comInitializationResult(err), err)
	}
}
