# Quick Start Guide - soci-store Local Testing

This guide will help you quickly test soci-store on your local machine.

## Prerequisites

- Linux system with FUSE support
- Podman or CRI-O installed
- Go 1.21+ (for building)
- Root/sudo access (for FUSE mounting)

## 1. Build soci-store

```bash
# From the repository root
make soci-store

# The binary will be at: out/soci-store
```

## 2. Prepare Directories

```bash
# Create mount point and root directory
sudo mkdir -p /var/lib/soci-store
sudo mkdir -p /etc/soci-store

# Copy minimal config
sudo cp cmd/soci-store/config.minimal.toml /etc/soci-store/config.toml
```

## 3. Run soci-store

### Option A: Run with default config

```bash
sudo ./out/soci-store /var/lib/soci-store
```

### Option B: Run with custom config

```bash
sudo ./out/soci-store --config /path/to/config.toml /var/lib/soci-store
```

### Option C: Run with debug logging

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

## 4. Configure Podman/CRI-O

Edit `/etc/containers/storage.conf`:

```toml
[storage.options]
additionallayerstores = [
  "/var/lib/soci-store:ref"
]
```

## 5. Verify the Mount

In another terminal:

```bash
# Check if the FUSE filesystem is mounted
mount | grep soci-store

# List the contents
ls -la /var/lib/soci-store
```

You should see:
```
drwxr-xr-x 2 root root 4096 Jan 17 11:00 pool -> ...
```

## 6. Test with a SOCI-indexed Image

### Create a SOCI index for an image

```bash
# Pull an image
podman pull docker.io/library/nginx:latest

# Create SOCI index (requires soci CLI tool)
sudo ./out/soci create docker.io/library/nginx:latest

# Push the index to the registry (or keep it local)
sudo ./out/soci push docker.io/library/nginx:latest
```

### Run container with lazy loading

```bash
# The soci-store will now provide layers on-demand
podman run -d --name test-nginx docker.io/library/nginx:latest

# Check logs
sudo journalctl -f | grep soci-store
```

## 7. Monitoring and Debugging

### View soci-store logs

```bash
# If running in foreground, logs appear in terminal
# If running as systemd service:
sudo journalctl -u soci-store -f
```

### Enable debug logging

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

### Check FUSE operations

Edit `/etc/soci-store/config.toml`:

```toml
[fuse]
log_fuse_operations = true
```

### Monitor metrics (if enabled)

```bash
# Prometheus metrics are exposed if no_prometheus = false
curl http://localhost:9000/metrics  # Default metrics port
```

## 8. Stopping soci-store

```bash
# If running in foreground: Ctrl+C
# If running as systemd service:
sudo systemctl stop soci-store

# Unmount manually if needed:
sudo umount /var/lib/soci-store
```

## Troubleshooting

### Issue: Permission denied

```bash
# Ensure you're running with sudo
sudo ./out/soci-store /var/lib/soci-store
```

### Issue: Mount point already mounted

```bash
# Unmount first
sudo umount /var/lib/soci-store
# Then run soci-store again
```

### Issue: FUSE not available

```bash
# Install FUSE
sudo apt-get install fuse  # Debian/Ubuntu
sudo yum install fuse      # RHEL/CentOS

# Load kernel module
sudo modprobe fuse
```

### Issue: No SOCI index found for layers

This is normal for images without SOCI indexes. The layers will be downloaded completely instead of using lazy loading. To benefit from lazy loading:

1. Create SOCI indexes using `soci create` command
2. Ensure the indexes are accessible (either in the artifact store or registry)
3. The manifest must have the SOCI index annotation

### Issue: Artifact store errors

```bash
# Check the content store directory exists
ls -la /var/lib/soci-store/content

# Ensure proper permissions
sudo chown -R root:root /var/lib/soci-store
```

## Configuration Tips

### For Local Testing
- Use `config.minimal.toml` - simplest setup
- Enable debug logging: `--log-level debug`
- Disable Prometheus: `no_prometheus = true`

### For Production
- Use `config.sample.toml` as template
- Configure registry authentication in `[resolver]` section
- Enable Prometheus metrics for monitoring
- Run as systemd service (see [README.md](README.md))

## Next Steps

1. Read [README.md](README.md) for production deployment
2. Read [IMPLEMENTATION.md](IMPLEMENTATION.md) to understand lazy loading architecture
3. Configure systemd service for automatic startup
4. Set up registry authentication if using private registries

## Example: Complete Local Test

```bash
# 1. Build
make soci-store

# 2. Setup
sudo mkdir -p /var/lib/soci-store /etc/soci-store
sudo cp cmd/soci-store/config.minimal.toml /etc/soci-store/config.toml

# 3. Configure storage
sudo vi /etc/containers/storage.conf
# Add: additionallayerstores = ["/var/lib/soci-store:ref"]

# 4. Run soci-store (in one terminal)
sudo ./out/soci-store --log-level debug /var/lib/soci-store

# 5. Test (in another terminal)
podman pull docker.io/library/alpine:latest
sudo ./out/soci create docker.io/library/alpine:latest
podman run --rm docker.io/library/alpine:latest echo "Hello from lazy-loaded container!"

# 6. Check logs for lazy loading activity
# Look for messages like: "found SOCI index for layer, enabling lazy loading"
```

## Resources

- SOCI Snapshotter: https://github.com/awslabs/soci-snapshotter
- Additional Layer Store: https://github.com/containers/storage/blob/main/docs/containers-storage-additional-layer-store.md
- Podman Documentation: https://docs.podman.io/
