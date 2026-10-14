-- +goose Up
CREATE TABLE products (
    id         BIGSERIAL PRIMARY KEY,
    code       TEXT NOT NULL UNIQUE,
    name       TEXT NOT NULL,
    spec       JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE prices (
    product_id BIGINT NOT NULL REFERENCES products (id),
    duration   TEXT NOT NULL CHECK (duration IN ('1d', '1w', '1m')),
    amount     BIGINT NOT NULL CHECK (amount > 0),
    enabled    BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (product_id, duration)
);

CREATE TABLE images (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    ref         TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    sort        INT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

-- 初始商品与价格（单位：分），参考竞品 ¥69/天、¥414/周、¥1599/月，可在管理后台修改
INSERT INTO products (code, name, spec, created_at, updated_at) VALUES
    ('spark-1', 'DGX Spark 整机',
     '{"gpu": "NVIDIA GB10 Grace Blackwell", "memory_gb": 128, "disk_tb": 4, "cpu": "20 核 Arm"}', now(), now());
INSERT INTO prices (product_id, duration, amount, updated_at)
SELECT id, d.duration, d.amount, now()
FROM products, (VALUES ('1d', 6900), ('1w', 41400), ('1m', 159900)) AS d (duration, amount)
WHERE code = 'spark-1';

-- 镜像地址在计划 2 搭建内网 Harbor 后替换
INSERT INTO images (name, ref, description, sort, created_at, updated_at) VALUES
    ('Ubuntu 24.04 + CUDA', 'registry.spark.internal/base/cuda:ubuntu24.04', '基础 CUDA 开发环境', 10, now(), now()),
    ('PyTorch', 'registry.spark.internal/base/pytorch:latest', 'PyTorch + JupyterLab', 20, now(), now()),
    ('vLLM', 'registry.spark.internal/base/vllm:latest', 'vLLM 推理服务环境', 30, now(), now());

-- +goose Down
DROP TABLE images;
DROP TABLE prices;
DROP TABLE products;
