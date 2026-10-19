package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/referral"
)

const referralUsage = `用法：
  platform referral backfill-fingerprints   # 为存量已实名用户计算同身份指纹（上线计划 8 后执行一次；不触发自动撤销）`

// referralMain 执行 `platform referral ...`：与服务相同的配置连接数据库（会先执行迁移）。
func referralMain(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	box, key, err := referralKeys(cfg)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runReferral(ctx, pool, box, key, args, os.Stdout)
}

// referralKeys 用与 app.Build 相同的输入——config.Load 解出的 32 字节 cfg.SecretKey（不是 base64 文本）——
// 构造 secretbox 并派生指纹密钥，保证回填写入的指纹与服务实名时写入的相等。
func referralKeys(cfg config.Config) (*secretbox.Box, []byte, error) {
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return nil, nil, err
	}
	key, err := referral.DeriveFingerprintKey(cfg.SecretKey)
	if err != nil {
		return nil, nil, err
	}
	return box, key, nil
}

// runReferral 处理 `platform referral ...` 子命令。
// backfill-fingerprints 只写指纹，不调用 OnKYCVerified：存量用户之间的同身份不自动撤销，由后台人工处置。
func runReferral(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, key []byte, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "backfill-fingerprints" {
		return errors.New(referralUsage)
	}
	rows, err := pool.Query(ctx, `SELECT id, kind, id_number_enc, license_no FROM kyc_records WHERE result = 'verified' AND fingerprint = '' ORDER BY id`)
	if err != nil {
		return err
	}
	type rec struct {
		id      int64
		kind    string
		enc     []byte
		license string
	}
	var list []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.kind, &r.enc, &r.license); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	done, skipped := 0, 0
	for _, r := range list {
		var fp string
		switch {
		case r.kind == "personal" && len(r.enc) > 0:
			plain, err := box.Open(r.enc)
			if err != nil {
				fmt.Fprintf(out, "记录 %d：解密失败（%v），跳过\n", r.id, err)
				skipped++
				continue
			}
			fp = referral.Fingerprint(key, "kyc", string(plain))
		case r.kind == "enterprise" && r.license != "":
			fp = referral.Fingerprint(key, "lic", r.license)
		default:
			fmt.Fprintf(out, "记录 %d：无号码，跳过\n", r.id)
			skipped++
			continue
		}
		if _, err := pool.Exec(ctx, `UPDATE kyc_records SET fingerprint = $2 WHERE id = $1 AND fingerprint = ''`, r.id, fp); err != nil {
			return err
		}
		done++
	}
	fmt.Fprintf(out, "已回填 %d 条，跳过 %d 条（解密失败 / 无号码）\n", done, skipped)
	return nil
}
