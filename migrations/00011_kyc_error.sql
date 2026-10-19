-- +goose Up
-- error：核验接口报错或核验中断；计入每日次数，不改变用户实名状态
ALTER TABLE kyc_records DROP CONSTRAINT kyc_records_result_check;
ALTER TABLE kyc_records ADD CONSTRAINT kyc_records_result_check
    CHECK (result IN ('pending', 'verified', 'rejected', 'error'));
CREATE INDEX kyc_records_user_kind_idx ON kyc_records (user_id, kind, created_at DESC);

-- +goose Down
DROP INDEX kyc_records_user_kind_idx;
UPDATE kyc_records SET result = 'rejected' WHERE result = 'error';
ALTER TABLE kyc_records DROP CONSTRAINT kyc_records_result_check;
ALTER TABLE kyc_records ADD CONSTRAINT kyc_records_result_check
    CHECK (result IN ('pending', 'verified', 'rejected'));
