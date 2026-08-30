// Package diskoci stores VM disk images in OCI registries.
//
// qcow2 is the canonical wire format: any supported format goes in
// (raw/VHDX/qcow2), qcow2 lives in the registry as fixed-size zstd-compressed
// chunk layers, and any format comes out on request. It is the disk-image twin
// of go-nixoci and is consumed by devcell; the cache key/tag is the caller's
// concern — this library takes an opaque ref.
package diskoci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/klauspost/compress/zstd"
)

// ErrNotFound is returned by Pull and ResolveImage when the ref does not
// exist in the registry (cache miss).
var ErrNotFound = errors.New("diskoci: image not found")

// Digest is an OCI content digest in "sha256:<hex>" form.
type Digest string

// Descriptor describes a resolved image without downloading it.
type Descriptor struct {
	Digest    Digest
	Size      int64 // manifest size in bytes
	MediaType string
}

type options struct {
	chunkSize    int64
	auth         authn.Authenticator
	sourceFormat string // input format override for Push ("" = auto-detect)
	outputFormat string // output format for Pull ("" = qcow2 as stored)
	tmpDir       string // scratch space ("" = os.TempDir)
	annotations  map[string]string
}

// Option configures Push, Pull or ResolveImage.
type Option func(*options)

// PushOption is an Option accepted by Push.
type PushOption = Option

// PullOption is an Option accepted by Pull.
type PullOption = Option

// WithChunkSize sets the uncompressed size of each disk layer (default 512 MiB).
func WithChunkSize(size int64) Option {
	return func(o *options) { o.chunkSize = size }
}

// WithCredentials uses explicit basic-auth credentials instead of the default
// keychain (env-driven for CI).
func WithCredentials(username, password string) Option {
	return func(o *options) {
		o.auth = &authn.Basic{Username: username, Password: password}
	}
}

// WithSourceFormat overrides input format auto-detection on Push
// ("raw", "vhdx", "qcow2").
func WithSourceFormat(format string) PushOption {
	return func(o *options) { o.sourceFormat = format }
}

// WithOutputFormat converts the pulled image to the given format
// ("raw", "vhdx", "qcow2"); requires qemu-img for anything but qcow2.
func WithOutputFormat(format string) PullOption {
	return func(o *options) { o.outputFormat = format }
}

// WithAnnotations adds custom annotations to the pushed manifest (labels).
// Keys must not collide with the org.devcell.disk.* keys the library sets.
func WithAnnotations(annotations map[string]string) PushOption {
	return func(o *options) { o.annotations = annotations }
}

// WithTempDir sets the scratch directory for compression and conversion
// temp files (default: the system temp directory).
func WithTempDir(dir string) Option {
	return func(o *options) { o.tmpDir = dir }
}

func makeOptions(opts []Option) *options {
	o := &options{chunkSize: DefaultChunkSize}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

func (o *options) remoteOptions(ctx context.Context) []remote.Option {
	ro := []remote.Option{remote.WithContext(ctx)}
	if o.auth != nil {
		ro = append(ro, remote.WithAuth(o.auth))
	} else {
		ro = append(ro, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	return ro
}

// notFound maps registry 404s to ErrNotFound, passing other errors through.
func notFound(err error) error {
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// Push uploads a local disk image as an OCI artifact and returns the manifest
// digest. Non-qcow2 inputs are converted to qcow2 first (requires qemu-img).
// Blob uploads are skipped for blobs the registry already has.
func Push(ctx context.Context, ref string, diskPath string, opts ...PushOption) (Digest, error) {
	o := makeOptions(opts)

	tag, err := name.ParseReference(ref)
	if err != nil {
		return "", err
	}

	sourceFormat := o.sourceFormat
	if sourceFormat == "" {
		sourceFormat, err = detectFormat(diskPath)
		if err != nil {
			return "", err
		}
	}

	// Convert to the canonical wire format if needed.
	pushPath := diskPath
	if sourceFormat != formatQCOW2 {
		converted, err := os.CreateTemp(o.tmpDir, "diskoci-convert-*.qcow2")
		if err != nil {
			return "", err
		}
		converted.Close()
		defer os.Remove(converted.Name())
		if err := convertImage(ctx, diskPath, converted.Name(), sourceFormat, formatQCOW2); err != nil {
			return "", err
		}
		pushPath = converted.Name()
	}

	info, err := os.Stat(pushPath)
	if err != nil {
		return "", err
	}

	scratch, err := os.MkdirTemp(o.tmpDir, "diskoci-push-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)

	chunks, err := layerize(pushPath, o.chunkSize, scratch)
	if err != nil {
		return "", err
	}

	ro := o.remoteOptions(ctx)
	repo := tag.Context()

	// Upload chunk blobs; remote.WriteLayer checks blob existence first, so
	// re-pushing an existing image performs no uploads.
	for _, c := range chunks {
		if err := remote.WriteLayer(repo, &fileLayer{chunk: c}, ro...); err != nil {
			return "", fmt.Errorf("pushing chunk %d: %w", c.index, err)
		}
	}

	// Config blob.
	configBytes, err := buildConfig(sourceFormat)
	if err != nil {
		return "", err
	}
	configLayer := static.NewLayer(configBytes, types.MediaType(ConfigMediaType))
	if err := remote.WriteLayer(repo, configLayer, ro...); err != nil {
		return "", fmt.Errorf("pushing config: %w", err)
	}

	// Manifest.
	manifestBytes, err := buildManifest(configBytes, chunks, info.Size(), sourceFormat, o.annotations)
	if err != nil {
		return "", err
	}
	if err := remote.Put(tag, rawManifest(manifestBytes), ro...); err != nil {
		return "", fmt.Errorf("pushing manifest: %w", err)
	}
	return Digest(sha256Digest(manifestBytes)), nil
}

// Pull fetches ref into destPath as a byte-identical copy of the pushed image.
// It returns ErrNotFound on cache miss. The image is assembled in a temp file
// and renamed into place, so a failed pull never leaves a corrupt destPath.
func Pull(ctx context.Context, ref string, destPath string, opts ...PullOption) error {
	o := makeOptions(opts)

	tag, err := name.ParseReference(ref)
	if err != nil {
		return err
	}
	ro := o.remoteOptions(ctx)

	desc, err := remote.Get(tag, ro...)
	if err != nil {
		return notFound(err)
	}

	var m ociManifest
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}
	if m.ArtifactType != ArtifactType {
		return fmt.Errorf("ref %s is not a devcell disk artifact (artifactType %q)", ref, m.ArtifactType)
	}

	// Assemble into a temp file next to destPath so the final rename is
	// atomic on the same filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".diskoci-pull-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename

	if err := assembleDisk(ctx, tmp, tag.Context(), m, ro); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Convert out of the canonical wire format (qcow2) if requested.
	if o.outputFormat != "" && o.outputFormat != formatQCOW2 {
		converted, err := os.CreateTemp(filepath.Dir(destPath), ".diskoci-convert-*")
		if err != nil {
			return err
		}
		converted.Close()
		defer os.Remove(converted.Name())
		if err := convertImage(ctx, tmpPath, converted.Name(), formatQCOW2, o.outputFormat); err != nil {
			return err
		}
		return os.Rename(converted.Name(), destPath)
	}

	return os.Rename(tmpPath, destPath)
}

// assembleDisk downloads all chunk layers into f, writing sparsely (all-zero
// 4 MiB blocks are skipped over a hole-punched file) and verifying each
// chunk's uncompressed content digest.
func assembleDisk(ctx context.Context, f *os.File, repo name.Repository, m ociManifest, ro []remote.Option) error {
	var totalSize int64
	for _, l := range m.Layers {
		if l.MediaType != ChunkMediaType {
			continue
		}
		size, err := strconv.ParseInt(l.Annotations[annotationUncompressedSize], 10, 64)
		if err != nil {
			return fmt.Errorf("layer %s: missing/invalid %s annotation", l.Digest, annotationUncompressedSize)
		}
		totalSize += size
	}
	if err := f.Truncate(totalSize); err != nil {
		return err
	}

	buf := make([]byte, holeGranularity)
	var offset int64
	for _, l := range m.Layers {
		if l.MediaType != ChunkMediaType {
			continue
		}
		wantDigest := l.Annotations[annotationUncompressedContentDigest]
		n, err := fetchChunk(ctx, f, repo, l, offset, buf, wantDigest, ro)
		if err != nil {
			return fmt.Errorf("pulling chunk at offset %d: %w", offset, err)
		}
		offset += n
	}
	return nil
}

// fetchChunk downloads one compressed chunk blob, decompresses it and writes
// it at the given offset, skipping all-zero blocks. Returns the number of
// uncompressed bytes written.
func fetchChunk(ctx context.Context, f *os.File, repo name.Repository, l ociDescriptor, offset int64, buf []byte, wantDigest string, ro []remote.Option) (int64, error) {
	dig, err := name.NewDigest(repo.String() + "@" + l.Digest)
	if err != nil {
		return 0, err
	}
	layer, err := remote.Layer(dig, ro...)
	if err != nil {
		return 0, err
	}
	rc, err := layer.Compressed()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	dec, err := zstd.NewReader(rc)
	if err != nil {
		return 0, err
	}
	defer dec.Close()

	h := sha256.New()
	var written int64
	for {
		n, rerr := io.ReadFull(dec, buf)
		if n > 0 {
			h.Write(buf[:n])
			if !isZero(buf[:n]) {
				if _, werr := f.WriteAt(buf[:n], offset+written); werr != nil {
					return 0, werr
				}
			}
			written += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}

	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != wantDigest {
		return 0, fmt.Errorf("chunk digest mismatch: got %s, want %s", got, wantDigest)
	}
	return written, nil
}

// ResolveImage checks existence and digest of ref without downloading
// (cheap hit/miss probe). Miss returns ErrNotFound.
func ResolveImage(ctx context.Context, ref string, opts ...Option) (Descriptor, error) {
	o := makeOptions(opts)

	tag, err := name.ParseReference(ref)
	if err != nil {
		return Descriptor{}, err
	}
	desc, err := remote.Head(tag, o.remoteOptions(ctx)...)
	if err != nil {
		return Descriptor{}, notFound(err)
	}
	return Descriptor{
		Digest:    Digest(desc.Digest.String()),
		Size:      desc.Size,
		MediaType: string(desc.MediaType),
	}, nil
}

// fileLayer adapts a compressed chunk temp file to v1.Layer for upload.
type fileLayer struct {
	chunk chunk
}

func (l *fileLayer) Digest() (v1.Hash, error) {
	return v1.NewHash(l.chunk.compressedDigest)
}

func (l *fileLayer) DiffID() (v1.Hash, error) {
	return v1.NewHash(l.chunk.digest)
}

func (l *fileLayer) Compressed() (io.ReadCloser, error) {
	return os.Open(l.chunk.path)
}

func (l *fileLayer) Uncompressed() (io.ReadCloser, error) {
	f, err := os.Open(l.chunk.path)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return readCloser{dec.IOReadCloser(), f}, nil
}

func (l *fileLayer) Size() (int64, error) {
	return l.chunk.compressedSize, nil
}

func (l *fileLayer) MediaType() (types.MediaType, error) {
	return types.MediaType(ChunkMediaType), nil
}

// readCloser closes both the decompressor stream and the underlying file.
type readCloser struct {
	io.ReadCloser
	f *os.File
}

func (r readCloser) Close() error {
	err := r.ReadCloser.Close()
	if ferr := r.f.Close(); err == nil {
		err = ferr
	}
	return err
}

// rawManifest makes pre-serialized manifest bytes remote.Put-able.
type rawManifest []byte

func (m rawManifest) RawManifest() ([]byte, error) {
	return m, nil
}

func (m rawManifest) MediaType() (types.MediaType, error) {
	return types.MediaType(manifestMediaType), nil
}
