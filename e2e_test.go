package diskoci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// TestE2ELayeredQcow2 exercises the full workflow against a local registry:
//
//  0. record the test start timestamp
//  1. create a base qcow2 carrying "1.txt: <ts>"
//  2. add a COW overlay layer on top carrying "2.txt: <ts>"
//  3. start a local OCI registry (in-process)
//  4. push the overlay with label layer-layer:<ts> as layer-test:<ts>-src
//  5. pull it back and re-push as layer-test:<ts>-dst; both digests must
//     match and the flattened chain must contain both file markers
//
// File content lives at fixed offsets instead of a real filesystem because
// creating one inside an image needs mtools/guestfish/root, none of which are
// available here.
func TestE2ELayeredQcow2(t *testing.T) {
	requireQemuImg(t)

	// 0. Timestamp. Colons are illegal in OCI tags, so tags use the ISO 8601
	// basic format while markers carry the full RFC 3339 form.
	now := time.Now().UTC()
	tsISO := now.Format(time.RFC3339)
	tsTag := now.Format("20060102T150405Z")
	t.Logf("test run started: %s", tsISO)

	work := t.TempDir()
	const (
		diskSize      = 8 << 20
		marker1Offset = 1 << 20
		marker2Offset = 2 << 20
	)
	marker1 := []byte("1.txt: " + tsISO)
	marker2 := []byte("2.txt: " + tsISO)

	qemuImg := func(args ...string) {
		t.Helper()
		cmd := exec.Command("qemu-img", args...)
		cmd.Dir = work // keep the overlay's backing-file reference relative
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}

	// 1. Base layer: raw image with 1.txt, converted to qcow2.
	state1 := make([]byte, diskSize)
	copy(state1[marker1Offset:], marker1)
	if err := os.WriteFile(filepath.Join(work, "state1.raw"), state1, 0o644); err != nil {
		t.Fatal(err)
	}
	qemuImg("convert", "-f", "raw", "-O", "qcow2", "state1.raw", "base.qcow2")

	// 2. Overlay layer: state2 adds 2.txt; convert with -B records only the
	// difference against base.qcow2 as a COW overlay.
	state2 := make([]byte, diskSize)
	copy(state2[marker1Offset:], marker1)
	copy(state2[marker2Offset:], marker2)
	if err := os.WriteFile(filepath.Join(work, "state2.raw"), state2, 0o644); err != nil {
		t.Fatal(err)
	}
	qemuImg("convert", "-f", "raw", "-O", "qcow2", "-B", "base.qcow2", "-F", "qcow2", "state2.raw", "overlay.qcow2")

	// The overlay must be a true COW layer referencing base.qcow2.
	infoCmd := exec.Command("qemu-img", "info", "overlay.qcow2")
	infoCmd.Dir = work
	info, err := infoCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img info: %v: %s", err, info)
	}
	if !strings.Contains(string(info), "backing file: base.qcow2") {
		t.Fatalf("overlay has no backing-file reference to base.qcow2:\n%s", info)
	}

	// 3. Local OCI registry.
	host := newTestRegistry(t)
	ctx := context.Background()

	// 4. Push the overlay with the layer label.
	// Chunk size far below the overlay's ~450 KiB so the image is stored as
	// multiple chunk layers, not one blob.
	const chunkSize = 128 << 10
	srcRef := fmt.Sprintf("%s/test/layer-test:%s-src", host, tsTag)
	label := "layer-layer:" + tsTag
	srcDigest, err := Push(ctx, srcRef, filepath.Join(work, "overlay.qcow2"),
		WithChunkSize(chunkSize),
		WithAnnotations(map[string]string{"org.devcell.disk.label": label}))
	if err != nil {
		t.Fatal(err)
	}

	// The label must be on the manifest in the registry.
	parsed, err := name.ParseReference(srcRef)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(desc.Manifest), label) {
		t.Errorf("pushed manifest does not carry label %q", label)
	}

	// The stored artifact must consist of more than one chunk layer.
	var pushed ociManifest
	if err := json.Unmarshal(desc.Manifest, &pushed); err != nil {
		t.Fatal(err)
	}
	layerCount := 0
	for _, l := range pushed.Layers {
		if l.MediaType == ChunkMediaType {
			layerCount++
		}
	}
	t.Logf("stored as %d chunk layers of %d KiB", layerCount, chunkSize>>10)
	if layerCount <= 1 {
		t.Errorf("manifest has %d chunk layers, want >1", layerCount)
	}

	// 5. Pull, then re-push the pulled file under the -dst tag.
	dstPath := filepath.Join(t.TempDir(), fmt.Sprintf("layer-test-%s-dst.qcow2", tsTag))
	if err := Pull(ctx, srcRef, dstPath); err != nil {
		t.Fatal(err)
	}
	if got, want := sha256File(t, dstPath), sha256File(t, filepath.Join(work, "overlay.qcow2")); got != want {
		t.Errorf("pulled overlay differs from pushed: %s != %s", got, want)
	}

	dstRef := fmt.Sprintf("%s/test/layer-test:%s-dst", host, tsTag)
	// Same chunk size as the src push, or the manifests (and digests) differ.
	dstDigest, err := Push(ctx, dstRef, dstPath,
		WithChunkSize(chunkSize),
		WithAnnotations(map[string]string{"org.devcell.disk.label": label}))
	if err != nil {
		t.Fatal(err)
	}
	if srcDigest != dstDigest {
		t.Errorf("src and dst manifest digests differ: %s != %s", srcDigest, dstDigest)
	}

	// Flatten the pulled overlay against the base and verify both markers.
	// (v1 pushes a single file; the backing chain stays a local concern.)
	flatDir := filepath.Dir(dstPath)
	if err := os.Link(filepath.Join(work, "base.qcow2"), filepath.Join(flatDir, "base.qcow2")); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(flatDir, "flat.raw")
	cmd := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", dstPath, flat)
	cmd.Dir = flatDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("flatten: %v: %s", err, out)
	}
	flatData, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(flatData[marker1Offset:marker1Offset+int64(len(marker1))], marker1) {
		t.Error("1.txt marker (base layer) missing from flattened chain")
	}
	if !bytes.Equal(flatData[marker2Offset:marker2Offset+int64(len(marker2))], marker2) {
		t.Error("2.txt marker (overlay layer) missing from flattened chain")
	}
	t.Logf("e2e complete: src=%s dst=%s digest=%s", srcRef, dstRef, srcDigest)
}
