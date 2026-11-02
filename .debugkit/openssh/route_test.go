package openssh

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTarget(t *testing.T) {
	tests := map[string]struct {
		value   string
		want    Target
		display string
	}{
		"alias":           {"studio", Target{Hostname: "studio"}, "studio"},
		"user and port":   {"wes@studio:2222", Target{User: "wes", Hostname: "studio", Port: 2222}, "wes@studio:2222"},
		"IPv6":            {"wes@[2001:db8::1]:2200", Target{User: "wes", Hostname: "2001:db8::1", Port: 2200}, "wes@[2001:db8::1]:2200"},
		"URI destination": {"ssh://wes@studio:2222", Target{User: "wes", Hostname: "studio", Port: 2222}, "wes@studio:2222"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseTarget(test.value)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.Equal(t, test.display, got.String())
		})
	}
}

func TestCommandDestinationLeavesIPv6Unbracketed(t *testing.T) {
	assert := assert.New(t)
	target := Target{User: "wes", Hostname: "2001:db8::1", Port: 2200}

	destination, port := target.CommandDestination()

	assert.Equal("wes@2001:db8::1", destination)
	assert.Equal(2200, port)
	assert.Equal("wes@[2001:db8::1]:2200", target.String())
}

func TestParseTargetRejectsAmbiguousDestinations(t *testing.T) {
	for _, value := range []string{
		"", "@studio", "studio:", "studio:70000",
		"ssh://wes:secret@studio", "ssh://studio/path", "[2001:db8::1]:",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseTarget(value)
			var configErr *ConfigError
			require.ErrorAs(t, err, &configErr)
		})
	}
}

func TestParseTargetRedactsRejectedURIPassword(t *testing.T) {
	for name, value := range map[string]string{
		"lowercase":  "ssh://wes:super-secret@studio",
		"mixed case": "SSH://wes:super-secret@studio",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTarget(value)

			require.Error(t, err)
			assert.NotContains(t, err.Error(), "super-secret")
		})
	}
}

func TestValidateTargetRejectsUnsafeComponents(t *testing.T) {
	tests := map[string]Target{
		"shell metacharacter in user": {User: "wes;touch", Hostname: "studio"},
		"whitespace in user":          {User: "domain user", Hostname: "studio"},
		"shell metacharacter in host": {Hostname: "studio$(touch pwned)"},
		"leading option in host":      {Hostname: "-oProxyCommand=touch"},
		"out of range port":           {Hostname: "studio", Port: 70000},
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateTarget(target)
			var configErr *ConfigError
			require.ErrorAs(t, err, &configErr)
		})
	}
}

func TestValidateTargetRejectsDotOnlyComponents(t *testing.T) {
	tests := map[string]Target{
		"dot user":     {User: ".", Hostname: "studio"},
		"dot-dot user": {User: "..", Hostname: "studio"},
		"dot hostname": {Hostname: "."},
		"dot-dot host": {Hostname: ".."},
	}
	for name, target := range tests {
		t.Run(name, func(t *testing.T) {
			err := ValidateTarget(target)
			var configErr *ConfigError
			require.ErrorAs(t, err, &configErr)
		})
	}
}

func TestParseTargetRejectsUnsafeComponents(t *testing.T) {
	for _, value := range []string{
		"wes;touch@studio",
		"domain user@studio",
		"studio$(touch pwned)",
		"-oProxyCommand=touch",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseTarget(value)
			var configErr *ConfigError
			require.ErrorAs(t, err, &configErr)
		})
	}
}

func TestResolveRouteEnumeratesDirectProxyJumps(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	configs := map[string]EffectiveConfig{
		"app": {
			Hostname:  "app.internal",
			ProxyJump: "edge-a,ops@edge-b:2222",
		},
		"edge-a":          {Hostname: "192.0.2.10"},
		"ops@edge-b:2222": {Hostname: "192.0.2.11"},
	}
	provider := func(_ context.Context, target Target) (EffectiveConfig, error) {
		return configs[target.String()], nil
	}

	route, err := ResolveRoute(
		t.Context(), Target{Hostname: "app"}, provider,
	)
	require.NoError(err)
	require.Len(route, 3)
	assert.Equal("edge-a", route[0].Target.String())
	assert.Equal("ops@edge-b:2222", route[1].Target.String())
	assert.Equal("app", route[2].Target.String())
}

func TestResolveRouteRejectsUnsafeEndpointBeforeProvider(t *testing.T) {
	called := false
	provider := func(context.Context, Target) (EffectiveConfig, error) {
		called = true
		return EffectiveConfig{}, nil
	}

	_, err := ResolveRoute(
		t.Context(),
		Target{User: "wes;touch", Hostname: "studio"},
		provider,
	)

	var configErr *ConfigError
	require.ErrorAs(t, err, &configErr)
	assert.False(t, called)
}

func TestResolveRouteRejectsRoutesItCannotInspect(t *testing.T) {
	tests := map[string]struct {
		configs map[string]EffectiveConfig
		kind    error
	}{
		"opaque command": {
			map[string]EffectiveConfig{"app": {ProxyCommand: "nc gateway 22"}},
			ErrOpaqueProxyCommand,
		},
		"nested jump": {
			map[string]EffectiveConfig{
				"app":  {ProxyJump: "edge"},
				"edge": {ProxyJump: "bastion"},
			},
			ErrNestedProxyRoute,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			provider := func(_ context.Context, target Target) (EffectiveConfig, error) {
				return test.configs[target.String()], nil
			}
			_, err := ResolveRoute(
				t.Context(), Target{Hostname: "app"}, provider,
			)
			require.ErrorIs(t, err, test.kind)
			var routeErr *RouteError
			require.ErrorAs(t, err, &routeErr)
		})
	}
}
