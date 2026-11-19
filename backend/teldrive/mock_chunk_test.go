package teldrive

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

func TestMockOpenChunkWriter(t *testing.T) {
	m := newMock(t)
	m.partSize = 4
	f := m.newFs(t, nil)
	ctx := context.Background()
	data := []byte("0123456789") // 10 bytes, three parts at partSize 4
	src := object.NewStaticObjectInfo("chunk.bin", testModTime(), int64(len(data)), true, nil, f)

	open := f.Features().OpenChunkWriter
	if open == nil {
		t.Fatal("OpenChunkWriter not supported")
	}
	info, writer, err := open(ctx, "chunk.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	if info.ChunkSize != m.partSize {
		t.Fatalf("ChunkSize = %d, want %d", info.ChunkSize, m.partSize)
	}
	parts := 0
	for off := 0; off < len(data); off += int(info.ChunkSize) {
		end := min(off+int(info.ChunkSize), len(data))
		n, err := writer.WriteChunk(ctx, parts, bytes.NewReader(data[off:end]))
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(end-off) {
			t.Fatalf("part %d wrote %d, want %d", parts, n, end-off)
		}
		parts++
	}
	if parts < 2 {
		t.Fatalf("parts = %d, want multiple", parts)
	}
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	obj, err := f.NewObject(ctx, "chunk.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, obj), data) {
		t.Fatalf("chunked content = %q", readAll(t, obj))
	}
}

func TestMockAbortUpload(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()
	data := []byte("abcdef")
	src := object.NewStaticObjectInfo("aborted.bin", testModTime(), int64(len(data)), true, nil, f)
	_, writer, err := f.Features().OpenChunkWriter(ctx, "aborted.bin", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteChunk(ctx, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewObject(ctx, "aborted.bin"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("aborted upload lookup = %v", err)
	}
	m.mu.Lock()
	left := len(m.uploads)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("uploads left = %d, want 0", left)
	}
}
