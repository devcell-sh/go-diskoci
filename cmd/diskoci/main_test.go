package main

import (
	"bytes"
	"crypto/rand"
	"io"
	"log"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
)

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

// makeQcow2Marked creates a file that format detection treats as qcow2.
func makeQcow2Marked(t *testing.T, size int) string {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	copy(data, []byte{'Q', 'F', 'I', 0xfb})
	path := filepath.Join(t.TempDir(), "disk.qcow2")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runCLI executes the root command with args, returning stdout and error.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPushPullCommands(t *testing.T) {
	host := newTestRegistry(t)
	disk := makeQcow2Marked(t, 2<<20)
	ref := host + "/test/disk:v1"

	out, err := runCLI(t, "push", disk, ref, "--chunk-size", "1048576")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sha256:") {
		t.Errorf("push output missing digest: %q", out)
	}

	dest := filepath.Join(t.TempDir(), "pulled.qcow2")
	out, err = runCLI(t, "pull", ref, dest)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("pulled file differs from pushed file")
	}
}

func TestPushDefaultTag(t *testing.T) {
	host := newTestRegistry(t)
	disk := makeQcow2Marked(t, 1<<20)

	// NAME without :TAG defaults to :latest, like docker.
	if out, err := runCLI(t, "push", disk, host+"/test/disk"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	dest := filepath.Join(t.TempDir(), "pulled.qcow2")
	if out, err := runCLI(t, "pull", host+"/test/disk:latest", dest); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
}

// TestPullDefaultFilename pulls without a destination arg: the file lands in
// the working directory as <imageName>.<ext>.
func TestPullDefaultFilename(t *testing.T) {
	host := newTestRegistry(t)
	disk := makeQcow2Marked(t, 1<<20)
	ref := host + "/test/windisk:v1"

	if out, err := runCLI(t, "push", disk, ref); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	out, err := runCLI(t, "pull", ref)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "windisk.qcow2")); err != nil {
		t.Errorf("default-named file missing: %v (output: %q)", err, out)
	}
	if !strings.Contains(out, "windisk.qcow2") {
		t.Errorf("output should print the destination: %q", out)
	}
}

func TestPullDefaultFilenameExtFollowsOutputFormat(t *testing.T) {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not in PATH")
	}
	host := newTestRegistry(t)
	// A real qcow2 is required: --output-format raw runs a real conversion.
	disk := filepath.Join(t.TempDir(), "disk.qcow2")
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", disk, "1M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create: %v: %s", err, out)
	}
	ref := host + "/test/windisk:v1"

	if out, err := runCLI(t, "push", disk, ref); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	if out, err := runCLI(t, "pull", ref, "--output-format", "raw"); err != nil {
		t.Fatalf("pull: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "windisk.raw")); err != nil {
		t.Errorf("windisk.raw missing: %v", err)
	}
}

func TestPullNotFoundExitsWithError(t *testing.T) {
	host := newTestRegistry(t)
	dest := filepath.Join(t.TempDir(), "pulled.qcow2")

	out, err := runCLI(t, "pull", host+"/test/missing:v1", dest)
	if err == nil {
		t.Fatalf("expected error, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestArgValidation(t *testing.T) {
	for _, args := range [][]string{
		{"push"},
		{"push", "one-arg"},
		{"pull"},
		{"push", "a", "b", "c"},
		{"pull", "a", "b", "c"},
	} {
		if _, err := runCLI(t, args...); err == nil {
			t.Errorf("args %v: expected usage error", args)
		}
	}
}

func TestPushWithAnnotationFlag(t *testing.T) {
	host := newTestRegistry(t)
	disk := makeQcow2Marked(t, 1<<20)

	out, err := runCLI(t, "push", disk, host+"/test/disk:v1",
		"--annotation", "org.devcell.disk.label=layer-layer:test")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}

	if out, err := runCLI(t, "push", disk, host+"/test/disk:v2",
		"--annotation", "malformed-no-equals"); err == nil {
		t.Errorf("malformed annotation accepted:\n%s", out)
	}
}
