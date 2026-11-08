package api

import (
	"crypto/sha256"
	"encoding/json/v2"
	"time"

	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/store"
)

type noteContextCursor struct {
	Version           int      `json:"v"`
	IssuedAt          int64    `json:"iat"`
	QueryHash         [32]byte `json:"q"`
	ContentVersionID  string   `json:"cv"`
	GenerationID      string   `json:"g"`
	DatabaseEpoch     string   `json:"epoch"`
	ParserRecipe      string   `json:"parser"`
	ResolverRecipe    string   `json:"resolver"`
	NamespaceRevision int64    `json:"rev"`
	SourceRef         []byte   `json:"source"`
	Ordinal           int64    `json:"ordinal"`
}

func noteContextQueryHash(request NoteContextRequest) [32]byte {
	profile, binding := "", ""
	if request.Coverage != nil {
		profile, binding = request.Coverage.Profile, request.Coverage.BindingID
	}
	encoded, _ := json.Marshal([]string{request.Publication, request.SourceRef, request.Direction, profile, binding})
	return sha256.Sum256(encoded)
}

func (service *documentQueryService) encodeNoteContextCursor(request NoteContextRequest, version string, position store.NoteContextPosition) (string, error) {
	payload, err := json.Marshal(noteContextCursor{Version: 1, IssuedAt: service.now().Unix(), QueryHash: noteContextQueryHash(request),
		ContentVersionID: version, GenerationID: position.GenerationID, DatabaseEpoch: position.DatabaseEpoch,
		ParserRecipe: notemarkdown.Recipe, ResolverRecipe: notemarkdown.ResolverRecipe,
		NamespaceRevision: position.NamespaceRevision, SourceRef: []byte(position.SourceRef), Ordinal: position.Ordinal})
	if err != nil {
		return "", err
	}
	return service.signCursorEnvelope(payload), nil
}

func (service *documentQueryService) decodeNoteContextCursor(request NoteContextRequest) (string, *store.NoteContextPosition, error) {
	payload, err := service.verifyCursorEnvelope(request.Cursor)
	if err != nil {
		return "", nil, store.ErrNoteContextStale
	}
	var cursor noteContextCursor
	if err := json.Unmarshal(payload, &cursor, json.RejectUnknownMembers(true)); err != nil || cursor.Version != 1 ||
		cursor.ParserRecipe != notemarkdown.Recipe || cursor.ResolverRecipe != notemarkdown.ResolverRecipe ||
		cursor.QueryHash != noteContextQueryHash(request) || cursor.ContentVersionID == "" ||
		(request.ContentVersionID != "" && request.ContentVersionID != cursor.ContentVersionID) {
		return "", nil, store.ErrNoteContextStale
	}
	now, issued := service.now(), time.Unix(cursor.IssuedAt, 0)
	if now.Before(issued) || !now.Before(issued.Add(documentCursorTTL)) {
		return "", nil, store.ErrNoteContextStale
	}
	return cursor.ContentVersionID, &store.NoteContextPosition{GenerationID: cursor.GenerationID, DatabaseEpoch: cursor.DatabaseEpoch,
		NamespaceRevision: cursor.NamespaceRevision, SourceRef: string(cursor.SourceRef), Ordinal: cursor.Ordinal}, nil
}
