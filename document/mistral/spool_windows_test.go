//go:build windows

package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/winsecurity"
	"golang.org/x/sys/windows"
)

func TestPrepareCreatesRestrictedWindowsFile(t *testing.T) {
	prepared := prepareTestDocument(t, testPolicy(t, 1024, 10), []byte("%PDF-1.7\nprivate"))
	file, err := winsecurity.OpenRestrictedCurrentUserFile(prepared.path)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func TestPrepareAndProcessRejectBroadWindowsDACLs(t *testing.T) {
	policy := testPolicy(t, 1024, 10)
	content := []byte("%PDF-1.7\nprivate")
	digest := sha256.Sum256(content)
	directory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, directory)
	require.NoError(t, setEveryoneDACL(directory))
	_, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(content)), policy, PrepareOptions{
		Directory: directory, DeclaredMediaType: mediaTypePDF,
		ExpectedSize: int64(len(content)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1024, MinFreeBytes: 1,
	})
	require.ErrorContains(t, err, "restricted DACL")

	prepared := prepareTestDocument(t, policy, content)
	require.NoError(t, setEveryoneDACL(prepared.path))
	authorization, err := policy.Authorize(syntheticManifest(t, policy, true), "pdf")
	require.NoError(t, err)
	client, err := NewClient(policy, ClientConfig{
		APIKey: "synthetic-key",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("unexpected provider request")
		})},
	})
	require.NoError(t, err)
	_, err = client.Process(t.Context(), prepared, authorization)
	require.ErrorContains(t, err, "unexpected principal")
}

func setEveryoneDACL(path string) error {
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_TYPE(windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}
