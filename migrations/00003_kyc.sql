-- +goose Up
CREATE TABLE kyc_records (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users (id),
    kind          TEXT NOT NULL CHECK (kind IN ('personal', 'enterprise')),
    real_name     TEXT NOT NULL,             -- 个人姓名或企业名称
    id_number_enc BYTEA,                     -- 个人身份证号（AES-GCM 加密）
    license_no    TEXT NOT NULL DEFAULT '',  -- 统一社会信用代码
    license_file  TEXT NOT NULL DEFAULT '',  -- 营业执照文件地址
    result        TEXT NOT NULL CHECK (result IN ('pending', 'verified', 'rejected')),
    reason        TEXT NOT NULL DEFAULT '',
    reviewer_id   BIGINT,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);
CREATE INDEX kyc_records_user_idx ON kyc_records (user_id, created_at DESC);
CREATE INDEX kyc_records_pending_idx ON kyc_records (created_at) WHERE result = 'pending';

-- +goose Down
DROP TABLE kyc_records;
