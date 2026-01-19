# Comparison: fs.go vs manager.go SOCI Index Handling

This document compares how `fs/fs.go` (snapshotter) and `store/manager.go` (additional layer store) handle SOCI index fetching and layer resolution.

## Architecture Differences

### fs.go (Snapshotter Mode)
- **Purpose**: Full containerd snapshotter integration
- **Control**: Full control over image pulling and unpacking
- **SOCI Index**: Fetched during image preparation phase (before layer mounting)
- **Storage**: Uses containerd's content store
- **Lifecycle**: Manages the entire snapshot lifecycle

### manager.go (Store Mode)
- **Purpose**: Additional layer store for Podman/CRI-O
- **Control**: Reactive - only serves layers when requested
- **SOCI Index**: Must be fetched on-demand per layer request
- **Storage**: Uses local SOCI content store
- **Lifecycle**: Only provides layer data when asked

## SOCI Index Fetching Comparison

### fs.go Approach

#### 1. **Initialization Phase** (sociContext.Init)
```go
func (c *sociContext) Init(ctx, fs, imageRef, indexDigest, imageManifestDigest, client) {
    c.fetchOnce.Do(func() {
        // Fetch SOCI index once per image
        index, err := fs.fetchSociIndex(ctx, imageRef, indexDigest, imageManifestDigest, client)
        c.sociIndex = index

        // Pre-build mapping of layer digest -> ztoc descriptor
        c.populateImageLayerToSociMapping(index)
    })
}
```

**Key Points:**
- Fetches SOCI index **once per image** during initialization
- Uses `sync.Once` to ensure single fetch even with concurrent requests
- Pre-builds a map: `imageLayerToSociDesc map[string]ocispec.Descriptor`
- Has full HTTP client and remote store access
- Can search for SOCI index in multiple ways (v1 referrers, v2 annotations)

#### 2. **Index Discovery** (findSociIndexDesc)
```go
func (fs *filesystem) findSociIndexDesc(ctx, imageManifestDigest, sociIndexDigest, remoteStore) {
    // 1. Check explicit index digest (from snapshot labels)
    if sociIndexDigest != "" {
        return parseIndexDigest(sociIndexDigest)
    }

    // 2. Check manifest annotations (SOCI v2)
    if fs.pullModes.SOCIv2.Enable {
        desc, err := findSociIndexDescAnnotation(ctx, imgDigest, remoteStore)
        if err == nil {
            return desc
        }
    }

    // 3. Check referrers API (SOCI v1)
    if fs.pullModes.SOCIv1.Enable {
        desc, err := findSociIndexDescReferrer(ctx, imgDigest, remoteStore)
        if err == nil {
            return desc
        }
    }

    return errdefs.ErrNotFound
}
```

**Discovery Methods:**
1. Explicit digest from snapshot labels
2. Manifest annotations (`com.amazon.soci.index-digest`)
3. OCI Referrers API

#### 3. **Layer Resolution** (resolve layer)
```go
// Lookup is simple - already have the mapping
sociDesc, ok := c.imageLayerToSociDesc[s.Target.Digest.String()]
if !ok {
    // No ztoc for this layer - cannot mount
    return snapshot.ErrNoZtoc
}

// Pass to resolver
l, err := fs.resolver.Resolve(ctx, s.Hosts, s.Name, s.Target, sociDesc, ...)
```

**Key Points:**
- Simple O(1) map lookup
- Fails fast if no ztoc available
- Never calls Resolve without a valid sociDesc

---

### manager.go Approach (Current Implementation)

#### 1. **On-Demand Fetching** (getSociDescriptor)
```go
func (r *LayerManager) getSociDescriptor(ctx, refspec, layerDesc) ocispec.Descriptor {
    // Step 1: Load manifest to get SOCI index digest
    manifest, _, err := r.refPool.loadRef(ctx, refspec)

    // Step 2: Check for SOCI index annotation in manifest
    sociIndexDigestStr, ok := manifest.Annotations[soci.ImageAnnotationSociIndexDigest]
    if !ok {
        return ocispec.Descriptor{} // No SOCI index
    }

    // Step 3: Fetch and cache the SOCI index
    sociIndex, err := r.getSociIndex(ctx, sociIndexDigest)

    // Step 4: Find ztoc for this specific layer
    for _, blob := range sociIndex.Blobs {
        if blob.Annotations[soci.IndexAnnotationImageLayerDigest] == layerDesc.Digest.String() {
            return blob // Found ztoc descriptor
        }
    }

    return ocispec.Descriptor{} // No ztoc for this layer
}
```

**Key Points:**
- Fetches on-demand when layer is requested
- Must load manifest every time (cached in refPool)
- Implements its own caching for SOCI indexes
- Only supports manifest annotations (SOCI v2)
- Searches through blobs to find matching ztoc

#### 2. **Index Caching** (getSociIndex)
```go
func (r *LayerManager) getSociIndex(ctx, indexDigest) (*soci.Index, error) {
    // Check cache first
    r.indexCacheMu.RLock()
    if cached, ok := r.sociIndexCache[indexDigestStr]; ok {
        return cached, nil
    }
    r.indexCacheMu.RUnlock()

    // Fetch from artifact store
    indexReader, err := r.artifactStore.Fetch(ctx, indexDesc)
    indexBytes, err := io.ReadAll(indexReader)

    // Unmarshal
    var sociIndex soci.Index
    err := soci.UnmarshalIndex(indexBytes, &sociIndex)

    // Cache it
    r.sociIndexCache[indexDigestStr] = &sociIndex

    return &sociIndex, nil
}
```

**Key Points:**
- Implements its own in-memory cache
- Fetches from local artifact store (not remote)
- No sync.Once - relies on RWMutex for concurrency
- Simple fetch + unmarshal approach

#### 3. **Layer Resolution** (resolveLayer)
```go
// Get SOCI descriptor for this layer
sociDesc := r.getSociDescriptor(ctx, refspec, target)

// Check if valid before calling Resolve
if sociDesc.Digest == "" {
    // Cannot provide lazy loading - return error
    return nil, fmt.Errorf("layer has no SOCI index; lazy loading not available")
}

// Pass to resolver
l, err := r.resolver.Resolve(ctx, registryHosts, refspec, target, sociDesc, nil, ...)
```

**Key Points:**
- Must check if descriptor is valid
- Returns error if no SOCI index (cannot fall back to full download)
- Similar to fs.go, never calls Resolve with empty descriptor

---

## Key Differences Summary

| Aspect | fs.go (Snapshotter) | manager.go (Store) |
|--------|--------------------|--------------------|
| **When fetched** | During image preparation (once) | On-demand per layer request |
| **Fetching strategy** | Proactive, bulk operation | Reactive, per-layer |
| **Caching** | Pre-built map at init | On-demand cache with RWMutex |
| **Index discovery** | 3 methods (v1/v2/explicit) | 1 method (manifest annotations only) |
| **Remote access** | Full HTTP client + remote store | Local artifact store only |
| **Concurrency** | sync.Once per image | RWMutex per index |
| **Fallback** | Returns ErrNoZtoc | Returns error (no fallback) |
| **Performance** | O(1) map lookup | O(n) blob search per layer |

## Critical Differences Impacting Functionality

### 1. **SOCI Index Discovery**

**fs.go** has multiple discovery mechanisms:
- Explicit digest from snapshot preparation labels
- Manifest annotations (v2)
- OCI Referrers API (v1)

**manager.go** only checks:
- Manifest annotations (v2 only)

**Impact**: manager.go cannot find SOCI indexes that use v1 (referrers) format or those passed via explicit digest.

### 2. **Artifact Storage**

**fs.go**:
- Uses containerd's content store
- Can fetch from remote registries
- Has full ORAS remote store support

**manager.go**:
- Uses local SOCI content store only
- Cannot fetch from remote (assumes artifacts are local)
- SOCI indexes must be pre-populated via `soci create` command

**Impact**: manager.go requires manual `soci create` before images can use lazy loading.

### 3. **Error Handling**

**fs.go**:
- Returns specific error types (`snapshot.ErrNoZtoc`)
- Snapshotter can handle the error and fall back to normal pulling

**manager.go**:
- Returns generic errors
- Container runtime may not handle gracefully
- No fallback mechanism currently

**Impact**: Non-SOCI images will fail in store mode.

## Why manager.go Cannot Auto-Fetch from Remote

The additional layer store protocol is **reactive**:

1. Podman/CRI-O requests a layer by digest
2. Store must provide it or return error
3. No image metadata or manifest is provided in the request

This means:
- manager.go doesn't know the image name/reference
- Cannot determine registry to fetch from
- Cannot use ORAS or other remote protocols
- Must rely on pre-populated local artifact store

**Solution**: Users must run `soci create <image>` before using the image.

## Recommendations for Improvement

### Short-term (Current Architecture)

1. **Better error messages**:
   ```go
   return nil, fmt.Errorf("layer %s requires SOCI index; run 'soci create %s' first",
       target.Digest, refspec.String())
   ```

2. **Add v1 referrers support** (if possible with store architecture)

3. **Improve caching**:
   - Use sync.Map instead of manual RWMutex
   - Add TTL or size limits to cache

### Long-term (Architecture Changes)

1. **Pre-fetch on store startup**:
   - Scan local images
   - Pre-populate SOCI index mappings
   - Similar to fs.go's Init phase

2. **Background SOCI index sync**:
   - Watch for new images
   - Automatically create SOCI indexes
   - Keep artifact store in sync

3. **Fallback to full download**:
   - Implement non-lazy layer serving
   - Download complete layer when no SOCI index
   - Requires significant code changes

## Testing Recommendations

### For Current Implementation

1. **Always create SOCI index first**:
   ```bash
   podman pull <image>
   sudo soci create <image>
   podman run <image>  # Now works with lazy loading
   ```

2. **Verify SOCI index exists**:
   ```bash
   sudo soci index list
   sudo soci index info <image>
   ```

3. **Check artifact store**:
   ```bash
   ls -la /var/lib/soci-store/content/blobs/sha256/
   ```

4. **Enable debug logging**:
   ```bash
   sudo soci-store --log-level debug /var/lib/soci-store
   ```

## Conclusion

The main architectural difference is:

- **fs.go**: Proactive, image-level initialization with full remote access
- **manager.go**: Reactive, layer-level on-demand with local-only access

This fundamental difference means **manager.go requires manual SOCI index creation** before images can benefit from lazy loading. This is not a bug but a consequence of the additional layer store architecture, which operates at the layer level without image-level context.
