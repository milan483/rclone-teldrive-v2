// Mock TelDrive API server for backend tests that must run without a real
// server or API key.
//
// Reuse newMock, newFs, getJSON, putContent, readAll and assertNames from
// other tests in this package instead of editing this file. Set partSize,
// ignoreRange or forceEncryption on the mock before the operation they
// should affect.
package teldrive

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
)

// mockAPIKey is the key newFs configures and the mock requires.
const mockAPIKey = "test-key"

// mockTeldrive is an httptest TelDrive v2 API with an in-memory file tree.
//
// files and uploads are guarded by mu. partSize, ignoreRange and
// forceEncryption are read by request handlers; set them before the
// operation they should affect.
type mockTeldrive struct {
	mu              sync.Mutex
	files           map[string]*mockFile
	uploads         map[string]*mockUpload
	nextID          int
	partSize        int64
	ignoreRange     bool
	forceEncryption bool
	server          *httptest.Server

	mux        *http.ServeMux
	requests   int
	authMisses int
}

type mockFile struct {
	api.FileInfo
	content []byte
}

type mockUpload struct {
	api.UploadSession
	request api.UploadCreateRequest
	parts   map[int][]byte
}

// newMock starts an in-memory TelDrive API. partSize defaults to 1024 so
// uploads are chunked without using the client's 64MiB minimum.
func newMock(t *testing.T) *mockTeldrive {
	t.Helper()
	m := &mockTeldrive{
		files:    map[string]*mockFile{},
		uploads:  map[string]*mockUpload{},
		partSize: 1024,
	}
	m.mux = http.NewServeMux()
	m.routes()
	m.server = httptest.NewServer(m)
	t.Cleanup(m.Close)
	return m
}

// Close shuts down the mock server.
func (m *mockTeldrive) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

// newFs builds a teldrive Fs pointed at the mock.
// extra overrides the default api_host, api_key and channel_id values.
func (m *mockTeldrive) newFs(t *testing.T, extra configmap.Simple) fs.Fs {
	t.Helper()
	cfg := configmap.Simple{
		"type":       "teldrive",
		"api_host":   m.server.URL,
		"api_key":    mockAPIKey,
		"channel_id": "123",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	f, err := NewFs(context.Background(), "TestTeldrive", "", cfg)
	if err != nil {
		t.Fatalf("NewFs: %v", err)
	}
	return f
}

// getJSON issues an authenticated GET and decodes the JSON body into out.
func (m *mockTeldrive) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, m.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(apiKeyHeaderName, mockAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %s: %s", path, resp.Status, body)
	}
	if out == nil {
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

func (m *mockTeldrive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !m.authorize(r) {
		writeAPIError(w, http.StatusUnauthorized, "missing or invalid api key")
		return
	}
	m.mux.ServeHTTP(w, r)
}

func (m *mockTeldrive) authorize(r *http.Request) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests++
	if r.Header.Get(apiKeyHeaderName) != mockAPIKey {
		m.authMisses++
		return false
	}
	return true
}

func (m *mockTeldrive) routes() {
	m.mux.HandleFunc("GET /api/v1/me", m.handleMe)
	m.mux.HandleFunc("GET /api/v1/files/statistics/categories", m.handleCategories)
	m.mux.HandleFunc("GET /api/v1/files/{id}/content", m.handleContent)
	m.mux.HandleFunc("GET /api/v1/files/{id}", m.handleGetFile)
	m.mux.HandleFunc("GET /api/v1/files", m.handleList)
	m.mux.HandleFunc("POST /api/v1/folders", m.handleCreateFolder)
	m.mux.HandleFunc("POST /api/v1/uploads", m.handleCreateUpload)
	m.mux.HandleFunc("GET /api/v1/uploads/{id}/parts", m.handleListParts)
	m.mux.HandleFunc("PUT /api/v1/uploads/{id}/parts/{partNo}", m.handlePutPart)
	m.mux.HandleFunc("POST /api/v1/uploads/{id}/complete", m.handleComplete)
	m.mux.HandleFunc("DELETE /api/v1/uploads/{id}", m.handleAbortUpload)
	m.mux.HandleFunc("PATCH /api/v1/files/{id}", m.handlePatchFile)
	m.mux.HandleFunc("POST /api/v1/files/{id}/move", m.handleMove)
	m.mux.HandleFunc("POST /api/v1/files/{id}/copy", m.handleCopy)
	m.mux.HandleFunc("DELETE /api/v1/files/{id}", m.handleDelete)
	m.mux.HandleFunc("/", m.handleNotFound)
}

func (m *mockTeldrive) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusNotFound, "no route for "+r.Method+" "+r.URL.RequestURI())
}

func (m *mockTeldrive) handleMe(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Session{UserId: 1, UserName: "test", Name: "test"})
}

func (m *mockTeldrive) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []*mockFile
	for _, file := range m.files {
		if file.ParentId == q.Get("parentId") {
			items = append(items, file)
		}
	}
	sortFiles(items, q.Get("sort"))
	items, next, ok := pageFiles(w, items, q.Get("cursor"), limit)
	if !ok {
		return
	}
	out := make([]api.FileInfo, len(items))
	for i, file := range items {
		out[i] = file.FileInfo
	}
	writeJSON(w, http.StatusOK, api.ReadMetadataResponse{Files: out, NextCursor: next})
}

func (m *mockTeldrive) handleGetFile(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	file, ok := m.files[r.PathValue("id")]
	var info api.FileInfo
	if ok {
		info = file.FileInfo
	}
	m.mu.Unlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *mockTeldrive) handleContent(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	file, found := m.files[r.PathValue("id")]
	if !found {
		m.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	if file.Type != "file" {
		m.mu.Unlock()
		writeAPIError(w, http.StatusBadRequest, "not a file")
		return
	}
	content := append([]byte(nil), file.content...)
	name := file.Name
	modTime := file.ModTime
	ignoreRange := m.ignoreRange
	m.mu.Unlock()
	if ignoreRange {
		// ServeContent honors Range itself; drop it to exercise clients
		// that must detect a server which ignored the requested range.
		r = r.Clone(r.Context())
		r.Header.Del("Range")
		r.Header.Del("If-Range")
	}
	http.ServeContent(w, r, name, modTime, bytes.NewReader(content))
}

func (m *mockTeldrive) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req api.FolderCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "name required")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.findByName(req.ParentID, req.Name); existing != nil {
		if req.ConflictPolicy == "replace" && existing.Type == "folder" {
			writeJSON(w, http.StatusOK, existing.FileInfo)
			return
		}
		writeAPIError(w, http.StatusConflict, "already exists")
		return
	}
	modTime := req.ModTime
	if modTime.IsZero() {
		modTime = time.Now().UTC()
	}
	info := api.FileInfo{
		Id:       m.allocID(),
		Name:     req.Name,
		ParentId: req.ParentID,
		Type:     "folder",
		ModTime:  modTime.UTC(),
	}
	m.files[info.Id] = &mockFile{FileInfo: info}
	writeJSON(w, http.StatusOK, info)
}

func (m *mockTeldrive) handleCreateUpload(w http.ResponseWriter, r *http.Request) {
	var req api.UploadCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "name required")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Ignore the client's preferred size: rclone aligns chunk_size to at
	// least 64MiB, so tests set partSize to force small multi-part uploads.
	partSize := m.partSize
	if partSize <= 0 {
		partSize = 1024
	}
	enc := req.Encryption
	if m.forceEncryption {
		enc = true
	}
	session := api.UploadSession{
		ID:           "upl-" + m.allocID(),
		ParentID:     req.ParentID,
		Name:         req.Name,
		ExpectedSize: req.Size,
		PartSize:     partSize,
		State:        "open",
		Encryption:   enc,
	}
	m.uploads[session.ID] = &mockUpload{
		UploadSession: session,
		request:       req,
		parts:         map[int][]byte{},
	}
	writeJSON(w, http.StatusOK, session)
}

func (m *mockTeldrive) handleListParts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	up, found := m.uploads[r.PathValue("id")]
	if !found {
		writeAPIError(w, http.StatusNotFound, "upload not found")
		return
	}
	nums := make([]int, 0, len(up.parts))
	for n := range up.parts {
		nums = append(nums, n)
	}
	slices.Sort(nums)
	cursor := q.Get("cursor")
	if cursor != "" {
		partNo, err := strconv.Atoi(cursor)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		idx := slices.Index(nums, partNo)
		if idx < 0 {
			writeAPIError(w, http.StatusBadRequest, "unknown cursor")
			return
		}
		nums = nums[idx+1:]
	}
	var next string
	if limit > 0 && len(nums) > limit {
		next = strconv.Itoa(nums[limit-1])
		nums = nums[:limit]
	}
	items := make([]api.UploadPart, len(nums))
	for i, n := range nums {
		items[i] = api.UploadPart{
			UploadID:  up.ID,
			PartNo:    n,
			State:     "stored",
			PlainSize: int64(len(up.parts[n])),
		}
	}
	writeJSON(w, http.StatusOK, api.UploadPartList{Items: items, NextCursor: next})
}

func (m *mockTeldrive) handlePutPart(w http.ResponseWriter, r *http.Request) {
	partNo, err := strconv.Atoi(r.PathValue("partNo"))
	if err != nil || partNo < 1 {
		writeAPIError(w, http.StatusBadRequest, "invalid part number")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "read part")
		return
	}
	if int64(len(body)) > 64<<20 {
		writeAPIError(w, http.StatusRequestEntityTooLarge, "part too large")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	up, found := m.uploads[r.PathValue("id")]
	if !found || up.State == "completed" {
		writeAPIError(w, http.StatusNotFound, "upload not found")
		return
	}
	up.parts[partNo] = append([]byte(nil), body...)
	writeJSON(w, http.StatusOK, api.UploadPart{
		UploadID:  up.ID,
		PartNo:    partNo,
		State:     "stored",
		PlainSize: int64(len(body)),
	})
}

func (m *mockTeldrive) handleComplete(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	up, found := m.uploads[r.PathValue("id")]
	if !found {
		writeAPIError(w, http.StatusNotFound, "upload not found")
		return
	}
	if up.State == "completed" {
		if file, ok := m.files[up.FileID]; ok {
			writeJSON(w, http.StatusOK, file.FileInfo)
			return
		}
	}
	nums := make([]int, 0, len(up.parts))
	for n := range up.parts {
		nums = append(nums, n)
	}
	slices.Sort(nums)
	var buf bytes.Buffer
	for i, n := range nums {
		if n != i+1 {
			writeAPIError(w, http.StatusBadRequest, "missing part "+strconv.Itoa(i+1))
			return
		}
		buf.Write(up.parts[n])
	}
	if int64(buf.Len()) != up.ExpectedSize {
		writeAPIError(w, http.StatusBadRequest, "assembled size does not match expected size")
		return
	}
	for id, file := range m.files {
		if file.Type == "file" && file.ParentId == up.ParentID && file.Name == up.Name {
			delete(m.files, id)
		}
	}
	info := api.FileInfo{
		Id:         m.allocID(),
		Name:       up.Name,
		MimeType:   up.request.MimeType,
		Size:       int64(buf.Len()),
		ParentId:   up.ParentID,
		Type:       "file",
		ModTime:    up.request.ModTime.UTC(),
		Encryption: up.Encryption,
	}
	m.files[info.Id] = &mockFile{FileInfo: info, content: append([]byte(nil), buf.Bytes()...)}
	up.State = "completed"
	up.FileID = info.Id
	writeJSON(w, http.StatusOK, info)
}

func (m *mockTeldrive) handleAbortUpload(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	if _, found := m.uploads[id]; !found {
		writeAPIError(w, http.StatusNotFound, "upload not found")
		return
	}
	delete(m.uploads, id)
	w.WriteHeader(http.StatusOK)
}

func (m *mockTeldrive) handlePatchFile(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateFileInformation
	if !decodeJSON(w, r, &req) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, found := m.files[r.PathValue("id")]
	if !found {
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	if req.Name != "" {
		file.Name = req.Name
	}
	if req.ModTime != nil {
		file.ModTime = req.ModTime.UTC()
	}
	if req.ParentID != "" {
		file.ParentId = req.ParentID
	}
	if req.Size != 0 {
		file.Size = req.Size
	}
	w.WriteHeader(http.StatusOK)
}

func (m *mockTeldrive) handleMove(w http.ResponseWriter, r *http.Request) {
	var req api.MoveFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, found := m.files[r.PathValue("id")]
	if !found {
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	name := file.Name
	if req.DestinationLeaf != "" {
		name = req.DestinationLeaf
	}
	if !m.resolveNameConflict(w, file.Id, req.Destination, name, req.ConflictPolicy) {
		return
	}
	file.ParentId = req.Destination
	file.Name = name
	w.WriteHeader(http.StatusOK)
}

func (m *mockTeldrive) handleCopy(w http.ResponseWriter, r *http.Request) {
	var req api.FileCopy
	if !decodeJSON(w, r, &req) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	src, found := m.files[r.PathValue("id")]
	if !found {
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	name := req.NewName
	if name == "" {
		name = src.Name
	}
	if !m.resolveNameConflict(w, src.Id, req.Destination, name, req.ConflictPolicy) {
		return
	}
	modTime := src.ModTime
	if !req.UpdatedAt.IsZero() {
		modTime = req.UpdatedAt.UTC()
	}
	info := src.FileInfo
	info.Id = m.allocID()
	info.Name = name
	info.ParentId = req.Destination
	info.ModTime = modTime
	m.files[info.Id] = &mockFile{
		FileInfo: info,
		content:  append([]byte(nil), src.content...),
	}
	writeJSON(w, http.StatusOK, info)
}

func (m *mockTeldrive) handleDelete(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	if _, found := m.files[id]; !found {
		writeAPIError(w, http.StatusNotFound, "file not found")
		return
	}
	m.deleteRecursive(id)
	w.WriteHeader(http.StatusOK)
}

func (m *mockTeldrive) handleCategories(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	type acc struct {
		files int64
		size  int64
	}
	by := map[string]*acc{}
	for _, file := range m.files {
		if file.Type != "file" {
			continue
		}
		cat := "other"
		if i := strings.IndexByte(file.MimeType, '/'); i > 0 {
			cat = file.MimeType[:i]
		}
		a := by[cat]
		if a == nil {
			a = &acc{}
			by[cat] = a
		}
		a.files++
		a.size += file.Size
	}
	names := make([]string, 0, len(by))
	for name := range by {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]api.CategorySize, 0, len(names))
	for _, name := range names {
		a := by[name]
		out = append(out, api.CategorySize{Category: name, TotalFiles: a.files, TotalSize: a.size})
	}
	writeJSON(w, http.StatusOK, out)
}

// allocID returns the next file or upload id. Call with mu held.
func (m *mockTeldrive) allocID() string {
	m.nextID++
	return strconv.Itoa(m.nextID)
}

// findByName returns the child of parentID named name. Call with mu held.
func (m *mockTeldrive) findByName(parentID, name string) *mockFile {
	for _, file := range m.files {
		if file.ParentId == parentID && file.Name == name {
			return file
		}
	}
	return nil
}

// deleteRecursive removes id and every descendant. Call with mu held.
func (m *mockTeldrive) deleteRecursive(id string) {
	delete(m.files, id)
	var children []string
	for childID, file := range m.files {
		if file.ParentId == id {
			children = append(children, childID)
		}
	}
	for _, childID := range children {
		m.deleteRecursive(childID)
	}
}

// resolveNameConflict applies conflictPolicy to another entry with the same
// parent and name. Call with mu held. It reports whether the caller may proceed.
func (m *mockTeldrive) resolveNameConflict(w http.ResponseWriter, selfID, parentID, name, policy string) bool {
	var stale []string
	for id, file := range m.files {
		if id != selfID && file.ParentId == parentID && file.Name == name {
			stale = append(stale, id)
		}
	}
	if len(stale) == 0 {
		return true
	}
	if policy != "replace" {
		writeAPIError(w, http.StatusConflict, "already exists")
		return false
	}
	for _, id := range stale {
		m.deleteRecursive(id)
	}
	return true
}

func parseLimit(w http.ResponseWriter, raw string) (int, bool) {
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid limit")
		return 0, false
	}
	return n, true
}

func pageFiles(w http.ResponseWriter, items []*mockFile, cursor string, limit int) ([]*mockFile, string, bool) {
	if cursor != "" {
		idx := -1
		for i, file := range items {
			if file.Id == cursor {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeAPIError(w, http.StatusBadRequest, "unknown cursor")
			return nil, "", false
		}
		items = items[idx+1:]
	}
	var next string
	if limit > 0 && len(items) > limit {
		next = items[limit-1].Id
		items = items[:limit]
	}
	return items, next, true
}

func sortFiles(items []*mockFile, sortKey string) {
	slices.SortFunc(items, func(a, b *mockFile) int {
		switch sortKey {
		case "name":
			if c := cmp.Compare(a.Name, b.Name); c != 0 {
				return c
			}
		case "modTime":
			if c := a.ModTime.Compare(b.ModTime); c != 0 {
				return c
			}
		}
		return cmpID(a.Id, b.Id)
	})
}

func cmpID(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return cmp.Compare(ai, bi)
	}
	return cmp.Compare(a, b)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, api.Error{Code: status, Message: message})
}

func testModTime() time.Time {
	return time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
}

func putContent(t *testing.T, f fs.Fs, remote string, content []byte) fs.Object {
	t.Helper()
	src := object.NewStaticObjectInfo(remote, testModTime(), int64(len(content)), true, nil, f)
	obj, err := f.Put(context.Background(), bytes.NewReader(content), src)
	if err != nil {
		t.Fatalf("Put %s: %v", remote, err)
	}
	return obj
}

func readAll(t *testing.T, obj fs.Object, options ...fs.OpenOption) []byte {
	t.Helper()
	rc, err := obj.Open(context.Background(), options...)
	if err != nil {
		t.Fatalf("Open %s: %v", obj.Remote(), err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Read %s: %v", obj.Remote(), err)
	}
	return got
}

func entryNames(t *testing.T, f fs.Fs, dir string) []string {
	t.Helper()
	entries, err := f.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("List %q: %v", dir, err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Remote()
	}
	slices.Sort(names)
	return names
}

func assertNames(t *testing.T, f fs.Fs, dir string, want ...string) {
	t.Helper()
	got := entryNames(t, f, dir)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("List %q = %q, want %q", dir, got, want)
	}
}
