# Troubleshooting Guide - soci-store

This guide helps diagnose and resolve common issues when using soci-store.

## Error: "invalid digest" or "failed to resolve layer"

### Symptoms
```
{"error":"failed to resolve layer: ... invalid digest","level":"warning"}
```

### Root Cause
This error occurs when soci-store tries to provide a layer that **does not have a SOCI index**. The layer resolver requires a valid SOCI index (ztoc) to enable lazy loading.

### Why This Happens

soci-store is designed specifically for **SOCI-indexed images**. It cannot provide layers for regular (non-SOCI) images because:

1. The layer resolver requires a ztoc (zstd Table of Contents) to enable lazy loading
2. Without a SOCI index, the resolver cannot determine which parts of the layer to fetch on-demand
3. Regular layer serving would require different code paths that are handled by the container runtime

### Solution

**You must create SOCI indexes for your images before using them with soci-store.**

#### Step 1: Verify Your Image Has a SOCI Index

Check if the image manifest has a SOCI index annotation:

```bash
# Using the soci CLI
sudo ./out/soci index list

# Or inspect the manifest directly
podman inspect localhost:5000/library/rabbitmq:latest | grep -i soci
```

If you don't see a SOCI index digest annotation (`com.amazon.soci.index-digest`), the image doesn't have a SOCI index.

#### Step 2: Create a SOCI Index

```bash
# For a local image
sudo ./out/soci create localhost:5000/library/rabbitmq:latest

# For a remote image
sudo ./out/soci create docker.io/library/nginx:latest
```

#### Step 3: Push the SOCI Index (if using a registry)

```bash
# Push to the same registry as the image
sudo ./out/soci push localhost:5000/library/rabbitmq:latest
```

#### Step 4: Verify the Index is Accessible

```bash
# Check the local artifact store
ls -la /var/lib/soci-store/content/

# Or verify in the registry
sudo ./out/soci index info localhost:5000/library/rabbitmq:latest
```

### Understanding the Error Flow

When soci-store receives a request for a layer:

1. **Checks for SOCI index**: Looks in the image manifest for `com.amazon.soci.index-digest` annotation
2. **If found**: Fetches the SOCI index and extracts the ztoc for the specific layer
3. **If NOT found**: Returns an error because lazy loading is not possible
4. **Container runtime**: Should fall back to downloading the layer normally

### Current Behavior vs Expected Behavior

**Current (as of now):**
- soci-store **only** works with SOCI-indexed images
- Non-SOCI images will fail with "invalid digest" or "no SOCI index" errors
- Container runtime should handle these layers, but Podman may not fall back gracefully yet

**Future Enhancement:**
- Could add fallback to download complete layers for non-SOCI images
- Would require implementing non-lazy loading code path

## Error: "failed to fetch SOCI index from artifact store"

### Symptoms
```
{"error":"failed to fetch SOCI index from artifact store: not found","level":"warning"}
```

### Root Cause
The SOCI index referenced in the manifest cannot be found in the artifact store.

### Solutions

#### Option 1: Local Artifact Store
Ensure the SOCI index is in the local content store:

```bash
# Create the index locally
sudo ./out/soci create localhost:5000/library/rabbitmq:latest

# The index will be stored in /var/lib/soci-store/content/
ls -la /var/lib/soci-store/content/blobs/sha256/
```

#### Option 2: Registry-based Artifact Store
If using containerd content store type, ensure the index is pushed to the registry:

```bash
sudo ./out/soci push localhost:5000/library/rabbitmq:latest
```

## Error: "manifest has no SOCI index digest annotation"

### Symptoms
Log message indicating no SOCI annotation found:
```json
{"level":"info","msg":"manifest has no SOCI index digest annotation","image":"..."}
```

### Cause
The image manifest doesn't have the required SOCI index annotation.

### Solution
Create and attach a SOCI index to the image:

```bash
# Create SOCI index
sudo ./out/soci create <image-name>

# This adds the annotation to the manifest
# Verify with:
podman inspect <image-name> | grep soci
```

## Error: "no ztoc found in SOCI index for this layer"

### Symptoms
```json
{"level":"debug","msg":"no ztoc found in SOCI index for this layer","layer":"sha256:..."}
```

### Cause
The SOCI index exists but doesn't contain a ztoc for the specific layer being requested.

### Common Reasons
1. The SOCI index was created for a different version of the image
2. The layer was added after the SOCI index was created
3. Some layers were skipped during SOCI index creation

### Solution
Recreate the SOCI index:

```bash
# Force recreate the index
sudo ./out/soci create --force <image-name>

# Verify all layers have ztocs
sudo ./out/soci index info <image-name>
```

## Best Practices for Using soci-store

### 1. Always Create SOCI Indexes First

**Before running containers:**
```bash
# Pull image
podman pull docker.io/library/nginx:latest

# Create SOCI index
sudo ./out/soci create docker.io/library/nginx:latest

# Then run
podman run docker.io/library/nginx:latest
```

### 2. Verify SOCI Index Creation

After creating an index:
```bash
sudo ./out/soci index list
sudo ./out/soci index info <image-name>
```

### 3. Use Debug Logging During Testing

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

This will show:
- Whether SOCI indexes are found
- Which layers are being lazy-loaded
- Artifact store operations
- FUSE filesystem operations

### 4. Check Artifact Store Permissions

```bash
# Ensure proper ownership
sudo chown -R root:root /var/lib/soci-store

# Check content store
ls -la /var/lib/soci-store/content/blobs/sha256/
```

### 5. Test with a Known SOCI-indexed Image

Start with a simple test image:

```bash
# Pull a small image
podman pull docker.io/library/alpine:latest

# Create SOCI index
sudo ./out/soci create docker.io/library/alpine:latest

# Verify index
sudo ./out/soci index info docker.io/library/alpine:latest

# Run with soci-store
podman run --rm docker.io/library/alpine:latest echo "Success!"
```

## Debugging Checklist

When encountering errors, check these in order:

- [ ] Is soci-store running?
  ```bash
  mount | grep soci-store
  ```

- [ ] Does the image have a SOCI index?
  ```bash
  sudo ./out/soci index list | grep <image-name>
  ```

- [ ] Is the SOCI index accessible?
  ```bash
  ls -la /var/lib/soci-store/content/blobs/sha256/
  ```

- [ ] Are Podman storage settings correct?
  ```bash
  grep -A2 additionallayerstores /etc/containers/storage.conf
  ```

- [ ] Is debug logging enabled?
  ```bash
  # Check soci-store logs
  sudo journalctl -f | grep soci-store
  ```

- [ ] Can you pull the image normally?
  ```bash
  podman pull <image-name>
  ```

## Advanced Debugging

### Enable Maximum Logging

Edit `/etc/soci-store/config.toml`:
```toml
[fuse]
log_fuse_operations = true
```

Run with debug:
```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

### Inspect SOCI Index Contents

```bash
# Get index digest from manifest
SOCI_INDEX=$(podman inspect <image> | jq -r '.Annotations["com.amazon.soci.index-digest"]')

# Inspect the index
sudo ./out/soci index info <image>
```

### Check Layer Mappings

```bash
# See what layers are in the image
podman inspect <image> | jq '.RootFS.Layers'

# See what ztocs are in the SOCI index
sudo ./out/soci index info <image> | grep -A10 "Blobs"
```

## Common Image Issues

### Multi-platform Images

SOCI indexes are platform-specific. Ensure you create the index for your platform:

```bash
# Check your platform
uname -m  # e.g., x86_64 or aarch64

# Create index for specific platform
sudo ./out/soci create --platform linux/amd64 <image>
```

### Base Layer Issues

Some base layers might not have SOCI indexes:

```bash
# Check each layer
podman inspect <image> | jq -r '.RootFS.Layers[]'

# Create indexes for base images too
sudo ./out/soci create <base-image>
```

## Getting Help

If you're still experiencing issues:

1. **Collect logs:**
   ```bash
   sudo ./out/soci-store --log-level debug /var/lib/soci-store 2>&1 | tee soci-store.log
   ```

2. **Gather diagnostic info:**
   ```bash
   # System info
   uname -a
   podman --version

   # SOCI info
   sudo ./out/soci index list
   ls -la /var/lib/soci-store/

   # Mount info
   mount | grep soci
   ```

3. **Check for known issues:**
   - GitHub: https://github.com/awslabs/soci-snapshotter/issues
   - Look for similar error messages

## Summary

**Key Takeaway:** soci-store is designed for SOCI-indexed images only. Always create SOCI indexes before using images with soci-store. Non-indexed images will fail with "invalid digest" errors because lazy loading requires the ztoc information that only SOCI indexes provide.

**Quick Fix for "invalid digest" error:**
```bash
sudo ./out/soci create <your-image-name>
```
