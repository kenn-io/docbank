package mistral

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var generatedFixtureDigests = map[string]string{
	"pdf":        "37dfbd5a319fb1a314fbf6f792799cb37cc43940cdbf6d73e1fa6657204f0385",
	"docx":       "864408fffca736097f97ea6c721f577f4140228a55d600b9ecf5843f13ebddb6",
	"odt":        "20e2fb2083a4123bf8066eeacee47c1d99a40aae8366ac0facc89270c2c2c85e",
	"rtf":        "48c33dd0eac495ef688eb8d90ae07d55cc6e5af63a6d4f03bef58bc502101fba",
	"pptx":       "db6243fd2f4a02aeecd1c30d59fdab6d576702f8cf234145ebbbecc93fac3522",
	"xlsx":       "cf9950bc9c55f311ae2f734e6f40a4d70579d068de193add2f029d63ad370180",
	"ods":        "05319a38862c4e5b0cecf4901c28eee710cb58d8510cd5b201ba8bf431a6451e",
	"csv":        "45f98e97a73320c3c81264e33ab99372946b2462003ffc3e3fc1961f9c796447",
	"epub":       "5e56ec7771e11aef7cd34b53fd97af1a902b14276ad8522c551c609aa9f66c54",
	"txt":        "347eafb267551bfd505b02f14ea30b5a20ce755144e0d611dff2e84205b3da91",
	"markdown":   "523f4d9f98dfa8d0e57368f47c0d0d6178b9649b7ac335c97ccaea6859312352",
	"rst":        "42741fb7dc5f89a2129fbf5fe4007c0e2f6e16b395cedab491341f8f28b24dc5",
	"latex":      "a8d8eabb8ca58d0cd612141bb4142158b03ac638dfdbf28766a2a2f0edf07242",
	"json":       "054e871494e6d8b01016dca510483889219cb4906aa1e44b0e265e3bd9c9858d",
	"jsonl":      "3df88636af36823747f882be5a55014480d7e8fc4f3023ba61f8a279532415df",
	"xml":        "781e33c8b87ad6f8f2bf5a0fb7f42e0eb2e8719a332e5e59dc1d220ac125207e",
	"yaml":       "e4a2263e8792317257cfa89ed11e8195c920a58b2f77469bc8e40fdac06d9879",
	"go":         "c7be1969e35919bc9a9ba6e86e45c29aa6c053b5404ddb739da2c69a38cdf343",
	"python":     "9c68c278eecb7f4494279b49964055e55cccc73a7359dd0218631f31cf1f4487",
	"javascript": "e69388590bf3255c248592539484960490a41bd53444f669e8c42d66dbdb374b",
	"eml":        "2287f0ef3014867314f8afaea098710c8b88dcb4dc2b81e79fbe170349eb19a1",
}

func TestWriteProbeFixturesPublishesCompleteDeterministicMatrix(t *testing.T) {
	seeds := writeNativeSeeds(t)
	first := filepath.Join(t.TempDir(), "fixtures-first")
	second := filepath.Join(t.TempDir(), "fixtures-second")
	require.NoError(t, WriteProbeFixtures(t.Context(), first, FixtureOptions{SeedDirectory: seeds}))
	require.NoError(t, WriteProbeFixtures(t.Context(), second, FixtureOptions{SeedDirectory: seeds}))
	fileDigest := func(path string) string {
		content, err := os.ReadFile(path)
		require.NoError(t, err)
		digest := sha256.Sum256(content)
		return hex.EncodeToString(digest[:])
	}

	entries, err := os.ReadDir(first)
	require.NoError(t, err)
	require.Len(t, entries, len(candidateFormats))
	for _, candidate := range candidateFormats {
		firstPath := filepath.Join(first, candidate.ID)
		secondPath := filepath.Join(second, candidate.ID)
		info, err := os.Lstat(firstPath)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular())
		if runtime.GOOS != "windows" {
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		require.NoError(t, validateFixture(firstPath, candidate))
		if !slices.Contains(nativeSeedFormats, candidate.ID) {
			firstDigest := fileDigest(firstPath)
			assert.Equal(t, generatedFixtureDigests[candidate.ID], firstDigest, candidate.ID)
			assert.Equal(t, firstDigest, fileDigest(secondPath), candidate.ID)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Lstat(first)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	pdf, err := os.ReadFile(filepath.Join(first, "pdf"))
	require.NoError(t, err)
	assert.Contains(t, string(pdf), "/Kids [3 0 R 4 0 R] /Count 2")
}

func TestWriteProbeFixturesMissingOrInvalidSeedsLeavesNoDestination(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "fixtures")
	err := WriteProbeFixtures(t.Context(), destination, FixtureOptions{})
	require.ErrorContains(t, err, "doc, msg, numbers, ppt, xls")
	assert.NoDirExists(t, destination)

	seeds := writeNativeSeeds(t)
	require.NoError(t, os.WriteFile(filepath.Join(seeds, "doc"), []byte("not a document"), 0o600))
	err = WriteProbeFixtures(t.Context(), destination, FixtureOptions{SeedDirectory: seeds})
	require.ErrorContains(t, err, `validate fixture "doc"`)
	assert.NoDirExists(t, destination)
}

func TestValidateProbeFixturesIsLocalAndCleansItsSpool(t *testing.T) {
	fixtureDirectory := filepath.Join(t.TempDir(), "fixtures")
	require.NoError(t, WriteProbeFixtures(t.Context(), fixtureDirectory, FixtureOptions{
		SeedDirectory: writeNativeSeeds(t),
	}))
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	require.NoError(t, os.Mkdir(spoolDirectory, 0o700))
	policy := testPolicy(t, 1<<20, 10)
	require.NoError(t, ValidateProbeFixtures(t.Context(), policy, ProbeFixtureConfig{
		FixtureDirectory: fixtureDirectory, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: 32 << 20, MinFreeBytes: 1,
	}))
	entries, err := os.ReadDir(spoolDirectory)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func writeNativeSeeds(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "seeds")
	require.NoError(t, os.Mkdir(directory, 0o700))
	seeds := map[string][]byte{
		"doc":     compoundDocument(t, "WordDocument"),
		"ppt":     compoundDocument(t, "PowerPoint Document"),
		"xls":     compoundDocument(t, "Workbook"),
		"numbers": documentZIP(t, map[string]string{"Index/Tables/Table.iwa": "synthetic"}),
		"msg":     compoundDocument(t, "__properties_version1.0"),
	}
	for name, content := range seeds {
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), content, 0o600))
	}
	return directory
}
