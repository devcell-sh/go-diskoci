package diskoci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultChunkSize is the uncompressed size of a single disk layer.
	DefaultChunkSize = 512 * 1024 * 1024

	// holeGranularity is the block size used for zero-region detection when
	// writing a pulled disk sparsely.
	holeGranularity = 4 * 1024 * 1024
)

// zeroBlock is a static all-zero buffer; whole-buffer equality against it is
// far cheaper than a byte-by-byte scan (tart benchmarked ~30x).
var zeroBlock = make([]byte, holeGranularity)

// chunk describes one fixed-size slice of the disk file, zstd-compressed into
// a temp file awaiting upload.
type chunk struct {
	index  int
	offset int64  // uncompressed offset within the disk file
	size   int64  // uncompressed size
	zero   bool   // the whole chunk is zeros
	digest string // sha256 of the uncompressed chunk, "sha256:..." form
	path   string // temp file holding the zstd-compressed chunk

	compressedDigest string // sha256 of the compressed blob
	compressedSize   int64  // size of the compressed blob
}

// sha256Digest returns the "sha256:<hex>" digest of b.
func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// isZero reports whether the buffer is entirely zero bytes.
func isZero(b []byte) bool {
	for len(b) >= holeGranularity {
		if !bytes.Equal(b[:holeGranularity], zeroBlock) {
			return false
		}
		b = b[holeGranularity:]
	}
	return bytes.Equal(b, zeroBlock[:len(b)])
}

// layerize splits the disk file at diskPath into chunkSize slices, compressing
// each into a temp file under tmpDir. It streams with a small fixed buffer and
// never holds a whole chunk in memory.
func layerize(diskPath string, chunkSize int64, tmpDir string) ([]chunk, error) {
	f, err := os.Open(diskPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var chunks []chunk
	buf := make([]byte, holeGranularity)
	var offset int64

	for index := 0; ; index++ {
		c, n, err := compressChunk(f, buf, chunkSize, index, offset, tmpDir)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		chunks = append(chunks, c)
		offset += n
		if n < chunkSize {
			break
		}
	}
	return chunks, nil
}

// compressChunk reads up to chunkSize bytes from r, zstd-compressing them into
// a temp file while hashing the uncompressed data and tracking zero-ness.
// Returns the chunk metadata and the number of uncompressed bytes consumed.
func compressChunk(r io.Reader, buf []byte, chunkSize int64, index int, offset int64, tmpDir string) (chunk, int64, error) {
	out, err := os.CreateTemp(tmpDir, fmt.Sprintf("chunk-%d-*.zst", index))
	if err != nil {
		return chunk{}, 0, err
	}
	defer out.Close()

	compressedHash := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(out, compressedHash)}
	enc, err := zstd.NewWriter(counted)
	if err != nil {
		return chunk{}, 0, err
	}
	hash := sha256.New()

	var consumed int64
	zero := true
	for consumed < chunkSize {
		want := min(chunkSize-consumed, int64(len(buf)))
		n, err := io.ReadFull(r, buf[:want])
		if n > 0 {
			consumed += int64(n)
			if zero && !isZero(buf[:n]) {
				zero = false
			}
			hash.Write(buf[:n])
			if _, werr := enc.Write(buf[:n]); werr != nil {
				enc.Close()
				return chunk{}, 0, werr
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			enc.Close()
			return chunk{}, 0, err
		}
	}

	if err := enc.Close(); err != nil {
		return chunk{}, 0, err
	}
	if consumed == 0 {
		os.Remove(out.Name())
		return chunk{}, 0, nil
	}
	return chunk{
		index:            index,
		offset:           offset,
		size:             consumed,
		zero:             zero,
		digest:           "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		path:             out.Name(),
		compressedDigest: "sha256:" + hex.EncodeToString(compressedHash.Sum(nil)),
		compressedSize:   counted.n,
	}, consumed, nil
}

// countingWriter counts bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
