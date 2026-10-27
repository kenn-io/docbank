package config

import (
	"fmt"
	"os"
	"path/filepath"
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
			contents := fmt.Sprintf("[miss_log]\nenabled = true\nretention_days = %d\nmax_records = %d\n", tc.days, tc.count)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0600))
			got, err := Load(dir)
			require.NoError(t, err)
			err = got.Validate()
			if tc.valid {
				require.NoError(t, err)
				require.True(t, got.MissLog.Enabled)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestMissLogDisabledExplicitZeroBoundsRejected(t *testing.T) {
	dir := privateTestConfigDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[miss_log]\nenabled=false\nretention_days=0\nmax_records=0\n"), 0600))
	cfg, err := Load(dir)
	require.NoError(t, err)
	require.ErrorContains(t, cfg.Validate(), "retention_days")
	absent := Default()
	absent.MissLog = MissLogConfig{}
	require.NoError(t, absent.Validate(), "zero-value callers may omit the optional section")
}

func FuzzMissLogConfigBounds(f *testing.F) {
	f.Add(1, 1)
	f.Add(365, 100000)
	f.Add(0, 1)
	f.Add(366, 100001)
	f.Fuzz(func(t *testing.T, days, count int) {
		cfg := Default()
		cfg.MissLog.Enabled = true
		cfg.MissLog.RetentionDays = days
		cfg.MissLog.MaxRecords = count
		err := cfg.Validate()
		valid := days >= 1 && days <= 365 && count >= 1 && count <= 100000
		if valid && err != nil {
			t.Fatalf("valid bounds rejected: %v", err)
		}
		if !valid && err == nil {
			t.Fatal("out-of-range bounds accepted")
		}
	})
}
