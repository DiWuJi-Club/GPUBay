// Package loop 用稀疏镜像文件 + losetup + ext4 实现数据盘；每一步都先自检，可从任意一步重跑。
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
)

type Disk struct {
	Run      cmdrun.Runner
	Stat     func(string) (os.FileInfo, error)
	StateDir string // /var/lib/spark-agent
	Log      *slog.Logger
}

func (d *Disk) image(id string) string    { return filepath.Join(d.StateDir, "data", id+".img") }
func (d *Disk) MountDir(id string) string { return filepath.Join(d.StateDir, "mnt", id) }

func (d *Disk) log() *slog.Logger {
	if d.Log == nil {
		return slog.Default()
	}
	return d.Log
}

func (d *Disk) Exists(_ context.Context, id string) (bool, error) {
	_, err := d.Stat(d.image(id))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (d *Disk) FreeBytes(ctx context.Context) (uint64, error) {
	out, err := d.Run.Run(ctx, "df", "-B1", "--output=avail", d.StateDir)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strconv.ParseUint(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
}

// Create：镜像文件（存在且大小正确则跳过）→ mkfs（已是 ext4 则跳过）→ losetup（已附加则复用）→ 挂载（已挂载则跳过）→ 校验挂载源。
func (d *Disk) Create(ctx context.Context, id string, sizeGiB int) error {
	img := d.image(id)
	want := int64(sizeGiB) << 30
	info, err := d.Stat(img)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := d.Run.Run(ctx, "truncate", "-s", strconv.FormatInt(want, 10), img); err != nil {
			return err
		}
	case err != nil:
		return err
	case info.Size() != want:
		return fmt.Errorf("数据盘镜像 %s 大小 %d 与期望 %d 不符", img, info.Size(), want)
	}
	out, err := d.Run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", img)
	if err != nil || strings.TrimSpace(string(out)) != "ext4" { // blkid 对无文件系统的文件退出码 2
		// -F：对普通文件（非块设备）建文件系统不要交互确认
		if _, err := d.Run.Run(ctx, "mkfs.ext4", "-q", "-F", "-E", "lazy_itable_init=1,nodiscard", img); err != nil {
			return err
		}
	}
	dev, err := d.attach(ctx, img)
	if err != nil {
		return err
	}
	return d.mount(ctx, id, dev)
}

// Mount 重启恢复用：镜像必须已存在。
func (d *Disk) Mount(ctx context.Context, id string) error {
	img := d.image(id)
	if _, err := d.Stat(img); err != nil {
		return fmt.Errorf("数据盘镜像不存在: %w", err)
	}
	dev, err := d.attach(ctx, img)
	if err != nil {
		return err
	}
	return d.mount(ctx, id, dev)
}

// attach 返回镜像对应的 loop 设备，没有则新建。
func (d *Disk) attach(ctx context.Context, img string) (string, error) {
	if dev := d.loopOf(ctx, img); dev != "" {
		return dev, nil
	}
	out, err := d.Run.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		return "", err
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", errors.New("losetup 没有返回设备名")
	}
	return dev, nil
}

// loopOf 解析 `losetup -j <img>` 的 "/dev/loop5: []: (<img>)"；没有附加返回空串。
func (d *Disk) loopOf(ctx context.Context, img string) string {
	out, err := d.Run.Run(ctx, "losetup", "-j", img)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	dev, _, ok := strings.Cut(line, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(dev)
}

// mount：只在尚未挂载时建目录、设不可变属性并挂载。chattr +i 必须在挂载之前——已挂载后再执行会落到数据盘 ext4 的根目录，
// 之后容器往 /root/data 写任何东西都是 EPERM。
func (d *Disk) mount(ctx context.Context, id, dev string) error {
	dir := d.MountDir(id)
	if _, err := d.Run.Run(ctx, "mountpoint", "-q", dir); err != nil {
		if _, err := d.Run.Run(ctx, "mkdir", "-p", dir); err != nil {
			return err
		}
		if _, err := d.Run.Run(ctx, "chattr", "+i", dir); err != nil { // 未挂载时任何写入都失败
			return err
		}
		if _, err := d.Run.Run(ctx, "mount", "-o", "nosuid,nodev", dev, dir); err != nil {
			return err
		}
	}
	return d.verifySource(ctx, id, dev)
}

func (d *Disk) verifySource(ctx context.Context, id, dev string) error {
	out, err := d.Run.Run(ctx, "findmnt", "-n", "-o", "SOURCE", d.MountDir(id))
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != dev {
		return fmt.Errorf("数据盘未挂载：%s 的挂载源是 %q，期望 %s", d.MountDir(id), got, dev)
	}
	return nil
}

// Mounted：已挂载且挂载源是该镜像的 loop 设备。
func (d *Disk) Mounted(ctx context.Context, id string) (bool, error) {
	if _, err := d.Run.Run(ctx, "mountpoint", "-q", d.MountDir(id)); err != nil {
		return false, nil
	}
	dev := d.loopOf(ctx, d.image(id))
	if dev == "" {
		return false, nil
	}
	return d.verifySource(ctx, id, dev) == nil, nil
}

// Destroy：卸载 → losetup -d → 去掉不可变属性并删目录 → 删镜像 → fstrim / → 校验全部消失。
func (d *Disk) Destroy(ctx context.Context, id string) error {
	img, dir := d.image(id), d.MountDir(id)
	if _, err := d.Run.Run(ctx, "mountpoint", "-q", dir); err == nil {
		if _, err := d.Run.Run(ctx, "umount", dir); err != nil {
			return err
		}
	}
	if dev := d.loopOf(ctx, img); dev != "" {
		if _, err := d.Run.Run(ctx, "losetup", "-d", dev); err != nil {
			return err
		}
	}
	if _, err := d.Run.Run(ctx, "chattr", "-i", dir); err != nil {
		d.log().Warn("chattr -i", "dir", dir, "err", err)
	}
	if _, err := d.Run.Run(ctx, "rmdir", dir); err != nil {
		d.log().Warn("rmdir", "dir", dir, "err", err)
	}
	if _, err := d.Run.Run(ctx, "rm", "-f", img); err != nil {
		return err
	}
	if _, err := d.Run.Run(ctx, "fstrim", "/"); err != nil {
		d.log().Warn("fstrim failed", "err", err)
	}
	var problems []string
	if _, err := d.Stat(img); !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "镜像文件仍存在")
	}
	if _, err := d.Run.Run(ctx, "mountpoint", "-q", dir); err == nil {
		problems = append(problems, "挂载点仍挂载")
	}
	if d.loopOf(ctx, img) != "" {
		problems = append(problems, "loop 设备仍附加")
	}
	if len(problems) > 0 {
		return fmt.Errorf("洗机校验失败：%s", strings.Join(problems, "；"))
	}
	return nil
}
