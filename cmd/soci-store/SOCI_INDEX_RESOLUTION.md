# SOCI Index Resolution Implementation

## Overview

This document explains the SOCI index resolution strategy implemented in `store/manager.go`, which is modeled after the approach used in `fs/fs.go` but adapted for the additional layer store architecture.

## Resolution Strategy

The `getSociDescriptor()` function now uses a **multi-step resolution strategy** to find SOCI indexes:

### Step 1: Layer Descriptor Annotations (Explicit Digest)

```go
if layerDesc.Annotations != nil {
    if explicitDigest, ok := layerDesc.Annotations[soci.ImageAnnotationSociIndexDigest]; ok {
        sociIndexDigestStr = explicitDigest
        // Use this digest
    }
}
```

**When this is used:**
- Containerd/Podman passes the SOCI index digest in the layer descriptor
- Happens when the container runtime is aware of the SOCI index
- Most direct method - highest priority

**Example:**
```go
layerDesc.Annotations["com.amazon.soci.index-digest"] = "sha256:abc123..."
```

### Step 2: Manifest Annotations (SOCI v2)

```go
if sociIndexDigestStr == "" {
    manifest, _, err := r.refPool.loadRef(ctx, refspec)
    if manifest.Annotations != nil {
        if manifestDigest, ok := manifest.Annotations[soci.ImageAnnotationSociIndexDigest]; ok {
            sociIndexDigestStr = manifestDigest
            // Use this digest
        }
    }
}
```

**When this is used:**
- SOCI index digest is stored in the image manifest annotations
- Standard approach for SOCI v2
- Created when running `soci create <image>`

**Example:**
```json
{
  "annotations": {
    "com.amazon.soci.index-digest": "sha256:abc123..."
  }
}
```

### Step 3: Fetch from Artifact Store

```go
sociIndex, err := r.getSociIndex(ctx, sociIndexDigest)
```

**What happens:**
- Fetches the SOCI index from local artifact store
- Uses caching to avoid repeated fetches
- Index must exist at `/var/lib/soci-store/content/blobs/sha256/<digest>`

### Step 4: Find Layer-Specific ztoc

```go
for _, blob := range sociIndex.Blobs {
    if blob.MediaType == soci.SociLayerMediaType {
        if blob.Annotations[soci.IndexAnnotationImageLayerDigest] == layerDigest {
            return blob  // Found ztoc descriptor
        }
    }
}
```

**What happens:**
- Searches through SOCI index blobs
- Looks for ztoc with matching layer digest annotation
- Filters by media type (only ztoc, not prefetch artifacts)

## Comparison with fs.go

### Similarities

| Aspect | fs.go | manager.go |
|--------|-------|------------|
| **Step 1** | Explicit digest from snapshot labels | Layer descriptor annotations |
| **Step 2** | Manifest annotations (v2) | Manifest annotations (v2) |
| **Step 3** | Fetch from content store | Fetch from artifact store |
| **Caching** | Per-image via sync.Once | Per-index via RWMutex |
| **Blob search** | Find ztoc for layer | Find ztoc for layer |

### Differences

| Aspect | fs.go | manager.go |
|--------|-------|------------|
| **Referrers API** | ✅ Supports SOCI v1 | ❌ Not supported |
| **Remote access** | ✅ Can fetch from registry | ❌ Local only |
| **HTTP client** | ✅ Has full HTTP client | ❌ No network access |
| **Discovery** | 3 methods (v1/v2/explicit) | 2 methods (v2/explicit) |
| **Fallback** | Multiple discovery methods | Must have local index |

### Why No Referrers API Support?

The additional layer store architecture has fundamental limitations:

1. **No image context**: Store only receives layer digest, not image reference
2. **No network access**: Cannot make HTTP requests to registry
3. **Reactive mode**: Can only serve what's already local
4. **No ORAS client**: Would require full remote store setup

**Consequence**: SOCI indexes must be created locally with `soci create` command.

## Implementation Details

### Source Detection Logging

The implementation includes detailed logging to help understand where the SOCI index was found:

```go
// Layer annotation
log.G(ctx).WithFields(map[string]interface{}{
    "layer":      layerDesc.Digest.String(),
    "soci_index": sociIndexDigestStr,
    "source":     "layer_annotation",  // ← Shows source
}).Debug("found explicit SOCI index digest in layer annotations")

// Manifest annotation
log.G(ctx).WithFields(map[string]interface{}{
    "image":      refspec.String(),
    "soci_index": sociIndexDigestStr,
    "source":     "manifest_annotation",  // ← Shows source
}).Debug("found SOCI index digest in manifest annotations")
```

**Debug output example:**
```json
{
  "level": "debug",
  "msg": "found SOCI index digest in manifest annotations",
  "image": "localhost:5000/library/rabbitmq:latest",
  "soci_index": "sha256:abc123...",
  "source": "manifest_annotation"
}
```

### Media Type Filtering

Unlike the previous implementation, we now filter by media type:

```go
for _, blob := range sociIndex.Blobs {
    // Only look at ztoc blobs (skip prefetch artifacts, etc.)
    if blob.MediaType != soci.SociLayerMediaType {
        continue  // Skip non-ztoc blobs
    }
    // ... check annotations
}
```

**Why this matters:**
- SOCI indexes can contain multiple blob types:
  - `application/vnd.oci.image.layer.v1.tar+gzip+soci.ztoc` - ztoc
  - `application/vnd.oci.image.layer.v1.tar+gzip+soci.prefetch` - prefetch artifact
- We only want ztocs for layer resolution
- Prevents confusion from other artifact types

### Enhanced Error Messages

Better hints when SOCI index is not found:

```go
if err != nil {
    log.G(ctx).WithError(err).WithField("soci_index", sociIndexDigestStr).
        Warn("failed to fetch SOCI index from artifact store")
    log.G(ctx).Warn("hint: ensure SOCI index was created with 'soci create' command")
    return ocispec.Descriptor{}
}
```

**User sees:**
```json
{
  "level": "warn",
  "msg": "failed to fetch SOCI index from artifact store",
  "soci_index": "sha256:abc123...",
  "error": "blob not found"
}
{
  "level": "warn",
  "msg": "hint: ensure SOCI index was created with 'soci create' command"
}
```

## How It Works End-to-End

### Scenario 1: Image Created with `soci create`

```bash
# User runs
sudo soci create localhost:5000/library/rabbitmq:latest
```

**What happens:**
1. SOCI CLI creates ztocs for each layer
2. Stores them in local artifact store (`/var/lib/soci-store/content/`)
3. Adds annotation to manifest: `com.amazon.soci.index-digest: sha256:abc123...`
4. SOCI index stored at: `/var/lib/soci-store/content/blobs/sha256/abc123...`

**When container runs:**
1. Podman requests layer from soci-store
2. `getSociDescriptor()` called with layer descriptor
3. Checks layer annotations - empty (Step 1 skipped)
4. Loads manifest - finds `com.amazon.soci.index-digest` (Step 2 ✓)
5. Fetches SOCI index from artifact store (Step 3 ✓)
6. Finds ztoc for this layer in index (Step 4 ✓)
7. Returns ztoc descriptor
8. Lazy loading enabled! 🎉

### Scenario 2: Layer Descriptor Has Explicit Digest

```bash
# Containerd snapshotter passes explicit digest in layer request
```

**What happens:**
1. Layer descriptor includes annotation: `com.amazon.soci.index-digest: sha256:xyz789...`
2. `getSociDescriptor()` called
3. Checks layer annotations - found! (Step 1 ✓)
4. Skips manifest check (Step 2 skipped - already have digest)
5. Fetches SOCI index (Step 3 ✓)
6. Finds ztoc (Step 4 ✓)
7. Returns ztoc descriptor
8. Lazy loading enabled! 🎉

### Scenario 3: No SOCI Index

```bash
# User forgets to run soci create
podman run localhost:5000/library/nginx:latest
```

**What happens:**
1. `getSociDescriptor()` called
2. Checks layer annotations - empty (Step 1 skipped)
3. Loads manifest - no `com.amazon.soci.index-digest` annotation (Step 2 failed)
4. Logs: "no SOCI index digest found"
5. Returns empty descriptor
6. Validation check catches it
7. Returns error: "layer has no SOCI index; lazy loading not available"
8. Container runtime should handle layer (but may fail) ❌

## Testing the Implementation

### Test 1: Verify Resolution Source

Enable debug logging and check which method found the index:

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store 2>&1 | grep -i "source"
```

Expected output:
```json
{"source":"manifest_annotation","msg":"found SOCI index digest..."}
```

### Test 2: Test Layer Annotation Path

Manually add annotation to layer descriptor (advanced):

```go
layerDesc.Annotations = map[string]string{
    soci.ImageAnnotationSociIndexDigest: "sha256:abc123...",
}
```

Should see:
```json
{"source":"layer_annotation","msg":"found explicit SOCI index digest..."}
```

### Test 3: Test Fallback Behavior

Create image without SOCI index:

```bash
podman pull docker.io/library/alpine:latest
# Don't run soci create
podman run docker.io/library/alpine:latest
```

Should see:
```json
{"msg":"no SOCI index digest found (checked layer and manifest annotations)"}
{"msg":"no SOCI index found for layer, cannot provide lazy loading"}
```

## Benefits of This Implementation

### 1. **Flexibility**
- Supports multiple discovery methods
- Can work with different container runtimes
- Gracefully handles missing indexes

### 2. **Better Debugging**
- Source detection shows where index was found
- Clear error messages with hints
- Detailed logging for troubleshooting

### 3. **Future-Proof**
- Easy to add more resolution methods
- Prepared for Podman/CRI-O enhancements
- Aligns with fs.go patterns

### 4. **Performance**
- Checks layer annotations first (fastest)
- Only loads manifest if needed
- Caches fetched indexes
- Filters by media type early

## Limitations

### Cannot Support (Architecture Constraints)

1. **Referrers API** - No network access
2. **Remote fetching** - Local artifact store only
3. **Auto-discovery** - Cannot search registry
4. **OCI Distribution** - No ORAS client

### Requires Manual Steps

Users must:
1. Pull image
2. **Run `soci create`** ← Required!
3. Run container

## Code Location

**File**: `store/manager.go`

**Function**: `getSociDescriptor()` - Lines 294-408

**Key Changes**:
- Added layer descriptor annotation check (Step 1)
- Enhanced manifest annotation check with logging (Step 2)
- Added media type filtering in blob search
- Improved error messages with hints
- Source detection logging

## Summary

The enhanced SOCI index resolution provides:

✅ **Multi-method discovery** - Layer annotations → Manifest annotations
✅ **Source tracking** - Know where the index came from
✅ **Better errors** - Clear hints when index is missing
✅ **Media type filtering** - Only process ztocs
✅ **Aligned with fs.go** - Similar resolution strategy

**Key Requirement**: SOCI indexes must be created locally with `soci create` because store mode cannot access remote registries.
