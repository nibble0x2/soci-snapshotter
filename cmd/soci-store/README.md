# soci-store

`soci-store` is an implementation of the "additional layer store" plugin for CRI-O/Podman that enables lazy pulling of SOCI (Seekable OCI) images, similar to how `stargz-store` works for eStargz images.

## Overview

The additional layer store feature allows container runtimes like CRI-O and Podman to use remotely-mounted layers without fully downloading them first. `soci-store` provides SOCI-indexed layers as a FUSE filesystem that integrates with the containers/storage additional layer store mechanism.

## Installation

Build the binary:

```bash
go build -o soci-store ./cmd/soci-store
```

Or install it to your system:

```bash
sudo install -D -m 755 soci-store /usr/local/bin/soci-store
```

## Configuration

### Storage Configuration

To enable lazy pulling with CRI-O/Podman, add the following to your storage configuration (typically `/etc/containers/storage.conf`):

```toml
[storage.options]
additionallayerstores = [
  "/var/lib/soci-store:ref"
]
```

### SOCI Store Configuration

Create a configuration file at `/etc/soci-store/config.toml`:

```toml
# Metadata store type: "memory" (default) or "db"
metadata_store = "memory"

# Disable Prometheus metrics
no_prometheus = false

# Debug mode
[fuse]
log_fuse_operations = false

# Background fetch configuration
[background_fetch]
disable = false
silence_period_msec = 30000
fetch_period_msec = 10
max_queue_size = 10000
emit_metric_period_sec = 300

# Registry configuration
[resolver]
# Add registry-specific configurations here
```

## Usage

### Running as a Systemd Service

Create a systemd service file at `/etc/systemd/system/soci-store.service`:

```ini
[Unit]
Description=SOCI Additional Layer Store
After=network.target

[Service]
Type=notify
ExecStart=/usr/local/bin/soci-store /var/lib/soci-store
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable and start the service:

```bash
sudo systemctl daemon-reload
sudo systemctl enable soci-store
sudo systemctl start soci-store
```

### Manual Execution

```bash
# Mount the store at the default location
sudo soci-store /var/lib/soci-store

# Use custom configuration
sudo soci-store --config /path/to/config.toml /var/lib/soci-store

# Enable debug logging
sudo soci-store --log-level debug /var/lib/soci-store
```

## How It Works

1. **FUSE Filesystem**: `soci-store` mounts a FUSE filesystem at the specified mountpoint (e.g., `/var/lib/soci-store`)

2. **Layer Access**: When CRI-O/Podman needs a layer, it accesses it through the FUSE filesystem using the additional layer store mechanism

3. **Lazy Loading**: SOCI-indexed layers are fetched on-demand using SOCI's ztoc (zstd table of contents) for efficient random access

4. **Background Fetching**: Layers can be fetched in the background to improve performance

## Directory Structure

The mounted filesystem provides the following structure:

```
/var/lib/soci-store/
├── pool -> <link to reference pool>
└── <base64-encoded-image-ref>/
    └── <layer-digest>/
        ├── diff -> <mounted layer contents>
        ├── blob -> <raw blob data>
        └── info -> <layer metadata JSON>
```

## Layer Information Format

The `info` file contains JSON metadata compatible with containers/storage:

```json
{
  "compressed-diff-digest": "sha256:...",
  "compressed-size": 12345678,
  "diff-digest": "sha256:...",
  "diff-size": 23456789,
  "compression": 2
}
```

## Integration with Podman/CRI-O

Once configured, SOCI-indexed images can be used transparently:

```bash
# Pull a SOCI-indexed image
sudo podman pull registry.example.com/my-image:latest

# Run a container using lazy-pulled layers
sudo podman run -it registry.example.com/my-image:latest
```

## Differences from stargz-store

- Uses SOCI's ztoc format instead of eStargz
- Optimized for zstd compression
- Integrates with SOCI snapshotter ecosystem
- Supports SOCI-specific features like span-based prefetching

## Troubleshooting

### Check the service status
```bash
sudo systemctl status soci-store
```

### View logs
```bash
sudo journalctl -u soci-store -f
```

### Verify the mount
```bash
mount | grep soci-store
ls /var/lib/soci-store
```

### Enable debug logging
Edit the service file or run manually with `--log-level debug`

## Requirements

- Linux kernel with FUSE support
- CRI-O or Podman with additional layer store support
- SOCI-indexed container images

## See Also

- [SOCI Snapshotter Documentation](https://github.com/awslabs/soci-snapshotter)
- [Stargz Snapshotter](https://github.com/containerd/stargz-snapshotter)
- [Additional Layer Store](https://github.com/containers/storage/blob/main/docs/containers-storage-additional-layer-store.md)
