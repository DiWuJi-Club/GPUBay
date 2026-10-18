package loop_test

import (
	"context"
	"errors"
	"io/fs"
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
	d := &loop.Disk{Run: r, StateDir: "/var/lib/spark-agent", Stat: func(p string) (os.FileInfo, error) {
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
	if err := d.Create(context.Background(), "ins-a", 2); err != nil {
		t.Fatal(err)
	}
	want := "truncate -s 2147483648 /var/lib/spark-agent/data/ins-a.img\n" +
		"blkid -o value -s TYPE /var/lib/spark-agent/data/ins-a.img\n" +
		"mkfs.ext4 -q -F -E lazy_itable_init=1,nodiscard /var/lib/spark-agent/data/ins-a.img\n" +
		"losetup -j /var/lib/spark-agent/data/ins-a.img\n" +
		"losetup --find --show /var/lib/spark-agent/data/ins-a.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-a\n" +
		"mkdir -p /var/lib/spark-agent/mnt/ins-a\n" +
		"chattr +i /var/lib/spark-agent/mnt/ins-a\n" +
		"mount -o nosuid,nodev /dev/loop3 /var/lib/spark-agent/mnt/ins-a\n" +
		"findmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-a"
	if r.Lines() != want {
		t.Fatalf("create:\n got:\n%s\nwant:\n%s", r.Lines(), want)
	}
}

func TestCreateSkipsCompletedSteps(t *testing.T) {
	files := map[string]int64{"/var/lib/spark-agent/data/ins-a.img": 2 * gib}
	d, r := newDisk(files)
	r.Stub("blkid", "ext4\n", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-a.img)\n", nil)
	r.Stub("mountpoint -q", "", nil)
	r.Stub("findmnt", "/dev/loop5\n", nil)
	if err := d.Create(context.Background(), "ins-a", 2); err != nil {
		t.Fatal(err)
	}
	// 已挂载：不再 mkdir / chattr（chattr +i 落到已挂载的 ext4 根目录会让容器写 /root/data 失败）
	want := "blkid -o value -s TYPE /var/lib/spark-agent/data/ins-a.img\n" +
		"losetup -j /var/lib/spark-agent/data/ins-a.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-a\n" +
		"findmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-a"
	if r.Lines() != want {
		t.Fatalf("idempotent create:\n%s", r.Lines())
	}
	// 镜像大小不符 → 拒绝（不能缩小用户数据）
	files["/var/lib/spark-agent/data/ins-a.img"] = gib
	if err := d.Create(context.Background(), "ins-a", 2); err == nil {
		t.Fatal("size mismatch accepted")
	}
}

func TestCreateFailsWhenMountSourceIsWrong(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-a.img": 2 * gib})
	r.Stub("blkid", "ext4\n", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-a.img)\n", nil)
	r.Stub("mountpoint -q", "", nil)
	r.Stub("findmnt", "/dev/loop9\n", nil)
	if err := d.Create(context.Background(), "ins-a", 2); err == nil {
		t.Fatal("foreign mount accepted")
	}
	mounted, err := d.Mounted(context.Background(), "ins-a")
	if err != nil || mounted {
		t.Fatalf("mounted = %v %v", mounted, err)
	}
}

func TestMountedAndMount(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-a.img": 2 * gib})
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 1})
	if m, _ := d.Mounted(context.Background(), "ins-a"); m {
		t.Fatal("unmounted reported mounted")
	}
	r.Reset()
	r.Stub("losetup -j", "", nil)
	r.Stub("losetup --find --show", "/dev/loop2\n", nil)
	r.Stub("findmnt", "/dev/loop2\n", nil)
	if err := d.Mount(context.Background(), "ins-a"); err != nil {
		t.Fatal(err)
	}
	want := "losetup -j /var/lib/spark-agent/data/ins-a.img\nlosetup --find --show /var/lib/spark-agent/data/ins-a.img\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-a\nmkdir -p /var/lib/spark-agent/mnt/ins-a\nchattr +i /var/lib/spark-agent/mnt/ins-a\n" +
		"mount -o nosuid,nodev /dev/loop2 /var/lib/spark-agent/mnt/ins-a\nfindmnt -n -o SOURCE /var/lib/spark-agent/mnt/ins-a"
	if r.Lines() != want {
		t.Fatalf("mount:\n%s", r.Lines())
	}
	if err := d.Mount(context.Background(), "ins-missing"); err == nil {
		t.Fatal("mount without image must fail")
	}
}

// 什么都没挂载、没附加时的洗机：不调 umount / losetup -d；fstrim 失败只记日志；校验通过。
func TestDestroySequenceWhenNothingMounted(t *testing.T) {
	d, r := newDisk(map[string]int64{}) // Stat 一律 NotExist：rm 之后镜像不存在
	r.Stub("mountpoint -q", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("losetup -j", "", nil)
	r.Stub("fstrim", "", errors.New("fstrim unsupported"))
	if err := d.Destroy(context.Background(), "ins-a"); err != nil {
		t.Fatal(err)
	}
	want := "mountpoint -q /var/lib/spark-agent/mnt/ins-a\n" +
		"losetup -j /var/lib/spark-agent/data/ins-a.img\n" +
		"chattr -i /var/lib/spark-agent/mnt/ins-a\n" +
		"rmdir /var/lib/spark-agent/mnt/ins-a\n" +
		"rm -f /var/lib/spark-agent/data/ins-a.img\n" +
		"fstrim /\n" +
		"mountpoint -q /var/lib/spark-agent/mnt/ins-a\n" +
		"losetup -j /var/lib/spark-agent/data/ins-a.img"
	if r.Lines() != want {
		t.Fatalf("destroy (nothing mounted):\n%s", r.Lines())
	}
}

// 已挂载、已附加时的洗机：先 umount 再 losetup -d；校验阶段 stub 仍报「已挂载 / 已附加」→ 返回洗机校验失败。
func TestDestroyUnmountsDetachesAndVerifies(t *testing.T) {
	d, r := newDisk(map[string]int64{})
	r.Stub("mountpoint -q", "", nil)
	r.Stub("losetup -j", "/dev/loop5: []: (/var/lib/spark-agent/data/ins-a.img)\n", nil)
	r.Stub("umount", "", nil)
	r.Stub("losetup -d", "", nil)
	err := d.Destroy(context.Background(), "ins-a")
	if err == nil || !strings.Contains(err.Error(), "洗机校验失败") || !strings.Contains(err.Error(), "挂载点仍挂载") || !strings.Contains(err.Error(), "loop 设备仍附加") {
		t.Fatalf("verification = %v", err)
	}
	got := r.Lines()
	if !strings.Contains(got, "umount /var/lib/spark-agent/mnt/ins-a\nlosetup -j /var/lib/spark-agent/data/ins-a.img\nlosetup -d /dev/loop5\n") {
		t.Fatalf("destroy must unmount then detach:\n%s", got)
	}
	// umount 失败直接返回错误，不继续删镜像
	r.Reset()
	r.Stub("umount", "", &cmdrun.CmdError{ExitCode: 32, Stderr: "target is busy"})
	if err := d.Destroy(context.Background(), "ins-a"); err == nil || !strings.Contains(err.Error(), "target is busy") {
		t.Fatalf("umount failure = %v", err)
	}
	if strings.Contains(r.Lines(), "rm -f") {
		t.Fatal("must not delete the image after a failed umount")
	}
}

func TestFreeBytesAndExists(t *testing.T) {
	d, r := newDisk(map[string]int64{"/var/lib/spark-agent/data/ins-a.img": 1})
	r.Stub("df", "Avail\n123456\n", nil)
	free, err := d.FreeBytes(context.Background())
	if err != nil || free != 123456 || r.Lines() != "df -B1 --output=avail /var/lib/spark-agent" {
		t.Fatalf("free = %d %v calls=%s", free, err, r.Lines())
	}
	if ok, _ := d.Exists(context.Background(), "ins-a"); !ok {
		t.Fatal("exists")
	}
	if ok, _ := d.Exists(context.Background(), "ins-b"); ok {
		t.Fatal("not exists")
	}
	if d.MountDir("ins-a") != "/var/lib/spark-agent/mnt/ins-a" {
		t.Fatal(d.MountDir("ins-a"))
	}
}
