package teldrive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/backend/teldrive/tdhash"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/require"
)

func TestUploadHashBoundaries(t *testing.T) {
	for _, partSize := range []int{3, tdhash.BlockSize, tdhash.BlockSize + 1} {
		t.Run(strconv.Itoa(partSize), func(t *testing.T) {
			data := bytes.Repeat([]byte("abcdefg"), (2*partSize+5)/7+1)
			data = data[:2*partSize+5]
			ctx := context.Background()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					require.NoError(t, json.NewEncoder(w).Encode(api.FileInfo{Hash: &api.FileHash{Algorithm: "blake3", Value: tdhash.SumString(data)}}))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			f := &Fs{opt: Options{HashEnabled: true}, srv: rest.NewClient(server.Client()).SetRoot(server.URL), pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond)))}
			w := &objectChunkWriter{f: f, o: &Object{fs: f}, uploadInfo: &uploadInfo{uploadID: "session", fileID: "file", chunkSize: int64(partSize), totalChunks: int64((len(data) + partSize - 1) / partSize)}}
			// Out-of-order writes and retries must produce the logical file hash.
			for part := int(w.uploadInfo.totalChunks); part > 0; part-- {
				start := (part - 1) * partSize
				end := min(start+partSize, len(data))
				require.NoError(t, w.hashReader(part, bytes.NewReader(data[start:end]), int64(end-start)))
			}
			require.NoError(t, w.hashReader(1, bytes.NewReader(data[:partSize]), int64(partSize)))
			var wg sync.WaitGroup
			for part := 1; part <= int(w.uploadInfo.totalChunks); part++ {
				wg.Add(1)
				go func(part int) {
					defer wg.Done()
					start := (part - 1) * partSize
					end := min(start+partSize, len(data))
					if err := w.hashReader(part, bytes.NewReader(data[start:end]), int64(end-start)); err != nil {
						t.Error(err)
					}
				}(part)
			}
			wg.Wait()
			var tempName string
			if w.hashFile != nil {
				tempName = w.hashFile.Name()
			}
			require.NoError(t, w.Close(ctx))
			require.Equal(t, tdhash.SumString(data), w.uploadInfo.hash)
			if tempName != "" {
				_, err := os.Stat(tempName)
				require.True(t, os.IsNotExist(err))
			}
		})
	}
}

func TestUploadMultipartHash(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			for _, data := range [][]byte{nil, []byte("abcdefghijk")} {
				ctx := context.Background()
				var uploaded []byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodPost && r.URL.Path == "/api/v1/uploads":
						require.NoError(t, json.NewEncoder(w).Encode(api.UploadSession{ID: "session", FileID: "file", PartSize: 3, Encryption: encrypted}))
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/parts"):
						var parts api.UploadPartList
						if resumed && len(data) > 0 {
							parts.Items = []api.UploadPart{{PartNo: 1, State: "stored", PlainSize: 3}}
							uploaded = append(uploaded, data[:3]...)
						}
						require.NoError(t, json.NewEncoder(w).Encode(parts))
					case r.Method == http.MethodPut:
						part, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						uploaded = append(uploaded, part...)
						w.WriteHeader(http.StatusNoContent)
					case r.Method == http.MethodGet && r.URL.Path == "/api/v1/files/file":
						require.NoError(t, json.NewEncoder(w).Encode(api.FileInfo{Hash: &api.FileHash{Algorithm: "blake3", Value: tdhash.SumString(uploaded)}}))
					case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
						w.WriteHeader(http.StatusNoContent)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL)
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				f := &Fs{opt: Options{HashEnabled: true, EncryptFiles: encrypted}, srv: rest.NewClient(server.Client()).SetRoot(server.URL), pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond)))}
				f.dirCache = dircache.New("", "", f)
				require.NoError(t, f.dirCache.FindRoot(ctx, false))
				o := &Object{fs: f}
				src := object.NewStaticObjectInfo("test", time.Unix(1, 0), int64(len(data)), true, nil, f)
				info, err := o.uploadMultipart(ctx, "test", bytes.NewReader(data), src)
				require.NoError(t, err)
				require.Equal(t, tdhash.SumString(data), info.hash)
				require.NoError(t, o.createFile(ctx, src, info))
				require.Equal(t, string(data), string(uploaded))
				server.Close()
			}
		}
	}
}

func TestUploadHashVerification(t *testing.T) {
	for _, test := range []struct{ name, algorithm, value, wantErr string }{
		{"match", "blake3", strings.ToUpper(tdhash.SumString(nil)), ""},
		{"mismatch", "blake3", strings.Repeat("0", 64), "hash mismatch"},
		{"missing", "", "", ""},
		{"unsupported", "sha256", strings.Repeat("0", 64), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					require.NoError(t, json.NewEncoder(w).Encode(api.FileInfo{Hash: &api.FileHash{Algorithm: test.algorithm, Value: test.value}}))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			f := &Fs{opt: Options{HashEnabled: true}, srv: rest.NewClient(server.Client()).SetRoot(server.URL), pacer: fs.NewPacer(ctx, pacer.NewDefault(pacer.MinSleep(time.Millisecond)))}
			err := (&Object{fs: f}).createFile(ctx, nil, &uploadInfo{uploadID: "session", fileID: "file", hash: tdhash.SumString(nil)})
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
