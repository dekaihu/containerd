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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay/disk"
	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/sirupsen/logrus"
)

// upperdirKey is a key of an optional lablel to each snapshot.
// This optional label of a snapshot contains the location of "upperdir" where
// the change set between this snapshot and its parent is stored.
const upperdirKey = "containerd.io/snapshot/overlay.upperdir"

var (
	notebookLabelKey     = "type.system.hero.ai"
	versionKey           = "lastversions.system.hero.ai"
	notebookLabelValue   = "notebook"
	notebookActionKey    = "command.system.hero.ai"
	notebookStartValue   = "start"
	notebookNameLabelKey = "jobid.system.hero.ai"
	DefaultUpperdirRoot  = "/var/lib/disk"
	DefaultDiskDb        = "disk_manager"
	DefaultAddress       = "disk_manager.sock"
	DefaultRootfsSize    = 30
	DefaultTimerView     = 5 * time.Second
)

// SnapshotterConfig is used to configure the overlay snapshotter instance
type SnapshotterConfig struct {
	asyncRemove   bool
	upperdirLabel bool
	upperdirRoot  string
	diskTimeout   int
	rootfsQuota   int
	mountOptions  []string
	address       string
}

// Opt is an option to configure the overlay snapshotter
type Opt func(config *SnapshotterConfig) error

// AsynchronousRemove defers removal of filesystem content until
// the Cleanup method is called. Removals will make the snapshot
// referred to by the key unavailable and make the key immediately
// available for re-use.
func AsynchronousRemove(config *SnapshotterConfig) error {
	config.asyncRemove = true
	return nil
}

// WithUpperdirLabel adds as an optional label
// "containerd.io/snapshot/overlay.upperdir". This stores the location
// of the upperdir that contains the changeset between the labelled
// snapshot and its parent.
func WithUpperdirLabel(config *SnapshotterConfig) error {
	config.upperdirLabel = true
	return nil
}

func WithDiskTimeout(diskTimeout int) Opt {
	return func(config *SnapshotterConfig) error {
		config.diskTimeout = diskTimeout
		return nil
	}
}

func WithDiskAddress(address string) Opt {
	return func(config *SnapshotterConfig) error {
		config.address = address
		return nil
	}
}

func WithUpperdirRoot(ro string) Opt {
	return func(config *SnapshotterConfig) error {
		if err := os.MkdirAll(ro, 0700); err != nil {
			return err
		}
		config.upperdirRoot = ro
		return nil
	}
}

func WithRootfsQuota(quotaSize int) Opt {
	return func(config *SnapshotterConfig) error {
		config.rootfsQuota = quotaSize
		return nil
	}
}

// WithMountOptions defines the default mount options used for the overlay mount.
// NOTE: Options are not applied to bind mounts.
func WithMountOptions(options []string) Opt {
	return func(config *SnapshotterConfig) error {
		config.mountOptions = append(config.mountOptions, options...)
		return nil
	}
}

type snapshotter struct {
	root          string
	upperdirRoot  string
	rootfsQuota   int
	ms            *storage.MetaStore
	asyncRemove   bool
	upperdirLabel bool
	options       []string
	diskClient    *disk.DisksClient
}

// NewSnapshotter returns a Snapshotter which uses overlayfs. The overlayfs
// diffs are stored under the provided root. A metadata file is stored under
// the root.
func NewSnapshotter(root string, opts ...Opt) (snapshots.Snapshotter, error) {
	var config SnapshotterConfig
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	supportsDType, err := fs.SupportsDType(root)
	if err != nil {
		return nil, err
	}
	if !supportsDType {
		return nil, fmt.Errorf("%s does not support d_type. If the backing filesystem is xfs, please reformat with ftype=1 to enable d_type support", root)
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	if err := os.Mkdir(filepath.Join(root, "snapshots"), 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	if !hasOption(config.mountOptions, "userxattr", false) {
		// figure out whether "userxattr" option is recognized by the kernel && needed
		userxattr, err := overlayutils.NeedsUserXAttr(root)
		if err != nil {
			logrus.WithError(err).Warnf("cannot detect whether \"userxattr\" option needs to be used, assuming to be %v", userxattr)
		}
		if userxattr {
			config.mountOptions = append(config.mountOptions, "userxattr")
		}
	}

	if !hasOption(config.mountOptions, "index", false) && supportsIndex() {
		config.mountOptions = append(config.mountOptions, "index=off")
	}

	fmt.Printf("grpc address: %s\n", config.address)
	diskClient, err := disk.NewDisksClient(config.address)
	if err != nil {
		log.G(context.TODO()).Warn("failed to connect disk grpc")
	}
	return &snapshotter{
		root:          root,
		upperdirRoot:  config.upperdirRoot,
		rootfsQuota:   config.rootfsQuota,
		ms:            ms,
		asyncRemove:   config.asyncRemove,
		upperdirLabel: config.upperdirLabel,
		options:       config.mountOptions,
		diskClient:    diskClient,
	}, nil
}

func hasOption(options []string, key string, hasValue bool) bool {
	for _, option := range options {
		if hasValue {
			if strings.HasPrefix(option, key) && len(option) > len(key) && option[len(key)] == '=' {
				return true
			}
		} else if option == key {
			return true
		}
	}
	return false
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	defer t.Rollback()
	id, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return snapshots.Info{}, err
	}

	if o.upperdirLabel {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.upperPath(id, info.Labels)
	}

	return info, nil
}

func (o *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	info, err = storage.UpdateInfo(ctx, info, fieldpaths...)
	if err != nil {
		t.Rollback()
		return snapshots.Info{}, err
	}

	if o.upperdirLabel {
		id, _, _, err := storage.GetInfo(ctx, info.Name)
		if err != nil {
			return snapshots.Info{}, err
		}
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		info.Labels[upperdirKey] = o.upperPath(id, info.Labels)
	}

	if err := t.Commit(); err != nil {
		return snapshots.Info{}, err
	}

	return info, nil
}

// Usage returns the resources taken by the snapshot identified by key.
//
// For active snapshots, this will scan the usage of the overlay "diff" (aka
// "upper") directory and may take some time.
//
// For committed snapshots, the value is returned from the metadata database.
func (o *snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshots.Usage{}, err
	}
	id, info, usage, err := storage.GetInfo(ctx, key)
	t.Rollback() // transaction no longer needed at this point.

	if err != nil {
		return snapshots.Usage{}, err
	}

	if info.Kind == snapshots.KindActive {
		upperPath := o.upperPath(id, info.Labels)
		du, err := fs.DiskUsage(ctx, upperPath)
		if err != nil {
			// TODO(stevvooe): Consider not reporting an error in this case.
			return snapshots.Usage{}, err
		}

		usage = snapshots.Usage(du)
	}

	return usage, nil
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

func (o *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return o.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := storage.GetSnapshot(ctx, key)
	t.Rollback()
	if err != nil {
		return nil, fmt.Errorf("failed to get active mount: %w", err)
	}
	return o.mounts(s), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	// grab the existing id
	id, i, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return err
	}

	usage, err := fs.DiskUsage(ctx, o.upperPath(id, i.Labels))
	if err != nil {
		return err
	}

	if _, err = storage.CommitActive(ctx, key, name, snapshots.Usage(usage), opts...); err != nil {
		return fmt.Errorf("failed to commit snapshot: %w", err)
	}
	return t.Commit()
}

// Remove abandons the snapshot identified by key. The snapshot will
// immediately become unavailable and unrecoverable. Disk space will
// be freed up on the next call to `Cleanup`.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	_, info, _, err := storage.GetInfo(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to get snapshot info: %w", err)
	}
	_, _, err = storage.Remove(ctx, key)
	if err != nil {
		return fmt.Errorf("failed to remove: %w", err)
	}

	if !o.asyncRemove {
		//不删除upperdir
		if val, found := info.Labels[notebookLabelKey]; !found || val != notebookLabelValue {
			var removals []string
			removals, err = o.getCleanupDirectories(ctx, t)
			if err != nil {
				return fmt.Errorf("unable to get directories for removal: %w", err)
			}

			// Remove directories after the transaction is closed, failures must not
			// return error since the transaction is committed with the removal
			// key no longer available.
			defer func() {
				if err == nil {
					for _, dir := range removals {
						if err := os.RemoveAll(dir); err != nil {
							log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
						}
					}
				}
			}()
		}

	}

	return t.Commit()
}

// Walk the snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	if o.upperdirLabel {
		return storage.WalkInfo(ctx, func(ctx context.Context, info snapshots.Info) error {
			id, i, _, err := storage.GetInfo(ctx, info.Name)
			if err != nil {
				return err
			}
			if info.Labels == nil {
				info.Labels = make(map[string]string)
			}
			info.Labels[upperdirKey] = o.upperPath(id, i.Labels)
			return fn(ctx, info)
		}, fs...)
	}
	return storage.WalkInfo(ctx, fn, fs...)
}

// Cleanup cleans up disk resources from removed or abandoned snapshots
func (o *snapshotter) Cleanup(ctx context.Context) error {
	cleanup, err := o.cleanupDirectories(ctx)
	if err != nil {
		return err
	}

	for _, dir := range cleanup {
		if len(o.upperdirRoot) != 0 {
			if strings.Contains(dir, o.upperdirRoot) {
				continue
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			log.G(ctx).WithError(err).WithField("path", dir).Warn("failed to remove directory")
		}
	}

	return nil
}

func (o *snapshotter) cleanupDirectories(ctx context.Context) ([]string, error) {
	// Get a write transaction to ensure no other write transaction can be entered
	// while the cleanup is scanning.
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	defer t.Rollback()
	return o.getCleanupDirectories(ctx, t)
}

func (o *snapshotter) getCleanupDirectories(ctx context.Context, t storage.Transactor) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	snapshotDir := filepath.Join(o.root, "snapshots")
	fd, err := os.Open(snapshotDir)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		if _, ok := ids[d]; ok {
			continue
		}

		//孤儿目录如果是notebook应该保留
		cleanup = append(cleanup, filepath.Join(snapshotDir, d))
	}

	return cleanup, nil
}

func (o *snapshotter) createSnapshot(ctx context.Context, kind snapshots.Kind, key, parent string, opts []snapshots.Opt) (_ []mount.Mount, err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	var td, path string
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					log.G(ctx).WithError(err1).Warn("failed to cleanup temp snapshot directory")
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					log.G(ctx).WithError(err1).WithField("path", path).Error("failed to reclaim snapshot directory, directory may need removal")
					err = fmt.Errorf("failed to remove path: %v: %w", err1, err)
				}
			}
		}
	}()

	snapshotDir := filepath.Join(o.root, "snapshots")
	td, err = o.prepareDirectory(ctx, snapshotDir, kind)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
		}
		return nil, fmt.Errorf("failed to create prepare snapshot dir: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
		}
	}()

	s, err := storage.CreateSnapshot(ctx, kind, key, parent, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot: %w", err)
	}

	path = filepath.Join(snapshotDir, s.ID)
	doRename := true
	//通用限额和共享存储限额,默认xfs文件系统
	if val, found := s.Labels[notebookLabelKey]; found && val == notebookLabelValue {
		//rootfs配置限额,view.Prepare,参数key=====系统盘路径
		if _, found := s.Labels[notebookNameLabelKey]; !found {
			return nil, fmt.Errorf("failed to get jobID: %s", key)
		}
		if val, found := s.Labels[notebookActionKey]; found && val == notebookStartValue {
			doRename = false
			//判断系统盘是否准备就绪
			if _, found := s.Labels[versionKey]; !found {
				return nil, fmt.Errorf("failed to get found version: %s", key)
			}
			o.timeViewDisk(ctx, s.Labels[notebookNameLabelKey], s.Labels[versionKey], DefaultTimerView)
			td = filepath.Join(o.upperdirRoot, s.Labels[notebookNameLabelKey])
			isExist, err := o.dirExists(td)
			if err != nil {
				return nil, err
			}

			if !isExist {
				return nil, fmt.Errorf("no found upperdir: %s", td)
			}

		} else {
			path = filepath.Join(o.upperdirRoot, s.Labels[notebookNameLabelKey])
		}

	}

	if len(s.ParentIDs) > 0 {
		st, err := os.Stat(o.upperPath(s.ParentIDs[0], nil))
		if err != nil {
			return nil, fmt.Errorf("failed to stat parent: %w", err)
		}

		stat := st.Sys().(*syscall.Stat_t)

		if err := os.Lchown(filepath.Join(td, "fs"), int(stat.Uid), int(stat.Gid)); err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("failed to rollback transaction")
			}
			return nil, fmt.Errorf("failed to chown: %w", err)
		}
	}

	if doRename {
		if err := os.Rename(td, path); err != nil {
			log.G(ctx).WithError(err).Warnf("failed to rename file %s", path)
			//return nil, fmt.Errorf("failed to rename %q -> %q: %w", td, path, err)
		}
	}

	td = ""

	rollback = false
	if err = t.Commit(); err != nil {
		return nil, fmt.Errorf("commit failed: %w", err)
	}

	return o.mounts(s), nil
}

func (o *snapshotter) dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.IsDir(), nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}

	return false, err
}

func (o *snapshotter) prepareDirectory(ctx context.Context, snapshotDir string, kind snapshots.Kind) (string, error) {
	td, err := os.MkdirTemp(snapshotDir, "new-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	if err := os.Mkdir(filepath.Join(td, "fs"), 0755); err != nil {
		return td, err
	}

	if kind == snapshots.KindActive {
		if err := os.Mkdir(filepath.Join(td, "work"), 0711); err != nil {
			return td, err
		}
	}

	return td, nil
}
func (o *snapshotter) timeViewDisk(ctx context.Context, key, version string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			viewResp, err := o.diskClient.View(ctx, key)
			if err != nil {
				fmt.Printf("disk view failed: %s", err)
				continue
			}

			fmt.Printf("job id %s disk info: %v\n", key, viewResp.Disk)
			if viewResp.Disk.Status == "Completed" && viewResp.Disk.Version == version {
				return nil
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (o *snapshotter) mounts(s storage.Snapshot) []mount.Mount {
	if len(s.ParentIDs) == 0 {
		// if we only have one layer/no parents then just return a bind mount as overlay
		// will not work
		roFlag := "rw"
		if s.Kind == snapshots.KindView {
			roFlag = "ro"
		}

		return []mount.Mount{
			{
				Source: o.upperPath(s.ID, s.Labels),
				Type:   "bind",
				Options: []string{
					roFlag,
					"rbind",
				},
			},
		}
	}

	options := o.options
	if s.Kind == snapshots.KindActive {
		options = append(options,
			fmt.Sprintf("workdir=%s", o.workPath(s.ID, s.Labels)),
			fmt.Sprintf("upperdir=%s", o.upperPath(s.ID, s.Labels)),
		)

	} else if len(s.ParentIDs) == 1 {
		return []mount.Mount{
			{
				Source: o.upperPath(s.ParentIDs[0], s.Labels),
				Type:   "bind",
				Options: []string{
					"ro",
					"rbind",
				},
			},
		}
	}

	parentPaths := make([]string, len(s.ParentIDs))
	for i := range s.ParentIDs {
		parentPaths[i] = o.upperPath(s.ParentIDs[i], nil)
	}

	options = append(options, fmt.Sprintf("lowerdir=%s", strings.Join(parentPaths, ":")))
	return []mount.Mount{
		{
			Type:    "overlay",
			Source:  "overlay",
			Options: options,
		},
	}

}

func (o *snapshotter) upperPath(id string, labels map[string]string) string {
	if labels == nil {
		return filepath.Join(o.root, "snapshots", id, "fs")
	}
	if _, found := labels[notebookLabelKey]; found {
		return o.backupUpperPath(labels)
	}
	return filepath.Join(o.root, "snapshots", id, "fs")
}

func (o *snapshotter) workPath(id string, labels map[string]string) string {
	if labels == nil {
		return filepath.Join(o.root, "snapshots", id, "work")
	}
	if _, found := labels[notebookLabelKey]; found {
		return o.backupWorkPath(labels)
	}
	return filepath.Join(o.root, "snapshots", id, "work")
}

func (o *snapshotter) backupWorkPath(labels map[string]string) string {
	return filepath.Join(o.upperdirRoot, labels[notebookNameLabelKey], "work")
}

func (o *snapshotter) backupUpperPath(labels map[string]string) string {
	return filepath.Join(o.upperdirRoot, labels[notebookNameLabelKey], "fs")
}

// Close closes the snapshotter
func (o *snapshotter) Close() error {
	return o.ms.Close()
}

// supportsIndex checks whether the "index=off" option is supported by the kernel.
func supportsIndex() bool {
	if _, err := os.Stat("/sys/module/overlay/parameters/index"); err == nil {
		return true
	}
	return false
}
