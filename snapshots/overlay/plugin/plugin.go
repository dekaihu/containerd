//go:build linux
// +build linux

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

package overlay

import (
	"errors"
	"fmt"
	"sync"

	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/containerd/containerd/snapshots/overlay/quota"
)

// Config represents configuration for the overlay plugin.
type Config struct {
	// Root directory for the plugin
	RootPath          string `toml:"root_path"`
	UpperdirRoot      string `toml:"upperdir_root"`
	UpperdirLabel     bool   `toml:"upperdir_label"`
	SyncRemove        bool   `toml:"sync_remove"`
	RootfsQuota       int    `toml:"rootfs_quota"`
	RootfsStorageType string `toml:"rootfs_quota_type"`

	// MountOptions are options used for the overlay mount (not used on bind mounts)
	MountOptions []string `toml:"mount_options"`
}

var InitQuotaFn map[string]quota.RootfsQuota
var pluginMutex sync.Mutex

type PluginBuilder func(root string) (quota.RootfsQuota, error)

var quotaBuilders = map[string]PluginBuilder{}

func RegisterQuotaPlugin(name string, pc PluginBuilder) {
	pluginMutex.Lock()
	defer pluginMutex.Unlock()
	quotaBuilders[name] = pc
}

var quotaInstances = map[string]quota.RootfsQuota{}

func createQuota(root, upperdirRoot string) error {
	pluginMutex.Lock()
	defer pluginMutex.Unlock()

	for name, pc := range quotaBuilders {
		var ro string
		switch name {
		case "xfs":
			ro = root
		default:
			ro = upperdirRoot
		}
		instance, err := pc(ro)
		if err != nil {
			return fmt.Errorf("rootfsquota %s not support quota, path: %s ", name, ro)
		}

		InitQuotaFn[name] = instance
	}

	return nil
}

func init() {
	plugin.Register(&plugin.Registration{
		Type:   plugin.SnapshotPlugin,
		ID:     "overlayfs",
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ic.Meta.Platforms = append(ic.Meta.Platforms, platforms.DefaultSpec())

			config, ok := ic.Config.(*Config)
			if !ok {
				return nil, errors.New("invalid overlay configuration")
			}

			root := ic.Root
			if config.RootPath != "" {
				root = config.RootPath
			}

			var oOpts []overlay.Opt
			if config.UpperdirLabel {
				oOpts = append(oOpts, overlay.WithUpperdirLabel)
			}
			if !config.SyncRemove {
				oOpts = append(oOpts, overlay.AsynchronousRemove)
			}

			upperdirRoot := overlay.DefaultUpperdirRoot
			if len(config.UpperdirRoot) != 0 {
				upperdirRoot = config.UpperdirRoot
			}
			oOpts = append(oOpts, overlay.WithUpperdirRoot(upperdirRoot))

			if len(config.MountOptions) > 0 {
				oOpts = append(oOpts, overlay.WithMountOptions(config.MountOptions))
			}

			//xfs -----> root     other ------> upperdir
			if quotaBuilders != nil {
				createQuota(root, upperdirRoot)
				oOpts = append(oOpts, overlay.WithQuotas(InitQuotaFn))
			}

			quotaSize := overlay.DefaultQuotaSize
			if config.RootfsQuota > 0 {
				quotaSize = config.RootfsQuota
			}
			oOpts = append(oOpts, overlay.WithRootfsQuota(quotaSize))

			quotaType := overlay.DefaultNotebookQuotaType
			if len(config.RootfsStorageType) > 0 {
				quotaType = config.RootfsStorageType
			}
			oOpts = append(oOpts, overlay.WithUpperdirQuotaType(quotaType))

			ic.Meta.Exports[plugin.SnapshotterRootDir] = root
			return overlay.NewSnapshotter(root, oOpts...)
		},
	})
}
