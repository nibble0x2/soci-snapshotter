/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/soci-snapshotter/config"
	bf "github.com/awslabs/soci-snapshotter/fs/backgroundfetcher"
	sociFS "github.com/awslabs/soci-snapshotter/fs"
	"github.com/awslabs/soci-snapshotter/fs/layer"
	layermetrics "github.com/awslabs/soci-snapshotter/fs/metrics/layer"
	"github.com/awslabs/soci-snapshotter/fs/source"
	"github.com/awslabs/soci-snapshotter/metadata"
	"github.com/awslabs/soci-snapshotter/soci"
	socistore "github.com/awslabs/soci-snapshotter/soci/store"
	"github.com/awslabs/soci-snapshotter/util/namedmutex"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/docker/go-metrics"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	remoteSnapshotLogKey = "remote-snapshot-prepared"
	prepareSucceeded     = "true"
	prepareFailed        = "false"

	defaultMaxConcurrency = 2
)

func NewLayerManager(ctx context.Context, root string, hosts source.RegistryHosts, metadataStore metadata.Store, artifactStore socistore.Store, cfg config.Config) (*LayerManager, error) {
	refPool, err := newRefPool(ctx, root, hosts)
	if err != nil {
		return nil, err
	}

	// Initialize background fetcher if enabled
	var bgFetcher *bf.BackgroundFetcher
	if !cfg.FSConfig.BackgroundFetchConfig.Disable {
		bgFetcher, err = bf.NewBackgroundFetcher(
			bf.WithSilencePeriod(time.Duration(cfg.FSConfig.BackgroundFetchConfig.SilencePeriodMsec)*time.Millisecond),
			bf.WithFetchPeriod(time.Duration(cfg.FSConfig.BackgroundFetchConfig.FetchPeriodMsec)*time.Millisecond),
			bf.WithMaxQueueSize(cfg.FSConfig.BackgroundFetchConfig.MaxQueueSize),
			bf.WithEmitMetricPeriod(time.Duration(cfg.FSConfig.BackgroundFetchConfig.EmitMetricPeriodSec)*time.Second),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to create background fetcher: %w", err)
		}
		go bgFetcher.Run(ctx)
	}

	// Pass artifactStore to resolver for SOCI index/ztoc fetching
	r, err := layer.NewResolver(root, cfg.FSConfig, nil, metadataStore, artifactStore, layer.OverlayOpaqueAll, bgFetcher)
	if err != nil {
		return nil, fmt.Errorf("failed to setup resolver: %w", err)
	}
	var ns *metrics.Namespace
	if !cfg.NoPrometheus {
		ns = metrics.NewNamespace("soci", "fs", nil)
	}
	c := layermetrics.NewLayerMetrics(ns)
	if ns != nil {
		metrics.Register(ns)
	}
	return &LayerManager{
		refPool:             refPool,
		hosts:               hosts,
		resolver:            r,
		artifactStore:       artifactStore,
		disableVerification: cfg.FSConfig.DisableVerification,
		metricsController:   c,
		resolveLock:         new(namedmutex.NamedMutex),
		layer:               make(map[string]map[string]layer.Layer),
		refcounter:          make(map[string]map[string]int),
		sociIndexCache:      make(map[string]*soci.Index),
	}, nil
}

// LayerManager manages layers of images and their resource lifetime.
type LayerManager struct {
	refPool *refPool
	hosts   source.RegistryHosts

	resolver            *layer.Resolver
	artifactStore       socistore.Store // For fetching SOCI indexes and ztocs
	disableVerification bool
	metricsController   *layermetrics.Controller
	resolveLock         *namedmutex.NamedMutex

	layer          map[string]map[string]layer.Layer
	refcounter     map[string]map[string]int
	sociIndexCache map[string]*soci.Index // Cache SOCI indexes by digest

	mu           sync.Mutex
	indexCacheMu sync.RWMutex
}

func (r *LayerManager) cacheLayer(refspec reference.Spec, dgst digest.Digest, l layer.Layer) (_ layer.Layer, added bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil {
		r.layer = make(map[string]map[string]layer.Layer)
	}
	if r.layer[refspec.String()] == nil {
		r.layer[refspec.String()] = make(map[string]layer.Layer)
	}
	if cl, ok := r.layer[refspec.String()][dgst.String()]; ok {
		return cl, false // already exists
	}
	r.layer[refspec.String()][dgst.String()] = l
	return l, true
}

func (r *LayerManager) getCachedLayer(refspec reference.Spec, dgst digest.Digest) layer.Layer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.layer == nil || r.layer[refspec.String()] == nil {
		return nil
	}
	if l, ok := r.layer[refspec.String()][dgst.String()]; ok {
		return l
	}
	return nil
}

func (r *LayerManager) getLayerInfo(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (Layer, error) {
	manifest, config, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return Layer{}, fmt.Errorf("failed to get manifest and config: %w", err)
	}
	return genLayerInfo(ctx, dgst, manifest, config)
}

func (r *LayerManager) getLayer(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (layer.Layer, error) {
	gotL := r.getCachedLayer(refspec, dgst)
	if gotL != nil {
		return gotL, nil
	}

	// resolve the layer and all other layers in the specified reference.
	var (
		result     layer.Layer
		resultChan = make(chan layer.Layer)
		errChan    = make(chan error)
	)
	manifest, _, err := r.refPool.loadRef(ctx, refspec)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest and config: %w", err)
	}
	var target ocispec.Descriptor
	var preResolve []ocispec.Descriptor
	var found bool
	for _, l := range manifest.Layers {
		if l.Digest == dgst {
			l := l
			found = true
			target = l
			continue
		}
		preResolve = append(preResolve, l)
	}
	if !found {
		return nil, fmt.Errorf("unknown digest %v for ref %q", target, refspec.String())
	}
	for _, l := range append([]ocispec.Descriptor{target}, preResolve...) {
		l := l

		// Check if layer is already resolved before creating goroutine.
		gotL := r.getCachedLayer(refspec, l.Digest)
		if gotL != nil {
			// Layer already resolved
			if l.Digest.String() != target.Digest.String() {
				continue // This is not the target layer; nop
			}
			result = gotL
			continue
		}

		// Resolve the layer
		go func() {
			// Avoids to get canceled by client.
			ctx := context.Background()
			gotL, err := r.resolveLayer(ctx, refspec, l)
			if l.Digest.String() != target.Digest.String() {
				return // This is not target layer
			}
			if err != nil {
				errChan <- fmt.Errorf("failed to resolve layer %q / %q: %w", refspec, l.Digest, err)
				return
			}
			// Log this as preparation success
			log.G(ctx).WithField(remoteSnapshotLogKey, prepareSucceeded).Debugf("successfully resolved layer")
			resultChan <- gotL
		}()
	}

	if result != nil {
		return result, nil
	}

	// Wait for resolving completion
	var l layer.Layer
	select {
	case l = <-resultChan:
	case err := <-errChan:
		log.G(ctx).WithError(err).Debug("failed to resolve layer")
		return nil, fmt.Errorf("failed to resolve layer: %w", err)
	case <-time.After(30 * time.Second):
		log.G(ctx).Debug("failed to resolve layer (timeout)")
		return nil, fmt.Errorf("failed to resolve layer (timeout)")
	}

	return l, nil
}

func (r *LayerManager) resolveLayer(ctx context.Context, refspec reference.Spec, target ocispec.Descriptor) (layer.Layer, error) {
	key := refspec.String() + "/" + target.Digest.String()

	// Wait if resolving this layer is already running.
	r.resolveLock.Lock(key)
	defer r.resolveLock.Unlock(key)

	gotL := r.getCachedLayer(refspec, target.Digest)
	if gotL != nil {
		// layer already resolved
		return gotL, nil
	}

	// Resolve this layer using the Resolver
	// Convert RegistryHosts to []docker.RegistryHost
	registryHosts, err := r.hosts(refspec)
	if err != nil {
		return nil, fmt.Errorf("failed to get registry hosts: %w", err)
	}

	// Extract SOCI index descriptor from layer annotations for lazy loading
	// The SOCI index contains the ztoc which enables lazy loading
	sociDesc := r.getSociDescriptor(ctx, refspec, target)

	// Check if we have a valid SOCI descriptor
	// The layer resolver requires a valid SOCI descriptor to enable lazy loading
	if sociDesc.Digest == "" {
		// No SOCI index found - cannot use lazy loading
		// Return an error to let the container runtime handle the layer download
		log.G(ctx).WithField("layer", target.Digest).Warn("no SOCI index found for layer, cannot provide lazy loading - container runtime must handle this layer")
		return nil, fmt.Errorf("layer %s has no SOCI index; lazy loading not available", target.Digest)
	}

	log.G(ctx).WithFields(map[string]interface{}{
		"layer": target.Digest,
		"ztoc":  sociDesc.Digest,
	}).Info("found SOCI index for layer, enabling lazy loading")

	// Call the Resolve function with proper parameters including SOCI descriptor
	// func Resolve(ctx, hosts, refspec, desc, sociDesc, opCounter, disableVerification, prefetchDesc, ...metadataOpts)
	// The sociDesc parameter is critical - it contains the ztoc descriptor that enables lazy loading
	l, err := r.resolver.Resolve(ctx, registryHosts, refspec, target, sociDesc, nil, r.disableVerification, nil)
	if err != nil {
		return nil, err
	}

	// Cache this layer.
	cachedL, added := r.cacheLayer(refspec, target.Digest, l)
	if added {
		r.metricsController.Add(key, cachedL)
	} else {
		l.Done() // layer is already cached. use the cached one instead. discard this layer.
	}

	return cachedL, nil
}

// getSociDescriptor extracts the SOCI index descriptor (ztoc) from layer annotations
// This descriptor points to the ztoc which enables lazy loading of the layer
//
// Resolution strategy (matching fs.go):
// 1. Check layer descriptor annotations for explicit SOCI index digest
// 2. Check manifest annotations for SOCI index digest (SOCI v2)
// 3. Try to query the OCI referrers API for SOCI artifacts (SOCI v1)
//
// Once the SOCI index digest is found, it fetches the index from the local
// artifact store and finds the ztoc blob for the requested layer.
func (r *LayerManager) getSociDescriptor(ctx context.Context, refspec reference.Spec, layerDesc ocispec.Descriptor) ocispec.Descriptor {
	// If no artifact store is configured, we cannot fetch SOCI indexes
	if r.artifactStore == nil {
		log.G(ctx).Debug("no artifact store configured, skipping SOCI index lookup")
		return ocispec.Descriptor{}
	}

	var sociIndexDigestStr string

	// Step 1: Check if the layer descriptor has an explicit SOCI index digest annotation
	// This would be passed from containerd/Podman if they know about the SOCI index
	if layerDesc.Annotations != nil {
		if explicitDigest, ok := layerDesc.Annotations[soci.ImageAnnotationSociIndexDigest]; ok && explicitDigest != "" {
			sociIndexDigestStr = explicitDigest
			log.G(ctx).WithFields(map[string]interface{}{
				"layer":        layerDesc.Digest.String(),
				"soci_index":   sociIndexDigestStr,
				"source":       "layer_annotation",
			}).Debug("found explicit SOCI index digest in layer annotations")
		}
	}

	// Step 2: If not found in layer annotations, check manifest annotations (SOCI v2)
	if sociIndexDigestStr == "" {
		manifest, _, err := r.refPool.loadRef(ctx, refspec)
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to load manifest for SOCI index lookup")
			return ocispec.Descriptor{}
		}

		if manifest.Annotations != nil {
			if manifestDigest, ok := manifest.Annotations[soci.ImageAnnotationSociIndexDigest]; ok && manifestDigest != "" {
				sociIndexDigestStr = manifestDigest
				log.G(ctx).WithFields(map[string]interface{}{
					"image":      refspec.String(),
					"soci_index": sociIndexDigestStr,
					"source":     "manifest_annotation",
				}).Debug("found SOCI index digest in manifest annotations")
			}
		}

		// Step 3: If still not found, try the OCI referrers API (SOCI v1)
		if sociIndexDigestStr == "" {
			log.G(ctx).Debug("checking for SOCI v1 index via referrers API")

			// Get manifest digest for referrers API query
			manifestDigest := manifest.Config.Digest
			if manifestDigest.String() == "" {
				// If config digest is not available, we can't query referrers
				log.G(ctx).Debug("manifest digest not available for referrers API")
			} else {
				// Get registry hosts to obtain HTTP client
				registryHosts, err := r.hosts(refspec)
				if err != nil {
					log.G(ctx).WithError(err).Debug("failed to get registry hosts for referrers API")
				} else if len(registryHosts) > 0 {
					// Use the first registry host's HTTP client
					client := registryHosts[0].Client
					if client == nil {
						client = http.DefaultClient
					}

					// Create remote store for registry access
					remoteStore, err := newRemoteStore(refspec, client)
					if err != nil {
						log.G(ctx).WithError(err).Debug("failed to create remote store for referrers API")
					} else {
						// Query referrers API
						sociIndexDesc, err := findSociIndexDescReferrer(ctx, manifestDigest, remoteStore)
						if err != nil {
							if !errors.Is(err, sociFS.ErrNoReferrers) && !errors.Is(err, errdefs.ErrNotFound) {
								log.G(ctx).WithError(err).Debug("referrers API query failed")
							} else {
								log.G(ctx).Debug("no SOCI v1 index found via referrers API")
							}
						} else {
							// Found SOCI index via referrers API!
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

		// If still not found after all methods, return empty
		if sociIndexDigestStr == "" {
			log.G(ctx).WithFields(map[string]interface{}{
				"image":       refspec.String(),
				"layer":       layerDesc.Digest.String(),
			}).Info("no SOCI index digest found (checked layer annotations, manifest annotations, and referrers API)")
			return ocispec.Descriptor{}
		}
	}

	// Step 4: Parse the SOCI index digest
	sociIndexDigest, err := digest.Parse(sociIndexDigestStr)
	if err != nil {
		log.G(ctx).WithError(err).Warnf("invalid SOCI index digest: %s", sociIndexDigestStr)
		return ocispec.Descriptor{}
	}

	// Step 4: Fetch and cache the SOCI index from artifact store
	sociIndex, err := r.getSociIndex(ctx, sociIndexDigest)
	if err != nil {
		log.G(ctx).WithError(err).WithField("soci_index", sociIndexDigestStr).Warn("failed to fetch SOCI index from artifact store")
		log.G(ctx).Warn("hint: ensure SOCI index was created with 'soci create' command")
		return ocispec.Descriptor{}
	}

	// Step 5: Find the ztoc blob for this specific layer
	layerDigestStr := layerDesc.Digest.String()
	for _, blob := range sociIndex.Blobs {
		// Only look at ztoc blobs (skip prefetch artifacts, etc.)
		if blob.MediaType != soci.SociLayerMediaType {
			continue
		}

		// Check if this blob's annotations match our layer digest
		if blob.Annotations != nil {
			if blobLayerDigest, ok := blob.Annotations[soci.IndexAnnotationImageLayerDigest]; ok {
				if blobLayerDigest == layerDigestStr {
					// Found the ztoc for this layer!
					log.G(ctx).WithFields(map[string]interface{}{
						"layer":        layerDigestStr,
						"ztoc":         blob.Digest.String(),
						"ztoc_size":    blob.Size,
						"soci_index":   sociIndexDigestStr,
					}).Debug("found ztoc for layer in SOCI index")

					return ocispec.Descriptor{
						MediaType:   soci.SociLayerMediaType,
						Digest:      blob.Digest,
						Size:        blob.Size,
						Annotations: blob.Annotations,
					}
				}
			}
		}
	}

	log.G(ctx).WithFields(map[string]interface{}{
		"layer":      layerDigestStr,
		"soci_index": sociIndexDigestStr,
		"num_blobs":  len(sociIndex.Blobs),
	}).Debug("no ztoc found in SOCI index for this layer")
	return ocispec.Descriptor{}
}

// getSociIndex fetches and caches a SOCI index from the artifact store
func (r *LayerManager) getSociIndex(ctx context.Context, indexDigest digest.Digest) (*soci.Index, error) {
	indexDigestStr := indexDigest.String()

	// Check cache first
	r.indexCacheMu.RLock()
	if cached, ok := r.sociIndexCache[indexDigestStr]; ok {
		r.indexCacheMu.RUnlock()
		log.G(ctx).WithField("index", indexDigestStr).Debug("using cached SOCI index")
		return cached, nil
	}
	r.indexCacheMu.RUnlock()

	// Not in cache, fetch from artifact store
	log.G(ctx).WithField("index", indexDigestStr).Debug("fetching SOCI index from artifact store")

	// Create descriptor for the SOCI index
	indexDesc := ocispec.Descriptor{
		Digest: indexDigest,
		// Size and MediaType will be determined by the artifact store
	}

	// Fetch the index content
	indexReader, err := r.artifactStore.Fetch(ctx, indexDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch SOCI index from artifact store: %w", err)
	}
	defer indexReader.Close()

	// Read all content into bytes
	indexBytes, err := io.ReadAll(indexReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read SOCI index content: %w", err)
	}

	// Unmarshal the SOCI index
	var sociIndex soci.Index
	if err := soci.UnmarshalIndex(indexBytes, &sociIndex); err != nil {
		return nil, fmt.Errorf("failed to unmarshal SOCI index: %w", err)
	}

	// Cache the index
	r.indexCacheMu.Lock()
	r.sociIndexCache[indexDigestStr] = &sociIndex
	r.indexCacheMu.Unlock()

	log.G(ctx).WithFields(map[string]interface{}{
		"index":         indexDigestStr,
		"num_blobs":     len(sociIndex.Blobs),
		"media_type":    sociIndex.MediaType,
		"artifact_type": sociIndex.ArtifactType,
	}).Debug("successfully fetched and cached SOCI index")

	return &sociIndex, nil
}

// newRemoteStore creates an ORAS remote repository for accessing the registry
// This is used for the referrers API to discover SOCI indexes (v1)
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

// findSociIndexDescReferrer queries the OCI referrers API to find SOCI index artifacts
// This is used for SOCI v1 discovery method
func findSociIndexDescReferrer(ctx context.Context, imgDigest digest.Digest, remoteStore *remote.Repository) (ocispec.Descriptor, error) {
	artifactClient := sociFS.NewOCIArtifactClient(remoteStore)

	desc, err := artifactClient.SelectReferrer(ctx, ocispec.Descriptor{Digest: imgDigest}, sociFS.SelectFirstPolicy)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("cannot fetch list of referrers: %w", err)
	}
	return desc, nil
}

func (r *LayerManager) release(ctx context.Context, refspec reference.Spec, dgst digest.Digest) (int, error) {
	r.refPool.release(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil || r.refcounter[refspec.String()] == nil {
		return 0, fmt.Errorf("ref %q not tracked", refspec.String())
	} else if _, ok := r.refcounter[refspec.String()][dgst.String()]; !ok {
		return 0, fmt.Errorf("layer %q/%q not tracked", refspec.String(), dgst.String())
	}
	r.refcounter[refspec.String()][dgst.String()]--
	i := r.refcounter[refspec.String()][dgst.String()]
	if i <= 0 {
		// No reference to this layer. release it.
		delete(r.refcounter, dgst.String())
		if len(r.refcounter[refspec.String()]) == 0 {
			delete(r.refcounter, refspec.String())
		}
		if r.layer == nil || r.layer[refspec.String()] == nil {
			return 0, fmt.Errorf("layer of reference %q is not registered (ref=%d)", refspec, i)
		}
		l, ok := r.layer[refspec.String()][dgst.String()]
		if !ok {
			return 0, fmt.Errorf("layer of digest %q/%q is not registered (ref=%d)", refspec, dgst, i)
		}
		l.Done()
		delete(r.layer[refspec.String()], dgst.String())
		if len(r.layer[refspec.String()]) == 0 {
			delete(r.layer, refspec.String())
		}
		log.G(ctx).WithField("refcounter", i).Infof("layer %v/%v is released due to no reference", refspec, dgst)
	}
	return i, nil
}

func (r *LayerManager) use(refspec reference.Spec, dgst digest.Digest) int {
	r.refPool.use(refspec)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refcounter == nil {
		r.refcounter = make(map[string]map[string]int)
	}
	if r.refcounter[refspec.String()] == nil {
		r.refcounter[refspec.String()] = make(map[string]int)
	}
	if _, ok := r.refcounter[refspec.String()][dgst.String()]; !ok {
		r.refcounter[refspec.String()][dgst.String()] = 1
		return 1
	}
	r.refcounter[refspec.String()][dgst.String()]++
	return r.refcounter[refspec.String()][dgst.String()]
}

func colon2dash(s string) string {
	return strings.ReplaceAll(s, ":", "-")
}

// Layer represents the layer information. Format is compatible to the one required by
// "additional layer store" of github.com/containers/storage.
type Layer struct {
	CompressedDigest   digest.Digest `json:"compressed-diff-digest,omitempty"`
	CompressedSize     int64         `json:"compressed-size,omitempty"`
	UncompressedDigest digest.Digest `json:"diff-digest,omitempty"`
	UncompressedSize   int64         `json:"diff-size,omitempty"`
	CompressionType    int           `json:"compression,omitempty"`
	ReadOnly           bool          `json:"-"`
}

// Defined in https://github.com/containers/storage/blob/b64e13a1afdb0bfed25601090ce4bbbb1bc183fc/pkg/archive/archive.go#L108-L119
const gzipTypeMagicNum = 2

func genLayerInfo(ctx context.Context, dgst digest.Digest, manifest ocispec.Manifest, config ocispec.Image) (Layer, error) {
	if len(manifest.Layers) != len(config.RootFS.DiffIDs) {
		return Layer{}, fmt.Errorf(
			"len(manifest.Layers) != len(config.Rootfs): %d != %d",
			len(manifest.Layers), len(config.RootFS.DiffIDs))
	}
	var (
		layerIndex = -1
	)
	for i, l := range manifest.Layers {
		if l.Digest == dgst {
			layerIndex = i
		}
	}
	if layerIndex == -1 {
		return Layer{}, fmt.Errorf("layer %q not found in the manifest", dgst.String())
	}
	// Try to get uncompressed size from annotations
	// Note: For SOCI layers, this information might not be available in annotations
	var uncompressedSize int64
	const uncompressedSizeAnnotation = "containerd.io/uncompressed-size"
	if uncompressedSizeStr, ok := manifest.Layers[layerIndex].Annotations[uncompressedSizeAnnotation]; ok {
		var err error
		uncompressedSize, err = strconv.ParseInt(uncompressedSizeStr, 10, 64)
		if err != nil {
			log.G(ctx).WithError(err).Debugf("layer %q has invalid uncompressed size annotation", dgst.String())
		}
	}
	return Layer{
		CompressedDigest:   manifest.Layers[layerIndex].Digest,
		CompressedSize:     manifest.Layers[layerIndex].Size,
		UncompressedDigest: config.RootFS.DiffIDs[layerIndex],
		UncompressedSize:   uncompressedSize,
		CompressionType:    gzipTypeMagicNum,
		ReadOnly:           true,
	}, nil
}
