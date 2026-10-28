package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissLogConfigDefaultsAndBounds(t *testing.T) {
	dir := privateTestConfigDir(t)
	cfg, err := Load(dir)
	require.NoError(t, err)
	require.False(t, cfg.MissLog.Enabled)
	require.Equal(t, 90, cfg.MissLog.RetentionDays)
	require.Equal(t, 5000, cfg.MissLog.MaxRecords)
	for _, tc := range []struct {
		days, count int
		valid       bool
	}{
		{1, 1, true}, {365, 100000, true}, {0, 1, false}, {366, 1, false}, {1, 0, false}, {1, 100001, false},
	} {
		t.Run(fmt.Sprintf("%d-%d", tc.days, tc.count), func(t *testing.T) {
			got := cfg
			got.MissLog = MissLogConfig{Enabled: true, RetentionDays: tc.days, MaxRecords: tc.count}
			err := got.Validate()
			if tc.valid {
				require.NoError(t, err)
				require.True(t, got.MissLog.Enabled)
			} else {
				require.Error(t, err)
			}
		})
	}
}
