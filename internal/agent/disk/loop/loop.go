// Package loop 用稀疏镜像文件 + losetup + ext4 实现数据盘；每一步都先自检，可从任意一步重跑。
package loop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
)

var idPattern = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)

func checkID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("非法实例 ID %q", id)
	}
	return nil
}

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
	if err := checkID(id); err != nil {
		return false, err
	}
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
	if err := checkID(id); err != nil {
		return err
	}
	img := d.image(id)
	want := int64(sizeGiB) << 30
	info, err := d.Stat(img)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if _, err := d.Run.Run(ctx, "mkdir", "-p", filepath.Dir(img)); err != nil {
			return err
		}
		if _, err := d.Run.Run(ctx, "truncate", "-s", strconv.FormatInt(want, 10), img); err != nil {
			return err
		}
	case err != nil:
		return err
	case info.Size() != want:
		return fmt.Errorf("数据盘镜像 %s 大小 %d 与期望 %d 不符", img, info.Size(), want)
	}
	if err := d.ensureExt4(ctx, img); err != nil {
		return err
	}
	dev, err := d.attach(ctx, img)
	if err != nil {
		return err
	}
	return d.mount(ctx, id, dev)
}

// Mount 重启恢复用：镜像必须已存在。
func (d *Disk) Mount(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
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

// ensureExt4：blkid 退出码 2（或 0 且无输出）表示没有文件系统才 mkfs；其它错误、非 ext4 的类型一律报错，绝不强制格式化。
func (d *Disk) ensureExt4(ctx context.Context, img string) error {
	out, err := d.Run.Run(ctx, "blkid", "-o", "value", "-s", "TYPE", img)
	var ce *cmdrun.CmdError
	switch {
	case err == nil:
		switch typ := strings.TrimSpace(string(out)); typ {
		case "ext4":
			return nil
		case "":
		default:
			return fmt.Errorf("数据盘镜像 %s 已有 %s 文件系统，拒绝重新格式化", img, typ)
		}
	case errors.As(err, &ce) && ce.ExitCode == 2:
	default:
		return fmt.Errorf("blkid %s: %w", img, err)
	}
	// -F：对普通文件（非块设备）建文件系统不要交互确认
	_, err = d.Run.Run(ctx, "mkfs.ext4", "-q", "-F", "-E", "lazy_itable_init=1,nodiscard", img)
	return err
}

// attach 返回镜像对应的 loop 设备，没有则新建。
func (d *Disk) attach(ctx context.Context, img string) (string, error) {
	devs, err := d.loopDevs(ctx, img)
	if err != nil {
		return "", err
	}
	if len(devs) > 0 {
		return devs[0], nil
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

// loopDevs 解析 `losetup -j <img>` 的每一行 "/dev/loop5: []: (<img>)"；没有附加返回空。
func (d *Disk) loopDevs(ctx context.Context, img string) ([]string, error) {
	out, err := d.Run.Run(ctx, "losetup", "-j", img)
	if err != nil {
		return nil, err
	}
	var devs []string
	for _, line := range strings.Split(string(out), "\n") {
		if dev, _, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && dev != "" {
			devs = append(devs, strings.TrimSpace(dev))
		}
	}
	return devs, nil
}

// isMounted：mountpoint 退出码 0=是，1（非挂载点/不存在）与 32（目录但不是挂载点）=否，其它视为错误。
func (d *Disk) isMounted(ctx context.Context, dir string) (bool, error) {
	_, err := d.Run.Run(ctx, "mountpoint", "-q", dir)
	if err == nil {
		return true, nil
	}
	var ce *cmdrun.CmdError
	if errors.As(err, &ce) && (ce.ExitCode == 1 || ce.ExitCode == 32) {
		return false, nil
	}
	return false, err
}

// mount：只在尚未挂载时建目录、设不可变属性并挂载。chattr +i 必须在挂载之前——已挂载后再执行会落到数据盘 ext4 的根目录，
// 之后容器往 /root/data 写任何东西都是 EPERM。
func (d *Disk) mount(ctx context.Context, id, dev string) error {
	dir := d.MountDir(id)
	mounted, err := d.isMounted(ctx, dir)
	if err != nil {
		return err
	}
	if !mounted {
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
	if err := checkID(id); err != nil {
		return false, err
	}
	mounted, err := d.isMounted(ctx, d.MountDir(id))
	if err != nil || !mounted {
		return false, err
	}
	devs, err := d.loopDevs(ctx, d.image(id))
	if err != nil || len(devs) == 0 {
		return false, err
	}
	return d.verifySource(ctx, id, devs[0]) == nil, nil
}

// Destroy：卸载 → 卸下镜像的全部 loop 设备（并复查）→ 去掉不可变属性并删目录 → 删镜像 → fstrim / → 校验全部消失。
func (d *Disk) Destroy(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	img, dir := d.image(id), d.MountDir(id)
	mounted, err := d.isMounted(ctx, dir)
	if err != nil {
		return err
	}
	if mounted {
		if _, err := d.Run.Run(ctx, "umount", dir); err != nil {
			return err
		}
	}
	devs, err := d.loopDevs(ctx, img)
	if err != nil {
		return err
	}
	for _, dev := range devs {
		if _, err := d.Run.Run(ctx, "losetup", "-d", dev); err != nil {
			return err
		}
	}
	if len(devs) > 0 { // losetup -d 在设备忙时只会置 autoclear；删镜像前必须确认真的卸下了
		left, err := d.loopDevs(ctx, img)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return fmt.Errorf("洗机校验失败：loop 设备仍附加 %s，未删除镜像", strings.Join(left, ","))
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
	if m, err := d.isMounted(ctx, dir); err != nil {
		problems = append(problems, "无法确认挂载状态: "+err.Error())
	} else if m {
		problems = append(problems, "挂载点仍挂载")
	}
	// 镜像已删，losetup -j 查不到了；按设备查它是否还引用着该镜像（设备已不存在则命令报错，视为已消失）
	for _, dev := range devs {
		out, err := d.Run.Run(ctx, "losetup", "-l", "-n", "-O", "BACK-FILE", dev)
		if err == nil && strings.Contains(string(out), img) {
			problems = append(problems, "loop 设备仍附加 "+dev)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("洗机校验失败：%s", strings.Join(problems, "；"))
	}
	return nil
}
