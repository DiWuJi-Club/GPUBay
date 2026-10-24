package llm

import (
	"context"
	"net"
)

// ProbeModels 仅测试：DefaultProbe 的 HTTP 部分，dial 可替换。
func ProbeModels(ctx context.Context, dial func(context.Context) (net.Conn, error), slug, key string) error {
	return probeModels(ctx, dial, slug, key)
}
