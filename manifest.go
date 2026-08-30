package diskoci

import (
	"encoding/json"
	"strconv"
)

// Wire-format constants. Changing any of these breaks compatibility with
// already-pushed images — guarded by TestManifestGolden.
const (
	// ArtifactType identifies a devcell disk image artifact.
	ArtifactType = "application/vnd.devcell.disk.v1"

	// ConfigMediaType is the media type of the artifact config blob.
	ConfigMediaType = "application/vnd.devcell.disk.config.v1+json"

	// ChunkMediaType is the media type of a zstd-compressed disk chunk layer.
	ChunkMediaType = "application/vnd.devcell.disk.chunk.v1+zstd"

	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"

	// Layer annotations.
	annotationUncompressedSize          = "org.devcell.disk.uncompressed-size"
	annotationUncompressedContentDigest = "org.devcell.disk.uncompressed-content-digest"

	// Manifest annotations.
	annotationUncompressedDiskSize = "org.devcell.disk.uncompressed-disk-size"
	annotationSourceFormat         = "org.devcell.disk.source-format"
)

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        ociDescriptor     `json:"config"`
	Layers        []ociDescriptor   `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// diskConfig is the artifact config blob content.
type diskConfig struct {
	Format string `json:"format"`
}

// buildConfig serializes the artifact config blob for a disk stored in the
// given format (always "qcow2" for v1 pushes).
func buildConfig(format string) ([]byte, error) {
	return json.Marshal(diskConfig{Format: format})
}

// buildManifest assembles the deterministic OCI manifest for the given
// compressed chunks. Layer descriptors reference the compressed blobs;
// annotations carry the uncompressed size and content digest of each chunk so
// pulls can verify and resume. extraAnnotations (caller labels) are merged in
// but cannot override the org.devcell.disk.* keys.
func buildManifest(configBytes []byte, chunks []chunk, uncompressedDiskSize int64, sourceFormat string, extraAnnotations map[string]string) ([]byte, error) {
	layers := make([]ociDescriptor, 0, len(chunks))
	for _, c := range chunks {
		layers = append(layers, ociDescriptor{
			MediaType: ChunkMediaType,
			Digest:    c.compressedDigest,
			Size:      c.compressedSize,
			Annotations: map[string]string{
				annotationUncompressedSize:          strconv.FormatInt(c.size, 10),
				annotationUncompressedContentDigest: c.digest,
			},
		})
	}

	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		ArtifactType:  ArtifactType,
		Config: ociDescriptor{
			MediaType: ConfigMediaType,
			Digest:    sha256Digest(configBytes),
			Size:      int64(len(configBytes)),
		},
		Layers:      layers,
		Annotations: make(map[string]string, len(extraAnnotations)+2),
	}
	for k, v := range extraAnnotations {
		m.Annotations[k] = v
	}
	m.Annotations[annotationUncompressedDiskSize] = strconv.FormatInt(uncompressedDiskSize, 10)
	m.Annotations[annotationSourceFormat] = sourceFormat
	return json.Marshal(m)
}
