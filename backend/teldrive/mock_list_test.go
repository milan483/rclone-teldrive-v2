package teldrive

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
)

func TestMockAPIKey(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	if _, err := f.List(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	requests, misses := m.requests, m.authMisses
	m.mu.Unlock()
	if requests < 2 {
		t.Fatalf("authenticated requests = %d, want at least 2", requests)
	}
	if misses != 0 {
		t.Fatalf("auth misses = %d, want 0", misses)
	}

	resp, err := http.Get(m.server.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing api key status = %d, want 401", resp.StatusCode)
	}
}

func TestMockListRootAndNested(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	ctx := context.Background()
	if err := f.Mkdir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "docs"); err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "docs/nested"); err != nil {
		t.Fatal(err)
	}
	putContent(t, f, "root.txt", []byte("root"))
	putContent(t, f, "docs/a.txt", []byte("a"))
	putContent(t, f, "docs/nested/b.txt", []byte("b"))

	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, f, "", "docs", "root.txt")
	for _, entry := range entries {
		switch entry.Remote() {
		case "docs":
			if _, ok := entry.(fs.Directory); !ok {
				t.Fatalf("docs type = %T, want directory", entry)
			}
		case "root.txt":
			if _, ok := entry.(fs.Object); !ok {
				t.Fatalf("root.txt type = %T, want object", entry)
			}
		}
	}
	assertNames(t, f, "docs", "docs/a.txt", "docs/nested")
	assertNames(t, f, "docs/nested", "docs/nested/b.txt")
}

func TestMockListPagination(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, configmap.Simple{"page_size": "1"})
	putContent(t, f, "c.txt", []byte("c"))
	putContent(t, f, "a.txt", []byte("a"))
	putContent(t, f, "b.txt", []byte("b"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("paginated list returned %d entries, want 3", len(entries))
	}

	q := url.Values{"sort": []string{"name"}, "limit": []string{"1"}}
	var first api.ReadMetadataResponse
	m.getJSON(t, "/api/v1/files?"+q.Encode(), &first)
	if len(first.Files) != 1 || first.Files[0].Name != "a.txt" || first.NextCursor == "" {
		t.Fatalf("name page = %+v", first)
	}
	q.Set("cursor", first.NextCursor)
	var second api.ReadMetadataResponse
	m.getJSON(t, "/api/v1/files?"+q.Encode(), &second)
	if len(second.Files) != 1 || second.Files[0].Name != "b.txt" {
		t.Fatalf("name cursor page = %+v", second)
	}

	var byID api.ReadMetadataResponse
	m.getJSON(t, "/api/v1/files?sort=id&limit=1", &byID)
	if len(byID.Files) != 1 || byID.Files[0].Name != "c.txt" {
		t.Fatalf("id page = %+v, want c.txt first", byID)
	}
}
