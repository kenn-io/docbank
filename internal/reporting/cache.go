package reporting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/report"
)

var (
	ErrUnavailable       = errors.New("report handle unavailable")
	ErrCapacity          = errors.New("report capacity exhausted")
	ErrInvalidRevision   = errors.New("invalid report revision")
	ErrVisibilityChanged = report.ErrVisibilityChanged
)

type sharedFrame struct {
	value report.Frame
	scope report.Budget
	refs  int
}

type cacheEntry struct {
	owner      string
	frame      *sharedFrame
	artifact   *report.Artifact
	summary    report.Summary
	choices    []report.DateChoice
	selected   []report.DateSelection
	scope      report.Budget
	pins       int
	readers    map[*pinnedReader]struct{}
	removed    bool
	visibility func(context.Context, report.Frame) error
}

type activeBuild struct {
	owner       string
	ctx         context.Context
	cancel      context.CancelFunc
	invalidated bool
}

type Cache struct {
	mu           sync.Mutex
	now          func() time.Time
	budget       report.Budget
	entries      map[string]*cacheEntry
	builders     map[*activeBuild]struct{}
	ownerHandles map[string]int
	ownerPending map[string]int
	secret       [32]byte
	closed       bool
	wg           sync.WaitGroup
}

func NewCache(now func() time.Time, budget report.Budget) *Cache {
	if now == nil {
		now = time.Now
	}
	cache := &Cache{now: now, budget: budget, entries: make(map[string]*cacheEntry),
		builders: make(map[*activeBuild]struct{}), ownerHandles: make(map[string]int),
		ownerPending: make(map[string]int)}
	if _, err := rand.Read(cache.secret[:]); err != nil {
		panic("report cursor randomness unavailable")
	}
	return cache
}

func (c *Cache) startBuild(parent context.Context, owner string) (context.Context, *activeBuild, error) {
	if c == nil || c.budget == nil || owner == "" {
		return nil, nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, report.DefaultTimeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepExpiredLocked()
	if c.closed {
		cancel()
		return nil, nil, ErrUnavailable
	}
	if len(c.builders) >= 2 || len(c.entries)+len(c.builders) >= 64 ||
		c.ownerHandles[owner]+c.ownerPending[owner] >= 8 {
		cancel()
		return nil, nil, ErrCapacity
	}
	build := &activeBuild{owner: owner, ctx: ctx, cancel: cancel}
	c.builders[build] = struct{}{}
	c.ownerPending[owner]++
	c.wg.Add(1)
	return ctx, build, nil
}

func (c *Cache) finishBuild(build *activeBuild) {
	c.mu.Lock()
	delete(c.builders, build)
	c.ownerPending[build.owner]--
	if c.ownerPending[build.owner] == 0 {
		delete(c.ownerPending, build.owner)
	}
	c.mu.Unlock()
	build.cancel()
	c.wg.Done()
}

func (c *Cache) Create(ctx context.Context, owner string, service *Service, request report.Request) (report.Summary, error) {
	if service == nil {
		return report.Summary{}, ErrUnavailable
	}
	ctx, build, err := c.startBuild(ctx, owner)
	if err != nil {
		return report.Summary{}, err
	}
	defer c.finishBuild(build)
	frameScope := c.budget.Child()
	serviceCopy := *service
	serviceCopy.Budget = frameScope
	frame, err := serviceCopy.Prepare(ctx, request)
	if err != nil {
		_ = frameScope.Close()
		return report.Summary{}, err
	}
	shared := &sharedFrame{value: frame, scope: frameScope, refs: 1}
	entry, err := c.makeEntry(ctx, owner, "", shared, service)
	if err != nil {
		_ = frameScope.Close()
		return report.Summary{}, err
	}
	if err := c.publish(build, entry); err != nil {
		c.releaseEntry(entry)
		return report.Summary{}, err
	}
	return cloneSummary(entry.summary), nil
}

func (c *Cache) Revise(ctx context.Context, owner, id string, service *Service, choices []report.DateChoice) (report.Summary, error) {
	if service == nil {
		return report.Summary{}, ErrUnavailable
	}
	parent, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return report.Summary{}, err
	}
	parent.frame.refs++
	shared := parent.frame
	expires := parent.summary.ExpiresAt
	mergedChoices := slices.Clone(parent.choices)
	c.mu.Unlock()
	ctx, build, err := c.startBuild(ctx, owner)
	if err != nil {
		c.releaseFrame(shared)
		return report.Summary{}, err
	}
	defer c.finishBuild(build)
	if !c.now().Before(expires) {
		c.releaseFrame(shared)
		return report.Summary{}, ErrUnavailable
	}
	incoming := shared.value.Request
	incoming.DateChoices = choices
	if _, err := report.NormalizeRequest(incoming); err != nil {
		c.releaseFrame(shared)
		return report.Summary{}, errors.Join(ErrInvalidRevision, err)
	}
	frame := shared.value
	choiceIndex := make(map[report.Identity]int, len(mergedChoices))
	for index, choice := range mergedChoices {
		choiceIndex[choice.Document] = index
	}
	for _, choice := range choices {
		if index, exists := choiceIndex[choice.Document]; exists {
			mergedChoices[index] = choice
		} else {
			choiceIndex[choice.Document] = len(mergedChoices)
			mergedChoices = append(mergedChoices, choice)
		}
	}
	frame.Request.DateChoices = mergedChoices
	if _, err := report.NormalizeRequest(frame.Request); err != nil {
		c.releaseFrame(shared)
		return report.Summary{}, errors.Join(ErrInvalidRevision, err)
	}
	entry, err := c.makeEntryWithFrame(ctx, owner, id, shared, frame, service)
	if err != nil {
		c.releaseFrame(shared)
		return report.Summary{}, err
	}
	if err := c.publish(build, entry); err != nil {
		c.releaseEntry(entry)
		return report.Summary{}, err
	}
	return cloneSummary(entry.summary), nil
}

func (c *Cache) makeEntry(ctx context.Context, owner, parent string, shared *sharedFrame,
	service *Service,
) (*cacheEntry, error) {
	return c.makeEntryWithFrame(ctx, owner, parent, shared, shared.value, service)
}

func (c *Cache) makeEntryWithFrame(ctx context.Context, owner, parent string, shared *sharedFrame,
	frame report.Frame, service *Service,
) (*cacheEntry, error) {
	id, err := randomReportID()
	if err != nil {
		return nil, err
	}
	expires := shared.value.ObservedAt.Add(30 * time.Minute)
	if shared.value.ObservedAt.IsZero() || !c.now().Before(expires) {
		return nil, ErrUnavailable
	}
	artifactScope := c.budget.Child()
	calculationScope := c.budget.Child()
	defer func() { _ = calculationScope.Close() }()
	serviceCopy := *service
	serviceCopy.Budget = calculationScope
	result, err := serviceCopy.Finalize(ctx, frame, nil)
	entry := &cacheEntry{owner: owner, frame: shared, scope: artifactScope,
		visibility: service.Visibility,
		readers:    make(map[*pinnedReader]struct{}), summary: report.Summary{
			ID: id, ParentID: parent, ObservedAt: shared.value.ObservedAt, ExpiresAt: expires,
			Terms: slices.Clone(frame.Request.Terms),
		}}
	entry.choices = slices.Clone(frame.Request.DateChoices)
	entry.selected = make([]report.DateSelection, len(frame.Members))
	if errors.Is(err, ErrReviewRequired) {
		_ = artifactScope.Close()
		entry.scope = nil
		entry.summary.State = "needs_review"
		choiceByDocument := make(map[report.Identity]*report.DateChoice, len(entry.choices))
		for i := range entry.choices {
			choiceByDocument[entry.choices[i].Document] = &entry.choices[i]
		}
		for i, member := range frame.Members {
			selection, err := report.SelectDate(member.Kind, member.Candidates,
				choiceByDocument[member.Identity], frame.Request)
			entry.selected[i] = selection
			if err != nil {
				entry.summary.UnresolvedDates++
			}
		}
		if err := checkSummaryLimit(entry.summary); err != nil {
			return nil, err
		}
		return entry, nil
	}
	if err != nil {
		_ = artifactScope.Close()
		return nil, err
	}
	var csv bytes.Buffer
	if err := report.WriteCSV(ctx, &csv, result); err != nil {
		_ = artifactScope.Close()
		return nil, err
	}
	if _, err := artifactScope.Reserve(ctx, int64(csv.Len())); err != nil {
		_ = artifactScope.Close()
		return nil, err
	}
	bundle, err := report.BuildBundle(ctx, artifactScope, result)
	if err != nil {
		_ = artifactScope.Close()
		return nil, err
	}
	entry.artifact = &report.Artifact{ID: id, CSV: csv.Bytes(), Bundle: bundle, ExpiresAt: expires}
	entry.summary.State = "complete"
	for i, member := range result.Frame.Members {
		entry.selected[i] = member.Selection
	}
	entry.summary.Counts = slices.Clone(result.Counts)
	entry.summary.Coverage = result.Frame.Coverage
	entry.summary.RowCoverage = slices.Clone(result.Frame.RowCoverage)
	entry.summary.CSVBytes = int64(csv.Len())
	entry.summary.BundleBytes = int64(len(bundle))
	entry.summary.CSVSHA256 = digestBytes(csv.Bytes())
	entry.summary.BundleSHA256 = digestBytes(bundle)
	if err := checkSummaryLimit(entry.summary); err != nil {
		_ = artifactScope.Close()
		return nil, err
	}
	return entry, nil
}

func checkSummaryLimit(summary report.Summary) error {
	encoded, err := json.Marshal(summary)
	if err != nil {
		return errors.Join(report.ErrReportLimit, err)
	}
	if len(encoded) > report.MaxRequestSummaryJSONBytes {
		return report.ErrReportLimit
	}
	return nil
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func randomReportID() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.Join(ErrUnavailable, err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (c *Cache) publish(build *activeBuild, entry *cacheEntry) error {
	if err := c.checkVisibility(build.ctx, entry); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || build.invalidated || !c.now().Before(entry.summary.ExpiresAt) {
		return ErrUnavailable
	}
	if entry.summary.ParentID != "" {
		parent := c.entries[entry.summary.ParentID]
		if parent == nil || parent.owner != entry.owner || parent.frame != entry.frame {
			return ErrUnavailable
		}
	}
	if _, exists := c.entries[entry.summary.ID]; exists {
		return ErrCapacity
	}
	c.entries[entry.summary.ID] = entry
	c.ownerHandles[entry.owner]++
	return nil
}

func (c *Cache) Summary(owner, id string) (report.Summary, error) {
	return c.SummaryContext(context.Background(), owner, id)
}

func (c *Cache) SummaryContext(ctx context.Context, owner, id string) (report.Summary, error) {
	entry, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return report.Summary{}, err
	}
	defer c.mu.Unlock()
	return cloneSummary(entry.summary), nil
}

// Request returns the reusable shape of a frozen run. Reviewed evidence is
// deliberately omitted because it belongs to that observation only.
func (c *Cache) Request(owner, id string) (report.Request, error) {
	return c.RequestContext(context.Background(), owner, id)
}

func (c *Cache) RequestContext(ctx context.Context, owner, id string) (report.Request, error) {
	entry, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return report.Request{}, err
	}
	defer c.mu.Unlock()
	request := entry.frame.value.Request
	request.CollectionIDs = slices.Clone(request.CollectionIDs)
	request.SelectedDocuments = slices.Clone(request.SelectedDocuments)
	request.Terms = slices.Clone(request.Terms)
	request.DateChoices = nil
	return request, nil
}

// MemberIdentities returns the exact member and relation dependencies for a
// durable history receipt. It checks live visibility before commit.
func (c *Cache) MemberIdentities(ctx context.Context, owner, id string) ([]report.Identity, error) {
	entry, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	return report.VisibilityIdentities(entry.frame.value)
}

func (c *Cache) Drop(owner, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[id]; entry != nil && entry.owner == owner {
		c.removeEntryLocked(id, entry)
	}
}

func cloneSummary(summary report.Summary) report.Summary {
	summary.Terms = slices.Clone(summary.Terms)
	summary.Counts = slices.Clone(summary.Counts)
	summary.RowCoverage = slices.Clone(summary.RowCoverage)
	summary.Coverage.Warnings = slices.Clone(summary.Coverage.Warnings)
	return summary
}

func (c *Cache) lookupLocked(owner, id string) (*cacheEntry, error) {
	entry := c.entries[id]
	if owner == "" || entry == nil || entry.owner != owner || entry.removed ||
		!c.now().Before(entry.summary.ExpiresAt) {
		return nil, ErrUnavailable
	}
	return entry, nil
}

func (c *Cache) checkVisibility(ctx context.Context, entry *cacheEntry) error {
	if entry.visibility == nil {
		return nil
	}
	return entry.visibility(ctx, entry.frame.value)
}

// visibleEntryLocked pins the entry while source I/O runs without the cache
// mutex. On success it returns with the mutex held, so the caller can read or
// pin the checked entry before a concurrent revoke or expiry removes it.
func (c *Cache) visibleEntryLocked(ctx context.Context, owner, id string) (*cacheEntry, error) {
	c.mu.Lock()
	c.sweepExpiredLocked()
	entry, err := c.lookupLocked(owner, id)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	entry.pins++
	c.mu.Unlock()

	err = ctx.Err()
	if err == nil {
		err = c.checkVisibility(ctx, entry)
	}
	c.mu.Lock()
	entry.pins--
	if entry.removed && entry.pins == 0 {
		c.releaseEntryLocked(entry)
	}
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	current, lookupErr := c.lookupLocked(owner, id)
	if lookupErr != nil || current != entry {
		c.mu.Unlock()
		return nil, ErrUnavailable
	}
	return entry, nil
}

type dateCursor struct {
	Owner     string `json:"owner"`
	ID        string `json:"id"`
	Member    int    `json:"member"`
	Candidate int    `json:"candidate"`
}

func (c *Cache) encodeCursor(cursor dateCursor) string {
	raw, _ := canonical.Marshal(cursor)
	mac := hmac.New(sha256.New, c.secret[:])
	_, _ = mac.Write(raw)
	return base64.RawURLEncoding.EncodeToString(raw) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (c *Cache) decodeCursor(encoded, owner, id string) (dateCursor, error) {
	var cursor dateCursor
	parts := bytes.Split([]byte(encoded), []byte{'.'})
	if len(parts) != 2 {
		return cursor, ErrUnavailable
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(parts[0]))
	if err != nil {
		return cursor, ErrUnavailable
	}
	sig, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		return cursor, ErrUnavailable
	}
	mac := hmac.New(sha256.New, c.secret[:])
	_, _ = mac.Write(raw)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return cursor, ErrUnavailable
	}
	if err := json.Unmarshal(raw, &cursor); err != nil || cursor.Owner != owner ||
		cursor.ID != id || cursor.Member < 0 || cursor.Candidate < 0 {
		return dateCursor{}, ErrUnavailable
	}
	return cursor, nil
}

func (c *Cache) Dates(ctx context.Context, owner, id string, page report.DatePageRequest) (report.DatePage, error) {
	if page.Limit == 0 {
		page.Limit = 50
	}
	if page.Limit < 1 || page.Limit > 100 {
		return report.DatePage{}, report.ErrReportLimit
	}
	entry, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return report.DatePage{}, err
	}
	entry.frame.refs++
	shared := entry.frame
	selectedDates := slices.Clone(entry.selected)
	choices := slices.Clone(entry.choices)
	c.mu.Unlock()
	defer c.releaseFrame(shared)
	cursor := dateCursor{Owner: owner, ID: id}
	if page.Cursor != "" {
		cursor, err = c.decodeCursor(page.Cursor, owner, id)
		if err != nil {
			return report.DatePage{}, err
		}
	}
	if cursor.Member > len(shared.value.Members) {
		return report.DatePage{}, ErrUnavailable
	}
	result := report.DatePage{Members: make([]report.DateReviewMember, 0, page.Limit)}
	candidates := 0
	for cursor.Member < len(shared.value.Members) && len(result.Members) < page.Limit && candidates < 1000 {
		if err := ctx.Err(); err != nil {
			return report.DatePage{}, err
		}
		member := shared.value.Members[cursor.Member]
		if cursor.Candidate > len(member.Candidates) {
			return report.DatePage{}, ErrUnavailable
		}
		remaining := min(len(member.Candidates)-cursor.Candidate, 1000-candidates)
		item := report.DateReviewMember{Document: member.Identity,
			Selection: selectedDates[cursor.Member], Candidates: slices.Clone(member.Candidates[cursor.Candidate : cursor.Candidate+remaining]),
			CandidatesComplete: cursor.Candidate+remaining == len(member.Candidates)}
		for i := range choices {
			if choices[i].Document == member.Identity {
				choice := choices[i]
				item.Choice = &choice
			}
		}
		result.Members = append(result.Members, item)
		for {
			bytes, exceeded, err := canonical.BoundedSize(result, 1<<20)
			if err != nil {
				return report.DatePage{}, err
			}
			if !exceeded && bytes <= 1<<20 {
				break
			}
			if len(item.Candidates) == 0 {
				return report.DatePage{}, report.ErrReportLimit
			}
			item.Candidates = item.Candidates[:len(item.Candidates)-1]
			item.CandidatesComplete = false
			result.Members[len(result.Members)-1] = item
		}
		consumed := len(item.Candidates)
		candidates += consumed
		cursor.Candidate += consumed
		if cursor.Candidate == len(member.Candidates) {
			cursor.Member++
			cursor.Candidate = 0
		} else if consumed == 0 {
			return report.DatePage{}, report.ErrReportLimit
		}
	}
	if cursor.Member < len(shared.value.Members) {
		result.NextCursor = c.encodeCursor(cursor)
	}
	return result, nil
}

type pinnedReader struct {
	cache  *Cache
	entry  *cacheEntry
	reader *bytes.Reader
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (reader *pinnedReader) Read(out []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := reader.reader.Read(out)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, errors.Join(ErrUnavailable, err)
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, nil
}

func (reader *pinnedReader) Close() error {
	reader.once.Do(func() {
		reader.cancel()
		reader.cache.mu.Lock()
		delete(reader.entry.readers, reader)
		reader.entry.pins--
		if reader.entry.removed && reader.entry.pins == 0 {
			reader.cache.releaseEntryLocked(reader.entry)
		}
		reader.cache.mu.Unlock()
	})
	return nil
}

func (c *Cache) Acquire(ctx context.Context, owner, id, format string) (io.ReadCloser, int64, string, error) {
	entry, err := c.visibleEntryLocked(ctx, owner, id)
	if err != nil {
		return nil, 0, "", err
	}
	defer c.mu.Unlock()
	if entry.artifact == nil {
		return nil, 0, "", ErrReviewRequired
	}
	var raw []byte
	var digest string
	switch format {
	case "csv":
		raw, digest = entry.artifact.CSV, entry.summary.CSVSHA256
	case "bundle":
		raw, digest = entry.artifact.Bundle, entry.summary.BundleSHA256
	default:
		return nil, 0, "", report.ErrReportLimit
	}
	readerCtx, cancel := context.WithCancel(ctx)
	reader := &pinnedReader{cache: c, entry: entry, reader: bytes.NewReader(raw),
		ctx: readerCtx, cancel: cancel}
	entry.pins++
	entry.readers[reader] = struct{}{}
	return reader, int64(len(raw)), digest, nil
}

func (c *Cache) Revoke(owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, entry := range c.entries {
		if entry.owner == owner {
			c.removeEntryLocked(id, entry)
		}
	}
	for build := range c.builders {
		if build.owner == owner {
			build.invalidated = true
			build.cancel()
		}
	}
}

func (c *Cache) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, entry := range c.entries {
		c.removeEntryLocked(id, entry)
	}
	for build := range c.builders {
		build.invalidated = true
		build.cancel()
	}
}

func (c *Cache) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.InvalidateAll()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Cache) sweepExpiredLocked() {
	for id, entry := range c.entries {
		if !c.now().Before(entry.summary.ExpiresAt) {
			c.removeEntryLocked(id, entry)
		}
	}
}

func (c *Cache) removeEntryLocked(id string, entry *cacheEntry) {
	delete(c.entries, id)
	c.ownerHandles[entry.owner]--
	if c.ownerHandles[entry.owner] == 0 {
		delete(c.ownerHandles, entry.owner)
	}
	entry.removed = true
	for reader := range entry.readers {
		reader.cancel()
	}
	if entry.pins == 0 {
		c.releaseEntryLocked(entry)
	}
}

func (c *Cache) releaseEntry(entry *cacheEntry) {
	c.mu.Lock()
	c.releaseEntryLocked(entry)
	c.mu.Unlock()
}

func (c *Cache) releaseEntryLocked(entry *cacheEntry) {
	if entry.scope != nil {
		_ = entry.scope.Close()
		entry.scope = nil
	}
	if entry.frame != nil {
		entry.frame.refs--
		if entry.frame.refs == 0 {
			_ = entry.frame.scope.Close()
		}
		entry.frame = nil
	}
}

func (c *Cache) releaseFrame(frame *sharedFrame) {
	c.mu.Lock()
	frame.refs--
	if frame.refs == 0 {
		_ = frame.scope.Close()
	}
	c.mu.Unlock()
}
