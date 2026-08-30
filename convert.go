package diskoci

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// Supported disk image formats (qemu-img format names).
const (
	formatQCOW2 = "qcow2"
	formatRaw   = "raw"
	formatVHDX  = "vhdx"
)

var (
	qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}
	vhdxMagic  = []byte("vhdxfile")
)

// detectFormat sniffs the image format from its magic bytes. Anything that is
// neither qcow2 nor VHDX is treated as a raw image.
func detectFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	magic := make([]byte, 8)
	n, _ := f.Read(magic)
	magic = magic[:n]

	switch {
	case bytes.HasPrefix(magic, qcow2Magic):
		return formatQCOW2, nil
	case bytes.HasPrefix(magic, vhdxMagic):
		return formatVHDX, nil
	default:
		return formatRaw, nil
	}
}

// convertImage converts src to dst between disk image formats by shelling out
// to qemu-img (pure Go conversion is a later milestone). Returns a descriptive
// error when qemu-img is not installed.
func convertImage(ctx context.Context, src, dst, srcFormat, dstFormat string) error {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("diskoci: format conversion %s→%s requires qemu-img in PATH: %w", srcFormat, dstFormat, err)
	}
	cmd := exec.CommandContext(ctx, qemuImg, "convert", "-f", srcFormat, "-O", dstFormat, src, dst)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert: %w: %s", err, stderr.String())
	}
	return nil
}

// qemuImgAvailable reports whether qemu-img is present (used by tests to skip
// the conversion tier).
func qemuImgAvailable() bool {
	_, err := exec.LookPath("qemu-img")
	return err == nil
}
