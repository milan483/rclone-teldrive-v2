package teldrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveBetweenEncoders(t *testing.T) {
	ctx := context.Background()
	const leaf = "日本語:report?.txt"
	var requests []api.UpdateFileInformation
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/api/v1/files/source", r.URL.Path)
		var update api.UpdateFileInformation
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, update)
		if len(requests) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	newFs := func(enc encoder.MultiEncoder) *Fs {
		f := &Fs{
			opt:   Options{Enc: enc},
			srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
			pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond))),
		}
		f.dirCache = dircache.New("", "", f)
		return f
	}
	srcFs := newFs(encoder.Standard | encoder.EncodeInvalidUtf8)
	dstFs := newFs(srcFs.opt.Enc | encoder.EncodeWin)
	src := &Object{fs: srcFs, remote: leaf, name: leaf, id: "source"}

	moved, err := dstFs.Move(ctx, src, leaf)
	require.NoError(t, err)
	require.Len(t, requests, 2)
	assert.Equal(t, dstFs.opt.Enc.FromStandardName(leaf), requests[0].Name)
	assert.Equal(t, requests[0], requests[1])
	assert.Equal(t, leaf, moved.Remote())
	assert.Equal(t, leaf, moved.(*Object).name)
	assert.Same(t, dstFs, moved.(*Object).fs)
}

func TestDirMoveEncodedDestination(t *testing.T) {
	ctx := context.Background()
	enc := encoder.Standard | encoder.EncodeWin
	const destination = "目录:renamed?"
	var renamed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			assert.Equal(t, "/api/v1/files", r.URL.Path)
			assert.NoError(t, json.NewEncoder(w).Encode(api.ReadMetadataResponse{}))
		case http.MethodPatch:
			assert.Equal(t, "/api/v1/files/directory", r.URL.Path)
			var update api.UpdateFileInformation
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			renamed = update.Name
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	f := &Fs{
		opt:   Options{Enc: enc},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond))),
	}
	f.dirCache = dircache.New("", "", f)
	require.NoError(t, f.dirCache.FindRoot(ctx, false))
	f.dirCache.Put("source", "directory")

	require.NoError(t, f.DirMove(ctx, f, "source", destination))
	assert.Equal(t, enc.FromStandardName(destination), renamed)
	_, found := f.dirCache.Get("source")
	assert.False(t, found)
}
