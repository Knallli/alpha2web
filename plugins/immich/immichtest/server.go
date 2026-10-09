// Package immichtest provides an in-memory fake of the Immich v3 endpoints
// used by alpha2web, for unit and end-to-end tests.
package immichtest

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Asset is an uploaded asset.
type Asset struct {
	ID, Filename, Checksum, CreatedAt, ModifiedAt string
	Size                                          int
}

// Server is a fake Immich server.
type Server struct {
	*httptest.Server
	APIKey string

	mu          sync.Mutex
	assets      map[string]*Asset // by id
	byChecksum  map[string]string
	albums      map[string]string   // id -> name
	albumAssets map[string][]string // album id -> asset ids
	stacks      [][]string
	failNext    int // respond 503 to the next N authenticated requests
	uploads     int
	rejectExt   map[string]bool
	seq         int
}

// New starts a fake server; it is closed when the test ends.
func New(t testing.TB) *Server {
	s := &Server{
		APIKey:      "test-key",
		assets:      map[string]*Asset{},
		byChecksum:  map[string]string{},
		albums:      map[string]string{},
		albumAssets: map[string][]string{},
		rejectExt:   map[string]bool{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// FailNext makes the next n authenticated requests fail with HTTP 503.
func (s *Server) FailNext(n int) { s.mu.Lock(); s.failNext = n; s.mu.Unlock() }

// RejectExtension makes bulk-upload-check reject files with this extension as unsupported.
func (s *Server) RejectExtension(ext string) {
	s.mu.Lock()
	s.rejectExt[strings.ToUpper(ext)] = true
	s.mu.Unlock()
}

// Assets returns a snapshot of uploaded assets.
func (s *Server) Assets() []Asset {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Asset
	for _, a := range s.assets {
		out = append(out, *a)
	}
	return out
}

// UploadCount is the number of POST /assets requests that carried a file.
func (s *Server) UploadCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.uploads }

// Stacks returns created stacks (asset ids, primary first).
func (s *Server) Stacks() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.stacks...)
}

// AlbumAssets returns asset ids in the album with this name.
func (s *Server) AlbumAssets(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.albums {
		if n == name {
			return append([]string(nil), s.albumAssets[id]...)
		}
	}
	return nil
}

// AssetByID returns an asset.
func (s *Server) AssetByID(id string) (Asset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.assets[id]
	if !ok {
		return Asset{}, false
	}
	return *a, true
}

func (s *Server) nextID() string {
	s.seq++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.seq)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/server/version" {
		writeJSON(w, 200, map[string]any{"major": 3, "minor": 3, "patch": 1, "prerelease": nil})
		return
	}
	if r.Header.Get("x-api-key") != s.APIKey {
		writeJSON(w, 401, map[string]any{"message": "Invalid API key"})
		return
	}
	s.mu.Lock()
	if s.failNext > 0 {
		s.failNext--
		s.mu.Unlock()
		io.Copy(io.Discard, r.Body)
		writeJSON(w, 503, map[string]any{"message": "unavailable"})
		return
	}
	s.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
		s.bulkCheck(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
		s.upload(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/api/albums":
		s.listAlbums(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/api/albums":
		s.createAlbum(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/albums/") && strings.HasSuffix(r.URL.Path, "/assets"):
		s.addToAlbum(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/albums/"), "/assets"))
	case r.Method == http.MethodPost && r.URL.Path == "/api/stacks":
		s.createStack(w, r)
	default:
		writeJSON(w, 404, map[string]any{"message": "not found"})
	}
}

func (s *Server) bulkCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Assets []struct{ ID, Checksum string } `json:"assets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]any{"message": err.Error()})
		return
	}
	if len(in.Assets) == 0 {
		// Like Immich v3.2.1, which builds invalid SQL for an empty list.
		writeJSON(w, 500, map[string]any{"message": "Failed to check bulk upload"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	results := []map[string]any{}
	for _, a := range in.Assets {
		ext := ""
		if i := strings.LastIndex(a.ID, "."); i >= 0 {
			ext = strings.ToUpper(a.ID[i:])
		}
		switch {
		case s.byChecksum[strings.ToLower(a.Checksum)] != "":
			results = append(results, map[string]any{"id": a.ID, "action": "reject", "reason": "duplicate", "assetId": s.byChecksum[strings.ToLower(a.Checksum)], "isTrashed": false})
		case s.rejectExt[ext]:
			results = append(results, map[string]any{"id": a.ID, "action": "reject", "reason": "unsupported-format"})
		default:
			results = append(results, map[string]any{"id": a.ID, "action": "accept"})
		}
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, 400, map[string]any{"message": err.Error()})
		return
	}
	fields := map[string]string{}
	h := sha1.New()
	size, gotFile := 0, false
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, 400, map[string]any{"message": err.Error()})
			return
		}
		if p.FormName() == "assetData" {
			n, err := io.Copy(h, p)
			if err != nil {
				writeJSON(w, 400, map[string]any{"message": err.Error()})
				return
			}
			size, gotFile = int(n), true
			continue
		}
		b, _ := io.ReadAll(p)
		fields[p.FormName()] = string(b)
	}
	for _, req := range []string{"fileCreatedAt", "fileModifiedAt"} {
		if fields[req] == "" {
			writeJSON(w, 400, map[string]any{"message": req + " is required"})
			return
		}
	}
	for _, removed := range []string{"deviceAssetId", "deviceId"} {
		if _, ok := fields[removed]; ok {
			writeJSON(w, 400, map[string]any{"message": "property " + removed + " should not exist"})
			return
		}
	}
	if !gotFile {
		writeJSON(w, 400, map[string]any{"message": "assetData is required"})
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if hc := r.Header.Get("x-immich-checksum"); hc != "" && !strings.EqualFold(hc, sum) {
		writeJSON(w, 400, map[string]any{"message": "checksum mismatch"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads++
	if id := s.byChecksum[sum]; id != "" {
		writeJSON(w, 200, map[string]any{"id": id, "status": "duplicate"})
		return
	}
	id := s.nextID()
	s.assets[id] = &Asset{ID: id, Filename: fields["filename"], Checksum: sum, CreatedAt: fields["fileCreatedAt"], ModifiedAt: fields["fileModifiedAt"], Size: size}
	s.byChecksum[sum] = id
	writeJSON(w, 201, map[string]any{"id": id, "status": "created"})
}

func (s *Server) listAlbums(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for id, n := range s.albums {
		if name == "" || n == name {
			out = append(out, map[string]any{"id": id, "albumName": n})
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) createAlbum(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AlbumName string `json:"albumName"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID()
	s.albums[id] = in.AlbumName
	writeJSON(w, 201, map[string]any{"id": id, "albumName": in.AlbumName})
}

func (s *Server) addToAlbum(w http.ResponseWriter, r *http.Request, albumID string) {
	var in struct {
		IDs []string `json:"ids"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.albums[albumID]; !ok {
		writeJSON(w, 400, map[string]any{"message": "album not found"})
		return
	}
	out := []map[string]any{}
	for _, id := range in.IDs {
		dup := false
		for _, existing := range s.albumAssets[albumID] {
			dup = dup || existing == id
		}
		if dup {
			out = append(out, map[string]any{"id": id, "success": false, "error": "duplicate"})
			continue
		}
		s.albumAssets[albumID] = append(s.albumAssets[albumID], id)
		out = append(out, map[string]any{"id": id, "success": true})
	}
	writeJSON(w, 200, out)
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AssetIDs []string `json:"assetIds"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if len(in.AssetIDs) < 2 {
		writeJSON(w, 400, map[string]any{"message": "assetIds must contain at least 2 elements"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stacks = append(s.stacks, in.AssetIDs)
	writeJSON(w, 201, map[string]any{"id": s.nextID(), "primaryAssetId": in.AssetIDs[0], "assets": []any{}})
}
