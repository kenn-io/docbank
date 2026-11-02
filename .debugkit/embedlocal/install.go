// Package embedlocal installs a shared local embedding server without loading ONNX Runtime.
//
// Call Install with the shared directory chosen by server discovery and startup
// and an app-owned Confirm callback. The callback receives approximate missing bytes
// before network access. A fresh install needs about 275 MB of model files plus
// a 20 MB bundle allowance. Refusal, cancellation, or a missing callback returns
// an error. Launch Installation.Executable with --data-dir Installation.DataDir;
// both paths are absolute.
package embedlocal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/selfupdate"
)

// InstallOptions controls installation and repair. Apps share DataDir,
// conventionally ~/.kenn/embeddings, and own the confirmation prompt.
type InstallOptions struct {
	DataDir string
	// Confirm runs once before any network request with the approximate missing
	// download bytes. A missing callback or refusal prevents downloads.
	Confirm func(context.Context, int64) (bool, error)
}

// Installation contains absolute paths to pass to the server launcher.
type Installation struct{ Executable, DataDir string }

// Bundle downloads include a platform-dependent native runtime and executable.
const bundleDownloadBytes = 20_000_000

// Install reuses the pinned server offline or confirms before downloading missing
// files. Concurrent callers share one installation. Use the shared DataDir from
// the caller's server discovery and startup integration. Pass the returned paths
// to the launcher; the installation lock is released before return.
func Install(ctx context.Context, opts InstallOptions) (Installation, error) {
	return install(ctx, opts, nil, embedassets.ModelAssets())
}

func install(ctx context.Context, opts InstallOptions, client *http.Client, assets []embedassets.Asset) (Installation, error) {
	if opts.DataDir == "" {
		return Installation{}, errors.New("install embeddings: DataDir is required")
	}
	dir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return Installation{}, err
	}
	release, err := (daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed-install"}).AcquireStartLock(ctx)
	if err != nil {
		return Installation{}, err
	}
	defer release()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Installation{}, err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".install-") {
			if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
				return Installation{}, err
			}
		}
	}
	bundle := filepath.Join(dir, "bin", embedassets.BundleTag)
	result := Installation{filepath.Join(bundle, embedassets.ExecutableName()), dir}
	ready := checkBundle(bundle) == nil
	modelDir := embedassets.ModelDir(dir)
	missing, err := embedassets.MissingModel(modelDir, assets)
	if err != nil {
		return Installation{}, err
	}
	var size int64
	for _, a := range missing {
		size += a.Size
	}
	if ready && len(missing) == 0 {
		return result, nil
	}
	if !ready {
		size += bundleDownloadBytes
	}
	if opts.Confirm == nil {
		return Installation{}, fmt.Errorf("install embeddings: provide Confirm to approve approximately %d download bytes", size)
	}
	approved, err := opts.Confirm(ctx, size)
	if err != nil {
		return Installation{}, err
	}
	if !approved {
		return Installation{}, errors.New("embedding download declined; approve installation to use local embeddings")
	}
	if !ready {
		if err := os.RemoveAll(bundle); err != nil {
			return Installation{}, err
		}
	}
	stage, err := os.MkdirTemp(dir, ".install-*")
	if err != nil {
		return Installation{}, err
	}
	defer os.RemoveAll(stage)
	stagedBundle := filepath.Join(stage, "bin")
	httpClient := cmp.Or(client, embedassets.DownloadClient())
	defer httpClient.CloseIdleConnections()
	if !ready {
		releaseClient := selfupdate.Client{
			Owner: "kenn-io", Repo: "kit", BinaryName: "kennembed", HTTPClient: client, AllowUnsignedChecksums: true,
			ReleaseManifestURL: "https://api.github.com/repos/kenn-io/kit/releases/tags/" + embedassets.BundleTag,
		}
		info, err := releaseClient.Check(ctx, selfupdate.CheckOptions{Force: true})
		if err != nil {
			return Installation{}, fmt.Errorf("resolve kennembed bundle: %w", err)
		}
		if info == nil {
			return Installation{}, errors.New("no kennembed bundle is available")
		}
		if err := embedassets.DownloadAsset(ctx, httpClient, stage, embedassets.Asset{Name: "archive", URL: info.DownloadURL, Digest: info.Checksum, Size: info.Size}); err != nil {
			return Installation{}, fmt.Errorf("download kennembed: %w", err)
		}
		archive := filepath.Join(stage, "archive")
		if strings.HasSuffix(info.AssetName, ".zip") {
			err = selfupdate.ExtractZip(archive, stagedBundle)
		} else {
			err = selfupdate.ExtractTarGz(archive, stagedBundle)
		}
		if err != nil {
			return Installation{}, err
		}
		if err := checkBundle(stagedBundle); err != nil {
			return Installation{}, err
		}
		if err := os.MkdirAll(filepath.Dir(bundle), 0o700); err != nil {
			return Installation{}, err
		}
		if err := atomicfile.Replace(stagedBundle, bundle); err != nil {
			return Installation{}, fmt.Errorf("publish kennembed: %w", err)
		}
	}
	if err := safefileio.EnsurePrivateDir(modelDir); err != nil {
		return Installation{}, err
	}
	for _, a := range missing {
		if err := embedassets.DownloadAsset(ctx, httpClient, modelDir, a, atomicfile.WithStagingDir(stage)); err != nil {
			return Installation{}, err
		}
	}
	return result, nil
}

func checkBundle(dir string) error {
	for _, name := range append(embedassets.NativeFiles(), embedassets.ExecutableName()) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("bundle file is not regular or is empty: %s", name)
		}
		if name == embedassets.ExecutableName() && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
			return errors.New("kennembed has no execute permission")
		}
	}
	return nil
}
