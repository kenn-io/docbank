package ingest

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func TestSourceChangeWindowsBasicInfoABI(t *testing.T) {
	require.Equal(t, uintptr(40), unsafe.Sizeof(fileBasicInfo{}))
	require.Equal(t, uintptr(24), unsafe.Offsetof(fileBasicInfo{}.changeTime))
	require.Equal(t, uintptr(32), unsafe.Offsetof(fileBasicInfo{}.fileAttributes))
}
