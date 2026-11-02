package fsname

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckPathUnix(t *testing.T) {
	tests := []struct {
		path    string
		wantErr bool
	}{
		{path: "/home/user-a/notes.txt"},
		{path: "relative/dir/file"},
		{path: "../up/./file"},
		{path: "/srv/data/..foo"},
		{path: "", wantErr: true},
		{path: "/srv/a:b", wantErr: true},
		{path: "/srv/NUL", wantErr: true},
		{path: "/srv/conin$.log", wantErr: true},
		{path: "/srv/trailing./file", wantErr: true},
		{path: `/srv/back\slash`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := checkPath(tt.path, false, pathConfig{})

			if tt.wantErr {
				assert.ErrorIs(t, err, ErrNotPortable)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestCheckPathWindows(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		cfg     pathConfig
		wantErr string
	}{
		{name: "drive path", path: `C:\Users\user-a\notes.txt`},
		{name: "forward slashes", path: `C:/Users/user-a/notes.txt`},
		{name: "relative", path: `data\file.txt`},
		{name: "stream", path: `C:\data\a:b`, wantErr: "portable form"},
		{name: "device element", path: `C:\data\CON.txt`, wantErr: "portable form"},
		{name: "trailing dot", path: `C:\data.\x`, wantErr: "portable form"},
		{name: "drive relative", path: `C:data\x`, wantErr: "current directory of drive"},
		{name: "rooted without drive", path: `\data\x`, wantErr: "rooted without a drive"},
		{name: "unc refused", path: `\\server\share\x`, wantErr: "network share"},
		{name: "unc allowed", path: `\\server\share\x`, cfg: pathConfig{allowUNC: true}},
		{name: "unc allowed checks elements", path: `\\server\share\a:b`, cfg: pathConfig{allowUNC: true}, wantErr: "portable form"},
		{name: "long path refused", path: `\\?\C:\data\x`, wantErr: "long-path form"},
		{name: "long path allowed", path: `\\?\C:\data\x`, cfg: pathConfig{allowLongPath: true}},
		{name: "long unc needs unc", path: `\\?\UNC\server\share\x`, cfg: pathConfig{allowLongPath: true}, wantErr: "network share"},
		{name: "long unc allowed", path: `\\?\UNC\server\share\x`, cfg: pathConfig{allowLongPath: true, allowUNC: true}},
		{name: "long path volume guid", path: `\\?\Volume{0}\x`, cfg: pathConfig{allowLongPath: true}, wantErr: "not a drive path"},
		{name: "device namespace", path: `\\.\PhysicalDrive0`, cfg: pathConfig{allowLongPath: true, allowUNC: true}, wantErr: "device namespace"},
		{name: "nt namespace", path: `\??\C:\x`, cfg: pathConfig{allowLongPath: true}, wantErr: "device namespace"},
		{name: "short name", path: `C:\PROGRA~1\app`, wantErr: "8.3 short name"},
		{name: "short name with extension", path: `C:\data\REPORT~1.TXT`, wantErr: "8.3 short name"},
		{name: "long name with tilde past eight characters", path: `C:\data\REPORT~12.TXT`},
		{name: "non-ASCII short name", path: `C:\data\RÉSUMÉ~1.TXT`, wantErr: "8.3 short name"},
		{name: "long path with slash", path: `\\?\C:/data/x`, cfg: pathConfig{allowLongPath: true}, wantErr: "uses / after"},
		{name: "long path with dot dot", path: `\\?\C:\data\..\x`, cfg: pathConfig{allowLongPath: true}, wantErr: `".." after`},
		{name: "long unc lower case", path: `\\?\unc\server\share\x`, cfg: pathConfig{allowLongPath: true, allowUNC: true}},
		{name: "short name allowed", path: `C:\PROGRA~1\app`, cfg: pathConfig{allowShortName: true}},
		{name: "long tilde name", path: `C:\data\backup~2024-01.tar`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPath(tt.path, true, tt.cfg)

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrNotPortable)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
