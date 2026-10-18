// Package disk 抽象实例数据盘：生产 loop（稀疏镜像 + losetup + ext4），测试 fake。
package disk

import "context"

type Disk interface {
	Create(ctx context.Context, id string, sizeGiB int) error
	Mount(ctx context.Context, id string) error
	Mounted(ctx context.Context, id string) (bool, error)
	Destroy(ctx context.Context, id string) error
	Exists(ctx context.Context, id string) (bool, error)
	FreeBytes(ctx context.Context) (uint64, error)
	MountDir(id string) string
}
