package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document/notemarkdown"
)

const MaxNoteBytes int64 = 4 << 20

// NoteTarget captures the source and placement observed by the note worker.
// The placement fence matters even when a move leaves the version unchanged.
type NoteTarget struct {
	NodeID, Revision                          int64
	VersionID, BlobHash, Path, Name, MIMEType string
	Size                                      int64
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
	Summary      NoteSummary
	State        string
	HeadingCount int
	Frontmatter  map[string]any
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

// NoteCollection returns the nearest containing watch destination, or root.
// A slash boundary prevents /work from also matching /workshop.
func NoteCollection(notePath string, watches []NoteWatch) NoteWatch {
	best := NoteWatch{Destination: "/"}
	for _, watch := range watches {
		dest := watch.Destination
		if dest != "/" && !strings.HasPrefix(notePath, dest+"/") {
			continue
		}
		if len(dest) > len(best.Destination) || (len(dest) == len(best.Destination) && (best.Name == "" || watch.Name < best.Name)) {
			best = watch
		}
	}
	return best
}

// Traverse live ancestry, so a file below a trashed directory is ineligible.
const noteTreeCTE = `WITH RECURSIVE note_tree(id,path) AS (
 SELECT id,CAST('/' AS TEXT) FROM nodes WHERE parent_id IS NULL AND trashed_at IS NULL
 UNION ALL
 SELECT n.id,CASE WHEN t.path='/' THEN '/'||n.name ELSE t.path||'/'||n.name END
 FROM nodes n JOIN note_tree t ON n.parent_id=t.id WHERE n.trashed_at IS NULL
) `

const noteCandidateSQL = `n.kind='file' AND (cv.mime_type='text/markdown'
 OR lower(n.name) LIKE '%.md' OR lower(n.name) LIKE '%.markdown')`

// MissingNoteTargetsAfter lists a bounded batch of missing/current-recipe or
// context-stale projections. Cached relative links are compared with the live
// path, rather than relying on wall clocks or duplicating path authority.
func (s *Store) MissingNoteTargetsAfter(ctx context.Context, recipe, after string, limit int) ([]NoteTarget, error) {
	if recipe == "" || len(recipe) > 256 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid note target query")
	}
	var targets []NoteTarget
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		cursor := after
		for len(targets) < limit {
			var page []NoteTarget
			cached := make(map[string]bool)
			rows, err := tx.QueryContext(ctx, noteTreeCTE+`
 SELECT n.id,n.revision,cv.version_id,cv.blob_hash,t.path,n.name,COALESCE(cv.mime_type,''),cv.size,
        p.version_id IS NOT NULL AND p.recipe=$1
 FROM note_tree t JOIN nodes n ON n.id=t.id JOIN content_versions cv ON cv.version_id=n.current_version_id
 LEFT JOIN note_parses p ON p.version_id=cv.version_id
 WHERE `+noteCandidateSQL+` AND cv.version_id>$2
 AND (p.version_id IS NULL OR p.recipe<>$1 OR EXISTS(SELECT 1 FROM note_links l
      WHERE l.version_id=cv.version_id AND l.target_path IS NOT NULL))
 ORDER BY cv.version_id LIMIT $3`, recipe, cursor, limit)
			if err != nil {
				return err
			}
			for rows.Next() {
				var target NoteTarget
				var current bool
				if err := rows.Scan(&target.NodeID, &target.Revision, &target.VersionID, &target.BlobHash, &target.Path, &target.Name, &target.MIMEType, &target.Size, &current); err != nil {
					_ = rows.Close()
					return err
				}
				page = append(page, target)
				cached[target.VersionID] = current
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(page) == 0 {
				return nil
			}
			for _, target := range page {
				stale := !cached[target.VersionID]
				if !stale {
					var err error
					stale, err = noteLinkContextChanged(ctx, tx, target)
					if err != nil {
						return err
					}
				}
				if stale {
					targets = append(targets, target)
				}
				if len(targets) == limit {
					return nil
				}
			}
			cursor = page[len(page)-1].VersionID
			if len(page) < limit {
				return nil
			}
		}
		return nil
	})
	return targets, err
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

// PruneNoteParses removes one bounded batch of projections that have no live
// current Markdown owner. Cascades remove all four dependent catalogs.
func (s *Store) PruneNoteParses(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 100 {
		return 0, errors.New("invalid note cleanup limit")
	}
	var removed int64
	var obsolete bool
	err := s.db.QueryRowContext(ctx, noteTreeCTE+`SELECT EXISTS(SELECT 1 FROM note_parses p WHERE NOT EXISTS(
 SELECT 1 FROM note_tree t JOIN nodes n ON n.id=t.id JOIN content_versions cv ON cv.version_id=n.current_version_id
 WHERE cv.version_id=p.version_id AND `+noteCandidateSQL+`))`).Scan(&obsolete)
	if err != nil || !obsolete {
		return 0, err
	}
	err = s.withStorageTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, noteTreeCTE+`DELETE FROM note_parses WHERE version_id IN (
 SELECT p.version_id FROM note_parses p WHERE NOT EXISTS(
 SELECT 1 FROM note_tree t JOIN nodes n ON n.id=t.id JOIN content_versions cv ON cv.version_id=n.current_version_id
 WHERE cv.version_id=p.version_id AND `+noteCandidateSQL+`) ORDER BY p.version_id LIMIT $1)`, limit)
		if err != nil {
			return err
		}
		removed, err = result.RowsAffected()
		return err
	})
	return removed, err
}

// PublishNoteParse stores only parser structure. Stale targets do no work;
// the next scan captures the replacement version or new placement.
func (s *Store) PublishNoteParse(ctx context.Context, target NoteTarget, recipe, state string, note notemarkdown.Note) error {
	frontmatter, diagnostic, err := validateNoteParse(target, recipe, state, note)
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
		return insertNoteStructure(ctx, tx, target, note)
	})
}

func validateNoteParse(target NoteTarget, recipe, state string, note notemarkdown.Note) (*string, *string, error) {
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
			encoded, err := json.Marshal(f.Raw)
			if err != nil {
				return nil, nil, fmt.Errorf("encoding note frontmatter: %w", err)
			}
			if len(encoded) > int(MaxNoteBytes)*6 {
				return invalid()
			}
			text := string(encoded)
			raw = &text
		} else {
			text := "invalid_frontmatter"
			diagnostic = &text
		}
	}
	return raw, diagnostic, nil
}

func insertNoteStructure(ctx context.Context, tx *sql.Tx, target NoteTarget, note notemarkdown.Note) error {
	if f := note.Frontmatter; f != nil {
		for _, field := range f.Fields {
			if _, err := tx.ExecContext(ctx, `INSERT INTO note_fields(version_id,key,ordinal,value) VALUES($1,$2,$3,$4)`, target.VersionID, field.Key, field.Ordinal, field.Value); err != nil {
				return err
			}
		}
		for _, alias := range f.Aliases {
			if _, err := tx.ExecContext(ctx, `INSERT INTO note_aliases(version_id,alias_key,alias) VALUES($1,$2,$3) ON CONFLICT(version_id,alias_key) DO NOTHING`, target.VersionID, strings.ToLower(alias), alias); err != nil {
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
		key := strings.ToLower(target)
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
	return &resolved, nil
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
		err = tx.QueryRowContext(ctx, `SELECT state,title,frontmatter_json,(SELECT count(*) FROM note_headings WHERE version_id=$1)
 FROM note_parses WHERE version_id=$1 AND recipe=$2`, n.CurrentVersionID, recipe).Scan(&view.State, &title, &raw, &view.HeadingCount)
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
	items := make([]NoteWatchStatus, 0, len(watches)+1)
	positions := map[string]int{}
	for _, watch := range append(append([]NoteWatch{}, watches...), NoteWatch{Destination: "/"}) {
		key := watch.Name + "\x00" + watch.Destination
		if _, exists := positions[key]; exists {
			continue
		}
		positions[key] = len(items)
		items = append(items, NoteWatchStatus{Name: watch.Name, Destination: watch.Destination})
	}
	lastAt := make(map[int]string)
	err := queryRows(ctx, s.db, noteTreeCTE+`SELECT t.path,COALESCE(p.state,'pending'),COALESCE(p.parsed_at,'')
 FROM note_tree t JOIN nodes n ON n.id=t.id JOIN content_versions cv ON cv.version_id=n.current_version_id
 LEFT JOIN note_parses p ON p.version_id=cv.version_id AND p.recipe=$1 WHERE `+noteCandidateSQL+` ORDER BY cv.version_id`, []any{recipe}, func(rows *sql.Rows) error {
		var notePath, state, at string
		if err := rows.Scan(&notePath, &state, &at); err != nil {
			return err
		}
		watch := NoteCollection(notePath, watches)
		index := positions[watch.Name+"\x00"+watch.Destination]
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
