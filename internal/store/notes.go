package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/notemarkdown"
	"golang.org/x/text/unicode/norm"
)

const MaxNoteBytes int64 = 4 << 20

// NoteTarget captures the source and placement observed by the note worker.
// The placement fence matters even when a move leaves the version unchanged.
type NoteTarget struct {
	NodeID, Revision          int64
	VersionID, BlobHash, Path string
	Size                      int64
	Prune                     bool
}

type NotePage struct {
	Targets []NoteTarget
	Next    string
	EOF     bool
}

// NoteWatch is effective runtime configuration, never another watch authority.
type NoteWatch struct{ Name, Destination string }

type NoteSummary struct {
	Path                     string
	NodeID                   int64
	ContentVersionID, SHA256 string
	Size                     int64
	ModifiedAt, Title        string
	Tags                     []string
	SourceState              string
}

type NoteView struct {
	Summary          NoteSummary
	State            string
	HeadingCount     int
	Frontmatter      map[string]jsontext.Value
	FrontmatterError string
}

type NoteWatchStatus struct {
	Name, Destination       string
	Parsed, Pending, Failed int
	LastError               string
}

func validNoteState(state string) bool {
	switch state {
	case "ok", "not_utf8", "too_large", "limit":
		return true
	}
	return false
}

// IsNoteCandidate includes oversize Markdown so the worker can record a
// terminal too_large outcome without opening its blob. Readers additionally
// require Size <= MaxNoteBytes.
func IsNoteCandidate(n Node) bool {
	name := strings.ToLower(n.Name)
	return n.Kind == nodeKindFile && n.TrashedAt == nil &&
		(n.MimeType == "text/markdown" || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".markdown"))
}

func noteDestinations(watches []NoteWatch) ([]string, error) {
	destinations := make([]string, len(watches))
	for i, watch := range watches {
		var err error
		destinations[i], err = canonicalBatchMovePath(watch.Destination)
		if err != nil {
			return nil, fmt.Errorf("note collection %q: %w", watch.Name, err)
		}
	}
	return destinations, nil
}

func noteCollection(notePath string, watches []NoteWatch, destinations []string) int {
	best, bestLength := len(watches), 1
	for i, watch := range watches {
		dest := destinations[i]
		if dest != "/" && !strings.HasPrefix(notePath, dest+"/") {
			continue
		}
		if len(dest) > bestLength || (len(dest) == bestLength && (best == len(watches) || watch.Name < watches[best].Name)) {
			best = i
			bestLength = len(dest)
		}
	}
	return best
}

// Traverse live ancestry, so a file below a trashed directory is ineligible.
const noteTreeCTE = `WITH RECURSIVE note_tree(id,path) AS (
 SELECT id,CASE WHEN parent_id IS NULL THEN '/' ELSE '/'||name END FROM nodes WHERE parent_id IS NULL AND trashed_at IS NULL
 UNION ALL
 SELECT n.id,CASE WHEN t.path='/' THEN '/'||n.name ELSE t.path||'/'||n.name END
 FROM nodes n JOIN note_tree t ON n.parent_id=t.id WHERE n.trashed_at IS NULL
) `

const noteCandidateSQL = `n.kind='file' AND (cv.mime_type='text/markdown'
 OR lower(n.name) LIKE '%.md' OR lower(n.name) LIKE '%.markdown')`

// ScanNoteTargetsAfter selects one bounded page before inspecting ancestry or
// cached links. Existing parses also participate so obsolete rows are cleaned
// in the same ordered pass. Next advances even when every row is unchanged.
func (s *Store) ScanNoteTargetsAfter(ctx context.Context, recipe, after string, limit int) (NotePage, error) {
	if recipe == "" || len(recipe) > 256 || !utf8.ValidString(recipe) || limit < 1 || limit > 100 || (after != "" && validateUUIDv4(after) != nil) {
		return NotePage{}, errors.New("invalid note target query")
	}
	var page NotePage
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `WITH candidates AS (
 SELECT cv.version_id FROM nodes n JOIN content_versions cv ON cv.version_id=n.current_version_id AND n.id=cv.node_id
 WHERE cv.version_id>$1 AND n.trashed_at IS NULL AND `+noteCandidateSQL+`
 ORDER BY cv.version_id LIMIT $2
 ), cached AS (
 SELECT version_id FROM note_parses WHERE version_id>$1 ORDER BY version_id LIMIT $2
 ), selected AS (
 SELECT version_id FROM candidates UNION SELECT version_id FROM cached
 ORDER BY version_id LIMIT $2
 ) SELECT cv.version_id,cv.node_id,p.recipe FROM selected s
 JOIN content_versions cv ON cv.version_id=s.version_id
 LEFT JOIN note_parses p ON p.version_id=cv.version_id ORDER BY cv.version_id`, after, limit)
		if err != nil {
			return err
		}
		var selected []NoteTarget
		var recipes []*string
		var ids []int64
		for rows.Next() {
			var target NoteTarget
			var cachedRecipe *string
			if err := rows.Scan(&target.VersionID, &target.NodeID, &cachedRecipe); err != nil {
				_ = rows.Close()
				return err
			}
			selected = append(selected, target)
			recipes = append(recipes, cachedRecipe)
			ids = append(ids, target.NodeID)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(selected) == 0 {
			page.EOF = true
			return nil
		}
		page.Next = selected[len(selected)-1].VersionID
		paths, err := s.pathsOf(ctx, tx, ids)
		if err != nil {
			return err
		}
		for i, target := range selected {
			n, err := nodeByIDTx(ctx, tx, target.NodeID)
			if err != nil {
				return err
			}
			live := n.CurrentVersionID == target.VersionID && IsNoteCandidate(n)
			if live {
				// pathsOf includes trashed ancestry; the owning live lookup
				// explicitly excludes it without walking the complete tree.
				_, err := s.nodeByPath(ctx, tx, paths[i])
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				live = err == nil
			}
			if !live {
				if recipes[i] != nil {
					target.Prune = true
					page.Targets = append(page.Targets, target)
				}
				continue
			}
			target.Path = paths[i]
			target.Revision, target.BlobHash, target.Size = n.Revision, n.BlobHash, n.Size
			stale := recipes[i] == nil || *recipes[i] != recipe
			if !stale {
				stale, err = noteLinkContextChanged(ctx, tx, target)
				if err != nil {
					return err
				}
			}
			if stale {
				page.Targets = append(page.Targets, target)
			}
		}
		return nil
	})
	return page, err
}

func noteLinkContextChanged(ctx context.Context, tx *sql.Tx, target NoteTarget) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT form,target,target_path FROM note_links WHERE version_id=$1 AND target_path IS NOT NULL`, target.VersionID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var form, raw, stored string
		if err := rows.Scan(&form, &raw, &stored); err != nil {
			return false, err
		}
		resolved, _ := noteLinkIdentity(target.Path, form, raw)
		if resolved == nil || *resolved != stored {
			return true, nil
		}
	}
	return false, rows.Err()
}

// PruneNoteParse revalidates ownership inside the deletion transaction. A
// restore or rename after discovery must not delete a newly valid projection.
func (s *Store) PruneNoteParse(ctx context.Context, target NoteTarget) error {
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(ctx, tx, target.NodeID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && n.CurrentVersionID == target.VersionID && IsNoteCandidate(n) {
			canonical, err := pathOf(ctx, tx, n.ID)
			if err != nil {
				return err
			}
			if _, err = s.nodeByPath(ctx, tx, canonical); err == nil {
				return nil
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM note_parses WHERE version_id=$1
 AND EXISTS(SELECT 1 FROM content_versions WHERE version_id=$1 AND node_id=$2)`, target.VersionID, target.NodeID)
		return err
	})
}

// PublishNoteParse stores only parser structure. Stale targets do no work;
// the next scan captures the replacement version or new placement.
func (s *Store) PublishNoteParse(ctx context.Context, target NoteTarget, recipe, state string, note notemarkdown.Note) error {
	frontmatter, diagnostic, err := validateNoteParse(ctx, target, recipe, state, note)
	if err != nil {
		return err
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		n, err := nodeByIDTx(ctx, tx, target.NodeID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !IsNoteCandidate(n) || n.Revision != target.Revision || n.CurrentVersionID != target.VersionID || n.BlobHash != target.BlobHash || n.Size != target.Size {
			return nil
		}
		view, err := nodeViewForNode(ctx, tx, n)
		if err != nil {
			return err
		}
		if view.Path != target.Path || view.Path == "" {
			return nil
		}
		if _, err := s.nodeByPath(ctx, tx, view.Path); errors.Is(err, ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		// Equal-recipe retries must preserve the immutable parse unless a
		// relative destination changed with the node or an ancestor move.
		var prior string
		err = tx.QueryRowContext(ctx, `SELECT recipe FROM note_parses WHERE version_id=$1`, target.VersionID).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && prior == recipe {
			changed, err := noteLinkContextChanged(ctx, tx, target)
			if err != nil || !changed {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM note_parses WHERE version_id=$1`, target.VersionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_parses(version_id,recipe,parsed_at,state,title,title_source,frontmatter_json,frontmatter_error,body_start,lines)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, target.VersionID, recipe, nowRFC3339(), state, note.Title, note.TitleSource, frontmatter, diagnostic, note.BodyStart, note.Lines); err != nil {
			return err
		}
		if err := insertNoteStructure(ctx, tx, target, note); err != nil {
			return err
		}
		return recordProcessedTx(ctx, tx, target.VersionID, "")
	})
}

func validateNoteParse(ctx context.Context, target NoteTarget, recipe, state string, note notemarkdown.Note) (*string, *string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	invalid := func() (*string, *string, error) { return nil, nil, errors.New("invalid note projection") }
	if target.VersionID == "" || recipe == "" || len(recipe) > 256 || !utf8.ValidString(recipe) || target.Size < 0 {
		return invalid()
	}
	if !validNoteState(state) {
		return invalid()
	}
	if state != "ok" {
		if note.Frontmatter != nil || len(note.Headings) > 0 || len(note.Links) > 0 || len(note.Diagnostics) > 0 || note.Title != "" || note.TitleSource != "" || note.BodyStart != 0 || note.Lines != 0 {
			return invalid()
		}
		if (state == "too_large") != (target.Size > MaxNoteBytes) {
			return invalid()
		}
		return nil, nil, nil
	}
	if target.Size > MaxNoteBytes || note.BodyStart < 0 || int64(note.BodyStart) > target.Size || note.Lines < 0 || len(note.Headings) > 4096 || len(note.Links) > 10000 || len(note.Diagnostics) > 32 {
		return invalid()
	}
	if note.TitleSource != "" && note.TitleSource != "frontmatter" && note.TitleSource != "h1" {
		return invalid()
	}
	for _, h := range note.Headings {
		if h.Ordinal < 0 || h.Level < 1 || h.Level > 6 || h.Start < note.BodyStart || h.LineEnd < h.Start || h.SectionEnd < h.LineEnd || int64(h.SectionEnd) > target.Size || h.Line < 1 || h.SectionEndLine < h.Line {
			return invalid()
		}
	}
	for _, l := range note.Links {
		if l.Ordinal < 0 || l.Start < note.BodyStart || l.End < l.Start || int64(l.End) > target.Size || l.Line < 1 {
			return invalid()
		}
		switch l.Form {
		case "markdown", "reference", "wikilink", "embed", "image":
		default:
			return invalid()
		}
	}
	var raw, diagnostic *string
	if f := note.Frontmatter; f != nil {
		if f.Start < 0 || f.End < f.Start || f.End > note.BodyStart || len(f.Raw) > 64 || len(f.Fields) > 64*256 {
			return invalid()
		}
		for _, field := range f.Fields {
			if field.Ordinal < 0 || len(field.Value) > 1024 {
				return invalid()
			}
		}
		if f.Raw != nil {
			encoded, err := encodeNoteRaw(ctx, f.Raw)
			if err != nil {
				return nil, nil, fmt.Errorf("encoding note frontmatter: %w", err)
			}
			raw = &encoded
		} else {
			// Parser diagnostics are bounded codes, never authored YAML text.
			if len(note.Diagnostics) != 1 {
				return invalid()
			}
			d := note.Diagnostics[0]
			if d.Span.Start < 0 || d.Span.End < d.Span.Start || int64(d.Span.End) > target.Size {
				return invalid()
			}
			switch d.Code {
			case "invalid_frontmatter", "unclosed_frontmatter":
				diagnostic = &d.Code
			default:
				return invalid()
			}
		}
	}
	return raw, diagnostic, validateNoteIndexes(ctx, target, note)
}

const maxNoteIndexTextBytes = 2048

// ErrNoteIndexLimit identifies deterministic indexed-text overflow, before SQL.
var ErrNoteIndexLimit = errors.New("note indexed text exceeds 2048 bytes")

// Each actual tuple admits 2048 text bytes plus a conservative 128-byte margin
// for headers, alignment, varlena lengths and an ordinal. This fits the standard
// 8 KiB PostgreSQL B-tree page without compression; SQLite uses the same limit.
// Version-only and version+ordinal PKs count version; fields count key+value+
// version (also bounding their PK), aliases count normalized key+version (also
// bounding the key index), and link path/key indexes each count their own text.
func validateNoteIndexes(ctx context.Context, target NoteTarget, note notemarkdown.Note) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	versionBytes := len(target.VersionID)
	if f := note.Frontmatter; f != nil {
		for _, field := range f.Fields {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(field.Key) > maxNoteIndexTextBytes-versionBytes || len(field.Value) > maxNoteIndexTextBytes-versionBytes-len(field.Key) {
				return ErrNoteIndexLimit
			}
		}
		for _, alias := range f.Aliases {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(strings.ToLower(norm.NFC.String(alias))) > maxNoteIndexTextBytes-versionBytes {
				return ErrNoteIndexLimit
			}
		}
	}
	for _, link := range note.Links {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, key := noteLinkIdentity(target.Path, link.Form, link.Target)
		if (p != nil && len(*p) > maxNoteIndexTextBytes) || (key != nil && len(*key) > maxNoteIndexTextBytes) {
			return ErrNoteIndexLimit
		}
	}
	return ctx.Err()
}

func insertNoteStructure(ctx context.Context, tx *sql.Tx, target NoteTarget, note notemarkdown.Note) error {
	if f := note.Frontmatter; f != nil {
		for _, field := range f.Fields {
			if _, err := tx.ExecContext(ctx, `INSERT INTO note_fields(version_id,key,ordinal,value) VALUES($1,$2,$3,$4)`, target.VersionID, field.Key, field.Ordinal, field.Value); err != nil {
				return err
			}
		}
		for _, alias := range f.Aliases {
			if _, err := tx.ExecContext(ctx, `INSERT INTO note_aliases(version_id,alias_key,alias) VALUES($1,$2,$3) ON CONFLICT(version_id,alias_key) DO NOTHING`, target.VersionID, strings.ToLower(norm.NFC.String(alias)), alias); err != nil {
				return err
			}
		}
	}
	for _, h := range note.Headings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_headings(version_id,ordinal,level,text,slug,path,start,line_end,section_end,line,section_end_line)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, target.VersionID, h.Ordinal, h.Level, h.Text, h.Slug, h.Path, h.Start, h.LineEnd, h.SectionEnd, h.Line, h.SectionEndLine); err != nil {
			return err
		}
	}
	for _, l := range note.Links {
		resolved, key := noteLinkIdentity(target.Path, l.Form, l.Target)
		if _, err := tx.ExecContext(ctx, `INSERT INTO note_links(version_id,ordinal,form,raw,target,target_path,target_key,fragment,label,start,"end",line)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, target.VersionID, l.Ordinal, l.Form, l.Raw, l.Target, resolved, key, l.Fragment, l.Label, l.Start, l.End, l.Line); err != nil {
			return err
		}
	}
	return nil
}

// URI schemes stay external, but a literal colon in a Markdown filename is
// legal. The parser already decoded the target; never decode a second time.
func noteLinkIdentity(notePath, form, target string) (*string, *string) {
	if target == "" {
		return &notePath, nil
	}
	if strings.HasPrefix(target, "//") {
		return nil, nil
	}
	u, err := url.Parse(target)
	if err == nil && u.Scheme != "" {
		// A:notes.md is a legal literal filename; URI-shaped targets and
		// recognized schemes retain their external meaning even with .md.
		scheme := strings.ToLower(u.Scheme)
		literal := !strings.Contains(target, "//") && (strings.HasSuffix(strings.ToLower(target), ".md") || strings.HasSuffix(strings.ToLower(target), ".markdown"))
		if !literal || scheme == "http" || scheme == "https" || scheme == "mailto" || scheme == "file" || scheme == "data" || scheme == "javascript" {
			return nil, nil
		}
	}
	if strings.ContainsAny(target, "\x00\\") {
		return nil, nil
	}
	if (form == "wikilink" || form == "embed") && !strings.Contains(target, "/") {
		key := strings.ToLower(norm.NFC.String(target))
		if strings.HasSuffix(key, ".markdown") {
			key = strings.TrimSuffix(key, ".markdown")
		} else {
			key = strings.TrimSuffix(key, ".md")
		}
		return nil, &key
	}
	resolved := path.Clean(target)
	if !strings.HasPrefix(target, "/") {
		resolved = path.Join(path.Dir(notePath), target)
	}
	canonical, err := canonicalBatchMovePath(resolved)
	if err != nil {
		return nil, nil
	}
	return &canonical, nil
}

// NoteByPath reads one current eligible note and its derived structure from a
// single snapshot. Filename title fallback always follows the live node name.
func (s *Store) NoteByPath(ctx context.Context, notePath, recipe string) (NoteView, error) {
	var view NoteView
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		n, err := s.nodeByPath(ctx, tx, notePath)
		if err != nil {
			return err
		}
		if !IsNoteCandidate(n) || n.Size > MaxNoteBytes {
			return ErrNotFound
		}
		canonical, err := pathOf(ctx, tx, n.ID)
		if err != nil {
			return err
		}
		view.Summary = NoteSummary{Path: canonical, NodeID: n.ID, ContentVersionID: n.CurrentVersionID, SHA256: n.BlobHash, Size: n.Size, ModifiedAt: n.ModifiedAt, Title: strings.TrimSuffix(n.Name, path.Ext(n.Name)), Tags: []string{}}
		view.State = "pending"
		var raw *string
		var title string
		err = tx.QueryRowContext(ctx, `SELECT state,title,frontmatter_json,COALESCE(frontmatter_error,''),(SELECT count(*) FROM note_headings WHERE version_id=$1)
 FROM note_parses WHERE version_id=$1 AND recipe=$2`, n.CurrentVersionID, recipe).Scan(&view.State, &title, &raw, &view.FrontmatterError, &view.HeadingCount)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !validNoteState(view.State) {
			return errors.New("invalid stored note parse state")
		}
		if title != "" {
			view.Summary.Title = title
		}
		if raw != nil {
			if err := json.Unmarshal([]byte(*raw), &view.Frontmatter); err != nil {
				return fmt.Errorf("decoding note frontmatter: %w", err)
			}
		}
		if err := queryRows(ctx, tx, `SELECT value FROM note_fields WHERE version_id=$1 AND key='tags' AND EXISTS(SELECT 1 FROM note_parses WHERE version_id=$1 AND recipe=$2) ORDER BY ordinal`, []any{n.CurrentVersionID, recipe}, func(rows *sql.Rows) error {
			var tag string
			if err := rows.Scan(&tag); err != nil {
				return err
			}
			view.Summary.Tags = append(view.Summary.Tags, tag)
			return nil
		}); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT CASE
 WHEN NOT EXISTS(SELECT 1 FROM watch_sources ws WHERE ws.node_id=n.id) THEN 'not_watched'
 WHEN `+sourcePresentSQL+` THEN 'present' ELSE 'gone' END FROM nodes n WHERE n.id=$1`, n.ID).Scan(&view.Summary.SourceState)
	})
	return view, err
}

// NotesStatus counts current candidates by their nearest configured collection.
// Oversize Markdown counts as a failed attempt once classified by the worker.
func (s *Store) NotesStatus(ctx context.Context, recipe string, watches []NoteWatch) ([]NoteWatchStatus, error) {
	destinations, err := noteDestinations(watches)
	if err != nil {
		return nil, err
	}
	items := make([]NoteWatchStatus, len(watches)+1)
	for i, watch := range watches {
		items[i] = NoteWatchStatus{Name: watch.Name, Destination: watch.Destination}
	}
	items[len(watches)].Destination = "/"
	lastAt := make(map[int]string)
	err = queryRows(ctx, s.db, noteTreeCTE+`SELECT t.path,COALESCE(p.state,'pending'),COALESCE(p.parsed_at,'')
 FROM note_tree t JOIN nodes n ON n.id=t.id JOIN content_versions cv ON cv.version_id=n.current_version_id
 LEFT JOIN note_parses p ON p.version_id=cv.version_id AND p.recipe=$1 WHERE `+noteCandidateSQL, []any{recipe}, func(rows *sql.Rows) error {
		var notePath, state, at string
		if err := rows.Scan(&notePath, &state, &at); err != nil {
			return err
		}
		index := noteCollection(notePath, watches, destinations)
		item := &items[index]
		switch state {
		case "pending":
			item.Pending++
		case "ok":
			item.Parsed++
		default:
			if !validNoteState(state) {
				return errors.New("invalid stored note parse state")
			}
			item.Failed++
			if at >= lastAt[index] {
				item.LastError = state
				lastAt[index] = at
			}
		}
		return nil
	})
	return items, err
}
