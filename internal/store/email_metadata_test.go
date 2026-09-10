package store

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestEmailMetadataRoundTripAndTamper(t *testing.T) {
	s := newTestStore(t)
	f := newEmailFixture(t, s, "source.eml")
	v, err := s.PublishEmailGeneration(t.Context(), f.publication)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &out))
	require.Contains(t, out.String(), `"type":"email_generation"`)
	require.Contains(t, out.String(), `"type":"email_part_artifact"`)
	require.Contains(t, out.String(), `"type":"email_attachment"`)
	require.Contains(t, out.String(), `"type":"email_head"`)
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(out.Bytes())))
	got, err := restored.EmailMetadata(t.Context(), v.Version.ID)
	require.NoError(t, err)
	require.Equal(t, v, got)
	var again bytes.Buffer
	require.NoError(t, restored.ExportMetadata(t.Context(), &again))
	require.Equal(t, out.String(), again.String())
	for _, tc := range []string{"checksum", "part", "timestamp", "unknown"} {
		t.Run(tc, func(t *testing.T) {
			lines := strings.Split(out.String(), "\n")
			for i, line := range lines {
				if strings.Contains(line, `"type":"email_generation"`) {
					switch tc {
					case "checksum":
						lines[i] = strings.Replace(line, v.Generation.Checksum, fakeHash("e1"), 1)
					case "timestamp":
						lines[i] = strings.Replace(line, v.Generation.CreatedAt, "invalid", 1)
					case "unknown":
						lines[i] = strings.TrimSuffix(line, "}") + `,"unknown":true}`
					}
				}
				if tc == "part" && strings.Contains(line, `"type":"email_part_artifact"`) {
					lines[i] = ""
					break
				}
			}
			target := newTestStore(t)
			require.Error(t, target.ImportMetadata(t.Context(), strings.NewReader(strings.Join(lines, "\n"))))
			require.Equal(t, []int{0, 0, 0, 0, 0, 0}, emailTableCounts(t, target))
		})
	}
	_, err = s.db.Exec(`UPDATE email_generations SET checksum=?`, fakeHash("e2"))
	require.NoError(t, err)
	require.Error(t, s.ValidateMetadata(t.Context()))
	require.Error(t, s.ExportMetadata(t.Context(), &bytes.Buffer{}))
}
