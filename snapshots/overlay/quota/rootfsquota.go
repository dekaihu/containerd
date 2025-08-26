package quota

import "context"

type QuotaInfo struct {
	ID        string
	HardQuota int
	SoftQuota int
}

type Opt func(quotaInfo *QuotaInfo) error

type RootfsQuota interface {
	CreateRootfsQuota(ctx context.Context, key string, opts ...Opt) (*QuotaInfo, error)
	ListRootfsQuota(ctx context.Context) ([]QuotaInfo, error)
	UpdateRootfsQuota(ctx context.Context, key string, opts ...Opt) (*QuotaInfo, error)
	GetRootfsQuota(ctx context.Context, key string) (*QuotaInfo, error)
	DeleteRootfsQuota(ctx context.Context, key string) error
}
