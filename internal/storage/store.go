// Package storage 保存用户上传的文件（营业执照等）：Store 接口 + 本地磁盘实现，uploads 表记录归属。
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"spark-cloud/internal/platform/apperr"
)

// Store 是文件存储后端；以后可替换为对象存储。
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

var keyRe = regexp.MustCompile(`^\d{4}/\d{2}/[a-z0-9]{32}\.(jpg|png|pdf|webp)$`)

// ValidKey 报告 key 是否是服务端签发格式的文件键（同时防止路径穿越）。
func ValidKey(key string) bool { return keyRe.MatchString(key) }

var errBadKey = apperr.BadRequest("文件键无效")

// LocalDisk 把文件保存在本机目录下，key 即相对路径。
type LocalDisk struct {
	root string
}

func NewLocalDisk(root string) (*LocalDisk, error) {
	if root == "" {
		return nil, errors.New("storage: 上传目录未设置")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("storage: 创建上传目录: %w", err)
	}
	return &LocalDisk{root: abs}, nil
}

func (d *LocalDisk) path(key string) string { return filepath.Join(d.root, filepath.FromSlash(key)) }

// Put 先写临时文件再改名，避免读到写了一半的文件。
func (d *LocalDisk) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	if !ValidKey(key) {
		return 0, errBadKey
	}
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o640)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, err
	}
	return n, nil
}

func (d *LocalDisk) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, apperr.ErrNotFound
	}
	f, err := os.Open(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Delete 删除文件；文件不存在视为成功。
func (d *LocalDisk) Delete(_ context.Context, key string) error {
	if !ValidKey(key) {
		return errBadKey
	}
	if err := os.Remove(d.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
