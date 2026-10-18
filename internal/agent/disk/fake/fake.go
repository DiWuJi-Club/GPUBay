package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type Disk struct {
	mu       sync.Mutex
	free     uint64
	images   map[string]*image
	failNext map[string]error
}

type image struct {
	sizeGiB int
	mounted bool
}

func New(freeBytes uint64) *Disk {
	return &Disk{free: freeBytes, images: map[string]*image{}, failNext: map[string]error{}}
}

func (d *Disk) FailNext(op string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNext[op] = err
}

func (d *Disk) fail(op string) error {
	if err, ok := d.failNext[op]; ok {
		delete(d.failNext, op)
		return err
	}
	return nil
}

func (d *Disk) MountDir(id string) string { return "/fake/mnt/" + id }

func (d *Disk) Create(_ context.Context, id string, sizeGiB int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fail("create"); err != nil {
		return err
	}
	if img, ok := d.images[id]; ok {
		if img.sizeGiB != sizeGiB {
			return fmt.Errorf("数据盘镜像大小 %d GiB 与期望 %d GiB 不符", img.sizeGiB, sizeGiB)
		}
		img.mounted = true
		return nil
	}
	if need := uint64(sizeGiB) << 30; need > d.free {
		return fmt.Errorf("磁盘空间不足：需要 %d，剩余 %d", need, d.free)
	}
	d.images[id] = &image{sizeGiB: sizeGiB, mounted: true}
	d.free -= uint64(sizeGiB) << 30
	return nil
}

func (d *Disk) Mount(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fail("mount"); err != nil {
		return err
	}
	img, ok := d.images[id]
	if !ok {
		return errors.New("数据盘镜像不存在")
	}
	img.mounted = true
	return nil
}

func (d *Disk) Mounted(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	img, ok := d.images[id]
	return ok && img.mounted, nil
}

func (d *Disk) Destroy(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.fail("destroy"); err != nil {
		return err
	}
	if img, ok := d.images[id]; ok {
		d.free += uint64(img.sizeGiB) << 30
		delete(d.images, id)
	}
	return nil
}

func (d *Disk) Exists(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.images[id]
	return ok, nil
}

func (d *Disk) FreeBytes(context.Context) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.free, nil
}

// Unmount 模拟挂载丢失（例如重启后 Agent 尚未恢复）。
func (d *Disk) Unmount(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if img, ok := d.images[id]; ok {
		img.mounted = false
	}
}

func (d *Disk) SetFree(bytes uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.free = bytes
}

func (d *Disk) State(id string) (created, mounted bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	img, ok := d.images[id]
	return ok, ok && img.mounted
}
