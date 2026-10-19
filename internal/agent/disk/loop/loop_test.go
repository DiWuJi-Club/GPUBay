package loop_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/disk/loop"
)

type fakeInfo struct{ size int64 }

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return 0o600 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return nil }

func newDisk(files map[string]int64) (*loop.Disk, *cmdrun.Recorder) {
	r := &cmdrun.Recorder{}
	d := &loop.Disk{Run: r, StateDir: "/var/lib/spark-agent", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Stat: func(p string) (os.FileInfo, error) {
		if size, ok := files[p]; ok {
			return fakeInfo{size}, nil
		}
		return nil, os.ErrNotExist
	}}
	return d, r
}

const gib = int64(1) << 30

func TestCreateFromScratchSequence(t *testing.T) {
	files := map[string]int64{}
	d, r := newDisk(files)
	r.Stub("blkid", "", &cmdrun.CmdError{ExitCode: 2})
	r.Stub("losetup -j", "", nil)
	r.Stub("losetup --find --show", "/dev/loop3\n", nil)
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("findmnt", "/dev/loop3\n", nil)
	// truncate 之后镜像文件就存在了
	r.Stub("truncate", "", nil)
	if err := d.Create(context.Background(), "ins-aaaaaaaa", 2); err != nil {
		t.Fatal(err)
	}
	want := "mkdir -p /var/lib/spark-agent/data\n" +
		"truncate -s 2147483648 /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"blkid -o value -s TYPE /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"mkfs.ext4 -q -F -E lazy_itable_init=1,nodiscard /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"losetup --find --show /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"mkdir -p /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"chattr +i /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"mount -o nosuid,nodev /dev/loop3 /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"findmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-aaaaaaaa"
	if r.Lines() != want {
		t.Fatalf("create:\n got:\n%s\nwant:\n%s", r.Lines(), want)
	}
}

func TestCreateSkipsCompletedSteps(t *testing.T) {
	files := map[string]int64{"/var/lib/spark-agent/data/ins-aaaaaaaa.img": 2 * gib}
	d, r := newDisk(files)
	r.Stub("blkid", "ext4\n", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-aaaaaaaa.img)\n", nil)
	r.Stub("mountpoint -q", "", nil)
	r.Stub("findmnt", "/dev/loop5\n", nil)
	if err := d.Create(context.Background(), "ins-aaaaaaaa", 2); err != nil {
		t.Fatal(err)
	}
	// 已挂载：不再 mkdir / chattr（chattr +i 落到已挂载的 ext4 根目录会让容器写 /root/data 失败）
	want := "blkid -o value -s TYPE /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"findmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-aaaaaaaa"
	if r.Lines() != want {
		t.Fatalf("idempotent create:\n%s", r.Lines())
	}
	// 镜像大小不符 → 拒绝（不能缩小用户数据）
	files["/var/lib/spark-agent/data/ins-aaaaaaaa.img"] = gib
	if err := d.Create(context.Background(), "ins-aaaaaaaa", 2); err == nil {
		t.Fatal("size mismatch accepted")
	}
}

func TestCreateFailsWhenMountSourceIsWrong(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-aaaaaaaa.img": 2 * gib})
	r.Stub("blkid", "ext4\n", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-aaaaaaaa.img)\n", nil)
	r.Stub("mountpoint -q", "", nil)
	r.Stub("findmnt", "/dev/loop9\n", nil)
	if err := d.Create(context.Background(), "ins-aaaaaaaa", 2); err == nil {
		t.Fatal("foreign mount accepted")
	}
	mounted, err := d.Mounted(context.Background(), "ins-aaaaaaaa")
	if err != nil || mounted {
		t.Fatalf("mounted = %v %v", mounted, err)
	}
}

func TestMountedAndMount(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-aaaaaaaa.img": 2 * gib})
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 1})
	if m, _ := d.Mounted(context.Background(), "ins-aaaaaaaa"); m {
		t.Fatal("unmounted reported mounted")
	}
	r.Reset()
	r.Stub("losetup -j", "", nil)
	r.Stub("losetup --find --show", "/dev/loop2\n", nil)
	r.Stub("findmnt", "/dev/loop2\n", nil)
	if err := d.Mount(context.Background(), "ins-aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	want := "losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img\nlosetup --find --show /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa\nmkdir -p /var/lib/spark-agent/mnt/ins-aaaaaaaa\nchattr +i /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"mount -o nosuid,nodev /dev/loop2 /var/lib/spark-agent/mnt/ins-aaaaaaaa\nfindmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-aaaaaaaa"
	if r.Lines() != want {
		t.Fatalf("mount:\n%s", r.Lines())
	}
	if err := d.Mount(context.Background(), "ins-nnnnnnnn"); err == nil {
		t.Fatal("mount without image must fail")
	}
}

// 什么都没挂载、没附加时的洗机：不调 umount / losetup -d；fstrim 失败只记日志；校验通过。
func TestDestroySequenceWhenNothingMounted(t *testing.T) {
	d, r := newDisk(map[string]int64{}) // Stat 一律 NotExist：rm 之后镜像不存在
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("losetup -j", "", nil)
	r.Stub("fstrim", "", errors.New("fstrim unsupported"))
	if err := d.Destroy(context.Background(), "ins-aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	want := "mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"chattr -i /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"rmdir /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"rm -f /var/lib/spark-agent/data/ins-aaaaaaaa.img\n" +
		"fstrim /\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa"
	if r.Lines() != want {
		t.Fatalf("destroy (nothing mounted):\n%s", r.Lines())
	}
}

// 已挂载、已附加：先 umount，再 losetup -d，复查仍附加（stub 恒返回已附加）→ 失败且不删镜像。
func TestDestroyFailsWhenLoopDeviceStaysAttached(t *testing.T) {
	d, r := newDisk(map[string]int64{})
	r.Stub("mountpoint -q", "", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-aaaaaaaa.img)\n/dev/loop6: []: (/var/lib/spark-agent/data/ins-aaaaaaaa.img)\n", nil)
	err := d.Destroy(context.Background(), "ins-aaaaaaaa")
	if err == nil || !strings.Contains(err.Error(), "洗机校验失败") || !strings.Contains(err.Error(), "loop 设备仍附加 /dev/loop5,/dev/loop6") {
		t.Fatalf("verification = %v", err)
	}
	want := "mountpoint -q /var/lib/spark-agent/mnt/ins-aaaaaaaa\numount /var/lib/spark-agent/mnt/ins-aaaaaaaa\n" +
		"losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img\nlosetup -d /dev/loop5\nlosetup -d /dev/loop6\n" +
		"losetup -j /var/lib/spark-agent/data/ins-aaaaaaaa.img"
	if r.Lines() != want {
		t.Fatalf("destroy must unmount, detach every device, re-check:\n%s", r.Lines())
	}
}

// 删除之后仍挂载 → 校验失败；删除后还按设备确认不再引用镜像。
func TestDestroyPostRemoveVerification(t *testing.T) {
	d, r := newDisk(map[string]int64{})
	r.Stub("mountpoint -q", "", nil)
	r.Stub("losetup -j", "", nil)
	err := d.Destroy(context.Background(), "ins-aaaaaaaa")
	if err == nil || !strings.Contains(err.Error(), "挂载点仍挂载") {
		t.Fatalf("verification = %v", err)
	}
	// umount 失败直接返回错误，不继续删镜像
	r.Reset()
	r.Stub("umount", "", &cmdrun.CmdError{ExitCode: 32, Stderr: "target is busy"})
	if err := d.Destroy(context.Background(), "ins-aaaaaaaa"); err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("umount failure = %v", err)
	}
	if strings.Contains(r.Lines(), "rm -f") {
		t.Fatal("must not delete the image after a failed umount")
	}
}

func TestDestroyDetachesAllAndChecksByDevice(t *testing.T) {
	d, r := newDisk(map[string]int64{})
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 32})
	// 第一次 -j 有两个设备，detach 之后复查为空：用带状态的 runner 包装
	calls := 0
	d.Run = runnerFunc(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		line := cmdrun.Line(name, args...)
		if strings.HasPrefix(line, "losetup -j") {
			calls++
			if calls == 1 {
				return []byte("/dev/loop5: []: (x)\n/dev/loop6: []: (x)\n"), nil
			}
			return nil, nil
		}
		if line == "losetup -l -n -O BACK-FILE /dev/loop6" {
			return []byte("/var/lib/spark-agent/data/ins-aaaaaaaa.img (deleted)\n"), nil
		}
		return r.Run(ctx, name, args...)
	})
	err := d.Destroy(context.Background(), "ins-aaaaaaaa")
	if err == nil || !strings.Contains(err.Error(), "loop 设备仍附加 /dev/loop6") || strings.Contains(err.Error(), "/dev/loop5") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(r.Lines(), "losetup -d /dev/loop5\nlosetup -d /dev/loop6\n") {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}

type runnerFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func (f runnerFunc) RunInput(ctx context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestBlkidDecidesMkfs(t *testing.T) {
	img := "/var/lib/spark-agent/data/ins-aaaaaaaa.img"
	run := func(out string, err error) (string, error) {
		d, r := newDisk(map[string]int64{img: 2 * gib})
		r.Stub("blkid", out, err)
		r.Stub("losetup -j", "/dev/loop5: []: ("+img+")\n", nil)
		r.Stub("mountpoint -q", "", nil)
		r.Stub("findmnt", "/dev/loop5\n", nil)
		e := d.Create(context.Background(), "ins-aaaaaaaa", 2)
		return r.Lines(), e
	}
	if l, err := run("", &cmdrun.CmdError{ExitCode: 2}); err != nil || !strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("rc2: %v\n%s", err, l)
	}
	if l, err := run("", nil); err != nil || !strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("rc0 empty: %v\n%s", err, l)
	}
	if l, err := run("ext4\n", nil); err != nil || strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("ext4: %v\n%s", err, l)
	}
	if l, err := run("", &cmdrun.CmdError{ExitCode: 4, Stderr: "io"}); err == nil || strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("rc4: %v\n%s", err, l)
	}
	if l, err := run("", errors.New("exec failed")); err == nil || strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("exec err: %v\n%s", err, l)
	}
	if l, err := run("xfs\n", nil); err == nil || strings.Contains(l, "mkfs.ext4") {
		t.Fatalf("xfs: %v\n%s", err, l)
	}
}

func TestMountpointExitCodes(t *testing.T) {
	img := "/var/lib/spark-agent/data/ins-aaaaaaaa.img"
	for _, code := range []int{1, 32} {
		d, r := newDisk(map[string]int64{img: 2 * gib})
		r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: code})
		if m, err := d.Mounted(context.Background(), "ins-aaaaaaaa"); m || err != nil {
			t.Fatalf("rc%d = %v %v", code, m, err)
		}
	}
	d, r := newDisk(map[string]int64{img: 2 * gib})
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 5, Stderr: "boom"})
	if _, err := d.Mounted(context.Background(), "ins-aaaaaaaa"); err == nil {
		t.Fatal("other rc must be an error")
	}
	if err := d.Mount(context.Background(), "ins-aaaaaaaa"); err == nil {
		t.Fatal("mount must propagate mountpoint error")
	}
	// losetup -j 出错不能当作「未附加」
	d, r = newDisk(map[string]int64{img: 2 * gib})
	r.Stub("losetup -j", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "boom"})
	if err := d.Mount(context.Background(), "ins-aaaaaaaa"); err == nil || strings.Contains(r.Lines(), "--find") {
		t.Fatalf("losetup -j error swallowed: %v", err)
	}
}

func TestInvalidIDsRejected(t *testing.T) {
	d, r := newDisk(map[string]int64{})
	ctx := context.Background()
	for _, id := range []string{"", "../etc", "ins-a", "ins-AAAAAAAA", "ins-aaaaaaaa/../x", "ins-aaaaaaaaa"} {
		if d.Create(ctx, id, 1) == nil || d.Mount(ctx, id) == nil || d.Destroy(ctx, id) == nil {
			t.Fatalf("id %q accepted", id)
		}
		if _, err := d.Mounted(ctx, id); err == nil {
			t.Fatalf("Mounted accepted %q", id)
		}
		if _, err := d.Exists(ctx, id); err == nil {
			t.Fatalf("Exists accepted %q", id)
		}
	}
	if r.Lines() != "" {
		t.Fatalf("commands ran for invalid id:\n%s", r.Lines())
	}
}

func TestFreeBytesAndExists(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-aaaaaaaa.img": 1})
	r.Stub("df", "Avail\n123456\n", nil)
	free, err := d.FreeBytes(context.Background())
	if err != nil || free != 123456 || r.Lines() != "df -B1 --output=avail /var/lib/spark-agent" {
		t.Fatalf("free = %d %v calls=%s", free, err, r.Lines())
	}
	if ok, _ := d.Exists(context.Background(), "ins-aaaaaaaa"); !ok {
		t.Fatal("exists")
	}
	if ok, _ := d.Exists(context.Background(), "ins-bbbbbbbb"); ok {
		t.Fatal("not exists")
	}
	if d.MountDir("ins-aaaaaaaa") != "/var/lib/spark-agent/mnt/ins-aaaaaaaa" {
		t.Fatal(d.MountDir("ins-aaaaaaaa"))
	}
}
