package processing

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/config"
)

func TestFaceEmbeddingCrowdFitsDescriptorBudget(t *testing.T) {
	t.Parallel()
	if os.Getenv("DOCBANK_FACE_DESCRIPTOR_CHILD") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestFaceEmbeddingCrowdFitsDescriptorBudget$", "-test.timeout=90s")
		command.Env = append(os.Environ(), "DOCBANK_FACE_DESCRIPTOR_CHILD=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	x := newFaceEmbeddingFixture(t, false)
	x.matchAll = true
	x.worker.maxRows = 128
	for i := len(x.inputs); i < 128; i++ {
		_, _, err := x.f.catalog.CreatePhotoFace(t.Context(), x.f.node.CurrentVersionID, uuid.New().String(), document.FaceBox{X: .2, Y: .2, Width: .2, Height: .2}, "")
		require.NoError(t, err)
	}
	x.f.grant(t, x.service, config.LocalFacesProfile)
	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit))
	limit.Cur = min(limit.Max, 256)
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit))
	x.worker.reconcileAt = time.Time{}
	n, err := x.worker.ScanOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	sets, err := x.f.catalog.FaceMatchingInputs(t.Context())
	require.NoError(t, err)
	require.Len(t, sets.Sets, 1)
	require.Equal(t, 128, sets.Sets[0].Set.VectorSet.RowCount)
	require.Len(t, x.boxes, 128)
}
