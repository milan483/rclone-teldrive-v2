package teldrive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilenameBoundaries(t *testing.T) {
	for _, enc := range []encoder.MultiEncoder{
		encoder.Standard | encoder.EncodeInvalidUtf8,
		encoder.Standard | encoder.EncodeInvalidUtf8 | encoder.EncodeWin | encoder.EncodeHashPercent | encoder.EncodeLeftSpace | encoder.EncodeRightSpace,
	} {
		t.Run(enc.String(), func(t *testing.T) {
			for _, rawName := range []string{"ordinary.txt", " spaced name ", "special:?#%.txt", "日本語😀.txt", "literal：？＃％‛.txt", "slash/name", "invalid\xff.txt"} {
				name := encoder.Standard.Encode(rawName)
				t.Run(name, func(t *testing.T) {
					ctx := context.Background()
					wireName := enc.FromStandardName(name)
					assert.Equal(t, name, enc.ToStandardName(wireName))
					assert.Equal(t, wireName, enc.FromStandardName(enc.ToStandardName(wireName)))
					var postedNames []string
					var parents []string
					var cursors []string
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodGet {
							if r.URL.Path == "/api/v1/uploads/session/parts" {
								assert.NoError(t, json.NewEncoder(w).Encode(api.UploadPartList{}))
								return
							}
							assert.Equal(t, "/api/v1/files", r.URL.Path)
							cursors = append(cursors, r.URL.Query().Get("cursor"))
							if r.URL.Query().Get("cursor") == "" {
								assert.NoError(t, json.NewEncoder(w).Encode(api.ReadMetadataResponse{NextCursor: "next"}))
								return
							}
							assert.NoError(t, json.NewEncoder(w).Encode(api.ReadMetadataResponse{Files: []api.FileInfo{
								{Id: "file", Name: wireName, Type: "file", ParentId: r.URL.Query().Get("parentId")},
								{Id: "folder", Name: enc.FromStandardName(name + " directory"), Type: "folder", ParentId: r.URL.Query().Get("parentId")},
							}}))
							return
						}
						assert.Equal(t, http.MethodPost, r.Method)
						var payload struct {
							Name     string `json:"name"`
							ParentID string `json:"parentId"`
						}
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						postedNames = append(postedNames, payload.Name)
						parents = append(parents, payload.ParentID)
						switch r.URL.Path {
						case "/api/v1/folders":
							assert.NoError(t, json.NewEncoder(w).Encode(api.FileInfo{Id: "created"}))
						case "/api/v1/uploads":
							assert.NoError(t, json.NewEncoder(w).Encode(api.UploadSession{ID: "session", Name: payload.Name, PartSize: 1024}))
						case "/api/v1/files/source/copy":
							assert.NoError(t, json.NewEncoder(w).Encode(api.FileInfo{Id: "copy", Name: payload.Name, Type: "file"}))
						default:
							t.Errorf("unexpected request %s", r.URL)
							w.WriteHeader(http.StatusBadRequest)
						}
					}))
					defer server.Close()
					f := &Fs{opt: Options{Enc: enc, PageSize: 2, ChunkSize: 1024},
						srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
						pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond)))}
					f.dirCache = dircache.New("", "", f)
					require.NoError(t, f.dirCache.FindRoot(ctx, false))
					f.dirCache.Put("父目录:parent/子目录?", "parent")
					for _, dir := range []string{"", "父目录:parent/子目录?"} {
						remote := path.Join(dir, name)
						entries, err := f.List(ctx, dir)
						require.NoError(t, err)
						require.Len(t, entries, 2)
						assert.Equal(t, remote, entries[0].Remote())
						assert.Equal(t, remote+" directory", entries[1].Remote())
						o, err := f.NewObject(ctx, remote)
						require.NoError(t, err)
						assert.Equal(t, remote, o.Remote())
						assert.Equal(t, name, o.(*Object).name)
						parentID, err := f.dirCache.FindDir(ctx, dir, false)
						require.NoError(t, err)
						found, err := f.findObject(ctx, parentID, name)
						require.NoError(t, err)
						require.Len(t, found, 1)
						assert.Equal(t, wireName, found[0].Name)
						id, exists, err := f.FindLeaf(ctx, parentID, name+" directory")
						require.NoError(t, err)
						assert.True(t, exists)
						assert.Equal(t, "folder", id)
						_, err = f.CreateDir(ctx, parentID, name)
						require.NoError(t, err)
						src := object.NewStaticObjectInfo(remote, time.Unix(123, 0), 1, true, nil, f)
						info, err := (&Object{fs: f}).prepareUpload(ctx, remote, src)
						require.NoError(t, err)
						assert.Equal(t, name, info.fileName)
						copied, err := f.Copy(ctx, &Object{fs: f, remote: "source", id: "source"}, remote)
						require.NoError(t, err)
						assert.Equal(t, remote, copied.Remote())
						assert.Equal(t, []string{wireName, wireName, wireName}, postedNames[len(postedNames)-3:])
						assert.Equal(t, []string{parentID, parentID, parentID}, parents[len(parents)-3:])
						var notified []string
						data, err := json.Marshal(api.Event{Type: "files.created", Source: api.EventSource{Name: wireName, ParentId: parentID, Type: "file"}})
						require.NoError(t, err)
						require.NoError(t, f.processSSEEvent(string(data), func(p string, typ fs.EntryType) {
							assert.Equal(t, fs.EntryObject, typ)
							notified = append(notified, p)
						}))
						assert.Equal(t, []string{remote}, notified)
						f.dirCache.Put("移動先:destination", "destination")
						data, err = json.Marshal(api.Event{Type: "files.moved", Source: api.EventSource{Name: wireName, ParentId: parentID, DestParentId: "destination", Type: "folder"}})
						require.NoError(t, err)
						notified = nil
						require.NoError(t, f.processSSEEvent(string(data), func(p string, typ fs.EntryType) {
							assert.Equal(t, fs.EntryDirectory, typ)
							notified = append(notified, p)
						}))
						assert.Equal(t, []string{path.Join("移動先:destination", name), remote}, notified)
					}
					for i, cursor := range cursors {
						if i%2 == 0 {
							assert.Empty(t, cursor)
						} else {
							assert.Equal(t, "next", cursor)
						}
					}
				})
			}
		})
	}
}
