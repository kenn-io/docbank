package api

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

const photoCheckpointPath = "/api/daemon/photo-import-checkpoint"
const photoProfileCookie = "docbank_photo_profile"

const photoCheckpointMaxBytes = 64 << 10
const photoProfileMaxAge = 365 * 24 * 60 * 60

type photoCheckpointHeader struct {
	FormatVersion int       `json:"formatVersion"`
	SourceID      uuid.UUID `json:"sourceID,omitzero"`
	Revision      uuid.UUID `json:"revision,omitzero"`
}
type photoImportCheckpoint struct {
	SourceID        uuid.UUID `json:"sourceID"`
	Revision        uuid.UUID `json:"revision"`
	Destination     string    `json:"destination"`
	ManifestVersion int       `json:"manifestVersion"`
	ManifestHash    string    `json:"manifestHash"`
	PickerKind      string    `json:"pickerKind"`
}
type photoCheckpointChange struct {
	Next     *photoImportCheckpoint   `json:"next"`
	Expected *photoCheckpointIdentity `json:"expected"`
}

func (change *photoCheckpointChange) UnmarshalJSON(raw []byte) error {
	type fields photoCheckpointChange
	var decoded fields
	if err := json.Unmarshal(raw, &decoded, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	if _, ok := members["expected"]; !ok {
		return errors.New("expected is required")
	}
	if _, ok := members["next"]; !ok {
		return errors.New("next is required")
	}
	*change = photoCheckpointChange(decoded)
	return nil
}

func (photoCheckpointChange) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	for _, name := range []string{"expected", "next"} {
		schema.Properties[name] = &huma.Schema{
			Type:     "object",
			Nullable: true,
			AnyOf: []*huma.Schema{
				schema.Properties[name],
				{Type: "null"},
			},
			Extensions: map[string]any{"x-omitempty": false},
		}
	}
	return schema
}

type photoCheckpointIdentity struct {
	SourceID uuid.UUID `json:"sourceID"`
	Revision uuid.UUID `json:"revision"`
}
type photoCheckpointStatus struct {
	Ready      bool                   `json:"ready"`
	Checkpoint *photoCheckpointHeader `json:"checkpoint,omitzero"`
}
type photoCheckpointResult struct {
	Checkpoint *photoImportCheckpoint `json:"checkpoint,omitzero"`
}

type photoCheckpointStore struct {
	mu  sync.Mutex
	dir string
}

func (s *photoCheckpointStore) read(profile string, full bool) (*photoCheckpointHeader, *photoImportCheckpoint, error) {
	f, err := os.Open(filepath.Join(s.dir, profile+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	formatErr := NewError(409, "checkpoint_format", "Saved import uses an unsupported or malformed format.")
	reader := bufio.NewReaderSize(f, 512)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, bufio.ErrBufferFull) {
			return nil, nil, formatErr
		}
		return nil, nil, fmt.Errorf("reading import header: %w", err)
	}
	var header photoCheckpointHeader
	if err := json.Unmarshal(line, &header, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, formatErr
	}
	if header.FormatVersion != 2 || header.SourceID == uuid.Nil() || header.Revision == uuid.Nil() {
		return nil, nil, formatErr
	}
	if !full {
		return &header, nil, nil
	}
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > photoCheckpointMaxBytes {
		return nil, nil, formatErr
	}
	data, err := io.ReadAll(io.LimitReader(reader, photoCheckpointMaxBytes))
	if err != nil {
		return nil, nil, err
	}
	var checkpoint photoImportCheckpoint
	if err := json.Unmarshal(data, &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, formatErr
	}
	if checkpoint.SourceID != header.SourceID || checkpoint.Revision != header.Revision || checkpoint.ManifestVersion != 2 || !isCanonicalHash(checkpoint.ManifestHash) || (checkpoint.PickerKind != "files" && checkpoint.PickerKind != "folder") || !strings.HasPrefix(checkpoint.Destination, "/") {
		return nil, nil, formatErr
	}
	return &header, &checkpoint, nil
}

func (s *photoCheckpointStore) replace(profile string, change photoCheckpointChange) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, _, err := s.read(profile, false)
	if err != nil {
		return err
	}
	expected := change.Expected
	if (current == nil) != (expected == nil) || current != nil && (current.SourceID != expected.SourceID || current.Revision != expected.Revision) {
		return NewError(409, "checkpoint_changed", "Import progress changed in another tab. Close and reopen Add photos.")
	}
	if change.Next == nil {
		if err := os.Remove(filepath.Join(s.dir, profile+".jsonl")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		err := atomicfile.SyncDir(s.dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	header := photoCheckpointHeader{FormatVersion: 2, SourceID: change.Next.SourceID, Revision: change.Next.Revision}
	serialized, err := json.Marshal(header)
	if err != nil {
		return err
	}
	checkpoint, err := json.Marshal(change.Next)
	if err != nil {
		return err
	}
	serialized = append(serialized, '\n')
	serialized = append(serialized, checkpoint...)
	serialized = append(serialized, '\n')
	if len(serialized) > photoCheckpointMaxBytes {
		return errors.New("photo import checkpoint exceeds storage limit")
	}
	if err := safefileio.EnsurePrivateDir(s.dir); err != nil {
		return fmt.Errorf("securing photo import checkpoints: %w", err)
	}
	f, err := atomicfile.Create(filepath.Join(s.dir, profile+".jsonl"), atomicfile.WithPrivate())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Abort()) }()
	if _, err := f.Write(serialized); err != nil {
		return err
	}
	return f.Commit()
}

// Kit atomicfile uses .<target>.tmp-<decimal random suffix>.
var photoCheckpointFilename = regexp.MustCompile(`^(?:[a-f0-9]{64}\.jsonl|\.[a-f0-9]{64}\.jsonl\.tmp-[0-9]+)$`)

func (s *photoCheckpointStore) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-time.Duration(photoProfileMaxAge)*time.Second - 24*time.Hour)
	removed := false
	for _, entry := range entries {
		if !photoCheckpointFilename.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(s.dir, entry.Name())) == nil {
			removed = true
		}
	}
	if removed {
		_ = atomicfile.SyncDir(s.dir)
	}
}

func setPhotoProfileCookie(w http.ResponseWriter, r *http.Request, profile string, secure bool) {
	domain := ""
	hostURL := url.URL{Host: r.Host}
	if strings.HasSuffix(hostURL.Hostname(), ".docbank.localhost") {
		domain = "docbank.localhost"
	}
	http.SetCookie(w, &http.Cookie{Name: photoProfileCookie, Value: profile, Domain: domain, Path: photoCheckpointPath, MaxAge: photoProfileMaxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure}) //nolint:gosec // HTTP loopback origin is intentional.
}

func registerPhotoImportCheckpoint(mux *http.ServeMux, api huma.API, d Deps) {
	owner := &photoCheckpointStore{dir: filepath.Join(d.VaultRoot, "web-photo-imports")}
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		allowed := false
		secure := r.TLS != nil
		for _, origin := range []string{d.Cfg.Web.PublicOrigin, d.WebURL} {
			parsed, err := url.Parse(origin)
			if err == nil && parsed.Host != "" && webSameOrigin(r, &webLoginPolicy{host: parsed.Host, origin: strings.TrimSuffix(origin, "/")}, r.Method == http.MethodPut) {
				allowed = true
				// The browser may use HTTPS through a TLS-terminating proxy.
				secure = secure || parsed.Scheme == "https"
				break
			}
		}
		if !allowed || r.URL.RawQuery != "" {
			writeError(w, NewError(403, "web_origin", "browser Host or Origin rejected"))
			return
		}
		cookies := r.CookiesNamed(photoProfileCookie)
		profile := ""
		if len(cookies) == 1 && len(cookies[0].Value) == 64 {
			if _, err := hex.DecodeString(cookies[0].Value); err == nil {
				profile = cookies[0].Value
			}
		}
		if profile == "" {
			if r.URL.Path != photoCheckpointPath+"/header" || r.Method != http.MethodGet {
				writeError(w, NewError(409, "checkpoint_cookie", "Allow cookies to save and resume photo imports."))
				return
			}
			var random [32]byte
			if _, err := rand.Read(random[:]); err != nil {
				writeError(w, NewError(500, "internal", "could not create import profile"))
				return
			}
			owner.sweep(time.Now())
			setPhotoProfileCookie(w, r, hex.EncodeToString(random[:]), secure)
			writeJSON(w, 200, photoCheckpointStatus{})
			return
		}
		if r.Method == http.MethodPut {
			var change photoCheckpointChange
			if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, photoCheckpointMaxBytes), &change, json.RejectUnknownMembers(true)); err != nil {
				writeError(w, NewError(400, "bad_request", "invalid import checkpoint"))
				return
			}
			if next := change.Next; next != nil {
				if next.SourceID == uuid.Nil() || next.Revision == uuid.Nil() || next.ManifestVersion != 2 || !isCanonicalHash(next.ManifestHash) || (next.PickerKind != "files" && next.PickerKind != "folder") || !strings.HasPrefix(next.Destination, "/") {
					writeError(w, NewError(400, "bad_request", "invalid import checkpoint"))
					return
				}
			}
			if err := owner.replace(profile, change); err != nil {
				checkpointError(w, err)
				return
			}
			setPhotoProfileCookie(w, r, profile, secure)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		header, checkpoint, err := owner.read(profile, r.URL.Path == photoCheckpointPath)
		if err != nil {
			checkpointError(w, err)
			return
		}
		if r.URL.Path == photoCheckpointPath+"/header" {
			writeJSON(w, 200, photoCheckpointStatus{Ready: true, Checkpoint: header})
		} else {
			writeJSON(w, 200, photoCheckpointResult{Checkpoint: checkpoint})
		}
	}
	mux.HandleFunc("GET "+photoCheckpointPath+"/header", handler)
	mux.HandleFunc("GET "+photoCheckpointPath, handler)
	mux.HandleFunc("PUT "+photoCheckpointPath, handler)
	doc := api.OpenAPI()
	body := func(value any) map[string]*huma.MediaType {
		return map[string]*huma.MediaType{jsonMediaType: {Schema: huma.SchemaFromType(doc.Components.Schemas, reflect.TypeOf(value))}}
	}
	for _, operation := range []*huma.Operation{
		{OperationID: "getPhotoImportCheckpointHeader", Method: http.MethodGet, Path: photoCheckpointPath + "/header", Summary: "Read saved photo import identity and confirm the profile cookie", Responses: map[string]*huma.Response{"200": {Description: "Profile and saved import identity", Content: body(photoCheckpointStatus{})}}},
		{OperationID: "getPhotoImportCheckpoint", Method: http.MethodGet, Path: photoCheckpointPath, Summary: "Read the saved photo selection digest", Responses: map[string]*huma.Response{"200": {Description: "Saved selection", Content: body(photoCheckpointResult{})}}},
		{OperationID: "replacePhotoImportCheckpoint", Method: http.MethodPut, Path: photoCheckpointPath, Summary: "Replace or clear saved photo import progress if its identity still matches", RequestBody: &huma.RequestBody{Required: true, Content: body(photoCheckpointChange{})}, Responses: map[string]*huma.Response{"204": {Description: "Saved import replaced or cleared"}}},
	} {
		doc.AddOperation(operation)
	}
}

func checkpointError(w http.ResponseWriter, err error) {
	if apiErr, ok := errors.AsType[*Error](err); ok {
		writeError(w, apiErr)
		return
	}
	writeError(w, NewError(500, "checkpoint_storage", "Could not save or read the photo import. Retry after checking available storage."))
}
