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
	"path/filepath"

	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshots/overlay"
)

// Config represents configuration for the overlay plugin.
type Config struct {
	// Root directory for the plugin
	RootPath      string `toml:"root_path"`
	UpperdirRoot  string `toml:"upperdir_root"`
	UpperdirLabel bool   `toml:"upperdir_label"`
	SyncRemove    bool   `toml:"sync_remove"`
	RootfsQuota   int    `toml:"rootfs_quota"`
	DiskAddress   string `toml:"disk_address"`
	// MountOptions are options used for the overlay mount (not used on bind mounts)
	MountOptions []string `toml:"mount_options"`
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

			rootfsSize := overlay.DefaultRootfsSize
			if config.RootfsQuota > 0 {
				rootfsSize = config.RootfsQuota
			}
			oOpts = append(oOpts, overlay.WithRootfsQuota(rootfsSize))

			diskAddress := filepath.Join(overlay.DefaultUpperdirRoot, overlay.DefaultAddress)
			if len(config.DiskAddress) > 0 {
				diskAddress = filepath.Join(config.UpperdirRoot, config.DiskAddress)
			}
			oOpts = append(oOpts, overlay.WithDiskAddress(diskAddress))

			ic.Meta.Exports[plugin.SnapshotterRootDir] = root
			return overlay.NewSnapshotter(root, oOpts...)
		},
	})
}
