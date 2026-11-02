package openssh

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConnectionArgumentsMakeMasterlessModeExplicit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	arguments, err := ClientArguments("")
	require.NoError(err)
	assert.Equal([]string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-S", "none",
	}, arguments)
	arguments, err = ClientArguments("/tmp/control.sock")
	require.NoError(err)
	assert.Equal([]string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-S", "/tmp/control.sock",
	}, arguments)
}

func TestMasterArgumentsAreBoundedAndNoninteractive(t *testing.T) {
	arguments, err := MasterArguments("/tmp/control.sock", testTarget("wes@studio"), ConnectionOptions{
		ConnectTimeout:      12 * time.Second,
		ServerAliveInterval: 20 * time.Second,
		ServerAliveCountMax: 4,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-MNf",
		"-o", "ControlMaster=yes",
		"-o", "ControlPersist=no",
		"-S", "/tmp/control.sock",
		"-o", "BatchMode=yes",
		"-o", "ConnectionAttempts=1",
		"-o", "ConnectTimeout=12",
		"-o", "ServerAliveInterval=20",
		"-o", "ServerAliveCountMax=4",
		"-o", "TCPKeepAlive=no",
		"--", "wes@studio",
	}, arguments)
}

func TestMasterArgumentsAllowExplicitCallerOwnedInteraction(t *testing.T) {
	options := DefaultConnectionOptions()
	options.AllowInteraction = true

	arguments, err := MasterArguments(
		"/tmp/control.sock", testTarget("wes@studio"), options,
	)

	require.NoError(t, err)
	assert.Contains(t, arguments, "BatchMode=no")
	assert.NotContains(t, arguments, "BatchMode=yes")
}

func TestMasterArgumentsBoundDetachedPersistence(t *testing.T) {
	options := DefaultConnectionOptions()
	options.ControlPersistTimeout = time.Hour

	arguments, err := MasterArguments(
		"/tmp/control.sock", testTarget("wes@studio"), options,
	)

	require.NoError(t, err)
	assert.Contains(t, arguments, "ControlPersist=3600")
}

func TestMasterArgumentsRejectNegativePersistenceTimeout(t *testing.T) {
	options := DefaultConnectionOptions()
	options.ControlPersistTimeout = -time.Second

	_, err := MasterArguments(
		"/tmp/control.sock", testTarget("wes@studio"), options,
	)

	var configErr *ConfigError
	require.ErrorAs(t, err, &configErr)
}

func TestMasterArgumentsRejectNonpositiveAliveCount(t *testing.T) {
	for _, count := range []int{0, -1} {
		options := DefaultConnectionOptions()
		options.ServerAliveCountMax = count

		_, err := MasterArguments(
			"/tmp/control.sock", testTarget("wes@studio"), options,
		)

		var configErr *ConfigError
		require.ErrorAs(t, err, &configErr)
	}
}

func TestSSHArgumentsRejectUnsafeTargets(t *testing.T) {
	target := Target{User: "wes;touch", Hostname: "studio"}
	builders := []struct {
		name  string
		build func() ([]string, error)
	}{
		{name: "master", build: func() ([]string, error) {
			return MasterArguments("/tmp/control.sock", target, DefaultConnectionOptions())
		}},
		{name: "check", build: func() ([]string, error) {
			return CheckArguments("/tmp/control.sock", target)
		}},
		{name: "exit", build: func() ([]string, error) {
			return ExitArguments("/tmp/control.sock", target)
		}},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			_, err := builder.build()
			var configErr *ConfigError
			require.ErrorAs(t, err, &configErr)
		})
	}
}

func TestControlPathArgumentsPreserveLiteralFilesystemPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "spaces and percent tokens",
			path: filepath.Join("/tmp", "control dir %h", "control.sock"),
			want: filepath.Join("/tmp", "control dir %%h", "control.sock"),
		},
		{
			name: "leading tilde",
			path: filepath.Join("~", "control.sock"),
			want: "./" + filepath.Join("~", "control.sock"),
		},
		{
			name: "reserved none value",
			path: "none",
			want: "./none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			clientArguments, err := ClientArguments(tt.path)
			require.NoError(err)
			masterArguments, err := MasterArguments(
				tt.path, testTarget("wes@studio"), DefaultConnectionOptions(),
			)
			require.NoError(err)
			for _, arguments := range [][]string{clientArguments, masterArguments} {
				index := -1
				for i, argument := range arguments {
					if argument == "-S" {
						index = i
						break
					}
				}
				require.GreaterOrEqual(index, 0)
				require.Greater(len(arguments), index+1)
				assert.Equal(tt.want, arguments[index+1])
			}
		})
	}
}

func TestControlPathArgumentsRejectEnvironmentExpansion(t *testing.T) {
	path := filepath.Join("/tmp", "${HOME}", "control.sock")
	builders := []struct {
		name  string
		build func() ([]string, error)
	}{
		{name: "client", build: func() ([]string, error) { return ClientArguments(path) }},
		{name: "master", build: func() ([]string, error) {
			return MasterArguments(path, testTarget("wes@studio"), DefaultConnectionOptions())
		}},
		{name: "check", build: func() ([]string, error) {
			return CheckArguments(path, testTarget("wes@studio"))
		}},
		{name: "exit", build: func() ([]string, error) {
			return ExitArguments(path, testTarget("wes@studio"))
		}},
	}
	for _, builder := range builders {
		t.Run(builder.name, func(t *testing.T) {
			_, err := builder.build()
			var pathErr *PathError
			require.ErrorAs(t, err, &pathErr)
			assert.Equal(t, path, pathErr.Path)
		})
	}
}

func TestRouteIdentityCoversScopeRouteAndEffectiveConfig(t *testing.T) {
	assert := assert.New(t)
	base := Route{{
		Target: Target{User: "wes", Hostname: "studio"},
		Config: EffectiveConfig{Options: []Option{
			{Name: "hostname", Value: "192.0.2.1"},
			{Name: "identityfile", Value: "~/.ssh/id_ed25519"},
		}},
	}}
	reordered := Route{{
		Target: base[0].Target,
		Config: EffectiveConfig{Options: []Option{
			{Name: "identityfile", Value: "~/.ssh/id_ed25519"},
			{Name: "hostname", Value: "192.0.2.1"},
		}},
	}}
	changed := Route{{
		Target: base[0].Target,
		Config: EffectiveConfig{Options: []Option{
			{Name: "hostname", Value: "192.0.2.2"},
			{Name: "identityfile", Value: "~/.ssh/id_ed25519"},
		}},
	}}

	identity := RouteIdentity("forge:studio", base)
	assert.Equal(identity, RouteIdentity("forge:studio", reordered))
	assert.NotEqual(identity, RouteIdentity("ghosthub:studio", base))
	assert.NotEqual(identity, RouteIdentity("forge:studio", changed))
	assert.Regexp(`^c-[a-z2-7]{26}$`, ControlName(identity))
	assert.Regexp(
		`^c-[a-z2-7]{26}$`,
		controlNameForTarget(identity, Target{User: "wes", Hostname: "studio"}),
	)
}

func TestControlPathEnforcesSocketBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	path, err := ControlPath("/tmp/ssh", "control-deadbeef", 100)
	require.NoError(err)
	assert.Equal(filepath.Join("/tmp/ssh", "control-deadbeef.sock"), path)

	_, err = ControlPath("/a/very/long/directory", "control-deadbeef", 20)
	var pathErr *PathError
	require.ErrorAs(err, &pathErr)
	assert.Equal(20, pathErr.MaximumBytes)
}
