package openssh

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConfigPreservesEffectivePolicy(t *testing.T) {
	assert := assert.New(t)
	config := ParseConfig([]byte(`user deploy
hostname 10.0.0.7
port 2222
stricthostkeychecking TRUE
proxyjump none
identityfile ~/.ssh/id_first
identityfile ~/.ssh/id_second
`))

	assert.Equal("deploy", config.User)
	assert.Equal("10.0.0.7", config.Hostname)
	assert.Equal(2222, config.Port)
	assert.Equal("yes", config.StrictHostKeyChecking)
	assert.Empty(config.ProxyJump)
	assert.Equal([]string{
		"hostname=10.0.0.7",
		"identityfile=~/.ssh/id_first",
		"identityfile=~/.ssh/id_second",
		"port=2222",
		"proxyjump=none",
		"stricthostkeychecking=TRUE",
		"user=deploy",
	}, config.CanonicalOptions())
}

func TestResolverKeepsExecutionPolicyInjectable(t *testing.T) {
	var got []string
	resolver := Resolver{
		Executable: "/usr/bin/ssh",
		BaseArgs: []string{
			"-l", "root", "-o", "User=admin", "-p", "22",
			"-F", "/tmp/ssh_config",
		},
		Run: func(
			_ context.Context,
			argv []string,
		) ([]byte, []byte, int, error) {
			got = append([]string(nil), argv...)
			return []byte("hostname studio.internal\nport 2200\n"), nil, 0, nil
		},
	}

	config, err := resolver.Resolve(
		t.Context(),
		Target{User: "wes", Hostname: "studio", Port: 2222},
	)
	require.NoError(t, err)
	assert.Equal(t, "studio.internal", config.Hostname)
	assert.Equal(t, []string{
		"/usr/bin/ssh", "-G", "-l", "wes", "-p", "2222",
		"-l", "root", "-o", "User=admin", "-p", "22",
		"-F", "/tmp/ssh_config", "--", "studio",
	}, got)
}

func TestResolverReportsTypedCommandFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	sentinel := errors.New("runner failed")
	resolver := Resolver{Run: func(
		context.Context,
		[]string,
	) ([]byte, []byte, int, error) {
		return nil, []byte("bad configuration option"), 255, sentinel
	}}

	_, err := resolver.Resolve(
		t.Context(),
		Target{Hostname: "studio"},
	)
	var commandErr *CommandError
	require.ErrorAs(err, &commandErr)
	require.ErrorIs(err, sentinel)
	assert.Equal(255, commandErr.ExitCode)
	assert.Equal("bad configuration option", commandErr.Diagnostic)
}

func TestResolverRejectsUnsafeTargetBeforeInvocation(t *testing.T) {
	called := false
	resolver := Resolver{Run: func(
		context.Context,
		[]string,
	) ([]byte, []byte, int, error) {
		called = true
		return nil, nil, 0, nil
	}}

	_, err := resolver.Resolve(
		t.Context(),
		Target{User: "wes;touch", Hostname: "studio"},
	)
	var configErr *ConfigError
	require.ErrorAs(t, err, &configErr)
	assert.False(t, called)
}
