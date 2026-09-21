package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/report"
)

type termRelationKey struct {
	operation string
	order     int
	child     sql.NullString
	relation  bool
}

type termRelationGroups struct {
	parent map[string]string
}

func newTermRelationGroups() *termRelationGroups {
	return &termRelationGroups{parent: make(map[string]string)}
}

func (g *termRelationGroups) root(id string) string {
	if g.parent[id] == "" {
		g.parent[id] = id
	}
	for g.parent[id] != id {
		g.parent[id] = g.parent[g.parent[id]]
		id = g.parent[id]
	}
	return id
}

func (g *termRelationGroups) join(a, b string) {
	a, b = g.root(a), g.root(b)
	if a == b {
		return
	}
	if a < b {
		g.parent[b] = a
	} else {
		g.parent[a] = b
	}
}

func readTermReportFamilies(ctx context.Context, q metadataQuerier, budget report.Budget, frame *report.Frame) error {
	versionIDs := make([]string, len(frame.Members))
	for i, member := range frame.Members {
		versionIDs[i] = member.Identity.VersionID
	}
	selected, err := json.Marshal(versionIDs)
	if err != nil {
		return err
	}
	// Traverse both directions so shared children retain connected parents outside
	// the selected collection. Historical or trashed versions cannot connect groups.
	rows, err := q.QueryContext(ctx, `WITH RECURSIVE family_versions(version_id) AS (
		SELECT value FROM json_each(?)
		UNION
		SELECT r.child_version_id FROM family_versions f
		JOIN email_document_publications p ON p.parent_version_id=f.version_id
		JOIN email_document_relations r ON r.operation_id=p.operation_id
		JOIN content_versions cv ON cv.version_id=r.child_version_id
		JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
		UNION
		SELECT p.parent_version_id FROM family_versions f
		JOIN email_document_relations r ON r.child_version_id=f.version_id
		JOIN email_document_publications p ON p.operation_id=r.operation_id
		JOIN content_versions cv ON cv.version_id=p.parent_version_id
		JOIN nodes n ON n.id=cv.node_id AND n.current_version_id=cv.version_id
		WHERE n.kind='file' AND n.trashed_at IS NULL
	) SELECT p.operation_id,COALESCE(r.occurrence_order,0),r.child_version_id,r.operation_id IS NOT NULL
		FROM family_versions f JOIN email_document_publications p ON p.parent_version_id=f.version_id
		LEFT JOIN email_document_relations r ON r.operation_id=p.operation_id
		ORDER BY p.operation_id,r.occurrence_order`, string(selected))
	if err != nil {
		return err
	}
	keys := make([]termRelationKey, 0)
	var relationCount int
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			_ = rows.Close() //nolint:sqlclosecheck // Close the cursor immediately on cancellation.
			return err
		}
		var key termRelationKey
		if err := rows.Scan(&key.operation, &key.order, &key.child, &key.relation); err != nil {
			_ = rows.Close()
			return err
		}
		if key.relation {
			if relationCount == 100000 {
				_ = rows.Close()
				return fmt.Errorf("%w: report family relation limit exceeded", report.ErrReportLimit)
			}
			relationCount++
		}
		if _, err := budget.Reserve(ctx, int64(256+len(key.operation)+len(key.child.String))); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	groups := newTermRelationGroups()
	incomplete := make(map[string]bool)
	childParents := make(map[string]int)
	var publication emailDocumentPublicationRecord
	var lastOperation string
	var parent report.Identity
	var parentCurrent bool
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if key.operation != lastOperation {
			publication, err = loadEmailDocumentPublication(ctx, q, key.operation)
			if err != nil {
				return fmt.Errorf("report family publication %s: %w", key.operation, err)
			}
			lastOperation = key.operation
			identity := publication.Request.Parent
			parent, parentCurrent, err = currentTermRelationIdentity(ctx, q,
				identity.NodeID, identity.VersionID, identity.SHA256)
			if err != nil {
				return err
			}
			if parentCurrent && publication.Receipt.InventoryState != report.StateComplete {
				incomplete[parent.VersionID] = true
			}
		}
		if !key.relation && len(publication.Receipt.Relations) == 0 {
			continue
		}
		if key.order < 1 || key.order > len(publication.Receipt.Relations) {
			return ErrEmailCorrupt
		}
		relation := publication.Receipt.Relations[key.order-1]
		if relation.Order != key.order || relation.OperationID != key.operation ||
			relation.Parent != publication.Request.Parent {
			return ErrEmailCorrupt
		}
		if relation.Child == nil && key.child.Valid ||
			relation.Child != nil && (!key.child.Valid || relation.Child.VersionID != key.child.String) {
			return ErrEmailCorrupt
		}
		if !parentCurrent {
			continue
		}
		if relation.Child == nil {
			incomplete[parent.VersionID] = true
			continue
		}
		child, childCurrent, err := currentTermRelationIdentity(ctx, q,
			relation.Child.NodeID, relation.Child.VersionID, relation.Child.SHA256)
		if err != nil {
			return err
		}
		if !childCurrent {
			incomplete[parent.VersionID] = true
			continue
		}
		encoded, err := canonical.Marshal(relation)
		if err != nil {
			return err
		}
		if _, err := budget.Reserve(ctx, int64(512+len(encoded))); err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		frame.Relations = append(frame.Relations, report.Relation{
			Parent: parent, Child: child,
			EvidenceID:     key.operation + "#" + strconv.Itoa(key.order),
			EvidenceSHA256: hex.EncodeToString(digest[:]),
		})
		groups.join(parent.VersionID, child.VersionID)
		childParents[child.VersionID]++
	}
	for versionID := range incomplete {
		if _, connected := groups.parent[versionID]; connected {
			incomplete[groups.root(versionID)] = true
		}
	}
	for i := range frame.Members {
		id := frame.Members[i].Identity.VersionID
		if _, connected := groups.parent[id]; !connected {
			if incomplete[id] {
				frame.Members[i].Coverage.FamilyState = "incomplete"
			}
			continue
		}
		root := groups.root(id)
		frame.Members[i].FamilyID = root
		if incomplete[root] {
			frame.Members[i].Coverage.FamilyState = "incomplete"
		}
	}
	sharedChildren := make([]string, 0)
	for child, parents := range childParents {
		if parents > 1 {
			sharedChildren = append(sharedChildren, child)
		}
	}
	slices.Sort(sharedChildren)
	for _, child := range sharedChildren {
		frame.Coverage.Warnings = append(frame.Coverage.Warnings,
			fmt.Sprintf("shared attachment child %s has %d parents", child, childParents[child]))
	}
	return nil
}

func currentTermRelationIdentity(ctx context.Context, q metadataQuerier,
	nodeID int64, versionID, sha string,
) (report.Identity, bool, error) {
	var actual string
	err := q.QueryRowContext(ctx, `SELECT cv.blob_hash FROM nodes n JOIN content_versions cv
		ON cv.node_id=n.id AND cv.version_id=n.current_version_id
		WHERE n.id=? AND n.current_version_id=? AND n.kind='file' AND n.trashed_at IS NULL`,
		nodeID, versionID).Scan(&actual)
	if errors.Is(err, sql.ErrNoRows) {
		return report.Identity{}, false, nil
	}
	if err != nil {
		return report.Identity{}, false, err
	}
	if actual != sha {
		return report.Identity{}, false, ErrEmailCorrupt
	}
	return report.Identity{NodeID: nodeID, VersionID: versionID, SHA256: sha}, true, nil
}
