package teldrive

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
)

func TestMockNewObject(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()
	if err := f.Mkdir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	putContent(t, f, "docs/a.txt", []byte("a"))

	obj, err := f.NewObject(ctx, "docs/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Size() != 1 || obj.Remote() != "docs/a.txt" {
		t.Fatalf("NewObject = %s size %d", obj.Remote(), obj.Size())
	}
	dir, err := f.NewObject(ctx, "docs")
	if dir != nil || !errors.Is(err, fs.ErrorIsDir) {
		t.Fatalf("NewObject(dir) = %v, %v; want ErrorIsDir", dir, err)
	}
	missing, err := f.NewObject(ctx, "missing.txt")
	if missing != nil || !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("NewObject(missing) = %v, %v", missing, err)
	}
}

func TestMockMkdirRmdirPurge(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()
	if err := f.Mkdir(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "full"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "full/nested"); err != nil {
		t.Fatal(err)
	}
	putContent(t, f, "full/a.txt", []byte("a"))
	putContent(t, f, "full/nested/b.txt", []byte("b"))

	if err := f.Rmdir(ctx, "full"); !errors.Is(err, fs.ErrorDirectoryNotEmpty) {
		t.Fatalf("Rmdir non-empty = %v", err)
	}
	assertNames(t, f, "", "empty", "full")
	if err := f.Rmdir(ctx, "nope"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("Rmdir missing = %v", err)
	}
	if err := f.Rmdir(ctx, "empty"); err != nil {
		t.Fatal(err)
	}
	assertNames(t, f, "", "full")

	purge := f.Features().Purge
	if purge == nil {
		t.Fatal("Purge not supported")
	}
	if err := purge(ctx, ""); err == nil {
		t.Fatal("Purge root succeeded")
	}
	if err := purge(ctx, "full"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewObject(ctx, "full/a.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("purged file lookup = %v", err)
	}
	if _, err := f.List(ctx, "full"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("List purged dir = %v", err)
	}
	if _, err := f.List(ctx, "full/nested"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("List purged nested dir = %v", err)
	}

	gone := putContent(t, f, "gone.txt", []byte("x"))
	if err := gone.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewObject(ctx, "gone.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("removed file lookup = %v", err)
	}
}

func TestMockMoveCopySetModTime(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()
	if err := f.Mkdir(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	content := []byte("payload")
	obj := putContent(t, f, "src/file.txt", content)
	// Same leaf in the destination is replaced before the rename to renamed.txt.
	putContent(t, f, "dst/file.txt", []byte("old"))

	move := f.Features().Move
	if move == nil {
		t.Fatal("Move not supported")
	}
	moved, err := move(ctx, obj, "dst/renamed.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.NewObject(ctx, "src/file.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("source still exists: %v", err)
	}
	if _, err := f.NewObject(ctx, "dst/file.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("replaced dest still exists: %v", err)
	}
	if !bytes.Equal(readAll(t, moved), content) {
		t.Fatalf("moved content = %q", readAll(t, moved))
	}
	if !bytes.Equal(readAll(t, mustObject(t, f, "dst/renamed.txt")), content) {
		t.Fatal("moved lookup content mismatch")
	}

	copy := f.Features().Copy
	if copy == nil {
		t.Fatal("Copy not supported")
	}
	copied, err := copy(ctx, moved, "dst/copy.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, copied), content) {
		t.Fatalf("copy = %q", readAll(t, copied))
	}
	updated := []byte("changed")
	updatedInfo := object.NewStaticObjectInfo(moved.Remote(), moved.ModTime(ctx), int64(len(updated)), true, nil, f)
	if err := moved.Update(ctx, bytes.NewReader(updated), updatedInfo); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, copied), content) {
		t.Fatal("copy changed with source")
	}

	want := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := copied.SetModTime(ctx, want); err != nil {
		t.Fatal(err)
	}
	if !copied.ModTime(ctx).Equal(want) {
		t.Fatalf("ModTime = %s, want %s", copied.ModTime(ctx), want)
	}
	id := copied.(fs.IDer).ID()
	m.mu.Lock()
	got := m.files[id].ModTime
	m.mu.Unlock()
	if !got.Equal(want) {
		t.Fatalf("server ModTime = %s, want %s", got, want)
	}

	if err := f.Mkdir(ctx, "tree/sub"); err != nil {
		t.Fatal(err)
	}
	putContent(t, f, "tree/sub/leaf.txt", []byte("leaf"))
	dirMove := f.Features().DirMove
	if dirMove == nil {
		t.Fatal("DirMove not supported")
	}
	if err := dirMove(ctx, f, "tree/sub", "moved/sub2"); err != nil {
		t.Fatal(err)
	}
	assertNames(t, f, "moved/sub2", "moved/sub2/leaf.txt")
	if _, err := f.List(ctx, "tree/sub"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatalf("List moved source = %v", err)
	}
}

func TestMockOpenRange(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	content := []byte("abcdefghijklmnopqrstuvwxyz")
	obj := putContent(t, f, "range.txt", content)
	if !bytes.Equal(readAll(t, obj), content) {
		t.Fatalf("full read = %q", readAll(t, obj))
	}
	got := readAll(t, obj, &fs.RangeOption{Start: 2, End: 5})
	if !bytes.Equal(got, content[2:6]) {
		t.Fatalf("range read = %q, want %q", got, content[2:6])
	}

	m.ignoreRange = true
	reader, err := obj.Open(context.Background(), &fs.RangeOption{Start: 2, End: 5})
	if reader != nil {
		_ = reader.Close()
		t.Fatal("ignored range returned a readable body")
	}
	if err == nil {
		t.Fatal("ignored range succeeded, want an error")
	}
}

func mustObject(t *testing.T, f fs.Fs, remote string) fs.Object {
	t.Helper()
	obj, err := f.NewObject(context.Background(), remote)
	if err != nil {
		t.Fatalf("NewObject %s: %v", remote, err)
	}
	return obj
}
