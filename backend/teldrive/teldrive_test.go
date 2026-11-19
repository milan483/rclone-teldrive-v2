package teldrive

import (
	"strings"
	"testing"

	"github.com/rclone/rclone/backend/teldrive/api"
	"github.com/rclone/rclone/fstest/fstests"
)

func TestValidateUploadSession(t *testing.T) {
	tests := []struct {
		name       string
		session    *api.UploadSession
		encryption bool
		wantErr    string
	}{
		{name: "valid encryption session", session: &api.UploadSession{ID: "upload-1", PartSize: 1024, Encryption: true}, encryption: true},
		{name: "missing session", encryption: true, wantErr: "session is missing"},
		{name: "missing id", session: &api.UploadSession{PartSize: 1024, Encryption: true}, encryption: true, wantErr: "missing session ID"},
		{name: "missing encrypted part size", session: &api.UploadSession{ID: "upload-1", Encryption: true}, encryption: true, wantErr: "part size"},
		{name: "encryption not enabled", session: &api.UploadSession{ID: "upload-1", PartSize: 1024}, encryption: true, wantErr: "did not enable encryption"},
		{name: "encryption disabled", session: &api.UploadSession{ID: "upload-1"}, wantErr: ""},
		{name: "server forced encryption", session: &api.UploadSession{ID: "upload-1", PartSize: 1024, Encryption: true}, wantErr: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateUploadSession(test.session, test.encryption)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("validateUploadSession() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validateUploadSession() error = %v, want substring %q", err, test.wantErr)
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
