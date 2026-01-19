# Referrers API Implementation for SOCI Index Discovery

## Overview

This document describes the implementation of OCI Referrers API support in soci-store for discovering SOCI indexes (SOCI v1 format).

## What is the Referrers API?

The [OCI Referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers) is part of the OCI Distribution Specification that allows discovering artifacts that reference a specific manifest. SOCI uses this to attach SOCI index artifacts to container images.

### SOCI v1 vs v2

- **SOCI v1**: Uses the OCI Referrers API to link SOCI indexes to images
  - SOCI index is stored as a separate artifact in the registry
  - Referenced via the referrers API using the manifest digest
  - More standards-compliant but requires registry support

- **SOCI v2**: Uses manifest annotations to store SOCI index digest
  - SOCI index digest stored in `com.amazon.soci.index-digest` annotation
  - Works with any registry (no special API support needed)
  - Simpler but modifies the image manifest

## Implementation Details

### Three-Step Resolution Strategy

The `getSociDescriptor()` function now implements the same three-step resolution as `fs.go`:

```go
// Step 1: Check layer descriptor annotations for explicit SOCI index digest
// Step 2: Check manifest annotations for SOCI index digest (SOCI v2)
// Step 3: Try to query the OCI referrers API for SOCI artifacts (SOCI v1)
```

### Key Functions

#### 1. `newRemoteStore(refspec, client)`

Creates an ORAS remote repository for accessing the registry:

```go
func newRemoteStore(refspec reference.Spec, client *http.Client) (*remote.Repository, error)
```

- Takes the image reference specification and HTTP client
- Creates ORAS repository for the registry
- Handles localhost detection for plain HTTP
- Returns remote repository for referrers queries

**Location**: [store/manager.go:472-486](store/manager.go#L472-L486)

#### 2. `findSociIndexDescReferrer(ctx, imgDigest, remoteStore)`

Queries the referrers API to find SOCI index artifacts:

```go
func findSociIndexDescReferrer(ctx, imgDigest, remoteStore) (ocispec.Descriptor, error)
```

- Creates OCIArtifactClient from remote store
- Queries referrers for the manifest digest
- Filters for SOCI index artifact type
- Returns first matching SOCI index descriptor

**Location**: [store/manager.go:488-498](store/manager.go#L488-L498)

#### 3. Enhanced `getSociDescriptor()`

Updated to include referrers API lookup:

**Location**: [store/manager.go:310-460](store/manager.go#L310-L460)

**Step 3 Implementation** (lines 351-398):

1. Checks if SOCI index digest found in Steps 1-2
2. If not found, attempts referrers API query:
   - Gets manifest digest from image config
   - Obtains HTTP client from registry hosts
   - Creates remote store for registry access
   - Queries referrers API for SOCI artifacts
3. Logs discovery source for debugging

## Code Flow

```
Container Runtime requests layer
    ↓
LayerManager.resolveLayer()
    ↓
getSociDescriptor(refspec, layerDesc)
    ↓
┌─────────────────────────────────────────┐
│ Step 1: Layer Annotations               │
│   layerDesc.Annotations[index-digest]   │
│   Source: "layer_annotation"            │
└─────────────────────────────────────────┘
    ↓ (if not found)
┌─────────────────────────────────────────┐
│ Step 2: Manifest Annotations (v2)       │
│   manifest.Annotations[index-digest]    │
│   Source: "manifest_annotation"         │
└─────────────────────────────────────────┘
    ↓ (if not found)
┌─────────────────────────────────────────┐
│ Step 3: Referrers API (v1)              │
│   GET /v2/.../referrers/<digest>        │
│   Filter: artifactType = soci-index     │
│   Source: "referrers_api"               │
└─────────────────────────────────────────┘
    ↓ (found SOCI index digest)
┌─────────────────────────────────────────┐
│ Step 4: Parse digest                    │
└─────────────────────────────────────────┘
    ↓
┌─────────────────────────────────────────┐
│ Step 5: Fetch from artifact store       │
│   getSociIndex(indexDigest)             │
└─────────────────────────────────────────┘
    ↓
┌─────────────────────────────────────────┐
│ Step 6: Find ztoc for layer             │
│   Match layer digest in index blobs     │
│   Filter: mediaType = ztoc              │
└─────────────────────────────────────────┘
    ↓
Return ztoc descriptor for lazy loading
```

## Registry Requirements

### For Referrers API (SOCI v1) Support

The registry must support the OCI Distribution Specification referrers API:

- **Supported registries**:
  - Docker Hub (as of 2023)
  - GitHub Container Registry (ghcr.io)
  - Azure Container Registry
  - Google Artifact Registry
  - Harbor v2.5+
  - distribution/distribution v3.0+

- **Not supported**:
  - Older registry implementations
  - Some cloud registries (check vendor documentation)

### Testing Registry Support

```bash
# Test if registry supports referrers API
curl -v https://registry.example.com/v2/<repo>/referrers/<manifest-digest>

# Expected: HTTP 200 with JSON response
# If unsupported: HTTP 404 or 501
```

## How to Use Different Discovery Methods

### Method 1: Explicit Layer Annotation (Highest Priority)

Pass SOCI index digest in layer descriptor annotations:

```go
layerDesc := ocispec.Descriptor{
    Digest: layerDigest,
    Annotations: map[string]string{
        "com.amazon.soci.index-digest": "sha256:abc123...",
    },
}
```

**Use case**: When containerd/Podman already knows the SOCI index

### Method 2: Manifest Annotation (SOCI v2)

Create image with SOCI index digest in manifest:

```bash
# This is done automatically by `soci create`
soci create docker.io/library/alpine:latest
```

Creates manifest with:
```json
{
  "annotations": {
    "com.amazon.soci.index-digest": "sha256:abc123..."
  }
}
```

**Use case**: Standard SOCI workflow with manifest annotations

### Method 3: Referrers API (SOCI v1)

Push SOCI index as referrer artifact:

```bash
# Using ORAS to attach SOCI index as referrer
oras attach docker.io/library/alpine:latest \
  --artifact-type application/vnd.amazon.soci.index.v1+json \
  ./soci-index.json
```

**Use case**: Standards-compliant registries, immutable image manifests

## Logging and Debugging

Enable debug logging to see which discovery method was used:

```bash
sudo ./out/soci-store --log-level debug /var/lib/soci-store
```

### Log Patterns

**Step 1 - Layer Annotation**:
```json
{
  "level": "debug",
  "msg": "found explicit SOCI index digest in layer annotations",
  "layer": "sha256:...",
  "soci_index": "sha256:...",
  "source": "layer_annotation"
}
```

**Step 2 - Manifest Annotation**:
```json
{
  "level": "debug",
  "msg": "found SOCI index digest in manifest annotations",
  "image": "docker.io/library/alpine:latest",
  "soci_index": "sha256:...",
  "source": "manifest_annotation"
}
```

**Step 3 - Referrers API**:
```json
{
  "level": "debug",
  "msg": "checking for SOCI v1 index via referrers API"
}
{
  "level": "debug",
  "msg": "found SOCI index via referrers API (v1)",
  "image": "docker.io/library/alpine:latest",
  "soci_index": "sha256:...",
  "source": "referrers_api",
  "manifest": "sha256:..."
}
```

**Not Found**:
```json
{
  "level": "info",
  "msg": "no SOCI index digest found (checked layer annotations, manifest annotations, and referrers API)",
  "image": "docker.io/library/alpine:latest",
  "layer": "sha256:..."
}
```

## Error Handling

### Common Errors and Meanings

1. **"manifest digest not available for referrers API"**
   - The image manifest doesn't have a config digest
   - Referrers API query skipped
   - Will still try other methods

2. **"failed to get registry hosts for referrers API"**
   - Cannot obtain registry configuration
   - Possible network issue or misconfiguration
   - Check registry hosts configuration

3. **"failed to create remote store for referrers API"**
   - Cannot create ORAS repository
   - Invalid image reference or registry URL
   - Check refspec format

4. **"no SOCI v1 index found via referrers API"**
   - Registry doesn't have referrers for this manifest
   - Normal if using SOCI v2 (manifest annotations)
   - Will fall back to other methods

5. **ErrNoReferrers**
   - Registry returned empty referrers list
   - Image has no SOCI index attached as referrer
   - Not an error - just means v1 not available

## Comparison with fs.go

| Aspect | fs.go (Snapshotter) | manager.go (Store) |
|--------|---------------------|-------------------|
| **Referrers API** | ✅ Supported (Step 3) | ✅ Supported (Step 3) |
| **Implementation** | Lines 882-899, 934-942 | Lines 351-398, 488-498 |
| **HTTP Client** | From containerd sources | From RegistryHosts |
| **Remote Store** | artifact_fetcher.go | manager.go:472-486 |
| **Error Handling** | errors.Is(ErrNoReferrers) | Same approach |
| **Logging** | Debug level | Debug level |
| **Configuration** | pullModes.SOCIv1.Enable | Always enabled |

### Key Differences

1. **Client Source**:
   - fs.go: Gets HTTP client from containerd's source.Source
   - manager.go: Gets HTTP client from RegistryHosts

2. **Configuration**:
   - fs.go: Can enable/disable v1 via config
   - manager.go: Always attempts all methods (no config flag yet)

3. **Context**:
   - fs.go: Called during image preparation (proactive)
   - manager.go: Called on-demand when layer requested (reactive)

## Testing

### Test with SOCI v1 Image (Referrers API)

```bash
# 1. Pull image
podman pull docker.io/library/alpine:latest

# 2. Create SOCI index and push as referrer
# (Requires SOCI CLI with v1 support)
soci create --push-as-referrer docker.io/library/alpine:latest

# 3. Run container with soci-store
podman run docker.io/library/alpine:latest echo "Success!"

# 4. Check logs - should see "source": "referrers_api"
journalctl -u soci-store -f
```

### Test Fallback Behavior

```bash
# 1. Image with no SOCI index in referrers
# 2. But has SOCI index in manifest annotations
# Expected: Falls back to Step 2 (manifest annotations)

# 3. Image with no SOCI index anywhere
# Expected: Returns error, container runtime handles
```

### Test Registry Compatibility

```bash
# Test different registries:
# - Docker Hub: docker.io/...
# - GHCR: ghcr.io/...
# - Local: localhost:5000/...
# - Harbor: harbor.example.com/...

# Each should work if registry supports referrers API
```

## Future Improvements

1. **Add v1/v2 mode configuration**:
   ```go
   type StoreConfig struct {
       EnableSOCIv1 bool // Enable referrers API
       EnableSOCIv2 bool // Enable manifest annotations
   }
   ```

2. **Parallel discovery**:
   - Query referrers API while checking manifest annotations
   - Use first result to complete

3. **Caching referrers results**:
   - Cache referrers API responses per manifest digest
   - Reduce registry queries

4. **Metrics**:
   - Track which discovery method succeeded
   - Monitor referrers API latency
   - Count fallback attempts

## References

- [OCI Distribution Spec - Referrers API](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers)
- [ORAS Documentation](https://oras.land/docs/)
- [SOCI Snapshotter fs.go Implementation](../../fs/fs.go#L882-L942)
- [SOCI Index Format Specification](https://github.com/awslabs/soci-snapshotter/blob/main/docs/soci-index.md)

## Summary

The referrers API implementation enables soci-store to discover SOCI indexes using the standards-compliant OCI Referrers API (SOCI v1), in addition to the existing manifest annotation method (SOCI v2). This provides:

- **Standards compliance**: Uses OCI Distribution Specification
- **Immutable manifests**: No need to modify image manifests
- **Registry compatibility**: Works with modern OCI registries
- **Fallback support**: Gracefully falls back to other methods
- **Full parity**: Matches fs.go snapshotter implementation

All three discovery methods work together seamlessly, with automatic fallback if one method fails.
