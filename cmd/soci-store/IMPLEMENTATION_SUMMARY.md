# Implementation Summary: Referrers API Support for soci-store

## What Was Implemented

Added OCI Referrers API support to soci-store's SOCI index discovery, achieving full parity with the snapshotter (`fs.go`) implementation.

## Files Modified

### `/home/insanebarala/git_projects/soci-snapshotter/store/manager.go`

#### 1. Added Imports (lines 19-48)

**New imports**:
```go
sociFS "github.com/awslabs/soci-snapshotter/fs"  // For OCIArtifactClient and ErrNoReferrers
```

**Existing imports now used**:
- `"errors"` - For ErrNoReferrers checking
- `"net/http"` - For HTTP client in remote store
- `"github.com/containerd/containerd/remotes/docker"` - For registry host matching
- `"github.com/containerd/errdefs"` - For ErrNotFound
- `"oras.land/oras-go/v2/registry/remote"` - For ORAS remote repository

#### 2. Updated Documentation (lines 300-309)

Changed the function comment from:
```go
// Note: Unlike fs.go, we cannot use the referrers API because store mode
// doesn't have access to remote registries. All SOCI indexes must be in
// the local artifact store (created via `soci create` command).
```

To:
```go
// Resolution strategy (matching fs.go):
// 1. Check layer descriptor annotations for explicit SOCI index digest
// 2. Check manifest annotations for SOCI index digest (SOCI v2)
// 3. Try to query the OCI referrers API for SOCI artifacts (SOCI v1)
//
// Once the SOCI index digest is found, it fetches the index from the local
// artifact store and finds the ztoc blob for the requested layer.
```

#### 3. Enhanced `getSociDescriptor()` - Step 3 (lines 351-398)

Added referrers API lookup between Step 2 (manifest annotations) and final parsing:

```go
// Step 3: If still not found, try the OCI referrers API (SOCI v1)
if sociIndexDigestStr == "" {
    log.G(ctx).Debug("checking for SOCI v1 index via referrers API")

    // Get manifest digest for referrers API query
    manifestDigest := manifest.Config.Digest
    if manifestDigest.String() == "" {
        log.G(ctx).Debug("manifest digest not available for referrers API")
    } else {
        // Get registry hosts to obtain HTTP client
        registryHosts, err := r.hosts(refspec)
        if err != nil {
            log.G(ctx).WithError(err).Debug("failed to get registry hosts for referrers API")
        } else if len(registryHosts) > 0 {
            client := registryHosts[0].Client
            if client == nil {
                client = http.DefaultClient
            }

            // Create remote store and query referrers
            remoteStore, err := newRemoteStore(refspec, client)
            if err != nil {
                log.G(ctx).WithError(err).Debug("failed to create remote store for referrers API")
            } else {
                sociIndexDesc, err := findSociIndexDescReferrer(ctx, manifestDigest, remoteStore)
                if err != nil {
                    if !errors.Is(err, sociFS.ErrNoReferrers) && !errors.Is(err, errdefs.ErrNotFound) {
                        log.G(ctx).WithError(err).Debug("referrers API query failed")
                    } else {
                        log.G(ctx).Debug("no SOCI v1 index found via referrers API")
                    }
                } else {
                    // Success!
                    sociIndexDigestStr = sociIndexDesc.Digest.String()
                    log.G(ctx).WithFields(map[string]interface{}{
                        "image":        refspec.String(),
                        "soci_index":   sociIndexDigestStr,
                        "source":       "referrers_api",
                        "manifest":     manifestDigest.String(),
                    }).Debug("found SOCI index via referrers API (v1)")
                }
            }
        }
    }
}
```

**Key features**:
- Uses manifest config digest for referrers query
- Gets HTTP client from RegistryHosts (similar to how fs.go gets it from sources)
- Creates ORAS remote repository for registry access
- Queries referrers API using OCIArtifactClient
- Handles errors gracefully (ErrNoReferrers, ErrNotFound)
- Logs discovery source for debugging

#### 4. Updated Error Message (line 405)

Changed from:
```go
"no SOCI index digest found (checked layer and manifest annotations)"
```

To:
```go
"no SOCI index digest found (checked layer annotations, manifest annotations, and referrers API)"
```

#### 5. Added Helper Functions (lines 472-498)

**`newRemoteStore()`** - Creates ORAS remote repository:
```go
func newRemoteStore(refspec reference.Spec, client *http.Client) (*remote.Repository, error) {
    repo, err := remote.NewRepository(refspec.Locator)
    if err != nil {
        return nil, fmt.Errorf("cannot create repository %s: %w", refspec.Locator, err)
    }
    repo.Client = client
    repo.PlainHTTP, err = docker.MatchLocalhost(refspec.Hostname())
    if err != nil {
        return nil, fmt.Errorf("cannot create repository %s: %w", refspec.Locator, err)
    }
    return repo, nil
}
```

**`findSociIndexDescReferrer()`** - Queries referrers API:
```go
func findSociIndexDescReferrer(ctx context.Context, imgDigest digest.Digest, remoteStore *remote.Repository) (ocispec.Descriptor, error) {
    artifactClient := sociFS.NewOCIArtifactClient(remoteStore)

    desc, err := artifactClient.SelectReferrer(ctx, ocispec.Descriptor{Digest: imgDigest}, sociFS.SelectFirstPolicy)
    if err != nil {
        return ocispec.Descriptor{}, fmt.Errorf("cannot fetch list of referrers: %w", err)
    }
    return desc, nil
}
```

Both functions are direct ports from `fs/artifact_fetcher.go` and `fs/fs.go`.

## Files Created

### Documentation Files

1. **`cmd/soci-store/REFERRERS_API.md`**
   - Comprehensive guide to referrers API implementation
   - Explains SOCI v1 vs v2
   - Code flow diagrams
   - Registry requirements
   - Testing instructions
   - Troubleshooting guide

## Changes Summary

| File | Lines Changed | Description |
|------|---------------|-------------|
| store/manager.go | ~70 lines | Added referrers API support |

### Specific Changes

- ✅ Added 1 new import (sociFS)
- ✅ Made 5 existing imports active (errors, net/http, docker, errdefs, remote)
- ✅ Added 2 helper functions (newRemoteStore, findSociIndexDescReferrer)
- ✅ Enhanced getSociDescriptor() with Step 3 (referrers API)
- ✅ Updated documentation comments
- ✅ Updated error messages
- ✅ Created comprehensive documentation

## How It Works

### Three-Step Discovery Process

```
┌─────────────────────────────────────┐
│  Step 1: Layer Annotations          │
│  Check: layerDesc.Annotations       │
│  Priority: Highest (explicit)       │
└─────────────────────────────────────┘
              ↓ (if not found)
┌─────────────────────────────────────┐
│  Step 2: Manifest Annotations (v2)  │
│  Check: manifest.Annotations        │
│  Priority: Medium (SOCI v2)         │
└─────────────────────────────────────┘
              ↓ (if not found)
┌─────────────────────────────────────┐
│  Step 3: Referrers API (v1)         │
│  Query: GET /v2/.../referrers/...   │
│  Priority: Low (SOCI v1)            │
└─────────────────────────────────────┘
              ↓ (if found)
┌─────────────────────────────────────┐
│  Fetch SOCI index from artifact     │
│  store and return ztoc descriptor   │
└─────────────────────────────────────┘
```

### Registry Communication

1. **Get HTTP Client**: From RegistryHosts configuration
2. **Create Remote Store**: ORAS repository for registry access
3. **Query Referrers**: `GET /v2/<repo>/referrers/<manifest-digest>`
4. **Filter Results**: Only SOCI index artifact type
5. **Return Descriptor**: First matching SOCI index

### Error Handling

- **Graceful degradation**: Each step failure doesn't stop next steps
- **Specific error checking**: Distinguishes between "not found" and "error"
- **Debug logging**: All steps logged for troubleshooting
- **Source tracking**: Logs which method succeeded

## Testing

### Build Verification

```bash
$ make soci-store
✓ Build successful
✓ No compile errors
✓ Binary: out/soci-store (28M)
```

### Expected Behavior

**With SOCI v1 image** (referrers):
```json
{"level":"debug","msg":"checking for SOCI v1 index via referrers API"}
{"level":"debug","msg":"found SOCI index via referrers API (v1)","source":"referrers_api"}
```

**With SOCI v2 image** (annotations):
```json
{"level":"debug","msg":"found SOCI index digest in manifest annotations","source":"manifest_annotation"}
```

**With no SOCI index**:
```json
{"level":"info","msg":"no SOCI index digest found (checked layer annotations, manifest annotations, and referrers API)"}
```

## Comparison with fs.go

### Similarities

| Aspect | Implementation |
|--------|---------------|
| Discovery order | Layer annotations → Manifest → Referrers |
| Helper functions | newRemoteStore(), findSociIndexDescReferrer() |
| Error handling | errors.Is(ErrNoReferrers) |
| Logging level | Debug for discovery steps |
| Client type | OCIArtifactClient |
| Selection policy | SelectFirstPolicy |

### Differences

| Aspect | fs.go | manager.go |
|--------|-------|------------|
| HTTP client source | containerd sources | RegistryHosts |
| Configuration | pullModes.SOCIv1.Enable | Always enabled |
| Context | Proactive (during image prep) | Reactive (on layer request) |
| Manifest digest | From labels | From manifest.Config.Digest |

## Future Enhancements

1. **Add configuration support**:
   ```go
   type StoreConfig struct {
       EnableSOCIv1 bool // referrers API
       EnableSOCIv2 bool // manifest annotations
   }
   ```

2. **Performance optimizations**:
   - Cache referrers API responses
   - Parallel discovery attempts
   - Connection pooling

3. **Metrics and monitoring**:
   - Track discovery method success rates
   - Monitor referrers API latency
   - Count fallback attempts

4. **Enhanced error reporting**:
   - Differentiate registry errors vs missing artifacts
   - Provide actionable error messages

## Benefits

1. **Standards Compliance**: Uses OCI Distribution Spec referrers API
2. **Feature Parity**: Matches snapshotter (fs.go) implementation
3. **Registry Compatibility**: Works with modern OCI registries
4. **Immutable Manifests**: Can discover SOCI indexes without manifest modifications
5. **Graceful Fallback**: Automatically tries multiple methods
6. **Backward Compatible**: Still works with SOCI v2 (manifest annotations)
7. **Debugging Support**: Comprehensive logging for troubleshooting

## Conclusion

The referrers API implementation brings soci-store to full feature parity with the snapshotter implementation, enabling discovery of SOCI indexes using three complementary methods. This provides maximum compatibility with different SOCI index formats (v1 and v2) and registry configurations.

The implementation follows the same patterns as fs.go, reuses existing components (OCIArtifactClient), and maintains backward compatibility while adding new capabilities.

**Build Status**: ✅ Successful (no compile errors)
**Code Quality**: ✅ Follows existing patterns
**Documentation**: ✅ Comprehensive
**Testing**: ⏳ Ready for integration testing
