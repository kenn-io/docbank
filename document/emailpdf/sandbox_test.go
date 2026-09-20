package emailpdf

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
)

func TestSandboxDeniesHostAccessAndAllowsStagedFiles(t *testing.T) {
	if os.Getenv("DOCBANK_EMAILPDF_SANDBOX_HELPER") == "1" {
		input, err := os.ReadFile("/work/input")
		require.NoError(t, err)
		require.Equal(t, "synthetic allowed input", string(input))
		require.NoError(t, os.WriteFile("/work/output", []byte("synthetic allowed output"), 0o600))

		hostFile := os.Getenv("DOCBANK_EMAILPDF_HOST_FILE")
		_, err = os.ReadFile(hostFile)
		require.Error(t, err, "renderer must not read files outside its staging")
		require.Error(t, os.WriteFile(hostFile, []byte("unexpected host mutation"), 0o600))
		_, err = os.Stat("/proc/" + os.Getenv("DOCBANK_EMAILPDF_HOST_PID"))
		require.ErrorIs(t, err, os.ErrNotExist, "renderer must not see the host test process")
		connection, err := net.DialTimeout("tcp", os.Getenv("DOCBANK_EMAILPDF_HOST_LISTENER"), time.Second)
		if connection != nil {
			_ = connection.Close()
		}
		require.Error(t, err, "renderer must not connect to the host listener")
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("renderer isolation requires Linux")
	}
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skipf("distribution bubblewrap is unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	manager := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=ControlGroup", "--value")
	manager.Env = userManagerEnv()
	group, err := manager.Output()
	if err != nil {
		t.Skipf("systemd user manager is unavailable: %v", err)
	}
	controllers, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(strings.TrimSpace(string(group)), "/"), "cgroup.controllers"))
	if err != nil || !slices.Contains(strings.Fields(string(controllers)), "memory") || !slices.Contains(strings.Fields(string(controllers)), "pids") {
		t.Skip("systemd user manager needs cgroup v2 memory and pids delegation")
	}

	root := t.TempDir()
	bundle, fonts, work := filepath.Join(root, "bundle"), filepath.Join(root, "fonts"), filepath.Join(root, "work")
	for _, dir := range []string{bundle, fonts, work} {
		require.NoError(t, os.Mkdir(dir, 0o700))
	}
	chromium := filepath.Join(bundle, "synthetic-chromium")
	require.NoError(t, os.WriteFile(chromium, []byte("synthetic bundle inventory"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(fonts, "synthetic-font"), []byte("synthetic font inventory"), 0o600))
	worker, err := os.Executable()
	require.NoError(t, err)
	workerHash, err := FileSHA256(worker)
	require.NoError(t, err)
	bubblewrapHash, err := FileSHA256(bubblewrap)
	require.NoError(t, err)
	bundleHash, err := TreeSHA256(bundle)
	require.NoError(t, err)
	fontsHash, err := TreeSHA256(fonts)
	require.NoError(t, err)
	r, err := NewRuntime(RuntimeConfig{
		Worker: worker, WorkerSHA256: workerHash,
		Bubblewrap: bubblewrap, BubblewrapSHA256: bubblewrapHash,
		Chromium: chromium, Version: "synthetic", Bundle: bundle, BundleSHA256: bundleHash,
		Fonts: fonts, FontsSHA256: fontsHash, Spool: root,
	})
	require.NoError(t, err)
	require.NoError(t, r.CheckAvailable(ctx))

	hostFile := filepath.Join(root, "outside-work")
	require.NoError(t, os.WriteFile(hostFile, []byte("synthetic host contents"), 0o600))
	control, err := os.ReadFile(hostFile)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	connection, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err, "host control must reach its listener")
	require.NoError(t, connection.Close())
	hostPID := strconv.Itoa(os.Getpid())
	_, err = os.Stat("/proc/" + hostPID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(work, "input"), []byte("synthetic allowed input"), 0o600))

	unit := "docbank-email-pdf-" + uuid.New().String() + ".service"
	t.Cleanup(func() { require.NoError(t, stopUnit(context.Background(), unit)) })
	command := r.command(ctx, work, unit, "/usr/bin/env",
		"DOCBANK_EMAILPDF_SANDBOX_HELPER=1", "DOCBANK_EMAILPDF_HOST_FILE="+hostFile,
		"DOCBANK_EMAILPDF_HOST_PID="+hostPID, "DOCBANK_EMAILPDF_HOST_LISTENER="+listener.Addr().String(),
		"/runtime/worker", "-test.run=^TestSandboxDeniesHostAccessAndAllowsStagedFiles$",
	)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	runErr := command.Run()
	after, err := os.ReadFile(hostFile)
	require.NoError(t, err)
	require.Equal(t, control, after, "host fixture must remain unchanged")
	require.NoError(t, runErr, "sandbox helper: %s", output.String())
	staged, err := os.ReadFile(filepath.Join(work, "output"))
	require.NoError(t, err)
	require.Equal(t, "synthetic allowed output", string(staged))
}
