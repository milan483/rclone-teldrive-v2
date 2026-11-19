// Package teldrive implements the TelDrive v2 resumable upload protocol.
package teldrive

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/backend/teldrive/tdhash"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/rest"
)

const memoryBufferThreshold = 10 * 1024 * 1024

type uploadInfo struct {
	existingChunks map[int]api.PartFile
	uploadID       string
	encryptFile    bool
	chunkSize      int64
	totalChunks    int64
	fileName       string
	dir            string
	hash           string
	fileID         string
}

type objectChunkWriter struct {
	f          *Fs
	src        fs.ObjectInfo
	o          *Object
	uploadInfo *uploadInfo
	mu         sync.Mutex
	blocks     map[int][][]byte
	hashFile   *os.File
}

func getMD5Hash(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

func stableUUID(text string) string {
	v := getMD5Hash(text)
	return v[0:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:32]
}

func (w *objectChunkWriter) WriteChunk(ctx context.Context, chunkNumber int, reader io.ReadSeeker) (int64, error) {
	partNo := chunkNumber + 1
	if partNo < 1 {
		return 0, fmt.Errorf("invalid chunk number %d", chunkNumber)
	}
	if existing, ok := w.uploadInfo.existingChunks[partNo]; ok {
		if w.f.opt.HashEnabled {
			if _, err := reader.Seek(0, io.SeekStart); err != nil {
				return 0, err
			}
			if err := w.hashReader(partNo, reader, existing.Size); err != nil {
				return 0, err
			}
		}
		return existing.Size, nil
	}
	size, err := reader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err = reader.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if w.f.opt.HashEnabled {
		if err := w.hashReader(partNo, reader, size); err != nil {
			return 0, err
		}
		if _, err = reader.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
	}
	opts := rest.Opts{
		Method: "PUT", Body: reader, ContentLength: &size, ContentType: "application/octet-stream",
		Path: "/api/v1/uploads/" + w.uploadInfo.uploadID + "/parts/" + strconv.Itoa(partNo),
	}
	if w.f.opt.UploadHost != "" {
		opts.RootURL = joinAPIURL(w.f.opt.UploadHost, opts.Path)
		opts.Path = ""
	}
	err = w.f.pacer.Call(func() (bool, error) {
		if _, err := reader.Seek(0, io.SeekStart); err != nil {
			return false, err
		}
		resp, err := w.f.srv.Call(ctx, &opts)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return 0, fmt.Errorf("upload part %d: %w", partNo, err)
	}
	return size, nil
}

func (w *objectChunkWriter) hashReader(partNo int, reader io.Reader, size int64) error {
	if size == 0 {
		return nil
	}
	// Non-aligned parts can split hash blocks. Spool them at their logical
	// offsets so out-of-order writes and retries do not change block boundaries.
	if w.uploadInfo.chunkSize%tdhash.BlockSize != 0 {
		w.mu.Lock()
		if w.hashFile == nil {
			file, err := os.CreateTemp("", "rclone-teldrive-hash-*")
			if err != nil {
				w.mu.Unlock()
				return err
			}
			w.hashFile = file
		}
		file := w.hashFile
		w.mu.Unlock()
		_, err := io.CopyN(io.NewOffsetWriter(file, int64(partNo-1)*w.uploadInfo.chunkSize), reader, size)
		return err
	}
	blocks := make([][]byte, 0, (size+tdhash.BlockSize-1)/tdhash.BlockSize)
	for remaining := size; remaining > 0; {
		h := tdhash.NewBlockHash()
		n := min(remaining, int64(tdhash.BlockSize))
		if _, err := io.CopyN(h, reader, n); err != nil {
			return fmt.Errorf("hash upload part %d: %w", partNo, err)
		}
		blocks = append(blocks, h.Sum(nil))
		remaining -= n
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.blocks == nil {
		w.blocks = make(map[int][][]byte)
	}
	w.blocks[partNo] = blocks
	return nil
}

func (w *objectChunkWriter) Close(ctx context.Context) error {
	defer w.removeHashFile()
	if w.f.opt.HashEnabled {
		if w.hashFile != nil {
			if _, err := w.hashFile.Seek(0, io.SeekStart); err != nil {
				return err
			}
			h := tdhash.New()
			if _, err := io.Copy(h, w.hashFile); err != nil {
				return err
			}
			w.uploadInfo.hash = hex.EncodeToString(h.Sum(nil))
		} else {
			w.mu.Lock()
			parts := make([][]byte, 0)
			for partNo := 1; partNo <= int(w.uploadInfo.totalChunks); partNo++ {
				parts = append(parts, w.blocks[partNo]...)
			}
			w.mu.Unlock()
			w.uploadInfo.hash = hex.EncodeToString(tdhash.SumBlockHashes(parts))
		}
	}
	return w.o.createFile(ctx, w.src, w.uploadInfo)
}

func (w *objectChunkWriter) removeHashFile() {
	if w.hashFile != nil {
		name := w.hashFile.Name()
		_ = w.hashFile.Close()
		_ = os.Remove(name)
		w.hashFile = nil
	}
}

func (w *objectChunkWriter) Abort(ctx context.Context) error {
	defer w.removeHashFile()
	opts := rest.Opts{Method: "DELETE", Path: "/api/v1/uploads/" + w.uploadInfo.uploadID, NoResponse: true}
	_, err := w.f.srv.Call(ctx, &opts)
	return err
}

func (o *Object) prepareUpload(ctx context.Context, remote string, src fs.ObjectInfo) (*uploadInfo, error) {
	leaf, directoryID, err := o.fs.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	payload := api.UploadCreateRequest{
		ParentID: directoryID, Name: leaf, Size: src.Size(), MimeType: fs.MimeType(ctx, src),
		ModTime: src.ModTime(ctx).UTC(), Encryption: o.fs.opt.EncryptFiles,
		ConflictPolicy: "replace", PreferredPartSize: int64(o.fs.opt.ChunkSize),
	}
	idempotencyKey := stableUUID(fmt.Sprintf("upload:%s:%s:%d:%d:%d", directoryID, leaf, src.Size(), src.ModTime(ctx).UTC().UnixNano(), o.fs.userId))
	opts := rest.Opts{
		Method: "POST", Path: "/api/v1/uploads",
		ExtraHeaders: map[string]string{"Idempotency-Key": idempotencyKey},
	}
	var session api.UploadSession
	err = o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(ctx, &opts, &payload, &session)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, fmt.Errorf("create upload: %w", err)
	}
	chunkSize := session.PartSize
	if chunkSize <= 0 {
		chunkSize = int64(o.fs.opt.ChunkSize)
	}
	existing := make(map[int]api.PartFile)
	params := url.Values{"limit": []string{"200"}}
	for {
		var page api.UploadPartList
		listOpts := rest.Opts{Method: "GET", Path: "/api/v1/uploads/" + session.ID + "/parts", Parameters: params}
		err = o.fs.pacer.Call(func() (bool, error) {
			resp, err := o.fs.srv.CallJSON(ctx, &listOpts, nil, &page)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			return nil, fmt.Errorf("list upload parts: %w", err)
		}
		for _, part := range page.Items {
			if part.State == "stored" {
				existing[part.PartNo] = api.PartFile{PartNo: part.PartNo, Size: part.PlainSize}
			}
		}
		if page.NextCursor == "" {
			break
		}
		params.Set("cursor", page.NextCursor)
	}
	totalChunks := int64(0)
	if src.Size() > 0 {
		totalChunks = (src.Size() + chunkSize - 1) / chunkSize
	}
	return &uploadInfo{
		existingChunks: existing, uploadID: session.ID, encryptFile: session.Encryption,
		chunkSize: chunkSize, totalChunks: totalChunks, fileName: leaf, dir: directoryID,
		fileID: session.FileID,
	}, nil
}

func (o *Object) uploadMultipart(ctx context.Context, remote string, in io.Reader, src fs.ObjectInfo) (*uploadInfo, error) {
	info, err := o.prepareUpload(ctx, remote, src)
	if err != nil {
		return nil, err
	}
	var localHash hash.Hash
	if o.fs.opt.HashEnabled {
		localHash = tdhash.New()
		in = io.TeeReader(in, localHash)
	}
	var uploaded int64
	for partNo := 1; partNo <= int(info.totalChunks); partNo++ {
		partSize := info.chunkSize
		if remaining := src.Size() - uploaded; remaining < partSize {
			partSize = remaining
		}
		if existing, ok := info.existingChunks[partNo]; ok {
			if _, err := io.CopyN(io.Discard, in, existing.Size); err != nil {
				return nil, err
			}
			uploaded += existing.Size
			continue
		}
		partReader := io.LimitReader(in, partSize)
		opts := rest.Opts{
			Method: "PUT", Body: partReader, ContentLength: &partSize, ContentType: "application/octet-stream",
			Path: "/api/v1/uploads/" + info.uploadID + "/parts/" + strconv.Itoa(partNo),
		}
		if o.fs.opt.UploadHost != "" {
			opts.RootURL = joinAPIURL(o.fs.opt.UploadHost, opts.Path)
			opts.Path = ""
		}
		err = o.fs.pacer.Call(func() (bool, error) {
			resp, err := o.fs.srv.Call(ctx, &opts)
			return shouldRetry(ctx, resp, err)
		})
		if err != nil {
			return nil, fmt.Errorf("upload part %d: %w", partNo, err)
		}
		uploaded += partSize
	}
	if localHash != nil {
		info.hash = hex.EncodeToString(localHash.Sum(nil))
	}
	return info, nil
}

func (o *Object) createFile(ctx context.Context, _ fs.ObjectInfo, info *uploadInfo) error {
	opts := rest.Opts{
		Method: "POST", Path: "/api/v1/uploads/" + info.uploadID + "/complete",
		ExtraHeaders: map[string]string{"Idempotency-Key": stableUUID("complete:" + info.uploadID)},
	}
	err := o.fs.pacer.Call(func() (bool, error) {
		resp, err := o.fs.srv.CallJSON(ctx, &opts, nil, nil)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return fmt.Errorf("complete upload: %w", err)
	}
	if o.fs.opt.HashEnabled && info.hash != "" {
		var file api.FileInfo
		if info.fileID != "" {
			getOpts := rest.Opts{Method: "GET", Path: "/api/v1/files/" + info.fileID}
			err = o.fs.pacer.Call(func() (bool, error) {
				resp, err := o.fs.srv.CallJSON(ctx, &getOpts, nil, &file)
				return shouldRetry(ctx, resp, err)
			})
			if err != nil {
				return fmt.Errorf("get completed upload %q: %w", info.fileName, err)
			}
		}
		if file.Hash == nil || file.Hash.Value == "" {
			fs.Debugf(o, "upload %q completed without a server hash; integrity verification skipped", info.fileName)
		} else if file.Hash.Algorithm != "" && !strings.EqualFold(file.Hash.Algorithm, "blake3") {
			fs.Debugf(o, "upload hash verification skipped: unsupported algorithm %q", file.Hash.Algorithm)
		} else if !strings.EqualFold(info.hash, file.Hash.Value) {
			return fmt.Errorf("upload %q hash mismatch: local %s, server %s", info.fileName, info.hash, file.Hash.Value)
		}
	}
	return nil
}

func (o *Object) uploadWithBuffering(ctx context.Context, remote string, in io.Reader, src fs.ObjectInfo) (*uploadInfo, int64, error) {
	var memory bytes.Buffer
	var file *os.File
	var size int64
	buf := make([]byte, 128*1024)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			if file == nil && size+int64(n) > memoryBufferThreshold {
				file, readErr = os.CreateTemp("", "rclone-teldrive-*")
				if readErr != nil {
					return nil, 0, readErr
				}
				if _, readErr = file.Write(memory.Bytes()); readErr != nil {
					return nil, 0, readErr
				}
				memory.Reset()
			}
			if file != nil {
				_, readErr = file.Write(buf[:n])
			} else {
				_, readErr = memory.Write(buf[:n])
			}
			if readErr != nil {
				return nil, 0, readErr
			}
			size += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
	}
	withSize := object.NewStaticObjectInfo(remote, src.ModTime(ctx), size, false, nil, o.fs)
	if file != nil {
		name := file.Name()
		defer func() { _ = file.Close(); _ = os.Remove(name) }()
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
		info, err := o.uploadMultipart(ctx, remote, file, withSize)
		return info, size, err
	}
	info, err := o.uploadMultipart(ctx, remote, bytes.NewReader(memory.Bytes()), withSize)
	return info, size, err
}

// joinAPIURL preserves an api_host path prefix (for example /tgd behind Caddy).
func joinAPIURL(base, endpoint string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(endpoint, "/")
}
