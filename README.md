# go-diskoci

Store VM disk images in any OCI registry — like `docker push`/`docker pull`, but for qcow2/raw/VHDX disks.

Built for [devcell](https://github.com/devcell-sh/devcell) to cache built Windows VM disks between machines and CI runs (the disk-image twin of go-nixoci). Works with GHCR, Docker Hub, or any OCI-compliant registry.

- Any format in (qcow2/raw/VHDX, auto-detected), qcow2 stored in the registry, any format out on request.
- Images are split into fixed-size zstd-compressed chunks and streamed — pushing a multi-GB disk never buffers it in memory.
- Re-pushing existing content uploads nothing (blob-level dedup).
- Pulls are atomic: assembled in a temp file and renamed into place, so a failed pull never leaves a corrupt file.

## 1. CLI

Install:

```sh
task install        # builds and copies to ~/.local/bin/diskoci
```

Push a disk (FROM local file TO registry ref):

```sh
diskoci push win11.qcow2 ghcr.io/org/win11:v1
# sha256:0b4b0119f40d...
```

Pull it back (FROM registry ref TO local file):

```sh
diskoci pull ghcr.io/org/win11:v1 restored.qcow2

# Destination is optional — defaults to <imageName>.<ext> in the current dir:
diskoci pull ghcr.io/org/win11:v1          # → win11.qcow2
diskoci pull ghcr.io/org/win11:v1 --output-format raw   # → win11.raw
```

Tags default to `:latest`, docker-style.

### Authentication

In order of precedence:

1. `-u`/`--username` and `-p`/`--password` flags
2. `DISKOCI_USERNAME` / `DISKOCI_PASSWORD` environment variables (CI-friendly)
3. Default keychain (`~/.docker/config.json`, credential helpers)

```sh
DISKOCI_USERNAME=ci-bot DISKOCI_PASSWORD=$TOKEN diskoci push disk.qcow2 ghcr.io/org/disk:v1
```

### Flags

| Flag | Command | Default | Purpose |
|------|---------|---------|---------|
| `--chunk-size` | push | 512 MiB | Uncompressed size of each layer |
| `--source-format` | push | auto-detect | Input format (`qcow2`\|`raw`\|`vhdx`) |
| `--annotation k=v` | push | — | Manifest annotation/label (repeatable) |
| `--output-format` | pull | qcow2 as stored | Convert on the way out (`raw`\|`vhdx` need `qemu-img`) |

## 2. Library

```sh
go get github.com/devcell-sh/go-diskoci
```

```go
import diskoci "github.com/devcell-sh/go-diskoci"

// Push a local disk image; returns the manifest digest.
digest, err := diskoci.Push(ctx, "ghcr.io/org/win11:v1", "/tmp/win11.qcow2")

// Pull into destPath. Cache miss → diskoci.ErrNotFound.
err = diskoci.Pull(ctx, "ghcr.io/org/win11:v1", "/tmp/restored.qcow2")
if errors.Is(err, diskoci.ErrNotFound) {
    // build the image instead
}

// Cheap existence/digest probe — no download.
desc, err := diskoci.ResolveImage(ctx, "ghcr.io/org/win11:v1")
```

Options (all three functions take them variadically):

```go
diskoci.WithChunkSize(256 << 20)                    // layer size (default 512 MiB)
diskoci.WithCredentials("ci-bot", token)            // explicit auth instead of keychain
diskoci.WithSourceFormat("raw")                     // Push: skip format auto-detection
diskoci.WithOutputFormat("raw")                     // Pull: convert on the way out
diskoci.WithAnnotations(map[string]string{...})     // Push: custom manifest labels
diskoci.WithTempDir("/scratch")                     // compression/conversion temp space
```

The cache key/tag is the caller's concern — the library takes an opaque ref. devcell computes it from winkit's build fingerprint and composes `winkit.Build() → diskoci.Push()` / `diskoci.Pull() → boot`.

## Wire format

OCI image manifest, `artifactType: application/vnd.devcell.disk.v1`:

- Layers: `application/vnd.devcell.disk.chunk.v1+zstd` — fixed-size slices of the disk file, each an independently decompressible zstd stream.
- Layer annotations `org.devcell.disk.uncompressed-size` / `uncompressed-content-digest` let pulls verify each chunk.
- Manifest annotations carry the total uncompressed disk size and the source format.

Locked by a golden test (`testdata/manifest.golden.json`) — the format cannot drift silently.

Layerization mechanics follow [tart](https://github.com/cirruslabs/tart): streaming with fixed 4 MiB buffers, zero-hole skipping on pull (all-zero blocks are never written; the destination stays sparse), idempotent push via blob-existence checks. Compression is zstd (`klauspost/compress`), transport is `go-containerregistry` — pure Go except format conversion, which shells out to `qemu-img`.

## Development

```sh
task test       # full suite; conversion tests self-skip without qemu-img
task build      # ./bin/diskoci
task install    # → ~/.local/bin/diskoci
```

All registry tests run against an in-process registry (`go-containerregistry/pkg/registry`) — no network, no credentials, no daemons.
