package agentgw

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/db"
)

// Enroll：校验令牌 → 校验 CSR 签名并只取公钥 → 签发 CN=序列号 的证书 → 写指纹并清空连接归属 → 标记令牌已用 → 审计。
// 全部在一个事务里；令牌类失败统一返回「令牌无效」。
func (s *Server) Enroll(ctx context.Context, req *agentpb.EnrollRequest) (*agentpb.EnrollResponse, error) {
	ip := peerIP(ctx)
	if !s.limiter.allow(ip) {
		return nil, status.Error(codes.ResourceExhausted, "操作过于频繁，请稍后再试")
	}
	pub, err := pki.ParseCSR(req.GetCsrDer())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "CSR 无效")
	}
	now := s.d.Clock.Now()
	var certDER []byte
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		row, err := s.d.Fleet.ConsumeEnrollToken(ctx, tx, req.GetToken())
		if err != nil {
			return err
		}
		der, err := s.d.CA.IssueMachine(pub, row.Serial, s.d.CertClock.Now()) // 证书有效期按墙上时钟；审计仍用业务时钟 now
		if err != nil {
			return err
		}
		fp := pki.Fingerprint(der)
		if err := s.d.Fleet.SetCertFingerprint(ctx, tx, row.MachineID, fp); err != nil {
			return err
		}
		certDER = der
		return audit.Record(ctx, tx, now, audit.Actor{IP: ip}.Entry("machine.enrolled", "machine", strconv.FormatInt(row.MachineID, 10),
			map[string]any{"token_id": row.ID, "issued_by": row.IssuedBy, "fingerprint": fp}))
	})
	if errors.Is(err, fleet.ErrEnrollToken) {
		return nil, status.Error(codes.PermissionDenied, "令牌无效")
	}
	if err != nil {
		s.d.Log.Error("enroll failed", "ip", ip, "err", err)
		return nil, status.Error(codes.Internal, "内部错误")
	}
	return &agentpb.EnrollResponse{CertDer: certDER, CaDer: s.d.CA.CertDER()}, nil
}
