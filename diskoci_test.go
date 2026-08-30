package diskoci

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
)

// newTestRegistry starts an in-process OCI registry and returns its host:port.
func newTestRegistry(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// countingRegistry wraps the in-process registry and counts blob uploads
// (POST/PUT/PATCH to blob endpoints).
func newCountingRegistry(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var uploads atomic.Int64
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blobs/") &&
			(r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch) {
			uploads.Add(1)
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host, &uploads
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// makeDisk creates a test disk file: random head, zero hole, random tail.
func makeDisk(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.qcow2")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	head := make([]byte, size/4)
	if _, err := rand.Read(head); err != nil {
		t.Fatal(err)
	}
	// Stamp the qcow2 magic so format detection stores the file as-is
	// (a raw-detected input would be converted, breaking byte-identity).
	copy(head, qcow2Magic)
	if _, err := f.Write(head); err != nil {
		t.Fatal(err)
	}
	// hole in the middle: seek past it (sparse)
	tail := make([]byte, size/4)
	if _, err := rand.Read(tail); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(tail, size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPushPullRoundTrip(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/test/disk:v1"
	disk := makeDisk(t, 8<<20)

	digest, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(digest), "sha256:") {
		t.Errorf("digest = %q", digest)
	}

	dest := filepath.Join(t.TempDir(), "pulled.qcow2")
	if err := Pull(context.Background(), ref, dest); err != nil {
		t.Fatal(err)
	}

	if got, want := sha256File(t, dest), sha256File(t, disk); got != want {
		t.Errorf("pulled file differs: %s != %s", got, want)
	}
}

func TestPushIdempotent(t *testing.T) {
	host, uploads := newCountingRegistry(t)
	ref := host + "/test/disk:v1"
	disk := makeDisk(t, 4<<20)

	if _, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20)); err != nil {
		t.Fatal(err)
	}
	first := uploads.Load()
	if first == 0 {
		t.Fatal("first push uploaded no blobs")
	}

	if _, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20)); err != nil {
		t.Fatal(err)
	}
	if second := uploads.Load() - first; second != 0 {
		t.Errorf("re-push performed %d blob uploads, want 0", second)
	}
}

func TestPullNotFound(t *testing.T) {
	host := newTestRegistry(t)
	dest := filepath.Join(t.TempDir(), "pulled.qcow2")

	err := Pull(context.Background(), host+"/test/missing:v1", dest)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Error("destPath exists after failed pull")
	}
}

func TestPullPartialFailureLeavesNoFile(t *testing.T) {
	// Registry that serves the manifest but fails blob downloads midway.
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var fail atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/sha256:") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer s.Close()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	ref := u.Host + "/test/disk:v1"

	disk := makeDisk(t, 4<<20)
	if _, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20)); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	dest := filepath.Join(t.TempDir(), "pulled.qcow2")
	if err := Pull(context.Background(), ref, dest); err == nil {
		t.Fatal("expected pull to fail")
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("failed pull left files behind: %v", names)
	}
}

func TestResolveImage(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/test/disk:v1"
	disk := makeDisk(t, 2<<20)

	pushed, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}

	desc, err := ResolveImage(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Digest != pushed {
		t.Errorf("resolved digest = %s, want %s", desc.Digest, pushed)
	}
	if desc.Size == 0 {
		t.Error("descriptor size is zero")
	}

	_, err = ResolveImage(context.Background(), host+"/test/missing:v1")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("miss err = %v, want ErrNotFound", err)
	}
}

func TestExplicitCredentialsReachTransport(t *testing.T) {
	const user, pass = "ci-bot", "hunter2"
	inner := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	var sawAuth atomic.Bool
	var mu sync.Mutex
	var badAuth []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok {
			// Challenge so the client knows Basic auth is expected.
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if u != user || p != pass {
			mu.Lock()
			badAuth = append(badAuth, fmt.Sprintf("%s:%s", u, p))
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			return
		}
		sawAuth.Store(true)
		inner.ServeHTTP(w, r)
	}))
	defer s.Close()
	u, err := url.Parse(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	ref := u.Host + "/test/disk:v1"

	disk := makeDisk(t, 1<<20)
	if _, err := Push(context.Background(), ref, disk, WithCredentials(user, pass)); err != nil {
		t.Fatalf("push with credentials: %v (bad auth seen: %v)", err, badAuth)
	}
	if !sawAuth.Load() {
		t.Error("registry never saw the expected Basic auth credentials")
	}

	dest := filepath.Join(t.TempDir(), "pulled.qcow2")
	if err := Pull(context.Background(), ref, dest, WithCredentials(user, pass)); err != nil {
		t.Fatalf("pull with credentials: %v", err)
	}
}

func TestPushStreamsWithoutBufferingWholeFile(t *testing.T) {
	host := newTestRegistry(t)
	ref := host + "/test/bigdisk:v1"

	// 512 MiB sparse file: tiny random regions, rest is holes. Cheap to
	// create, but a whole-file read would allocate/buffer 512 MiB.
	const size = 512 << 20
	path := filepath.Join(t.TempDir(), "big.qcow2")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	blk := make([]byte, 1<<20)
	if _, err := rand.Read(blk); err != nil {
		t.Fatal(err)
	}
	copy(blk, qcow2Magic) // store as-is; see makeDisk
	for _, off := range []int64{0, size / 2, size - int64(len(blk))} {
		if _, err := f.WriteAt(blk, off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	f.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	if _, err := Push(context.Background(), ref, path, WithChunkSize(64<<20)); err != nil {
		t.Fatal(err)
	}

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	// A whole-file buffer would show up as >= 512 MiB of allocations.
	// Streaming with fixed buffers plus one zstd encoder stays far below.
	const ceiling = 256 << 20
	if allocated > ceiling {
		t.Errorf("Push allocated %d MiB, ceiling %d MiB — likely buffering the whole file",
			allocated>>20, int64(ceiling)>>20)
	}
}
