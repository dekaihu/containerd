package xfs

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/containerd/containerd/snapshots/overlay/overlayutils"
	overlay "github.com/containerd/containerd/snapshots/overlay/plugin"
	"github.com/containerd/containerd/snapshots/overlay/quota"
)

var (
	storageType = "xfs"
)


func init() {
	overlay.RegisterQuotaPlugin(storageType, NewXfs)
}

type xfs struct {
	root       string
	mountPoint string
}

func WithProjectID(id string) quota.Opt {
	return func(config *quota.QuotaInfo) error {
		config.ID = id
		return nil
	}
}

func WithHardQuotaSize(size int) quota.Opt {
	return func(config *quota.QuotaInfo) error {
		config.HardQuota = size
		return nil
	}
}

func WithSoftQuotaSize(size int) quota.Opt {
	return func(config *quota.QuotaInfo) error {
		config.SoftQuota = size
		return nil
	}
}




func checkXFSQuota(root string) (bool, string, error) {
	cmd := fmt.Sprintf("findmnt -T %s -o TARGET,FSTYPE,OPTIONS -n", root)
	out, err := overlayutils.ExecuteShell(cmd)
	if err != nil {
		return false, "", err
	}

	fields := strings.Fields(out)
	if len(fields) < 3 {
		return false, "", fmt.Errorf("unexpected findmnt output: %s", out)
	}

	target := fields[0]
	fsType := fields[1]
	options := fields[2]

	if fsType != storageType {
		return false, target, nil
	}
	isXFSQuota := strings.Contains(options, "prjquota")

	return isXFSQuota, target, nil
}

func NewXfs(root string) (quota.RootfsQuota, error) {
	var x = xfs{}
	isXfsQuota, mountPoint, err := checkXFSQuota(root)
	if err != nil {
		return &x, err
	}

	if !isXfsQuota {
		return &x, fmt.Errorf("file system does not support quotas")
	}

	x.root = root
	x.mountPoint = mountPoint
	return &x, nil
}

func (x *xfs) CreateRootfsQuota(ctx context.Context, key string, opts ...quota.Opt) (*quota.QuotaInfo, error) {
	var quotaInfo quota.QuotaInfo
	for _, opt := range opts {
		if err := opt(&quotaInfo); err != nil {
			return nil, err
		}
	}

	if !strings.Contains(key, x.root) {
		return nil, nil
	}

	//限额，使用容器默认snapshot id
	cmd := fmt.Sprintf("xfs_quota -x -c 'project -s -p %s %s'", key, quotaInfo.ID)
	_, err := overlayutils.ExecuteShell(cmd)
	if err != nil {
		return nil, fmt.Errorf("exec project xfs_quota.CreateRootfsQuota id %s, failed: %s", quotaInfo.ID, err)
	}

	cmd = fmt.Sprintf("xfs_quota -x -c 'limit -p bsoft=%dg bhard=%dg %s' %s", quotaInfo.SoftQuota, quotaInfo.HardQuota, quotaInfo.ID, x.mountPoint)
	_, err = overlayutils.ExecuteShell(cmd)
	if err != nil {
		return nil, fmt.Errorf("exec limit xfs_quota.CreateRootfsQuota id %s, failed: %s", quotaInfo.ID, err)
	}

	return &quotaInfo, nil
}

func (x *xfs) ListRootfsQuota(ctx context.Context) ([]quota.QuotaInfo, error) {
	return nil, nil
}

func (x *xfs) UpdateRootfsQuota(ctx context.Context, key string, opts ...quota.Opt) (*quota.QuotaInfo, error) {
	var quotaInfo quota.QuotaInfo
	for _, opt := range opts {
		if err := opt(&quotaInfo); err != nil {
			return nil, err
		}
	}

	if !strings.Contains(key, x.root) {
		return nil, nil
	}

	isExist, err := x.checkIsAlreadySetQuota(key)
	if err != nil {
		return nil, fmt.Errorf("quota changes are not supported, err: %s", err)
	}
	if !isExist {
		return nil, fmt.Errorf("quota changes are not supported")
	}

	cmd := fmt.Sprintf("xfs_quota -x -c 'limit -p bsoft=%dg bhard=%dg %s' %s", quotaInfo.SoftQuota, quotaInfo.HardQuota, quotaInfo.ID, x.mountPoint)
	_, err = overlayutils.ExecuteShell(cmd)
	if err != nil {
		return nil, fmt.Errorf("adjustment limit failed: %s", err)
	}

	return nil, nil
}

func (x *xfs) checkIsAlreadySetQuota(key string) (bool, error) {
	cmd := fmt.Sprintf("xfs_io -c 'stat' %s | grep projid", key)
	out, err := overlayutils.ExecuteShell(cmd)
	if err != nil {
		return false, fmt.Errorf("exec xfs_io get path %s projid failed: %s", key, err)
	}

	re := regexp.MustCompile(`projid\s*=\s*(\d+)`)
	matches := re.FindStringSubmatch(out)
	if len(matches) < 2 {
		return false, fmt.Errorf("not match projid")
	}

	projid, err := strconv.Atoi(matches[1])
	if err != nil {
		return true, nil
	}

	return projid > 0, nil
}

func (x *xfs) GetRootfsQuota(ctx context.Context, key string) (*quota.QuotaInfo, error) {
	return nil, nil
}

func (x *xfs) DeleteRootfsQuota(ctx context.Context, key string) error {
	if !x.dirExists(key) {
		return nil
	}

	cmd := fmt.Sprintf("xfs_io -c 'chproj 0' %s", key)
	_, err := overlayutils.ExecuteShell(cmd)
	if err != nil {
		return fmt.Errorf("exec xfs_quota.DeleteRootfsQuota id %s, failed: %s", key, err)
	}
	return nil
}

func (x *xfs) dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return !os.IsNotExist(err)
	}

	return info.IsDir()
}
