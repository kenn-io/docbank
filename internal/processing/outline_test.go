package processing

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestBuildPassageOutlineUsesMarkdownStructureAndStableDuplicateKeys(t *testing.T) {
	body := []byte("preamble\n\n#  **Repeated**   \nintro\n## Child _heading_\nchild\n```md\n# fenced heading\n```\n# Repeated\nlast\n")
	firstHeading := bytes.Index(body, []byte("#  **Repeated**"))
	childHeading := bytes.Index(body, []byte("## Child"))
	secondHeading := bytes.LastIndex(body, []byte("# Repeated"))
	navigation := []document.RenditionNavigationEntryV1{{Key: "unit:first", Byte: firstHeading}}
	units := []store.RenditionUnitRecord{{EvidenceUnitID: "unit:first", Locator: document.EvidenceLocatorV1{
		Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 7, End: 7}}}

	outline, err := buildPassageOutline(body, strings64("a"), strings64("b"), navigation, units)
	require.NoError(t, err)
	require.Len(t, outline.Sections, 3)
	preamble, first, second := outline.Sections[0], outline.Sections[1], outline.Sections[2]
	assert.True(t, preamble.Preamble)
	assert.Equal(t, 0, preamble.ByteStart)
	assert.Equal(t, firstHeading, preamble.ByteEnd)
	assert.Equal(t, "Repeated", first.Title)
	assert.Equal(t, 1, first.Level)
	assert.Equal(t, 1, first.Occurrence)
	assert.Equal(t, firstHeading, first.ByteStart)
	assert.Equal(t, secondHeading, first.ByteEnd)
	assert.Equal(t, childHeading, first.OwnByteEnd)
	assert.Equal(t, 1, first.ChildCount)
	require.Len(t, first.Children, 1)
	assert.Equal(t, "Child heading", first.Children[0].Title)
	assert.Equal(t, childHeading, first.Children[0].ByteStart)
	assert.Equal(t, secondHeading, first.Children[0].ByteEnd)
	assert.Equal(t, "Repeated", second.Title)
	assert.Equal(t, 2, second.Occurrence)
	assert.NotEqual(t, first.Key, second.Key)
	assert.NotEmpty(t, preamble.Key)
	assert.Equal(t, len(body)-secondHeading, second.EstimatedUTF8Bytes)
	assert.Equal(t, utf8.RuneCount(body[secondHeading:]), second.EstimatedRunes)
	require.NotNil(t, first.SourceLocator)
	assert.EqualValues(t, 7, first.SourceLocator.Start)
	assert.Nil(t, first.Children[0].SourceLocator)
}

func TestBuildPassageOutlineAlwaysReturnsExplicitPreamble(t *testing.T) {
	for name, body := range map[string][]byte{
		"no heading": []byte("plain text\nwith lines\n"),
		"empty":      {},
	} {
		t.Run(name, func(t *testing.T) {
			outline, err := buildPassageOutline(body, strings64("a"), strings64("b"), nil, nil)
			require.NoError(t, err)
			require.Len(t, outline.Sections, 1)
			assert.True(t, outline.Sections[0].Preamble)
			assert.Equal(t, 0, outline.Sections[0].ByteStart)
			assert.Equal(t, len(body), outline.Sections[0].ByteEnd)
			assert.Equal(t, len(body), outline.Sections[0].EstimatedUTF8Bytes)
		})
	}
}

func TestBuildPassageOutlineBoundsPublishedTitles(t *testing.T) {
	body := []byte("# " + strings.Repeat("😀", 8193) + "\n")
	outline, err := buildPassageOutline(body, strings64("a"), strings64("b"), nil, nil)
	require.NoError(t, err)
	require.Len(t, outline.Sections, 2)
	assert.Equal(t, 8192, utf8.RuneCountInString(outline.Sections[1].Title))
}

func TestBuildPassageOutlineRejectsUndeliverableResult(t *testing.T) {
	var body strings.Builder
	for index := range 2000 {
		fmt.Fprintf(&body, "# Synthetic heading %04d\n", index)
	}
	_, err := buildPassageOutline([]byte(body.String()), strings64("a"), strings64("b"), nil, nil)
	require.ErrorContains(t, err, "outline exceeds the response limit")
}

func TestReadPassageSectionPagesExactBytesWithinSerializedBudget(t *testing.T) {
	body := []byte("# Large section\n\n| left | right |\n|---|---|\n" +
		"```text\n# code, not a heading\n```\n" + strings.Repeat("evidence 😀\n", 7000))
	ref := testSectionRef(t, body)
	outline, err := buildPassageOutline(body, ref.RenditionBuildID, ref.BodySHA256, nil, nil)
	require.NoError(t, err)
	require.Len(t, outline.Sections, 2)
	section := outline.Sections[1]
	key := [32]byte{1, 2, 3}
	request := SectionReadRequest{Ref: ref, NavigationKey: section.Key, IncludeChildren: true,
		MaxBytes: DefaultPassageReadBytes}

	var joined []byte
	pages := 0
	for {
		page, readErr := readPassageSection(body, outline, request, key, "synthetic-principal", "synthetic-scope")
		require.NoError(t, readErr)
		encoded, marshalErr := json.Marshal(page)
		require.NoError(t, marshalErr)
		assert.LessOrEqual(t, len(encoded), request.MaxBytes)
		joined = append(joined, page.Text...)
		pages++
		if page.Complete {
			assert.Empty(t, page.Continuation)
			break
		}
		require.NotEmpty(t, page.Continuation)
		require.NotNil(t, page.Ref)
		require.NoError(t, document.ValidatePassageRefV1(*page.Ref, body))
		request.Continuation = page.Continuation
	}
	assert.Greater(t, pages, 2)
	assert.Equal(t, body[section.ByteStart:section.ByteEnd], joined)
}

func TestReadPassageSectionHonorsOptionalChildrenAndEmptyPreamble(t *testing.T) {
	body := []byte("# Parent\nparent\n## Child\nchild\n# Sibling\nsibling\n")
	ref := testSectionRef(t, body)
	outline, err := buildPassageOutline(body, ref.RenditionBuildID, ref.BodySHA256, nil, nil)
	require.NoError(t, err)
	parent := outline.Sections[1]
	key := [32]byte{9}

	without, err := readPassageSection(body, outline, SectionReadRequest{Ref: ref,
		NavigationKey: parent.Key, MaxBytes: 4096}, key, "principal", "scope")
	require.NoError(t, err)
	assert.True(t, without.Complete)
	assert.Equal(t, string(body[parent.ByteStart:parent.OwnByteEnd]), without.Text)
	assert.NotContains(t, without.Text, "## Child")

	withChildren, err := readPassageSection(body, outline, SectionReadRequest{Ref: ref,
		NavigationKey: parent.Key, IncludeChildren: true, MaxBytes: 4096}, key, "principal", "scope")
	require.NoError(t, err)
	assert.Equal(t, string(body[parent.ByteStart:parent.ByteEnd]), withChildren.Text)
	assert.Contains(t, withChildren.Text, "## Child")

	empty, err := readPassageSection(body, outline, SectionReadRequest{Ref: ref,
		NavigationKey: outline.Sections[0].Key, MaxBytes: 4096}, key, "principal", "scope")
	require.NoError(t, err)
	assert.True(t, empty.Complete)
	assert.Empty(t, empty.Text)
	assert.Nil(t, empty.Ref)
	assert.Nil(t, empty.Section.SourceLocator)
}

func TestReadPassageSectionRejectsChangedAuthorityMissingKeysAndSmallBudgets(t *testing.T) {
	body := []byte("# Section\n" + strings.Repeat("abcdefghij", 1000))
	ref := testSectionRef(t, body)
	outline, err := buildPassageOutline(body, ref.RenditionBuildID, ref.BodySHA256, nil, nil)
	require.NoError(t, err)
	request := SectionReadRequest{Ref: ref, NavigationKey: outline.Sections[1].Key, MaxBytes: 4096}
	key := [32]byte{4, 5, 6}
	first, err := readPassageSection(body, outline, request, key, "principal", "scope")
	require.NoError(t, err)
	require.False(t, first.Complete)

	changed := append([]byte(nil), body...)
	changed[len(changed)-1] = 'Z'
	changedRef := testSectionRef(t, changed)
	request.Ref = changedRef
	request.Continuation = first.Continuation
	_, err = readPassageSection(changed, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrSectionContinuation)

	request = SectionReadRequest{Ref: ref, NavigationKey: "absent", MaxBytes: 4096}
	_, err = readPassageSection(body, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrSectionNotFound)

	request = SectionReadRequest{Ref: ref, NavigationKey: outline.Sections[1].Key, MaxBytes: 1}
	_, err = readPassageSection(body, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrSectionBudget)

	request.MaxBytes = MaxPassageReadBytes + 1
	_, err = readPassageSection(body, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrPassageInvalid)

	request.MaxBytes = 4096
	request.Continuation = first.Continuation + "x"
	_, err = readPassageSection(body, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrSectionContinuation)
}

func TestReadPassageSectionContinuationBindsPrincipalScopeAndSelection(t *testing.T) {
	body := []byte("# Parent\n" + strings.Repeat("parent evidence\n", 800) +
		"## Child\n" + strings.Repeat("child evidence\n", 800))
	ref := testSectionRef(t, body)
	outline, err := buildPassageOutline(body, ref.RenditionBuildID, ref.BodySHA256, nil, nil)
	require.NoError(t, err)
	parent := outline.Sections[1]
	key := [32]byte{3, 2, 1}
	request := SectionReadRequest{Ref: ref, NavigationKey: parent.Key, IncludeChildren: true, MaxBytes: 4096}
	first, err := readPassageSection(body, outline, request, key, "principal", "scope")
	require.NoError(t, err)
	require.False(t, first.Complete)
	request.Continuation = first.Continuation

	_, err = readPassageSection(body, outline, request, key, "other-principal", "scope")
	require.ErrorIs(t, err, ErrSectionContinuation)
	_, err = readPassageSection(body, outline, request, key, "principal", "other-scope")
	require.ErrorIs(t, err, ErrSectionContinuation)
	request.IncludeChildren = false
	_, err = readPassageSection(body, outline, request, key, "principal", "scope")
	require.ErrorIs(t, err, ErrSectionContinuation)
}

func TestOutlineAndReadSectionUseExactAuthorizedRendition(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	outline, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs,
		OutlineRequest{Ref: fixture.ref})
	require.NoError(t, err)
	require.Len(t, outline.Sections, 2)
	section := outline.Sections[1]
	require.NotNil(t, section.SourceLocator)
	assert.Equal(t, document.EvidenceLocatorPage, section.SourceLocator.Kind)

	page, err := readAuthorizedPassageSection(t.Context(), fixture.catalog, fixture.blobs,
		SectionReadRequest{Ref: fixture.ref, NavigationKey: section.Key, IncludeChildren: true, MaxBytes: 4096},
		[32]byte{7}, "principal", "scope")
	require.NoError(t, err)
	assert.True(t, page.Complete)
	assert.Contains(t, page.Text, "# Synthetic heading")
	assert.Contains(t, page.Text, "😀 evidence")
	assert.Equal(t, 2, fixture.catalog.calls)
	assert.Equal(t, 2, fixture.blobs.calls)
}

func TestOutlineFailsClosedBeforeOpeningUnavailableOrUnauthorizedRenditions(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	foreign := fixture.ref
	foreign.VaultUID = "99999999-9999-4999-8999-999999999999"
	_, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: foreign})
	require.ErrorIs(t, err, ErrPassageUnauthorized)
	assert.Zero(t, fixture.catalog.calls)
	assert.Zero(t, fixture.blobs.calls)

	fixture = newPassageResolutionFixture(t)
	fixture.catalog.err = store.ErrPassageAuthorityUnavailable
	_, err = outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Equal(t, 1, fixture.catalog.calls)
	assert.Zero(t, fixture.blobs.calls)
}

func TestOutlineAndSectionDenyRevokedSuppliedInputBeforeOpeningBlob(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.inputBinding = passageProcessingHash("supplied input")
	fixture.catalog.inputVisible = false
	_, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs,
		OutlineRequest{Ref: fixture.ref, principal: "principal"})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls)

	_, err = readAuthorizedPassageSection(t.Context(), fixture.catalog, fixture.blobs,
		SectionReadRequest{Ref: fixture.ref, NavigationKey: strings64("a"), MaxBytes: 4096},
		[32]byte{1}, "principal", "scope")
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls)
}

func TestOutlineReadsVisibleSuppliedInput(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.inputBinding = passageProcessingHash("supplied input")
	fixture.catalog.inputVisible = true
	outline, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs,
		OutlineRequest{Ref: fixture.ref, principal: "principal"})
	require.NoError(t, err)
	require.Len(t, outline.Sections, 2)
}

func TestOutlineUsesAdoptedLocalBuildWhilePreservingOriginReference(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.ref.FederationDomainUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	localBuildID := passageProcessingHash("adopted local build")
	fixture.catalog.vaultID = "99999999-9999-4999-8999-999999999999"
	fixture.catalog.authority.Build.ID = localBuildID
	fixture.blobs.payload = bytes.Replace(fixture.blobs.payload,
		[]byte(fixture.ref.RenditionBuildID), []byte(localBuildID), 1)
	artifactHash := passageProcessingHashBytes(fixture.blobs.payload)
	fixture.catalog.authority.Artifact.BlobHash = artifactHash
	fixture.catalog.authority.Artifact.Size = int64(len(fixture.blobs.payload))

	outline, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.NoError(t, err)
	assert.Equal(t, localBuildID, outline.RenditionBuildID)
	require.Len(t, outline.Sections, 2)
	page, err := readAuthorizedPassageSection(t.Context(), fixture.catalog, fixture.blobs,
		SectionReadRequest{Ref: fixture.ref, NavigationKey: outline.Sections[1].Key, MaxBytes: 4096},
		[32]byte{1}, "principal", "scope")
	require.NoError(t, err)
	assert.Contains(t, page.Text, "Synthetic heading")
	assert.Equal(t, localBuildID, page.RenditionBuildID)
	require.NotNil(t, page.Ref)
	assert.Equal(t, fixture.ref.RenditionBuildID, page.Ref.RenditionBuildID)
}

func TestOutlineRejectsChangedRetainedRenditionBytes(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.blobs.payload[len(fixture.blobs.payload)-2] ^= 1
	_, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.ErrorIs(t, err, ErrPassageCorrupt)
}

func TestOutlineRejectsBlobVerificationFailure(t *testing.T) {
	fixture := newPassageResolutionFixture(t)
	fixture.blobs.verifyErr = errors.New("synthetic verification failure")
	_, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.ErrorIs(t, err, ErrPassageCorrupt)
}

func TestOutlineAndSectionRejectOversizeArtifactBeforeOpeningBlob(t *testing.T) {
	const maximum = int64((64 << 20) + (256 << 10))
	fixture := newPassageResolutionFixture(t)
	fixture.catalog.authority.Artifact.Size = maximum
	_, err := outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.ErrorIs(t, err, ErrPassageCorrupt, "an artifact at the limit must reach blob verification")
	assert.Equal(t, 1, fixture.blobs.calls)

	fixture = newPassageResolutionFixture(t)
	fixture.catalog.authority.Artifact.Size = maximum + 1
	_, err = outlinePassage(t.Context(), fixture.catalog, fixture.blobs, OutlineRequest{Ref: fixture.ref})
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls, "oversize outline must fail before retained bytes are opened")
	_, err = readAuthorizedPassageSection(t.Context(), fixture.catalog, fixture.blobs,
		SectionReadRequest{Ref: fixture.ref, NavigationKey: strings64("a"), MaxBytes: 4096},
		[32]byte{1}, "principal", "scope")
	require.ErrorIs(t, err, ErrPassageUnavailable)
	assert.Zero(t, fixture.blobs.calls, "oversize section must fail before retained bytes are opened")
}

func testSectionRef(t *testing.T, body []byte) document.PassageRefV1 {
	t.Helper()
	end := min(len(body), 1)
	if end == 0 {
		body = []byte("x")
		end = 1
	}
	ref, err := document.NewPassageRefV1(document.PassageRefV1{
		VaultUID: "11111111-1111-4111-8111-111111111111", DocumentUID: "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333", SourceSHA256: strings64("a"),
		RenditionBuildID: strings64("b"), AttachmentID: strings64("c"),
	}, body, 0, end)
	require.NoError(t, err)
	return ref
}

func strings64(value string) string { return string(bytes.Repeat([]byte(value), 64)) }
