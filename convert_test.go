package diskoci

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireQemuImg skips the conversion test tier when qemu-img is absent
// (mirrors go-wimlib's stub pattern).
func requireQemuImg(t *testing.T) {
	t.Helper()
	if !qemuImgAvailable() {
		t.Skip("qemu-img not in PATH; skipping conversion tier")
	}
}

func TestDetectFormat(t *testing.T) {
	dir := t.TempDir()

	qcow2 := filepath.Join(dir, "a.img")
	if err := os.WriteFile(qcow2, append(append([]byte{}, qcow2Magic...), 0, 0, 0, 3), 0o644); err != nil {
		t.Fatal(err)
	}
	vhdx := filepath.Join(dir, "b.img")
	if err := os.WriteFile(vhdx, append(append([]byte{}, vhdxMagic...), 1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(dir, "c.img")
	if err := os.WriteFile(raw, []byte("just some raw bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{qcow2: "qcow2", vhdx: "vhdx", raw: "raw"} {
		got, err := detectFormat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("detectFormat(%s) = %q, want %q", path, got, want)
		}
	}
}

// makeRawDisk creates a raw disk with recognizable content and a zero hole.
func makeRawDisk(t *testing.T, size int64) ([]byte, string) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data[:size/4]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(data[size-size/4:]); err != nil {
		t.Fatal(err)
	}
	// Ensure it does not look like qcow2/vhdx.
	data[0] = 0x00
	path := filepath.Join(t.TempDir(), "disk.raw")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return data, path
}

// TestRawInRawOut pushes a raw image (converted to qcow2 at the boundary) and
// pulls it back as raw: guest content must be identical.
func TestRawInRawOut(t *testing.T) {
	requireQemuImg(t)

	host := newTestRegistry(t)
	ref := host + "/test/rawdisk:v1"
	data, disk := makeRawDisk(t, 4<<20)

	if _, err := Push(context.Background(), ref, disk, WithChunkSize(1<<20)); err != nil {
		t.Fatal(err)
	}

	// Stored object must be qcow2, annotated with the raw source format.
	dest := filepath.Join(t.TempDir(), "as-stored.img")
	if err := Pull(context.Background(), ref, dest); err != nil {
		t.Fatal(err)
	}
	format, err := detectFormat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if format != "qcow2" {
		t.Errorf("stored format = %q, want qcow2", format)
	}

	// Pull with raw output: guest content matches the original bytes.
	rawDest := filepath.Join(t.TempDir(), "out.raw")
	if err := Pull(context.Background(), ref, rawDest, WithOutputFormat("raw")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(rawDest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("raw round-trip content mismatch (got %d bytes, want %d)", len(got), len(data))
	}
}

// TestQcow2InRawOut pushes a real qemu-img-created qcow2 and pulls its raw form.
func TestQcow2InRawOut(t *testing.T) {
	requireQemuImg(t)

	// Build a real qcow2 from known raw content.
	data, rawPath := makeRawDisk(t, 4<<20)
	qcowPath := filepath.Join(t.TempDir(), "disk.qcow2")
	if out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", rawPath, qcowPath).CombinedOutput(); err != nil {
		t.Fatalf("qemu-img: %v: %s", err, out)
	}

	host := newTestRegistry(t)
	ref := host + "/test/qcowdisk:v1"
	if _, err := Push(context.Background(), ref, qcowPath); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "out.raw")
	if err := Pull(context.Background(), ref, dest, WithOutputFormat("raw")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("qcow2→raw content mismatch (got %d bytes, want %d)", len(got), len(data))
	}
}

func TestConvertImageMissingQemuImg(t *testing.T) {
	// Force an empty PATH so LookPath fails regardless of the host.
	t.Setenv("PATH", t.TempDir())
	err := convertImage(context.Background(), "src", "dst", "raw", "qcow2")
	if err == nil {
		t.Fatal("expected error without qemu-img")
	}
}
