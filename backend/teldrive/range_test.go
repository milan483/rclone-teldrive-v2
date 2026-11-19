package teldrive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trackingBody records whether the HTTP response body was closed.
type trackingBody struct {
	io.ReadCloser

	mu     sync.Mutex
	closed bool
}

// Close closes the underlying body and records that it was closed.
func (b *trackingBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

// isClosed reports whether Close has been called.
func (b *trackingBody) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// bodyTrackingTransport wraps every response body so tests can check it was
// closed.
type bodyTrackingTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	bodies []*trackingBody
}

// RoundTrip implements http.RoundTripper.
func (t *bodyTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body := &trackingBody{ReadCloser: resp.Body}
	resp.Body = body
	t.mu.Lock()
	t.bodies = append(t.bodies, body)
	t.mu.Unlock()
	return resp, nil
}

// allClosed reports whether every response body seen was closed.
func (t *bodyTrackingTransport) allClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) == 0 {
		return false
	}
	for _, body := range t.bodies {
		if !body.isClosed() {
			return false
		}
	}
	return true
}

// newRangeTestObject returns an object backed by an httptest server serving
// size bytes. It also returns the tracker used to check response bodies were
// closed.
func newRangeTestObject(t *testing.T, size int64, handler http.HandlerFunc) (*Object, *bodyTrackingTransport) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	transport := &bodyTrackingTransport{base: http.DefaultTransport}
	f := &Fs{
		name: "test",
		srv:  rest.NewClient(&http.Client{Transport: transport}).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault(
			pacer.MinSleep(time.Millisecond),
			pacer.MaxSleep(5*time.Millisecond),
		)),
	}
	return &Object{
		fs:     f,
		remote: "file.bin",
		id:     "file-id",
		size:   size,
	}, transport
}

// readAndClose reads the whole reader and closes it.
func readAndClose(t *testing.T, in io.ReadCloser) string {
	t.Helper()
	require.NotNil(t, in)
	defer func() {
		require.NoError(t, in.Close())
	}()
	data, err := io.ReadAll(in)
	require.NoError(t, err)
	return string(data)
}

func TestOpenFullRead(t *testing.T) {
	const content = "abcdefghij"

	o, _ := newRangeTestObject(t, int64(len(content)), func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Range"))
		_, _ = io.WriteString(w, content)
	})

	in, err := o.Open(context.Background())
	require.NoError(t, err)
	assert.Equal(t, content, readAndClose(t, in))
}

func TestOpenValidRange(t *testing.T) {
	o, tracker := newRangeTestObject(t, 10, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=2-4", r.Header.Get("Range"))
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cde")
	})

	in, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 4})
	require.NoError(t, err)
	assert.Equal(t, "cde", readAndClose(t, in))
	assert.True(t, tracker.allClosed())
}

func TestOpenRangeIgnored(t *testing.T) {
	const content = "abcdefghij"
	var requests atomic.Int32

	o, tracker := newRangeTestObject(t, int64(len(content)), func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=2-4", r.Header.Get("Range"))
		requests.Add(1)
		// A server which ignores Range replies with the whole object.
		_, _ = io.WriteString(w, content)
	})

	in, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 4})
	assert.Nil(t, in)
	assert.ErrorIs(t, err, fs.ErrorRangeIgnored)
	assert.Equal(t, int32(1), requests.Load(), "ignored range must not be retried")
	assert.True(t, tracker.allClosed())
}

func TestOpenWrongContentRange(t *testing.T) {
	var requests atomic.Int32

	o, tracker := newRangeTestObject(t, 10, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=2-4", r.Header.Get("Range"))
		requests.Add(1)
		w.Header().Set("Content-Range", "bytes 0-2/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abc")
	})

	in, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 4})
	assert.Nil(t, in)
	require.Error(t, err)
	assert.NotErrorIs(t, err, fs.ErrorRangeIgnored)
	assert.Positive(t, requests.Load())
	assert.True(t, tracker.allClosed())
}

func TestOpenWholeObjectRange(t *testing.T) {
	const content = "abcdefghij"

	o, _ := newRangeTestObject(t, int64(len(content)), func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=0-9", r.Header.Get("Range"))
		// Replying with the whole object is fine when that is what was asked
		// for.
		_, _ = io.WriteString(w, content)
	})

	in, err := o.Open(context.Background(), &fs.RangeOption{Start: 0, End: 9})
	require.NoError(t, err)
	assert.Equal(t, content, readAndClose(t, in))
}

func TestOpenRangeNotSatisfiable(t *testing.T) {
	o, tracker := newRangeTestObject(t, 10, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "bytes=2-4", r.Header.Get("Range"))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	})

	in, err := o.Open(context.Background(), &fs.RangeOption{Start: 2, End: 4})
	assert.Nil(t, in)
	require.Error(t, err)
	assert.NotErrorIs(t, err, fs.ErrorRangeIgnored)
	assert.True(t, tracker.allClosed())
}
