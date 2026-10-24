package llm

import (
	"context"
	"fmt"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// NodeKey 解密节点的 vLLM --api-key（llm_nodes.api_key_enc，Deps.Box）；网关转发与 Prober 用。不存在 → llm_node_not_found。
func (s *Service) NodeKey(ctx context.Context, nodeID string) (string, error) {
	var enc []byte
	err := s.d.Pool.QueryRow(ctx, `SELECT api_key_enc FROM llm_nodes WHERE id = $1`, nodeID).Scan(&enc)
	if db.IsNoRows(err) {
		return "", apperr.ErrLLMNodeNotFound
	}
	if err != nil {
		return "", err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return "", fmt.Errorf("llm: open node key %s: %w", nodeID, err)
	}
	return string(plain), nil
}

// newNodeKey 生成节点密钥（32 字节随机 → base64url 无填充，43 字符，满足 vllm-node API_KEY 的 ^[A-Za-z0-9_-]{32,128}$）
// 并用 Deps.Box 加密。
func (s *Service) newNodeKey() (plain string, enc []byte, err error) {
	plain = randx.Token()
	return plain, s.d.Box.Seal([]byte(plain)), nil
}
