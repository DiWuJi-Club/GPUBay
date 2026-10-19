// Package paging 解析后台列表的 page / page_size 参数。
package paging

import (
	"net/url"
	"strconv"

	"spark-cloud/internal/platform/apperr"
)

const (
	DefaultSize = 20
	MaxSize     = 100
	maxNumber   = 1_000_000
)

// Page 是从 1 开始的页码与每页条数。
type Page struct {
	Number int
	Size   int
}

func Parse(q url.Values) (Page, error) {
	p := Page{Number: 1, Size: DefaultSize}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxNumber {
			return Page{}, apperr.BadRequest("page 必须是 1～1000000 之间的整数")
		}
		p.Number = n
	}
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSize {
			return Page{}, apperr.BadRequest("page_size 必须是 1～100 之间的整数")
		}
		p.Size = n
	}
	return p, nil
}

// Normalize 把零值或越界的页码修正为默认值，供服务层直接调用时使用。
func (p Page) Normalize() Page {
	if p.Number < 1 || p.Number > maxNumber {
		p.Number = 1
	}
	if p.Size < 1 {
		p.Size = DefaultSize
	}
	if p.Size > MaxSize {
		p.Size = MaxSize
	}
	return p
}

func (p Page) Limit() int  { return p.Normalize().Size }
func (p Page) Offset() int { n := p.Normalize(); return (n.Number - 1) * n.Size }
