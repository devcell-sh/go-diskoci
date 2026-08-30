package diskoci

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// TestManifestGolden locks the wire format: media types, artifactType and
// annotation keys must not drift, or already-pushed images become unreadable.
func TestManifestGolden(t *testing.T) {
	chunks := []chunk{
		{
			index:            0,
			offset:           0,
			size:             536870912,
			digest:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			compressedDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			compressedSize:   1000,
		},
		{
			index:            1,
			offset:           536870912,
			size:             100,
			zero:             true,
			digest:           "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			compressedDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			compressedSize:   50,
		},
	}

	configBytes, err := buildConfig("qcow2")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := buildManifest(configBytes, chunks, 536871012, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Manifest must be valid JSON with stable key content.
	var m map[string]any
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}

	goldenPath := filepath.Join("testdata", "manifest.golden.json")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, manifestBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(manifestBytes) != string(golden) {
		t.Errorf("manifest wire format drifted from golden file\ngot:  %s\nwant: %s", manifestBytes, golden)
	}
}

func TestManifestShape(t *testing.T) {
	configBytes, err := buildConfig("qcow2")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := buildManifest(configBytes, []chunk{{
		size:             42,
		digest:           "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		compressedDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		compressedSize:   10,
	}}, 42, "qcow2", nil)
	if err != nil {
		t.Fatal(err)
	}

	var m struct {
		SchemaVersion int    `json:"schemaVersion"`
		MediaType     string `json:"mediaType"`
		ArtifactType  string `json:"artifactType"`
		Config        struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Size      int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatal(err)
	}

	if m.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d", m.SchemaVersion)
	}
	if m.MediaType != "application/vnd.oci.image.manifest.v1+json" {
		t.Errorf("mediaType = %q", m.MediaType)
	}
	if m.ArtifactType != "application/vnd.devcell.disk.v1" {
		t.Errorf("artifactType = %q", m.ArtifactType)
	}
	if m.Config.MediaType != "application/vnd.devcell.disk.config.v1+json" {
		t.Errorf("config mediaType = %q", m.Config.MediaType)
	}
	if len(m.Layers) != 1 {
		t.Fatalf("layers = %d", len(m.Layers))
	}
	l := m.Layers[0]
	if l.MediaType != "application/vnd.devcell.disk.chunk.v1+zstd" {
		t.Errorf("layer mediaType = %q", l.MediaType)
	}
	if l.Digest != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || l.Size != 10 {
		t.Errorf("layer digest/size = %q/%d (must reference the compressed blob)", l.Digest, l.Size)
	}
	if l.Annotations["org.devcell.disk.uncompressed-size"] != "42" {
		t.Errorf("uncompressed-size annotation = %q", l.Annotations["org.devcell.disk.uncompressed-size"])
	}
	if l.Annotations["org.devcell.disk.uncompressed-content-digest"] != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Errorf("uncompressed-content-digest annotation = %q", l.Annotations["org.devcell.disk.uncompressed-content-digest"])
	}
	if m.Annotations["org.devcell.disk.uncompressed-disk-size"] != "42" {
		t.Errorf("uncompressed-disk-size annotation = %q", m.Annotations["org.devcell.disk.uncompressed-disk-size"])
	}
	if m.Annotations["org.devcell.disk.source-format"] != "qcow2" {
		t.Errorf("source-format annotation = %q", m.Annotations["org.devcell.disk.source-format"])
	}
}
