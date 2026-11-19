package teldrive

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
)

func TestMockPutAndUpdate(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()

	empty := putContent(t, f, "empty.txt", nil)
	if empty.Size() != 0 || len(readAll(t, empty)) != 0 {
		t.Fatalf("empty file size=%d body=%q", empty.Size(), readAll(t, empty))
	}

	original := []byte("one")
	obj := putContent(t, f, "a.txt", original)
	if !bytes.Equal(readAll(t, obj), original) {
		t.Fatalf("Put read = %q", readAll(t, obj))
	}

	replaced := []byte("two-two")
	obj = putContent(t, f, "a.txt", replaced)
	if obj.Size() != int64(len(replaced)) || !bytes.Equal(readAll(t, obj), replaced) {
		t.Fatalf("overwrite = size %d body %q", obj.Size(), readAll(t, obj))
	}

	updated := []byte("three")
	src := object.NewStaticObjectInfo("a.txt", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), int64(len(updated)), true, nil, f)
	if err := obj.Update(ctx, bytes.NewReader(updated), src); err != nil {
		t.Fatal(err)
	}
	if obj.Size() != int64(len(updated)) || !bytes.Equal(readAll(t, obj), updated) {
		t.Fatalf("Update = size %d body %q", obj.Size(), readAll(t, obj))
	}
	if _, err := obj.Hash(ctx, telDriveHash); err != hash.ErrUnsupported {
		t.Fatalf("Hash = %v, want unsupported", err)
	}
}

func TestMockEncryptionFlag(t *testing.T) {
	t.Run("matches config", func(t *testing.T) {
		m := newMock(t)
		f := m.newFs(t, configmap.Simple{"encrypt_files": "true"})
		putContent(t, f, "secret.txt", []byte("cipher"))
		req, session, file := encryptionOf(t, m, "secret.txt")
		if !req || !session || !file {
			t.Fatalf("encryption request=%v session=%v file=%v, want all true", req, session, file)
		}
	})
	t.Run("session honored", func(t *testing.T) {
		m := newMock(t)
		m.forceEncryption = true
		f := m.newFs(t, configmap.Simple{"encrypt_files": "false"})
		putContent(t, f, "plain.txt", []byte("plain"))
		req, session, file := encryptionOf(t, m, "plain.txt")
		if req {
			t.Fatal("UploadCreateRequest.Encryption = true, want false to match config")
		}
		if !session || !file {
			t.Fatalf("session encryption not honored: session=%v file=%v", session, file)
		}
	})
}

func encryptionOf(t *testing.T, m *mockTeldrive, name string) (request, session, file bool) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var foundUpload, foundFile bool
	for _, up := range m.uploads {
		if up.request.Name == name {
			request = up.request.Encryption
			session = up.Encryption
			foundUpload = true
		}
	}
	for _, item := range m.files {
		if item.Type == "file" && item.Name == name {
			file = item.Encryption
			foundFile = true
		}
	}
	if !foundUpload || !foundFile {
		t.Fatalf("missing upload or file %s", name)
	}
	return request, session, file
}
