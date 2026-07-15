package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const maxTagPageSize = 1000

// Tag is one stable organization label. Name is mutable; ID is permanent and
// never reused. AssignmentCount is the current number of tagged nodes.
type Tag struct {
	ID              string
	Name            string
	AssignmentCount int
}

// TaggedNode is one node carrying a tag plus its resolvable live path. Trashed
// nodes have an empty path.
type TaggedNode struct {
	Node Node
	Path string
}

// NormalizeTagName validates and NFC-normalizes a user-facing tag name.
// Names are compared case-sensitively and may contain spaces or '/'.
func NormalizeTagName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: name is not valid UTF-8", ErrInvalidTag)
	}
	name = norm.NFC.String(name)
	if name == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidTag)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("%w: contains a control character", ErrInvalidTag)
	}
	return name, nil
}

func scanTag(row interface{ Scan(args ...any) error }) (Tag, error) {
	var tag Tag
	if err := row.Scan(&tag.ID, &tag.Name, &tag.AssignmentCount); errors.Is(err, sql.ErrNoRows) {
		return Tag{}, ErrNotFound
	} else if err != nil {
		return Tag{}, fmt.Errorf("scanning tag: %w", err)
	}
	return tag, nil
}

// CreateTag defines a tag with a fresh stable identity.
func (s *Store) CreateTag(ctx context.Context, name string) (Tag, error) {
	name, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return Tag{}, fmt.Errorf("allocating tag ID: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tags(id, name) VALUES(?, ?)`, id, name); err != nil {
		if s.driver.IsUniqueViolation(err) {
			return Tag{}, fmt.Errorf("tag %q: %w", name, ErrExists)
		}
		return Tag{}, fmt.Errorf("creating tag %q: %w", name, err)
	}
	return Tag{ID: id, Name: name}, nil
}

// TagByID returns one tag by stable identity.
func (s *Store) TagByID(ctx context.Context, id string) (Tag, error) {
	if err := validateUUIDv4(id); err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", id, ErrNotFound)
	}
	tag, err := scanTag(s.db.QueryRowContext(ctx, `
		SELECT t.id, t.name, COUNT(nt.node_id)
		FROM tags t LEFT JOIN node_tags nt ON nt.tag_id = t.id
		WHERE t.id = ? GROUP BY t.id, t.name`, id))
	if err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", id, err)
	}
	return tag, nil
}

// TagByName returns the tag whose normalized name exactly matches name.
func (s *Store) TagByName(ctx context.Context, name string) (Tag, error) {
	name, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	tag, err := scanTag(s.db.QueryRowContext(ctx, `
		SELECT t.id, t.name, COUNT(nt.node_id)
		FROM tags t LEFT JOIN node_tags nt ON nt.tag_id = t.id
		WHERE t.name = ? GROUP BY t.id, t.name`, name))
	if err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", name, err)
	}
	return tag, nil
}

// Tags lists one name-sorted page and the total number of definitions.
func (s *Store) Tags(ctx context.Context, limit, offset int) ([]Tag, int, error) {
	if err := validateTagPage(limit, offset); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH page AS (
		  SELECT t.id, t.name, COUNT(nt.node_id) AS assignments
		  FROM tags t LEFT JOIN node_tags nt ON nt.tag_id = t.id
		  GROUP BY t.id, t.name ORDER BY t.name, t.id LIMIT ? OFFSET ?
		), totals AS (SELECT COUNT(*) AS total FROM tags)
		SELECT totals.total, COALESCE(page.id, ''), COALESCE(page.name, ''),
		       COALESCE(page.assignments, 0)
		FROM totals LEFT JOIN page ON true ORDER BY page.name, page.id`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		tags  []Tag
		total int
	)
	for rows.Next() {
		var tag Tag
		if err := rows.Scan(&total, &tag.ID, &tag.Name, &tag.AssignmentCount); err != nil {
			return nil, 0, fmt.Errorf("listing tags: scanning page: %w", err)
		}
		if tag.ID != "" {
			tags = append(tags, tag)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing tags: %w", err)
	}
	return tags, total, nil
}

// RenameTag changes a tag's display name and advances every assigned node's
// metadata revision. Repeating the current name is an idempotent no-op.
func (s *Store) RenameTag(ctx context.Context, id, name string) (Tag, error) {
	name, err := NormalizeTagName(name)
	if err != nil {
		return Tag{}, err
	}
	var renamed Tag
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := tagByIDTx(tx, id)
		if err != nil {
			return err
		}
		if current.Name == name {
			renamed = current
			return nil
		}
		if _, err := tx.Exec(`UPDATE tags SET name = ? WHERE id = ?`, name, id); err != nil {
			if s.driver.IsUniqueViolation(err) {
				return fmt.Errorf("tag %q: %w", name, ErrExists)
			}
			return fmt.Errorf("renaming tag %s: %w", id, err)
		}
		if err := touchTaggedNodesTx(tx, id, nowRFC3339()); err != nil {
			return err
		}
		renamed = Tag{ID: id, Name: name, AssignmentCount: current.AssignmentCount}
		return nil
	})
	if err != nil {
		return Tag{}, err
	}
	return renamed, nil
}

// DeleteTag removes one definition and all of its assignments, advancing each
// formerly assigned node's metadata revision exactly once.
func (s *Store) DeleteTag(ctx context.Context, id string) (Tag, error) {
	var deleted Tag
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, err := tagByIDTx(tx, id)
		if err != nil {
			return err
		}
		if err := touchTaggedNodesTx(tx, id, nowRFC3339()); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM tags WHERE id = ?`, id); err != nil {
			return fmt.Errorf("deleting tag %s: %w", id, err)
		}
		deleted = current
		return nil
	})
	if err != nil {
		return Tag{}, err
	}
	return deleted, nil
}

// AssignTag attaches a tag to a node under an optimistic revision check.
// Repeating an existing assignment is an idempotent no-op only when ifRev
// still matches the current node revision.
func (s *Store) AssignTag(
	ctx context.Context, tagID string, nodeID, ifRev int64,
) (Node, Tag, bool, error) {
	return s.changeTagAssignment(ctx, tagID, nodeID, ifRev, true)
}

// UnassignTag removes a tag from a node under an optimistic revision check.
// Repeating an absent assignment is an idempotent no-op only when ifRev still
// matches the current node revision.
func (s *Store) UnassignTag(
	ctx context.Context, tagID string, nodeID, ifRev int64,
) (Node, Tag, bool, error) {
	return s.changeTagAssignment(ctx, tagID, nodeID, ifRev, false)
}

func (s *Store) changeTagAssignment(
	ctx context.Context, tagID string, nodeID, ifRev int64, assign bool,
) (Node, Tag, bool, error) {
	var (
		node    Node
		tag     Tag
		changed bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		tag, err = tagByIDTx(tx, tagID)
		if err != nil {
			return err
		}
		node, err = nodeByIDTx(tx, nodeID)
		if err != nil {
			return err
		}
		if ifRev != UnconditionalRev && node.Revision != ifRev {
			return fmt.Errorf("node %d revision is %d, expected %d: %w",
				nodeID, node.Revision, ifRev, ErrStaleRevision)
		}
		var present int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM node_tags WHERE node_id = ? AND tag_id = ?`, nodeID, tagID,
		).Scan(&present); err != nil {
			return fmt.Errorf("checking tag %s assignment to node %d: %w", tagID, nodeID, err)
		}
		changed = (present == 0) == assign
		if !changed {
			return nil
		}
		if assign {
			if _, err := tx.Exec(`INSERT INTO node_tags(node_id, tag_id) VALUES(?, ?)`, nodeID, tagID); err != nil {
				return fmt.Errorf("assigning tag %s to node %d: %w", tagID, nodeID, err)
			}
			tag.AssignmentCount++
		} else {
			if _, err := tx.Exec(`DELETE FROM node_tags WHERE node_id = ? AND tag_id = ?`, nodeID, tagID); err != nil {
				return fmt.Errorf("unassigning tag %s from node %d: %w", tagID, nodeID, err)
			}
			tag.AssignmentCount--
		}
		now := nowRFC3339()
		if _, err := tx.Exec(
			`UPDATE nodes SET revision = revision + 1, modified_at = ? WHERE id = ?`, now, nodeID,
		); err != nil {
			return fmt.Errorf("advancing node %d after tag assignment change: %w", nodeID, err)
		}
		node, err = nodeByIDTx(tx, nodeID)
		return err
	})
	if err != nil {
		return Node{}, Tag{}, false, err
	}
	return node, tag, changed, nil
}

// NodeTags lists one name-sorted page of tags assigned to a node.
func (s *Store) NodeTags(ctx context.Context, nodeID int64, limit, offset int) ([]Tag, int, error) {
	if err := validateTagPage(limit, offset); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH target AS (SELECT id FROM nodes WHERE id = ?),
		page AS (
		  SELECT t.id, t.name, (SELECT COUNT(*) FROM node_tags all_nt WHERE all_nt.tag_id = t.id) AS assignments
		  FROM tags t JOIN node_tags nt ON nt.tag_id = t.id
		  WHERE nt.node_id = ? ORDER BY t.name, t.id LIMIT ? OFFSET ?
		), totals AS (SELECT COUNT(*) AS total FROM node_tags WHERE node_id = ?)
		SELECT totals.total, COALESCE(page.id, ''), COALESCE(page.name, ''),
		       COALESCE(page.assignments, 0)
		FROM target CROSS JOIN totals LEFT JOIN page ON true ORDER BY page.name, page.id`,
		nodeID, nodeID, limit, offset, nodeID)
	if err != nil {
		return nil, 0, fmt.Errorf("listing tags of node %d: %w", nodeID, err)
	}
	defer func() { _ = rows.Close() }()
	var tags []Tag
	var total int
	found := false
	for rows.Next() {
		found = true
		var tag Tag
		if err := rows.Scan(&total, &tag.ID, &tag.Name, &tag.AssignmentCount); err != nil {
			return nil, 0, fmt.Errorf("listing tags of node %d: scanning page: %w", nodeID, err)
		}
		if tag.ID != "" {
			tags = append(tags, tag)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing tags of node %d: %w", nodeID, err)
	}
	if !found {
		return nil, 0, fmt.Errorf("node %d: %w", nodeID, ErrNotFound)
	}
	return tags, total, nil
}

// TaggedNodes lists one ID-sorted page of nodes carrying tagID.
func (s *Store) TaggedNodes(ctx context.Context, tagID string, limit, offset int) ([]TaggedNode, int, error) {
	if err := validateTagPage(limit, offset); err != nil {
		return nil, 0, err
	}
	if err := validateUUIDv4(tagID); err != nil {
		return nil, 0, fmt.Errorf("tag %q: %w", tagID, ErrNotFound)
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE target AS (SELECT id FROM tags WHERE id = ?),
		matching AS (
		  SELECT n.id AS id, n.parent_id AS parent_id, n.name AS name, n.kind AS kind,
		         COALESCE(n.current_version_id, '') AS current_version_id,
		         COALESCE(cv.blob_hash, '') AS blob_hash,
		         COALESCE(cv.size, 0) AS size, COALESCE(cv.mime_type, '') AS mime_type,
		         n.revision AS revision, n.created_at AS created_at,
		         n.modified_at AS modified_at, n.trashed_at AS trashed_at
		  FROM `+nodeFrom+` JOIN node_tags nt ON nt.node_id = n.id
		  WHERE nt.tag_id = ?
		),
		page AS (
		  SELECT * FROM matching ORDER BY id LIMIT ? OFFSET ?
		), totals AS (SELECT COUNT(*) AS total FROM matching),
		ancestry(node_id, id, parent_id, path) AS (
		  SELECT id, id, parent_id, CASE WHEN name = '' THEN '/' ELSE name END
		  FROM page WHERE trashed_at IS NULL
		  UNION ALL
		  SELECT a.node_id, n.id, n.parent_id,
		         CASE WHEN n.name = '' THEN '/' || a.path ELSE n.name || '/' || a.path END
		  FROM nodes n JOIN ancestry a ON n.id = a.parent_id
		  WHERE n.trashed_at IS NULL
		), paths AS (SELECT node_id, path FROM ancestry WHERE parent_id IS NULL)
		SELECT totals.total, COALESCE(page.id, 0), page.parent_id,
		       COALESCE(page.name, ''), COALESCE(page.kind, ''),
		       COALESCE(page.current_version_id, ''), COALESCE(page.blob_hash, ''),
		       COALESCE(page.size, 0), COALESCE(page.mime_type, ''),
		       COALESCE(page.revision, 0), COALESCE(page.created_at, ''),
		       COALESCE(page.modified_at, ''), page.trashed_at,
		       COALESCE(paths.path, '')
		FROM target CROSS JOIN totals LEFT JOIN page ON true
		LEFT JOIN paths ON paths.node_id = page.id ORDER BY page.id`,
		tagID, tagID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("listing nodes for tag %s: %w", tagID, err)
	}
	defer func() { _ = rows.Close() }()
	var nodes []TaggedNode
	var total int
	found := false
	for rows.Next() {
		found = true
		var tagged TaggedNode
		if err := rows.Scan(&total, &tagged.Node.ID, &tagged.Node.ParentID,
			&tagged.Node.Name, &tagged.Node.Kind, &tagged.Node.CurrentVersionID,
			&tagged.Node.BlobHash, &tagged.Node.Size, &tagged.Node.MimeType,
			&tagged.Node.Revision, &tagged.Node.CreatedAt, &tagged.Node.ModifiedAt,
			&tagged.Node.TrashedAt, &tagged.Path); err != nil {
			return nil, 0, fmt.Errorf("listing nodes for tag %s: scanning page: %w", tagID, err)
		}
		if tagged.Node.ID != 0 {
			nodes = append(nodes, tagged)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing nodes for tag %s: %w", tagID, err)
	}
	if !found {
		return nil, 0, fmt.Errorf("tag %q: %w", tagID, ErrNotFound)
	}
	return nodes, total, nil
}

func tagByIDTx(tx *sql.Tx, id string) (Tag, error) {
	if err := validateUUIDv4(id); err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", id, ErrNotFound)
	}
	tag, err := scanTag(tx.QueryRow(`
		SELECT t.id, t.name, COUNT(nt.node_id)
		FROM tags t LEFT JOIN node_tags nt ON nt.tag_id = t.id
		WHERE t.id = ? GROUP BY t.id, t.name`, id))
	if err != nil {
		return Tag{}, fmt.Errorf("tag %q: %w", id, err)
	}
	return tag, nil
}

func touchTaggedNodesTx(tx *sql.Tx, tagID, timestamp string) error {
	if _, err := tx.Exec(`
		UPDATE nodes SET revision = revision + 1, modified_at = ?
		WHERE id IN (SELECT node_id FROM node_tags WHERE tag_id = ?)`, timestamp, tagID); err != nil {
		return fmt.Errorf("advancing nodes assigned tag %s: %w", tagID, err)
	}
	return nil
}

func validateTagPage(limit, offset int) error {
	if limit < 1 || limit > maxTagPageSize {
		return fmt.Errorf("tag limit must be between 1 and %d", maxTagPageSize)
	}
	if offset < 0 {
		return errors.New("tag offset must not be negative")
	}
	return nil
}
