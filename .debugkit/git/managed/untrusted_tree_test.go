package managedworktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/fslink"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

func TestSupportsUntrustedTreeCheckoutGitVersion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		output string
		goos   string
		want   bool
	}{
		{output: "git version 2.41.0", goos: "linux"},
		{output: "git version 2.41.9", goos: "linux"},
		{output: "git version 2.42.0", goos: "linux", want: true},
		{output: "git version 2.45.2 (Apple Git-145)", goos: "darwin", want: true},
		{output: "git version 2.52.2.windows.4", goos: "linux"},
		{output: "git version 2.52.2.windows.4", goos: "windows"},
		{output: "git version 2.53.0", goos: "windows"},
		{output: "git version 2.53.0.windows.2", goos: "windows"},
		{output: "git version 2.53.0.windows.3-malformed", goos: "windows"},
		{output: "git version 2.53.0.windows.3", goos: "linux", want: true},
		{output: "git version 2.53.0.windows.3", goos: "windows", want: true},
		{output: "git version 2.53.1.windows.1", goos: "windows", want: true},
		{output: "git version 2.54.0.windows.1", goos: "windows", want: true},
		{output: "not git", goos: "linux"},
	} {
		assert.Equal(t, test.want,
			supportsUntrustedTreeCheckoutGitVersion(test.output, test.goos),
			test.output)
	}
}

func TestMaterializeUntrustedTreePinsWorktree(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	repo := initLifecycleRepo(t)
	require.NoError(os.WriteFile(
		filepath.Join(repo, "payload"), []byte("imported"), 0o600,
	))
	lifecycleGit(t, repo, "add", "payload")
	lifecycleGit(t, repo, "commit", "-m", "add payload")

	worktree := filepath.Join(t.TempDir(), "worktree")
	lifecycleGit(t, repo, "worktree", "add", "--no-checkout", "-b", "import",
		worktree)
	external := t.TempDir()
	externalPayload := filepath.Join(external, "payload")
	require.NoError(os.WriteFile(externalPayload, []byte("preserve"), 0o600))
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[core]\n\tworktree = "+filepath.ToSlash(external)+"\n",
	), 0o600))
	env := append(gitenv.StripAll(os.Environ()),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_WORK_TREE="+external,
	)

	err := materializeUntrustedTree(t.Context(), worktree,
		untrustedTreeIsolation{runner: gitcmd.Runner{
			Env: env, StripEnv: false, NoSystemConfig: true,
		}})

	require.NoError(err)
	assert.FileExists(filepath.Join(worktree, "payload"))
	externalContents, err := os.ReadFile(externalPayload)
	require.NoError(err)
	assert.Equal("preserve", string(externalContents))
}

func TestDeferredCheckoutRejectsFutureTreeConfig(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"global", "system", "include", "conditional-include", "global-link", "include-link", "directory-link", "parent-link", "parent-include", "future-link", "future-include", "external", "external-link"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			root := initLifecycleRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitconfig"), []byte("[diff \"deferred\"]\ncommand = echo executed > driver-ran\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitattributes"), []byte("payload diff=deferred\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "payload"), []byte("original\n"), 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "nested", "deeper"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "deeper", "file"), nil, 0o600))
			if source == "future-link" || source == "future-include" {
				if err := os.Symlink("nested/deeper", filepath.Join(root, "route")); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("cannot create symlink: %v", err)
					}
					require.NoError(t, err)
				}
			}
			lifecycleGit(t, root, "add", ".")
			lifecycleGit(t, root, "commit", "-m", "tree configuration")
			lifecycleGit(t, root, "branch", "imported")
			lifecycleGit(t, root, "reset", "--hard", "HEAD^")
			path := filepath.Join(t.TempDir(), "checkout")
			runner := lifecycleTestRunner(t)
			switch source {
			case "global":
				runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL=.gitconfig")
			case "system":
				runner.Env = append(runner.Env, "GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_SYSTEM="+filepath.Join(path, ".gitconfig"))
			case "global-link", "include-link", "external-link":
				target := filepath.Join(path, ".gitconfig")
				if source == "external-link" {
					target = filepath.Join(t.TempDir(), "external.gitconfig")
					require.NoError(t, os.WriteFile(target, nil, 0o600))
				}
				links := t.TempDir()
				if err := os.Symlink(target, filepath.Join(links, "target")); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("cannot create file symlink: %v", err)
					}
					require.NoError(t, err)
				}
				link := filepath.Join(links, "config")
				require.NoError(t, os.Symlink("target", link))
				if source == "include-link" {
					lifecycleGit(t, root, "config", "include.path", link)
				} else {
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+link)
				}
			case "parent-link", "parent-include":
				link := filepath.Join(t.TempDir(), "route")
				_, err := fslink.LinkDir(filepath.Join(path, "nested"), link)
				require.NoError(t, err)
				configPath := link + "/../.gitconfig"
				if source == "parent-include" {
					relative, err := filepath.Rel(filepath.Join(root, ".git"), link)
					require.NoError(t, err)
					lifecycleGit(t, root, "config", "include.path", filepath.ToSlash(relative)+"/../.gitconfig")
				} else {
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+configPath)
				}
			case "future-link", "future-include":
				configPath := path + "/route/../../.gitconfig"
				if source == "future-include" {
					lifecycleGit(t, root, "config", "include.path", filepath.ToSlash(configPath))
				} else {
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+configPath)
				}
			case "directory-link":
				link := filepath.Join(t.TempDir(), "directory")
				_, err := fslink.LinkDir(path, link)
				require.NoError(t, err)
				runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+filepath.Join(link, ".gitconfig"))
			case "include", "conditional-include":
				key := "include.path"
				if source == "conditional-include" {
					key = "includeIf.onbranch:topic.path"
				}
				relative, err := filepath.Rel(filepath.Join(root, ".git"), filepath.Join(path, ".gitconfig"))
				require.NoError(t, err)
				lifecycleGit(t, root, "config", key, filepath.ToSlash(relative))
			}
			_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: path, Branch: "topic", BaseRef: "imported", NoCheckout: true,
				Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, Runner: runner,
			})
			if source != "external" && source != "external-link" {
				require.Error(t, err)
				assert.NoDirExists(t, path)
				assert.False(t, branchExistsInRepo(t, root, "topic"))
				return
			}
			require.NoError(t, err)
			_, stderr, err := runner.Run(t.Context(), path, nil, "reset", "--hard", "HEAD")
			require.NoError(t, err, "%s", stderr)
			require.NoError(t, os.WriteFile(filepath.Join(path, "payload"), []byte("changed\n"), 0o600))
			out, stderr, err := runner.Run(t.Context(), path, nil, "diff", "--", "payload")
			require.NoError(t, err, "%s", stderr)
			assert.Contains(t, string(out), "+changed")
			assert.NoFileExists(t, filepath.Join(path, "driver-ran"))
		})
	}
}

func TestIsolatedCheckoutRejectsConfigCaseAliasBeforeMaterializing(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"immediate", "deferred"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			probe := filepath.Join(base, "probe")
			require.NoError(t, os.Mkdir(probe, 0o755))
			if _, err := os.Stat(filepath.Join(base, "PROBE")); os.IsNotExist(err) {
				t.Skip("requires a case-insensitive filesystem")
			} else {
				require.NoError(t, err)
			}
			root := initLifecycleRepo(t)
			require.NoError(t, os.WriteFile(filepath.Join(root, ".gitconfig"), []byte("[fixture]\nvalue = imported\n"), 0o600))
			lifecycleGit(t, root, "add", ".")
			lifecycleGit(t, root, "commit", "-m", "tree configuration")
			lifecycleGit(t, root, "branch", "imported")
			lifecycleGit(t, root, "reset", "--hard", "HEAD^")
			path := filepath.Join(base, "checkout")
			link := filepath.Join(t.TempDir(), "config")
			if err := os.Symlink(filepath.Join(base, "CHECKOUT", ".gitconfig"), link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("cannot create symlink: %v", err)
				}
				require.NoError(t, err)
			}
			runner := lifecycleTestRunner(t)
			runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+link)
			_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
				ProjectRoot: root, Path: path, Branch: "topic", BaseRef: "imported", NoCheckout: mode == "deferred",
				Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, Runner: runner,
			})
			require.Error(t, err)
			assert.NoFileExists(t, filepath.Join(path, ".gitconfig"))
		})
	}
}

func TestDeferredCheckoutChecksDefaultConfigPaths(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"home", "xdg", "home-xdg"} {
		for _, override := range []bool{false, true} {
			name := source
			if override {
				name += "/explicit-global"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root := initLifecycleRepo(t)
				for _, config := range []string{".gitconfig", "git/config", ".config/git/config"} {
					path := filepath.Join(root, config)
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
					require.NoError(t, os.WriteFile(path, []byte("[fixture]\nvalue = imported\n"), 0o600))
				}
				require.NoError(t, os.MkdirAll(filepath.Join(root, "nested", "deeper"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(root, "nested", "deeper", "file"), nil, 0o600))
				if err := os.Symlink("nested/deeper", filepath.Join(root, "route")); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("cannot create symlink: %v", err)
					}
					require.NoError(t, err)
				}
				lifecycleGit(t, root, "add", ".")
				lifecycleGit(t, root, "commit", "-m", "tree configuration")
				lifecycleGit(t, root, "branch", "imported")
				lifecycleGit(t, root, "reset", "--hard", "HEAD^")
				path := filepath.Join(t.TempDir(), "checkout")
				runner := lifecycleTestRunner(t)
				runner.Env = append(gitenv.StripAll(runner.Env), "GIT_CONFIG_NOSYSTEM=1")
				selector := "HOME"
				switch source {
				case "xdg":
					selector = "XDG_CONFIG_HOME"
				case "home-xdg":
					runner.Env = append(runner.Env, "XDG_CONFIG_HOME=")
				}
				runner.Env = append(runner.Env, selector+"="+path+"/route/../..")
				if override {
					global := filepath.Join(t.TempDir(), "global")
					require.NoError(t, os.WriteFile(global, []byte("[fixture]\nvalue = external\n"), 0o600))
					runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+global)
				}
				_, err := CreateWorktreeOnDisk(t.Context(), CreateWorktreeOptions{
					ProjectRoot: root, Path: path, Branch: "topic", BaseRef: "imported", NoCheckout: true,
					Checkout: CheckoutIsolated, FailureCleanup: CleanupDeferred, Runner: runner,
				})
				if !override {
					require.Error(t, err)
					assert.NoDirExists(t, path)
					assert.False(t, branchExistsInRepo(t, root, "topic"))
					return
				}
				require.NoError(t, err)
				_, stderr, err := runner.Run(t.Context(), path, nil, "reset", "--hard", "HEAD")
				require.NoError(t, err, "%s", stderr)
				out, stderr, err := runner.Run(t.Context(), path, nil, "config", "--get", "fixture.value")
				require.NoError(t, err, "%s", stderr)
				assert.Equal(t, "external\n", string(out))
			})
		}
	}
}
