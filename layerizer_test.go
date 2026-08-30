package diskoci

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// writeTestDisk writes data to a temp file and returns its path.
func writeTestDisk(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// decompressChunk reads back the zstd-compressed temp file of a chunk.
func decompressChunk(t *testing.T, c chunk) []byte {
	t.Helper()
	f, err := os.Open(c.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dec.IOReadCloser()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLayerizeChunkBoundaries(t *testing.T) {
	// 2.5 chunks worth of random data at a small chunk size.
	const chunkSize = 1 << 20 // 1 MiB for test speed
	data := make([]byte, chunkSize*2+chunkSize/2)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	disk := writeTestDisk(t, data)

	chunks, err := layerize(disk, chunkSize, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	wantSizes := []int64{chunkSize, chunkSize, chunkSize / 2}
	var offset int64
	for i, c := range chunks {
		if c.index != i {
			t.Errorf("chunk %d: index = %d", i, c.index)
		}
		if c.offset != offset {
			t.Errorf("chunk %d: offset = %d, want %d", i, c.offset, offset)
		}
		if c.size != wantSizes[i] {
			t.Errorf("chunk %d: size = %d, want %d", i, c.size, wantSizes[i])
		}
		got := decompressChunk(t, c)
		if !bytes.Equal(got, data[c.offset:c.offset+c.size]) {
			t.Errorf("chunk %d: decompressed content mismatch", i)
		}
		offset += c.size
	}
}

func TestLayerizeZeroHoleDetection(t *testing.T) {
	const chunkSize = 1 << 20
	data := make([]byte, 3*chunkSize)
	// chunk 0: random, chunk 1: all zeros (sparse region), chunk 2: random
	if _, err := rand.Read(data[:chunkSize]); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(data[2*chunkSize:]); err != nil {
		t.Fatal(err)
	}
	disk := writeTestDisk(t, data)

	chunks, err := layerize(disk, chunkSize, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	if chunks[0].zero || chunks[2].zero {
		t.Error("random chunks misdetected as zero")
	}
	if !chunks[1].zero {
		t.Error("all-zero chunk not detected as zero")
	}
}

func TestLayerizeSingleChunkSmallFile(t *testing.T) {
	data := []byte("small disk image content")
	disk := writeTestDisk(t, data)

	chunks, err := layerize(disk, 1<<20, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.offset != 0 || c.size != int64(len(data)) {
		t.Errorf("chunk = offset %d size %d, want 0/%d", c.offset, c.size, len(data))
	}
	if !bytes.Equal(decompressChunk(t, c), data) {
		t.Error("decompressed content mismatch")
	}
}

func TestLayerizeEmptyFile(t *testing.T) {
	disk := writeTestDisk(t, nil)

	chunks, err := layerize(disk, 1<<20, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty file, got %d", len(chunks))
	}
}

func TestIsZero(t *testing.T) {
	if !isZero(make([]byte, 4096)) {
		t.Error("all-zero buffer not detected")
	}
	b := make([]byte, 4096)
	b[4095] = 1
	if isZero(b) {
		t.Error("non-zero buffer detected as zero")
	}
	if !isZero(nil) {
		t.Error("empty buffer should count as zero")
	}
}
