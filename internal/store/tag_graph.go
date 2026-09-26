package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
)

const (
	TagAssignmentDocument = "document"

	TagAssignmentOriginLegacy = "legacy"

	TagGraphNodeDocument = "document"
	TagGraphNodeTag      = "tag"

	DefaultTagNeighborhoodLimit = 20
	MaxTagNeighborhoodLimit     = 100
	MaxTagGraphHops             = 10
	MaxTagGraphVisitedNodes     = 1000
)

var ErrInvalidTagGraph = errors.New("invalid tag graph request")

// TagGraphFence is an exact, frozen population of current live documents.
type TagGraphFence struct {
	VaultUID          string
	ContentVersionIDs []string
}

// TagGraphSeed selects exactly one document, tag, or future passage identity.
// Passage seeds remain reserved until passage assignments are introduced.
type TagGraphSeed struct {
	NodeID    int64
	TagID     string
	PassageID string
}

// TagNeighborhoodRequest bounds one traversal of direct document-tag edges.
type TagNeighborhoodRequest struct {
	Fence           TagGraphFence
	Seed            TagGraphSeed
	AssignmentKinds []string
	Limit           int
	MaxHops         int
	MaxVisited      int
}

// TagGraphPathStep is one typed vertex in a shortest discovered path.
type TagGraphPathStep struct {
	Kind   string
	NodeID int64
	TagID  string
}

// TagGraphTag is one tag visible through assignments inside the fence.
type TagGraphTag struct {
	ID                  string
	Name                string
	ScopedDocumentCount int
	Weight              float64
	AssignmentOrigin    string
	Path                []TagGraphPathStep
}

// TagGraphDocument is one related document and its explainable topical score.
type TagGraphDocument struct {
	NodeID           int64
	ContentVersionID string
	Name             string
	PathName         string
	ModifiedAt       string
	Score            float64
	SharedTags       []TagGraphTag
	Path             []TagGraphPathStep
}

// TagNeighborhood is a bounded projection over one exact authorized fence.
type TagNeighborhood struct {
	VaultUID              string
	DocumentCount         int
	UntaggedDocumentCount int
	VisitedNodes          int
	Truncated             bool
	Documents             []TagGraphDocument
	Tags                  []TagGraphTag
}

type tagGraphDocumentRecord struct {
	nodeID           int64
	contentVersionID string
	name             string
	modifiedAt       string
	tagIDs           []string
}

type tagGraphTagRecord struct {
	tag  TagGraphTag
	docs []int64
}

type tagGraphVertex struct {
	kind   string
	nodeID int64
	tagID  string
}

// TagRarityWeight returns the topical contribution of one tag inside an
// authorized scope. Invalid or empty scope statistics carry no weight.
func TagRarityWeight(n, df int) float64 {
	if n <= 0 || df < 1 || df > n {
		return 0
	}
	return math.Log1p(float64(n) / (1 + float64(df)))
}

// TagNeighborhood traverses direct tag postings inside one exact current/live
// source fence. It never materializes document pairs and never calls a model.
func (s *Store) TagNeighborhood(
	ctx context.Context, request TagNeighborhoodRequest,
) (TagNeighborhood, error) {
	normalized, ids, err := s.normalizeTagNeighborhoodRequest(request)
	if err != nil {
		return TagNeighborhood{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TagNeighborhood{}, fmt.Errorf("starting tag graph snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := validateTagGraphFenceSnapshot(ctx, tx, ids); err != nil {
		return TagNeighborhood{}, err
	}
	documents, tags, untagged, err := loadTagGraphSnapshot(ctx, tx, ids)
	if err != nil {
		return TagNeighborhood{}, err
	}
	if normalized.Seed.TagID != "" {
		seedTag, err := tagByIDTx(tx, normalized.Seed.TagID)
		if err != nil {
			return TagNeighborhood{}, err
		}
		if _, ok := tags[seedTag.ID]; !ok {
			tags[seedTag.ID] = &tagGraphTagRecord{tag: TagGraphTag{
				ID: seedTag.ID, Name: seedTag.Name, AssignmentOrigin: TagAssignmentOriginLegacy,
			}}
		}
	}
	result, err := tagNeighborhoodFromPostings(ctx, tx, normalized, documents, tags)
	if err != nil {
		return TagNeighborhood{}, err
	}
	result.VaultUID = s.VaultID()
	result.DocumentCount = len(documents)
	result.UntaggedDocumentCount = untagged
	if err := tx.Commit(); err != nil {
		return TagNeighborhood{}, fmt.Errorf("closing tag graph snapshot: %w", err)
	}
	return result, nil
}

func (s *Store) normalizeTagNeighborhoodRequest(
	request TagNeighborhoodRequest,
) (TagNeighborhoodRequest, []string, error) {
	if validateUUIDv4(request.Fence.VaultUID) != nil || request.Fence.VaultUID != s.VaultID() {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: source fence belongs to another vault", ErrInvalidTagGraph)
	}
	seedCount := 0
	if request.Seed.NodeID > 0 {
		seedCount++
	}
	if request.Seed.TagID != "" {
		seedCount++
		if validateUUIDv4(request.Seed.TagID) != nil {
			return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: tag seed is invalid", ErrInvalidTagGraph)
		}
	}
	if request.Seed.PassageID != "" {
		seedCount++
	}
	if seedCount != 1 || request.Seed.NodeID < 0 {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: select exactly one seed", ErrInvalidTagGraph)
	}
	if request.Seed.PassageID != "" {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: passage assignments are not available", ErrInvalidTagGraph)
	}
	if len(request.AssignmentKinds) == 0 {
		request.AssignmentKinds = []string{TagAssignmentDocument}
	}
	if len(request.AssignmentKinds) != 1 || request.AssignmentKinds[0] != TagAssignmentDocument {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: only direct document assignments are available", ErrInvalidTagGraph)
	}
	if request.Limit == 0 {
		request.Limit = DefaultTagNeighborhoodLimit
	}
	if request.Limit < 1 || request.Limit > MaxTagNeighborhoodLimit {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidTagGraph, MaxTagNeighborhoodLimit)
	}
	if request.MaxHops == 0 {
		request.MaxHops = 2
	}
	if request.MaxHops < 1 || request.MaxHops > MaxTagGraphHops {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: max hops must be between 1 and %d", ErrInvalidTagGraph, MaxTagGraphHops)
	}
	if request.MaxVisited == 0 {
		request.MaxVisited = MaxTagGraphVisitedNodes
	}
	if request.MaxVisited < 1 || request.MaxVisited > MaxTagGraphVisitedNodes {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: max visited nodes must be between 1 and %d", ErrInvalidTagGraph, MaxTagGraphVisitedNodes)
	}

	ids := slices.Clone(request.Fence.ContentVersionIDs)
	sort.Strings(ids)
	if len(ids) > MaxSearchSourceFenceIDs {
		return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: source fence exceeds %d documents", ErrInvalidTagGraph, MaxSearchSourceFenceIDs)
	}
	for i, id := range ids {
		if validateUUIDv4(id) != nil {
			return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: content version ID is invalid", ErrInvalidTagGraph)
		}
		if i > 0 && ids[i-1] == id {
			return TagNeighborhoodRequest{}, nil, fmt.Errorf("%w: content version IDs must be unique", ErrInvalidTagGraph)
		}
	}
	request.Fence.ContentVersionIDs = ids
	return request, ids, nil
}

func validateTagGraphFenceSnapshot(ctx context.Context, tx *sql.Tx, ids []string) error {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encoding tag graph source fence: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		WITH requested(version_id) AS (SELECT value FROM json_each(?))
		SELECT requested.version_id,
		       cv.version_id IS NOT NULL,
		       COALESCE(n.current_version_id=requested.version_id AND n.trashed_at IS NULL,0)
		FROM requested
		LEFT JOIN content_versions cv ON cv.version_id=requested.version_id
		LEFT JOIN nodes n ON n.id=cv.node_id
		ORDER BY requested.version_id`, string(encoded))
	if err != nil {
		return fmt.Errorf("resolving tag graph source fence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	observed := 0
	for rows.Next() {
		var id string
		var exists, currentLive bool
		if err := rows.Scan(&id, &exists, &currentLive); err != nil {
			return fmt.Errorf("reading tag graph source fence: %w", err)
		}
		observed++
		if !exists {
			return fmt.Errorf("content version: %w", ErrNotFound)
		}
		if !currentLive {
			return ErrProcessingSourceFenceStaleVersion
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading tag graph source fence: %w", err)
	}
	if observed != len(ids) {
		return errors.New("tag graph source-fence identity count disagrees")
	}
	return nil
}

func loadTagGraphSnapshot(
	ctx context.Context, tx *sql.Tx, ids []string,
) (map[int64]*tagGraphDocumentRecord, map[string]*tagGraphTagRecord, int, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("encoding tag graph postings: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		WITH requested(version_id) AS (SELECT value FROM json_each(?))
		SELECT n.id, cv.version_id, n.name, n.modified_at,
		       COALESCE(t.id,''), COALESCE(t.name,'')
		FROM requested
		JOIN content_versions cv ON cv.version_id=requested.version_id
		JOIN nodes n ON n.id=cv.node_id
		LEFT JOIN node_tags nt ON nt.node_id=n.id
		LEFT JOIN tags t ON t.id=nt.tag_id
		ORDER BY n.id, t.id`, string(encoded))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("loading tag graph postings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	documents := make(map[int64]*tagGraphDocumentRecord, len(ids))
	tags := make(map[string]*tagGraphTagRecord)
	for rows.Next() {
		var nodeID int64
		var versionID, name, modifiedAt, tagID, tagName string
		if err := rows.Scan(&nodeID, &versionID, &name, &modifiedAt, &tagID, &tagName); err != nil {
			return nil, nil, 0, fmt.Errorf("reading tag graph postings: %w", err)
		}
		document := documents[nodeID]
		if document == nil {
			document = &tagGraphDocumentRecord{
				nodeID: nodeID, contentVersionID: versionID, name: name, modifiedAt: modifiedAt,
			}
			documents[nodeID] = document
		}
		if tagID == "" {
			continue
		}
		document.tagIDs = append(document.tagIDs, tagID)
		tag := tags[tagID]
		if tag == nil {
			tag = &tagGraphTagRecord{tag: TagGraphTag{
				ID: tagID, Name: tagName, AssignmentOrigin: TagAssignmentOriginLegacy,
			}}
			tags[tagID] = tag
		}
		tag.docs = append(tag.docs, nodeID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("reading tag graph postings: %w", err)
	}
	untagged := 0
	for _, document := range documents {
		if len(document.tagIDs) == 0 {
			untagged++
		}
	}
	for _, tag := range tags {
		tag.tag.ScopedDocumentCount = len(tag.docs)
		tag.tag.Weight = TagRarityWeight(len(documents), len(tag.docs))
	}
	return documents, tags, untagged, nil
}

func tagNeighborhoodFromPostings(
	ctx context.Context,
	tx *sql.Tx,
	request TagNeighborhoodRequest,
	documents map[int64]*tagGraphDocumentRecord,
	tags map[string]*tagGraphTagRecord,
) (TagNeighborhood, error) {
	start := tagGraphVertex{kind: TagGraphNodeTag, tagID: request.Seed.TagID}
	if request.Seed.NodeID > 0 {
		if documents[request.Seed.NodeID] == nil {
			return TagNeighborhood{}, fmt.Errorf("seed document: %w", ErrNotFound)
		}
		start = tagGraphVertex{kind: TagGraphNodeDocument, nodeID: request.Seed.NodeID}
	}
	queue := []tagGraphVertex{start}
	distance := map[string]int{tagGraphVertexKey(start): 0}
	predecessor := make(map[string]tagGraphVertex)
	vertices := map[string]tagGraphVertex{tagGraphVertexKey(start): start}
	result := TagNeighborhood{Documents: []TagGraphDocument{}, Tags: []TagGraphTag{}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		currentKey := tagGraphVertexKey(current)
		if distance[currentKey] >= request.MaxHops {
			continue
		}
		neighbors := tagGraphNeighbors(current, documents, tags)
		for _, neighbor := range neighbors {
			key := tagGraphVertexKey(neighbor)
			if _, seen := distance[key]; seen {
				continue
			}
			if len(distance) >= request.MaxVisited {
				result.Truncated = true
				continue
			}
			distance[key] = distance[currentKey] + 1
			predecessor[key] = current
			vertices[key] = neighbor
			queue = append(queue, neighbor)
		}
	}
	result.VisitedNodes = len(distance)

	seedTags := make(map[string]struct{})
	if start.kind == TagGraphNodeDocument {
		for _, tagID := range documents[start.nodeID].tagIDs {
			seedTags[tagID] = struct{}{}
		}
	} else {
		seedTags[start.tagID] = struct{}{}
	}
	for key, vertex := range vertices {
		if key == tagGraphVertexKey(start) {
			continue
		}
		path := buildTagGraphPath(start, vertex, predecessor)
		if vertex.kind == TagGraphNodeTag {
			tag := tags[vertex.tagID].tag
			tag.Path = path
			result.Tags = append(result.Tags, tag)
			continue
		}
		record := documents[vertex.nodeID]
		document := TagGraphDocument{
			NodeID: record.nodeID, ContentVersionID: record.contentVersionID,
			Name: record.name, ModifiedAt: record.modifiedAt, Path: path,
			SharedTags: []TagGraphTag{},
		}
		for _, tagID := range record.tagIDs {
			if _, shared := seedTags[tagID]; !shared {
				continue
			}
			tag := tags[tagID].tag
			tag.Path = nil
			document.SharedTags = append(document.SharedTags, tag)
			document.Score += tag.Weight
		}
		pathName, err := pathOf(ctx, tx, record.nodeID)
		if err != nil {
			return TagNeighborhood{}, fmt.Errorf("resolving tag graph document path: %w", err)
		}
		document.PathName = pathName
		result.Documents = append(result.Documents, document)
	}
	sort.Slice(result.Documents, func(i, j int) bool {
		if result.Documents[i].Score != result.Documents[j].Score {
			return result.Documents[i].Score > result.Documents[j].Score
		}
		return result.Documents[i].NodeID < result.Documents[j].NodeID
	})
	sort.Slice(result.Tags, func(i, j int) bool {
		if result.Tags[i].Weight != result.Tags[j].Weight {
			return result.Tags[i].Weight > result.Tags[j].Weight
		}
		return result.Tags[i].ID < result.Tags[j].ID
	})
	if len(result.Documents) > request.Limit {
		result.Documents = result.Documents[:request.Limit]
		result.Tags = nil
		result.Truncated = true
	} else if remaining := request.Limit - len(result.Documents); len(result.Tags) > remaining {
		result.Tags = result.Tags[:remaining]
		result.Truncated = true
	}
	return result, nil
}

func tagGraphNeighbors(
	vertex tagGraphVertex,
	documents map[int64]*tagGraphDocumentRecord,
	tags map[string]*tagGraphTagRecord,
) []tagGraphVertex {
	if vertex.kind == TagGraphNodeDocument {
		document := documents[vertex.nodeID]
		neighbors := make([]tagGraphVertex, 0, len(document.tagIDs))
		for _, tagID := range document.tagIDs {
			neighbors = append(neighbors, tagGraphVertex{kind: TagGraphNodeTag, tagID: tagID})
		}
		return neighbors
	}
	tag := tags[vertex.tagID]
	neighbors := make([]tagGraphVertex, 0, len(tag.docs))
	for _, nodeID := range tag.docs {
		neighbors = append(neighbors, tagGraphVertex{kind: TagGraphNodeDocument, nodeID: nodeID})
	}
	return neighbors
}

func tagGraphVertexKey(vertex tagGraphVertex) string {
	if vertex.kind == TagGraphNodeDocument {
		return fmt.Sprintf("d:%d", vertex.nodeID)
	}
	return "t:" + vertex.tagID
}

func buildTagGraphPath(
	start, target tagGraphVertex, predecessor map[string]tagGraphVertex,
) []TagGraphPathStep {
	vertices := []tagGraphVertex{target}
	for tagGraphVertexKey(vertices[len(vertices)-1]) != tagGraphVertexKey(start) {
		vertices = append(vertices, predecessor[tagGraphVertexKey(vertices[len(vertices)-1])])
	}
	slices.Reverse(vertices)
	path := make([]TagGraphPathStep, len(vertices))
	for i, vertex := range vertices {
		path[i] = TagGraphPathStep{Kind: vertex.kind, NodeID: vertex.nodeID, TagID: vertex.tagID}
	}
	return path
}
