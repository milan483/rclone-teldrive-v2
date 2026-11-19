package teldrive

import (
	"testing"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fstest/fstests"
)

func TestUsageFromCategoryStats(t *testing.T) {
	value := func(n int64) *int64 { return &n }

	tests := []struct {
		name  string
		stats []api.CategorySize
		want  *int64
	}{
		{name: "used only", stats: []api.CategorySize{{TotalSize: value(123)}}, want: value(123)},
		{name: "categories aggregate", stats: []api.CategorySize{
			{Category: "user files", TotalSize: value(10)},
			{Category: "encrypted files", TotalSize: value(20)},
			{Category: "trash", TotalSize: value(3)},
			{Category: "other", TotalSize: value(7)},
		}, want: value(40)},
		{name: "empty", stats: nil, want: value(0)},
		{name: "null size", stats: []api.CategorySize{{Category: "user files"}}, want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := usageFromCategoryStats(test.stats)
			if usage.Total != nil || usage.Free != nil || usage.Trashed != nil || usage.Other != nil {
				t.Fatalf("usage = %+v, quota fields must remain unset", usage)
			}
			if test.want == nil {
				if usage.Used != nil {
					t.Fatalf("used = %v, want unset", *usage.Used)
				}
				return
			}
			if usage.Used == nil || *usage.Used != *test.want {
				t.Fatalf("used = %+v, want %d", usage.Used, *test.want)
			}
		})
	}
}

func TestJoinAPIURLPreservesPathPrefix(t *testing.T) {
	for _, test := range []struct {
		base, endpoint, want string
	}{
		{"https://example.com/tgd", "/api/v1/me", "https://example.com/tgd/api/v1/me"},
		{"https://example.com/tgd/", "/api/v1/files", "https://example.com/tgd/api/v1/files"},
		{"http://127.0.0.1:54807", "/api/v1/me", "http://127.0.0.1:54807/api/v1/me"},
	} {
		if got := joinAPIURL(test.base, test.endpoint); got != test.want {
			t.Fatalf("joinAPIURL(%q, %q) = %q, want %q", test.base, test.endpoint, got, test.want)
		}
	}
}

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestTeldrive:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize:  minChunkSize,
			CeilChunkSize: fstests.NextPowerOfTwo,
		},
		SkipInvalidUTF8: true,
	})
}
