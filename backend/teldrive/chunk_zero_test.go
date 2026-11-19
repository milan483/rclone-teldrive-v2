package teldrive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockPartUpload struct {
	body []byte
}

type zeroChunkUpload struct {
	request  api.UploadCreateRequest
	parts    map[int]mockPartUpload
	complete bool
}

type mockUploadState struct {
	request  api.UploadCreateRequest
	parts    map[int]mockPartUpload
	complete bool
}

// mockTeldriveServer implements the subset of the TelDrive v2 API used by
// OpenChunkWriter and its follow-up complete/file lookup calls.
type mockTeldriveServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	partSize int64
	uploads  map[string]*zeroChunkUpload
	files    []api.FileInfo
	nextID   int
}

func newMockTeldriveServer(partSize int64) *mockTeldriveServer {
	m := &mockTeldriveServer{
		partSize: partSize,
		uploads:  make(map[string]*zeroChunkUpload),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, _ *http.Request) {
		writeZeroChunkJSON(w, api.Session{UserName: "test", UserId: 42})
	})
	mux.HandleFunc("POST /api/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		var req api.UploadCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.nextID++
		id := fmt.Sprintf("upload-%d", m.nextID)
		m.uploads[id] = &zeroChunkUpload{request: req, parts: make(map[int]mockPartUpload)}
		m.mu.Unlock()
		writeZeroChunkJSON(w, api.UploadSession{
			ID:           id,
			ParentID:     req.ParentID,
			Name:         req.Name,
			ExpectedSize: req.Size,
			PartSize:     m.partSize,
			State:        "active",
			FileID:       "file-" + id,
		})
	})
	mux.HandleFunc("GET /api/v1/uploads/{id}/parts", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		upload, ok := m.uploads[r.PathValue("id")]
		if !ok {
			m.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		partNos := make([]int, 0, len(upload.parts))
		for partNo := range upload.parts {
			partNos = append(partNos, partNo)
		}
		sort.Ints(partNos)
		items := make([]api.UploadPart, 0, len(partNos))
		for _, partNo := range partNos {
			items = append(items, api.UploadPart{
				UploadID:  r.PathValue("id"),
				PartNo:    partNo,
				State:     "stored",
				PlainSize: int64(len(upload.parts[partNo].body)),
			})
		}
		m.mu.Unlock()
		writeZeroChunkJSON(w, api.UploadPartList{Items: items})
	})
	mux.HandleFunc("PUT /api/v1/uploads/{id}/parts/{part}", func(w http.ResponseWriter, r *http.Request) {
		partNo, err := strconv.Atoi(r.PathValue("part"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.mu.Lock()
		upload, ok := m.uploads[r.PathValue("id")]
		if ok {
			upload.parts[partNo] = mockPartUpload{body: body}
		}
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeZeroChunkJSON(w, api.UploadPart{
			UploadID:  r.PathValue("id"),
			PartNo:    partNo,
			State:     "stored",
			PlainSize: int64(len(body)),
		})
	})
	mux.HandleFunc("POST /api/v1/uploads/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		file := api.FileInfo{Id: "file-" + r.PathValue("id"), Type: "file"}
		m.mu.Lock()
		upload, ok := m.uploads[r.PathValue("id")]
		if ok {
			upload.complete = true
			file.Name = upload.request.Name
			file.MimeType = upload.request.MimeType
			file.Size = upload.request.Size
			file.ParentId = upload.request.ParentID
			file.ModTime = upload.request.ModTime
			m.files = append(m.files, file)
		}
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeZeroChunkJSON(w, file)
	})
	mux.HandleFunc("GET /api/v1/files", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		files := append([]api.FileInfo(nil), m.files...)
		m.mu.Unlock()
		writeZeroChunkJSON(w, api.ReadMetadataResponse{Files: files})
	})
	m.server = httptest.NewServer(mux)
	return m
}

// snapshot returns a copy of the upload state so tests can inspect it
// without racing the HTTP handlers.
func (m *mockTeldriveServer) snapshot() map[string]mockUploadState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]mockUploadState, len(m.uploads))
	for id, upload := range m.uploads {
		parts := make(map[int]mockPartUpload, len(upload.parts))
		maps.Copy(parts, upload.parts)
		out[id] = mockUploadState{
			request:  upload.request,
			parts:    parts,
			complete: upload.complete,
		}
	}
	return out
}

func writeZeroChunkJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func newChunkWriterTestFs(t *testing.T, apiHost string) *Fs {
	t.Helper()
	f, err := NewFs(context.Background(), "TestTeldrive", "", configmap.Simple{
		"api_host":   apiHost,
		"api_key":    "test-key",
		"chunk_size": strconv.FormatInt(64*1024*1024, 10),
	})
	require.NoError(t, err)
	testFs, ok := f.(*Fs)
	require.True(t, ok)
	return testFs
}

func TestOpenChunkWriterEmptyFile(t *testing.T) {
	ctx := context.Background()
	mock := newMockTeldriveServer(5)
	defer mock.server.Close()
	f := newChunkWriterTestFs(t, mock.server.URL)

	const remote = "empty.txt"
	modTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := object.NewStaticObjectInfo(remote, modTime, 0, true, nil, f)

	info, writer, err := f.Features().OpenChunkWriter(ctx, remote, src)
	require.NoError(t, err)
	require.NotNil(t, writer)
	assert.Equal(t, int64(5), info.ChunkSize)

	uploads := mock.snapshot()
	require.Len(t, uploads, 1)
	for _, upload := range uploads {
		assert.Equal(t, int64(0), upload.request.Size)
		assert.False(t, upload.complete)
		assert.Empty(t, upload.parts)
	}

	require.NoError(t, writer.Close(ctx))

	uploads = mock.snapshot()
	require.Len(t, uploads, 1)
	for _, upload := range uploads {
		assert.True(t, upload.complete)
		assert.Empty(t, upload.parts)
	}

	o, err := f.NewObject(ctx, remote)
	require.NoError(t, err)
	assert.Equal(t, int64(0), o.Size())
}

func TestOpenChunkWriterUnknownSize(t *testing.T) {
	ctx := context.Background()
	mock := newMockTeldriveServer(5)
	defer mock.server.Close()
	f := newChunkWriterTestFs(t, mock.server.URL)

	src := object.NewStaticObjectInfo("stream.bin", time.Now(), -1, true, nil, f)

	_, writer, err := f.Features().OpenChunkWriter(ctx, "stream.bin", src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chunk writer requires a known size")
	assert.Nil(t, writer)
	assert.Empty(t, mock.snapshot())
}

func TestOpenChunkWriterMultipart(t *testing.T) {
	ctx := context.Background()
	mock := newMockTeldriveServer(5)
	defer mock.server.Close()
	f := newChunkWriterTestFs(t, mock.server.URL)

	const remote = "multipart.bin"
	data := []byte("0123456789")
	src := object.NewStaticObjectInfo(remote, time.Now(), int64(len(data)), true, nil, f)

	info, writer, err := f.Features().OpenChunkWriter(ctx, remote, src)
	require.NoError(t, err)
	require.NotNil(t, writer)
	require.Equal(t, int64(5), info.ChunkSize)

	for chunkNumber := 0; chunkNumber < 2; chunkNumber++ {
		chunk := data[chunkNumber*5 : (chunkNumber+1)*5]
		n, err := writer.WriteChunk(ctx, chunkNumber, bytes.NewReader(chunk))
		require.NoError(t, err)
		assert.Equal(t, int64(len(chunk)), n)
	}
	require.NoError(t, writer.Close(ctx))

	uploads := mock.snapshot()
	require.Len(t, uploads, 1)
	for _, upload := range uploads {
		assert.Equal(t, int64(len(data)), upload.request.Size)
		assert.True(t, upload.complete)
		require.Len(t, upload.parts, 2)
		assert.Equal(t, data[:5], upload.parts[1].body)
		assert.Equal(t, data[5:], upload.parts[2].body)
	}

	o, err := f.NewObject(ctx, remote)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), o.Size())
}
