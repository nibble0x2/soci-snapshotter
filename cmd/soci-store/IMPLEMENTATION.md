# SOCI Store Implementation - Lazy Loading Architecture

This document explains how the SOCI additional layer store implements lazy loading and how it differs from the standard container layer handling.

## Overview

The soci-store implementation provides an "additional layer store" for Podman/CRI-O that enables access to SOCI-indexed layers. While the current implementation doesn't include full lazy loading support, the architecture is designed to support it in the future.

## How SOCI Lazy Loading Works

### 1. **SOCI Index Structure**

SOCI (Seekable OCI) creates an index for container images that enables lazy loading:

```
Image Manifest
├── Annotations
│   └── com.amazon.soci.index-digest: "sha256:abc..."
└── Layers
    └── Layer (sha256:xyz...)
        └── Annotations (on the ztoc in SOCI index)
            └── com.amazon.soci.image-layer-digest: "sha256:xyz..."

SOCI Index (sha256:abc...)
├── Blobs
│   ├── ztoc for layer 1 (sha256:def...)
│   │   └── Annotations
│   │       └── com.amazon.soci.image-layer-digest: "sha256:xyz..."
│   └── ztoc for layer 2
└── Metadata
```

### 2. **Key Components for Lazy Loading**

#### a) **ztoc (zstd Table of Contents)**
- Contains file metadata for all files in a compressed layer
- Enables random access to files without decompressing the entire layer
- Stores span information for efficient chunk fetching

#### b) **Span Manager**
- Manages on-demand fetching of data spans from the layer blob
- Uses ztoc information to fetch only the required data
- Implements caching for fetched spans

#### c) **Background Fetcher**
- Optionally pre-fetches spans in the background
- Improves performance by prefetching likely-to-be-accessed data

#### d) **Artifact Store**
- Stores and retrieves SOCI artifacts (indexes and ztocs)
- Typically backed by containerd's content store

### 3. **Lazy Loading Flow in fs/fs.go**

Here's how the main snapshotter implementation achieves lazy loading:

```go
// 1. Resolve is called with both the layer descriptor and SOCI index descriptor
func (r *Resolver) Resolve(ctx, hosts, refspec, layerDesc, sociDesc, ...) {

    // 2. Fetch the ztoc from the artifact store using sociDesc
    ztocReader, err := r.artifactStore.Fetch(ctx, sociDesc)

    // 3. Unmarshal the ztoc to get file metadata
    ztoc, err := ztoc.Unmarshal(ztocReader)

    // 4. Create a metadata store with the ztoc
    meta, err := r.metadataStore(sectionReader, ztoc.TOC, opts...)

    // 5. Create span manager for on-demand data fetching
    spanManager := spanmanager.New(ztoc, sectionReader, spanCache, ...)

    // 6. Add to background fetcher for prefetching (optional)
    if r.bgFetcher != nil {
        bgLayerResolver := backgroundfetcher.NewSequentialResolver(layerDesc.Digest, spanManager)
        r.bgFetcher.Add(bgLayerResolver)
    }

    // 7. Create a verifiable reader that uses the span manager
    vr, err := reader.NewReader(meta, layerDesc.Digest, spanManager, ...)

    // 8. Return the layer with lazy-loading capability
    return newLayer(r, layerDesc, name, blobR, vr, bgLayerResolver, ...)
}
```

### 4. **Current store/manager.go Implementation**

The current additional layer store implementation has a simplified flow:

```go
func (r *LayerManager) resolveLayer(ctx, refspec, target) {
    // 1. Get registry hosts for the image
    registryHosts, err := r.hosts(refspec)

    // 2. Extract SOCI descriptor (currently returns empty)
    sociDesc := r.getSociDescriptor(ctx, target)
    // Note: This currently returns empty descriptor because we need:
    // - Access to the full image manifest to get SOCI index digest
    // - An artifact store to fetch the SOCI index
    // - Logic to find the correct ztoc for this layer

    // 3. Call Resolve with the SOCI descriptor
    l, err := r.resolver.Resolve(ctx, registryHosts, refspec,
        target,      // Layer descriptor
        sociDesc,    // SOCI index descriptor (empty for now)
        nil,         // Operation counter
        r.disableVerification,
        nil)         // Prefetch descriptor

    // When sociDesc is empty, the resolver will:
    // - Not find a ztoc
    // - Fall back to downloading the entire layer
    // - Still work, but without lazy loading benefits
}
```

## Future Enhancements for Full Lazy Loading

To enable full lazy loading in soci-store, the following would be needed:

### 1. **Add Artifact Store to LayerManager**

```go
type LayerManager struct {
    // ... existing fields ...
    artifactStore content.Storage  // Add this
}

func NewLayerManager(..., artifactStore content.Storage, ...) (*LayerManager, error) {
    return &LayerManager{
        // ... existing fields ...
        artifactStore: artifactStore,
    }
}
```

### 2. **Implement Full getSociDescriptor**

```go
func (r *LayerManager) getSociDescriptor(ctx context.Context, refspec reference.Spec, layerDesc ocispec.Descriptor) ocispec.Descriptor {
    // 1. Load the manifest to get SOCI index digest
    manifest, _, err := r.refPool.loadRef(ctx, refspec)
    if err != nil {
        return ocispec.Descriptor{}
    }

    // 2. Get SOCI index digest from manifest annotations
    sociIndexDigest, ok := manifest.Annotations[soci.ImageAnnotationSociIndexDigest]
    if !ok {
        return ocispec.Descriptor{} // No SOCI index for this image
    }

    // 3. Parse the digest
    indexDigest, err := digest.Parse(sociIndexDigest)
    if err != nil {
        return ocispec.Descriptor{}
    }

    // 4. Fetch the SOCI index from artifact store
    indexReader, err := r.artifactStore.Fetch(ctx, ocispec.Descriptor{
        Digest: indexDigest,
    })
    if err != nil {
        return ocispec.Descriptor{}
    }
    defer indexReader.Close()

    // 5. Unmarshal the SOCI index
    sociIndex, err := soci.UnmarshalIndex(indexReader)
    if err != nil {
        return ocispec.Descriptor{}
    }

    // 6. Find the ztoc blob for this layer
    for _, blob := range sociIndex.Blobs {
        if blob.Annotations[soci.IndexAnnotationImageLayerDigest] == layerDesc.Digest.String() {
            // Found the ztoc for this layer!
            return ocispec.Descriptor{
                MediaType: soci.SociLayerMediaType,
                Digest:    blob.Digest,
                Size:      blob.Size,
            }
        }
    }

    return ocispec.Descriptor{} // No ztoc found for this layer
}
```

### 3. **Update refPool to Include Manifest Annotations**

The refPool would need to cache not just the manifest layers but also the annotations that contain the SOCI index reference.

## Performance Implications

### Without Lazy Loading (Current)
- **Pro**: Simple, no additional infrastructure needed
- **Con**: Must download entire layers before use
- **Use Case**: Works well for small images or when network bandwidth is not a concern

### With Lazy Loading (Future Enhancement)
- **Pro**: Containers start much faster (only fetch needed files)
- **Pro**: Reduced network bandwidth usage
- **Pro**: Lower storage requirements
- **Con**: Requires SOCI index infrastructure
- **Con**: Slightly more complex implementation
- **Use Case**: Ideal for large images, slow networks, or ephemeral workloads

## Comparison with stargz-store

| Feature | stargz-store | soci-store (current) | soci-store (with enhancements) |
|---------|--------------|----------------------|--------------------------------|
| Lazy Loading | ✅ Yes | ❌ No | ✅ Yes (planned) |
| Index Format | eStargz TOC | N/A | SOCI ztoc |
| Compression | gzip | zstd | zstd |
| Span-based Prefetch | ❌ No | ❌ No | ✅ Yes |
| Background Fetching | ✅ Yes | ✅ Yes | ✅ Yes |
| Additional Layer Store | ✅ Yes | ✅ Yes | ✅ Yes |

## Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                     Podman/CRI-O                            │
│  ┌──────────────────────────────────────────────────────┐   │
│  │        containers/storage                            │   │
│  │  ┌────────────────────────────────────────────────┐  │   │
│  │  │   Additional Layer Store (soci-store FUSE)     │  │   │
│  │  │  ┌──────────────────────────────────────────┐  │  │   │
│  │  │  │  store/fs.go (FUSE filesystem)           │  │  │   │
│  │  │  │  - rootnode, refnode, layernode          │  │  │   │
│  │  │  │  - Exposes layer/{diff,blob,info}        │  │  │   │
│  │  │  └────────────┬─────────────────────────────┘  │  │   │
│  │  │               │                                 │  │   │
│  │  │  ┌────────────▼─────────────────────────────┐  │  │   │
│  │  │  │  store/manager.go (LayerManager)         │  │  │   │
│  │  │  │  - getLayer()                            │  │  │   │
│  │  │  │  - resolveLayer() ──┐                    │  │  │   │
│  │  │  │  - getSociDescriptor() [future]          │  │  │   │
│  │  │  └────────────┬─────────┼────────────────────┘  │  │   │
│  │  └───────────────┼─────────┼────────────────────────┘  │   │
│  └────────────────── ┼─────────┼────────────────────────────┘   │
└────────────────────┼─────────┼─────────────────────────────────┘
                     │         │
        ┌────────────▼─────────▼──────────────┐
        │  fs/layer/layer.go (Resolver)       │
        │  ┌──────────────────────────────┐   │
        │  │  Resolve(layerDesc,          │   │
        │  │          sociDesc, ...)      │   │
        │  │    │                         │   │
        │  │    ├─ Fetch ztoc ────────────┼───┼─► Artifact Store
        │  │    ├─ Create SpanManager     │   │     (content.Storage)
        │  │    ├─ Create metadata        │   │
        │  │    └─ Add to bgFetcher       │   │
        │  └──────────────────────────────┘   │
        └─────────────────────────────────────┘
                        │
        ┌───────────────▼──────────────────────┐
        │  fs/span-manager (SpanManager)       │
        │  - On-demand span fetching           │
        │  - Span caching                      │
        │  - Verification                      │
        └──────────────────────────────────────┘
```

## Key Takeaways

1. **Current Implementation**: Works as an additional layer store but downloads complete layers
2. **Lazy Loading**: Requires SOCI index lookup and artifact store integration
3. **Architecture**: Designed to support lazy loading with minimal changes
4. **Future Path**: Clear path to add full lazy loading support when needed

The implementation provides a solid foundation that works today and can be enhanced for lazy loading as requirements evolve.
