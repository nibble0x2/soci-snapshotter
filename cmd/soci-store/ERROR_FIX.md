# Fix for "invalid digest" Error

## The Error You Encountered

```json
{
  "error": "failed to resolve layer: failed to resolve layer \"localhost:5000/library/rabbitmq:latest\" / \"sha256:b605fd694c0f9e1210db78973a942ba6cd044602c15f6aa9b7f3699b82ae2c29\": : : invalid digest",
  "level": "warning",
  "msg": "failed to mount layer \"diff\": \"sha256:b605fd694c0f9e1210db78973a942ba6cd044602c15f6aa9b7f3699b82ae2c29\""
}
```

## Root Cause Analysis

### What Was Happening

1. **Podman requested a layer** from soci-store
2. **manager.go checked for SOCI index** in the manifest
3. **No SOCI index found** (image doesn't have one)
4. **Returned empty descriptor**: `ocispec.Descriptor{}`
5. **Passed empty descriptor to resolver**: `Resolve(..., sociDesc, ...)`
6. **Resolver tried to fetch ztoc**: `artifactStore.Fetch(ctx, sociDesc)`
7. **Empty digest caused error**: "invalid digest"

### The Problem

The layer resolver (`fs/layer/layer.go`) **always expects a valid SOCI descriptor**. It immediately tries to fetch the ztoc:

```go
// fs/layer/layer.go line 300
ztocReader, err := r.artifactStore.Fetch(ctx, sociDesc)
if err != nil {
    return nil, err  // "invalid digest" if sociDesc is empty
}
```

When we passed an empty descriptor (with `Digest == ""`), the artifact store fetch failed with "invalid digest".

## The Fix

### Before (Broken Code)

```go
// store/manager.go - BEFORE
func (r *LayerManager) resolveLayer(ctx, refspec, target) {
    // Get SOCI descriptor (might be empty)
    sociDesc := r.getSociDescriptor(ctx, refspec, target)

    // Always call Resolve, even with empty descriptor ❌
    l, err := r.resolver.Resolve(ctx, registryHosts, refspec, target, sociDesc, ...)
    // If sociDesc is empty, this causes "invalid digest" error
}
```

### After (Fixed Code)

```go
// store/manager.go - AFTER
func (r *LayerManager) resolveLayer(ctx, refspec, target) {
    // Get SOCI descriptor
    sociDesc := r.getSociDescriptor(ctx, refspec, target)

    // ✅ Check if descriptor is valid BEFORE calling Resolve
    if sociDesc.Digest == "" {
        // No SOCI index - return error to let container runtime handle it
        log.G(ctx).WithField("layer", target.Digest).Warn(
            "no SOCI index found for layer, cannot provide lazy loading - container runtime must handle this layer")
        return nil, fmt.Errorf("layer %s has no SOCI index; lazy loading not available", target.Digest)
    }

    log.G(ctx).WithFields(map[string]interface{}{
        "layer": target.Digest,
        "ztoc":  sociDesc.Digest,
    }).Info("found SOCI index for layer, enabling lazy loading")

    // Only call Resolve with valid descriptor ✅
    l, err := r.resolver.Resolve(ctx, registryHosts, refspec, target, sociDesc, ...)
}
```

### Changes Made

**File**: `store/manager.go`

**Lines 261-273**: Added validation check before calling Resolve:

```go
// Check if we have a valid SOCI descriptor
// The layer resolver requires a valid SOCI descriptor to enable lazy loading
if sociDesc.Digest == "" {
    // No SOCI index found - cannot use lazy loading
    // Return an error to let the container runtime handle the layer download
    log.G(ctx).WithField("layer", target.Digest).Warn("no SOCI index found for layer, cannot provide lazy loading - container runtime must handle this layer")
    return nil, fmt.Errorf("layer %s has no SOCI index; lazy loading not available", target.Digest)
}
```

## How This Matches fs.go Behavior

The fix aligns with how the snapshotter (`fs/fs.go`) handles this:

```go
// fs/fs.go - How snapshotter does it
sociDesc, ok := c.imageLayerToSociDesc[s.Target.Digest.String()]
if !ok {
    // No ztoc available - return error, don't call Resolve
    log.G(ctx).Infof("skipping mounting layer as FUSE mount: %v", snapshot.ErrNoZtoc)
    return snapshot.ErrNoZtoc  // ❌ Never calls Resolve without valid descriptor
}

// Only call Resolve if we have a valid descriptor ✅
l, err := fs.resolver.Resolve(ctx, s.Hosts, s.Name, s.Target, sociDesc, ...)
```

## Why This Error Occurs

### The Core Issue: soci-store Requires SOCI Indexes

**soci-store is designed ONLY for SOCI-indexed images**. It cannot serve regular (non-SOCI) layers because:

1. The layer resolver needs a ztoc to enable lazy loading
2. Without a ztoc, it cannot determine which file ranges to fetch on-demand
3. There's no fallback code path to download complete layers

### Your rabbitmq Image Didn't Have a SOCI Index

The error occurred because:
- You tried to use `localhost:5000/library/rabbitmq:latest`
- This image doesn't have a SOCI index
- manager.go returned empty descriptor
- Resolver tried to fetch ztoc with empty digest
- Result: "invalid digest" error

## Solution: Create SOCI Index First

### Step 1: Create the SOCI Index

```bash
# Create SOCI index for your image
sudo ./out/soci create localhost:5000/library/rabbitmq:latest
```

This command:
- Analyzes all layers in the image
- Creates ztocs (zstd Table of Contents) for each layer
- Stores them in the artifact store (`/var/lib/soci-store/content/`)
- Adds SOCI index annotation to the manifest

### Step 2: Verify the Index

```bash
# List all SOCI indexes
sudo ./out/soci index list

# Get detailed info about your image's index
sudo ./out/soci index info localhost:5000/library/rabbitmq:latest
```

Expected output:
```
Index Digest: sha256:abc123...
Artifact Type: application/vnd.oci.image.index.v1+json
Created: 2026-01-19T00:00:00Z

Blobs:
  - Digest: sha256:def456...
    Media Type: application/vnd.oci.image.layer.v1.tar+gzip+soci.ztoc
    Size: 12345
    Annotations:
      com.amazon.soci.image-layer-digest: sha256:b605fd694c0f9e1210db78973a942ba6cd044602c15f6aa9b7f3699b82ae2c29
```

### Step 3: Run Your Container

```bash
# Now it will work with lazy loading
podman run localhost:5000/library/rabbitmq:latest
```

## New Behavior After Fix

### With SOCI Index

```
✅ Layer has SOCI index
   ├─ getSociDescriptor() finds ztoc
   ├─ Returns valid descriptor with ztoc digest
   ├─ Passes to Resolve()
   └─ Lazy loading works!
```

Log output:
```json
{
  "level": "info",
  "msg": "found SOCI index for layer, enabling lazy loading",
  "layer": "sha256:b605fd...",
  "ztoc": "sha256:def456..."
}
```

### Without SOCI Index (After Fix)

```
❌ Layer has NO SOCI index
   ├─ getSociDescriptor() finds no ztoc
   ├─ Returns empty descriptor
   ├─ Validation check catches it
   ├─ Returns error WITHOUT calling Resolve()
   └─ Container runtime should handle layer
      (but Podman may not fallback gracefully yet)
```

Log output:
```json
{
  "level": "warn",
  "msg": "no SOCI index found for layer, cannot provide lazy loading - container runtime must handle this layer",
  "layer": "sha256:b605fd..."
}
{
  "level": "error",
  "error": "layer sha256:b605fd... has no SOCI index; lazy loading not available"
}
```

## Testing the Fix

### Test 1: Image Without SOCI Index (Should Fail Gracefully)

```bash
# Pull image without creating SOCI index
podman pull docker.io/library/alpine:latest

# Try to run (will fail with clear error message)
podman run docker.io/library/alpine:latest

# Expected: Clear error message about missing SOCI index
# NOT: "invalid digest" error
```

### Test 2: Image With SOCI Index (Should Work)

```bash
# Pull image
podman pull docker.io/library/alpine:latest

# Create SOCI index
sudo ./out/soci create docker.io/library/alpine:latest

# Run (should work with lazy loading)
podman run docker.io/library/alpine:latest echo "Success!"

# Expected: Container runs successfully with lazy-loaded layers
```

### Test 3: Verify Logs

With debug logging enabled:

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

You should see one of these patterns:

**Pattern A: SOCI Index Found**
```json
{"level":"debug","msg":"fetching SOCI index from artifact store","index":"sha256:..."}
{"level":"debug","msg":"found ztoc for layer","layer":"sha256:...","ztoc":"sha256:..."}
{"level":"info","msg":"found SOCI index for layer, enabling lazy loading"}
```

**Pattern B: No SOCI Index**
```json
{"level":"info","msg":"manifest has no SOCI index digest annotation","image":"..."}
{"level":"warn","msg":"no SOCI index found for layer, cannot provide lazy loading"}
```

## Summary

### What Was Fixed
- ✅ Prevented "invalid digest" error by validating descriptor before calling Resolve
- ✅ Added clear error message explaining SOCI index is required
- ✅ Aligned behavior with how fs.go handles missing SOCI indexes
- ✅ Improved logging to help diagnose issues

### What Still Needs Work
- ⚠️ Podman may not gracefully fallback when soci-store returns an error
- ⚠️ Users must manually create SOCI indexes (no auto-creation)
- ⚠️ No support for mixed images (some layers with SOCI, some without)

### Required Workflow
1. Pull image → 2. Create SOCI index → 3. Run container

**You must always run `soci create` before using an image with soci-store.**

## Files Modified

- `store/manager.go`: Lines 261-273 (added validation check)
- `store/manager.go`: Lines 306, 312, 318 (improved logging)

## Build and Test

```bash
# Rebuild
make soci-store

# Test with your image
sudo ./out/soci create localhost:5000/library/rabbitmq:latest
podman run localhost:5000/library/rabbitmq:latest
```
