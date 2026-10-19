-- +goose Up
CREATE TABLE pdus (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    driver        TEXT NOT NULL CHECK (driver IN ('snmp', 'mock')),
    address       TEXT NOT NULL,                        -- host 或 host:port
    community_enc BYTEA,                                -- AES-GCM 加密的 SNMP 写 community
    oid_prefix    TEXT NOT NULL DEFAULT '1.3.6.1.4.1.318.1.1.4.4.2.1.3',
    cycle_value   INT NOT NULL DEFAULT 3,
    on_value      INT NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

ALTER TABLE machines
    ADD COLUMN pdu_id BIGINT REFERENCES pdus (id),
    ADD COLUMN pdu_outlet INT CHECK (pdu_outlet BETWEEN 1 AND 256),
    ADD CONSTRAINT machines_pdu_pair_chk CHECK ((pdu_id IS NULL) = (pdu_outlet IS NULL));
-- 一个 PDU 插座只能接一台机器
CREATE UNIQUE INDEX machines_pdu_outlet_uniq ON machines (pdu_id, pdu_outlet) WHERE pdu_id IS NOT NULL;

-- +goose Down
DROP INDEX machines_pdu_outlet_uniq;
ALTER TABLE machines DROP CONSTRAINT machines_pdu_pair_chk, DROP COLUMN pdu_outlet, DROP COLUMN pdu_id;
DROP TABLE pdus;
