/*
   Copyright The Soci Snapshotter Authors.

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

package main

import (
	"context"
	"flag"
	"fmt"
	golog "log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/awslabs/soci-snapshotter/config"
	"github.com/awslabs/soci-snapshotter/fs/source"
	"github.com/awslabs/soci-snapshotter/metadata"
	"github.com/awslabs/soci-snapshotter/service/resolver"
	socistore "github.com/awslabs/soci-snapshotter/soci/store"
	"github.com/awslabs/soci-snapshotter/store"
	"github.com/containerd/log"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/sirupsen/logrus"
)

const (
	defaultLogLevel   = logrus.InfoLevel
	defaultConfigPath = "/etc/soci-store/config.toml"
	defaultRootDir    = "/var/lib/soci-store"
)

var (
	configPath = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel   = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir    = flag.String("root", defaultRootDir, "path to the root directory for this store")
)

func main() {
	flag.Parse()
	mountPoint := flag.Arg(0)
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
	})
	var (
		ctx = log.WithLogger(context.Background(), log.L)
		cfg *config.Config
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Store should use "github.com/containerd/log" otherwise
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(logrus.DebugLevel))

	if mountPoint == "" {
		log.G(ctx).Fatalf("mount point must be specified")
	}

	// Get configuration from specified file
	if *configPath != "" {
		cfg, err = config.NewConfigFromToml(*configPath)
		if err != nil {
			if !os.IsNotExist(err) || *configPath != defaultConfigPath {
				log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
			}
			// Use default config if default config path doesn't exist
			cfg = config.NewConfig()
			if cfg == nil {
				log.G(ctx).Fatal("failed to create default config")
			}
		}
	} else {
		cfg = config.NewConfig()
		if cfg == nil {
			log.G(ctx).Fatal("failed to create default config")
		}
	}

	// Use RegistryHosts based on config
	// Create a registry manager and get the RegistryHosts function
	registryManager := resolver.NewRegistryManager(cfg.FSConfig.RetryableHTTPClientConfig, cfg.ResolverConfig, nil)
	hosts := source.RegistryHosts(registryManager.AsRegistryHosts())

	// Configure and mount filesystem
	if _, err := os.Stat(mountPoint); err != nil {
		if err2 := os.MkdirAll(mountPoint, 0755); err2 != nil && !os.IsExist(err2) {
			log.G(ctx).WithError(err).WithError(err2).
				Fatalf("failed to prepare mountpoint %q", mountPoint)
		}
	}
	if !cfg.FSConfig.DisableVerification {
		log.G(ctx).Warnf("content verification is not fully supported in store mode; switching to non-verification mode")
		cfg.FSConfig.DisableVerification = true
	}
	mt, err := getMetadataStore(*rootDir, *cfg)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to configure metadata store")
	}

	// Create artifact store for SOCI indexes and ztocs
	// Use local soci content store within the root directory
	// The content will be stored at <rootDir>/content
	artifactStore, err := socistore.NewContentStore(
		socistore.WithType(socistore.SociContentStoreType),
		socistore.WithSnapshotterRoot(*rootDir),
	)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to create artifact store")
	}

	contentStorePath := filepath.Join(*rootDir, "content")
	log.G(ctx).WithField("path", contentStorePath).Info("initialized artifact store for SOCI indexes")

	layerManager, err := store.NewLayerManager(ctx, *rootDir, hosts, mt, artifactStore, *cfg)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to prepare layer manager")
	}
	if err := store.Mount(ctx, mountPoint, layerManager, cfg.FSConfig.Debug); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to mount fs at %q", mountPoint)
	}
	defer func() {
		syscall.Unmount(mountPoint, 0)
		log.G(ctx).Info("Exiting")
	}()

	if os.Getenv("NOTIFY_SOCKET") != "" {
		notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyReady)
		log.G(ctx).Debugf("SdNotifyReady notified=%v, err=%v", notified, notifyErr)
	}
	defer func() {
		if os.Getenv("NOTIFY_SOCKET") != "" {
			notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyStopping)
			log.G(ctx).Debugf("SdNotifyStopping notified=%v, err=%v", notified, notifyErr)
		}
	}()

	waitForSIGINT()
	log.G(ctx).Info("Got SIGINT")
}

func waitForSIGINT() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
}

const (
	memoryMetadataType = "memory"
	dbMetadataType     = "db"
)

func getMetadataStore(rootDir string, cfg config.Config) (metadata.Store, error) {
	switch cfg.MetadataStore {
	case "", memoryMetadataType:
		// For memory metadata store, return nil as the resolver will handle it
		// The layer resolver will use the default in-memory metadata reader
		return nil, nil
	case dbMetadataType:
		// For DB metadata store - currently not supported in store mode
		return nil, fmt.Errorf("db metadata store is not currently supported in store mode; use memory metadata store")
	default:
		return nil, fmt.Errorf("unknown metadata store type: %v; must be %v or %v",
			cfg.MetadataStore, memoryMetadataType, dbMetadataType)
	}
}
