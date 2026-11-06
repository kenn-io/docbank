package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMissLogConfigDefaults(t *testing.T) {
	dir := privateTestConfigDir(t)
	cfg, err := Load(dir)
	require.NoError(t, err)
	require.False(t, cfg.MissLog.Enabled)
	require.Equal(t, 90, cfg.MissLog.RetentionDays)
	require.Equal(t, 5000, cfg.MissLog.MaxRecords)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[miss_log]\nenabled=false\nretention_days=0\nmax_records=0\n"), 0600))
	cfg, err = Load(dir)
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
}

func FuzzMissLogConfigBounds(f *testing.F) {
	f.Add(1, 1)
	f.Add(365, 100000)
	f.Add(0, 1)
	f.Add(1, 0)
	f.Add(-1, 1)
	f.Add(1, -1)
	f.Add(366, 100001)
	f.Fuzz(func(t *testing.T, days, count int) {
		cfg := Default()
		cfg.MissLog.RetentionDays = days
		cfg.MissLog.MaxRecords = count
		valid := days >= 0 && days <= 365 && count >= 0 && count <= 100000
		for _, enabled := range []bool{false, true} {
			cfg.MissLog.Enabled = enabled
			err := cfg.Validate()
			if valid && err != nil {
				t.Fatalf("valid bounds rejected: %v", err)
			}
			if !valid && err == nil {
				t.Fatalf("out-of-range bounds accepted with enabled=%t", enabled)
			}
		}
	})
}
