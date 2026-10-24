package llm

import (
	"context"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// NodeKey 解密节点的 vLLM --api-key（llm_nodes.api_key_enc，secretbox）。网关转发与 Prober 探测用；节点不存在 → ErrLLMNodeNotFound。
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
		return "", err
	}
	return string(plain), nil
}
