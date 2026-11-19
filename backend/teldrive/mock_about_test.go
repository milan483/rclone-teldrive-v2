package teldrive

import (
	"testing"
)

func TestMockAbout(t *testing.T) {
	m := newMock(t)
	f := m.newFs(t, nil)
	if err := f.Mkdir(t.Context(), "docs"); err != nil {
		t.Fatal(err)
	}
	text := []byte("hello")
	bin := []byte("0123456789")
	putContent(t, f, "docs/note.txt", text)
	putContent(t, f, "data.bin", bin)
	m.mu.Lock()
	for _, file := range m.files {
		if file.Type == "folder" {
			file.Size = 999
		}
	}
	m.mu.Unlock()

	about := f.Features().About
	if about == nil {
		t.Fatal("About not supported")
	}
	usage, err := about(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := int64(len(text) + len(bin))
	if usage == nil || usage.Used == nil || *usage.Used != want {
		t.Fatalf("About used = %+v, want %d", usage, want)
	}

	var cats []struct {
		TotalSize int64 `json:"totalSize"`
	}
	m.getJSON(t, "/api/v1/files/statistics/categories", &cats)
	if len(cats) < 2 {
		t.Fatalf("categories = %+v, want more than one", cats)
	}
	var sum int64
	for _, cat := range cats {
		sum += cat.TotalSize
	}
	if sum != want {
		t.Fatalf("category sum = %d, want %d (%+v)", sum, want, cats)
	}
}
