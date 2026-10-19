-- +goose Up
-- 计划 7：应用目录（apps / app_versions）、实例上的应用部署（app_deployments / app_deploy_events）、
-- 应用自动登记的端口（instance_ports.deployment_id）、机器已缓存镜像（machines.cached_images）。
-- Down 删表删列；容器内已部署的应用进程不受影响（只是平台不再登记端口、不再在实例恢复后拉起它们）。

CREATE TABLE apps (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,          -- ^[a-z][a-z0-9-]{1,31}$；容器内目录名与端口备注都用它
    name        TEXT NOT NULL,                 -- 对用户展示，1～32 字符
    summary     TEXT NOT NULL DEFAULT '',      -- ≤ 80 字符
    description TEXT NOT NULL DEFAULT '',      -- ≤ 4000 字符，Markdown 子集
    category    TEXT NOT NULL CHECK (category IN ('llm', 'image', 'notebook', 'tool')),
    icon        TEXT NOT NULL DEFAULT 'app',   -- 固定图标名集合（前端映射）
    sort        INT  NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE app_versions (
    id           BIGSERIAL PRIMARY KEY,
    app_id       BIGINT NOT NULL REFERENCES apps (id),
    version      TEXT NOT NULL,                -- ^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    image_ids    BIGINT[] NOT NULL,            -- 兼容镜像，第一个为默认
    spec         JSONB NOT NULL,
    script       TEXT NOT NULL,                -- app.sh（POSIX sh），≤ 65536 字节
    spec_hash    TEXT NOT NULL,                -- sha256(canonical(spec) || "\n" || script) 十六进制
    changelog    TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (app_id, version)
);
CREATE INDEX app_versions_app_idx ON app_versions (app_id, status, published_at DESC);

CREATE TABLE app_deployments (
    id                TEXT PRIMARY KEY,        -- 'dep-' + 12 位 [a-z0-9]
    instance_id       TEXT NOT NULL REFERENCES instances (id),
    user_id           BIGINT NOT NULL REFERENCES users (id),
    app_id            BIGINT NOT NULL REFERENCES apps (id),
    app_version_id    BIGINT NOT NULL REFERENCES app_versions (id),
    state             TEXT NOT NULL CHECK (state IN ('queued', 'installing', 'running', 'stopped', 'failed', 'removing', 'removed', 'lost')),
    desired           TEXT NOT NULL DEFAULT 'running' CHECK (desired IN ('running', 'stopped', 'removed')),
    phase             TEXT NOT NULL DEFAULT '',   -- prepare / download / install / start / health
    progress          INT  NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    message           TEXT NOT NULL DEFAULT '',   -- ≤ 200 字符，已脱敏
    health            TEXT NOT NULL DEFAULT 'unknown' CHECK (health IN ('unknown', 'ok', 'unhealthy')),
    error_code        TEXT NOT NULL DEFAULT '',   -- script_failed / timeout / health_failed / upload_failed / unreachable / port_conflict / port_limit / instance_gone / crashed / disk
    env               JSONB NOT NULL DEFAULT '{}',
    secret_env_enc    BYTEA,                      -- 敏感变量 JSON 的 secretbox 密文
    port              INT  NOT NULL,              -- 主端口（spec.ports[0]）
    log_tail          TEXT NOT NULL DEFAULT '',   -- ≤ 65536 字节，已脱敏
    status_seq        BIGINT NOT NULL DEFAULT 0,
    attempt           INT  NOT NULL DEFAULT 0,
    spec_hash         TEXT NOT NULL,
    next_poll_at      TIMESTAMPTZ NOT NULL,
    last_polled_at    TIMESTAMPTZ,
    cleanup_pending   BOOLEAN NOT NULL DEFAULT false,  -- 实例停机时卸载：目录留在数据盘，实例恢复后由 Runner 删除
    unreachable_count INT NOT NULL DEFAULT 0,          -- 连续 DialError 次数（10 次 → failed(unreachable)）
    crash_count       INT NOT NULL DEFAULT 0,          -- 连续 alive=0 次数（3 次 → failed(crashed)）
    version           INT NOT NULL DEFAULT 0,          -- 乐观并发版本号：每次写本行都 +1，Runner 回写按它比较（不用 updated_at）
    started_at        TIMESTAMPTZ,
    finished_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX app_deployments_live_uniq ON app_deployments (instance_id, app_id) WHERE state NOT IN ('removed', 'lost');
CREATE INDEX app_deployments_poll_idx ON app_deployments (next_poll_at) WHERE state IN ('queued', 'installing', 'running', 'stopped', 'removing') OR cleanup_pending;
CREATE INDEX app_deployments_instance_idx ON app_deployments (instance_id, created_at);
CREATE INDEX app_deployments_user_idx ON app_deployments (user_id, created_at DESC);

CREATE TABLE app_deploy_events (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES app_deployments (id),
    at            TIMESTAMPTZ NOT NULL,
    from_state    TEXT NOT NULL,
    to_state      TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT ''     -- 可展示文案（已脱敏）
);
CREATE INDEX app_deploy_events_dep_idx ON app_deploy_events (deployment_id, id);

ALTER TABLE instance_ports ADD COLUMN deployment_id TEXT REFERENCES app_deployments (id);
CREATE INDEX instance_ports_deployment_idx ON instance_ports (deployment_id) WHERE deployment_id IS NOT NULL;
ALTER TABLE machines ADD COLUMN cached_images TEXT[] NOT NULL DEFAULT '{}', ADD COLUMN cached_images_at TIMESTAMPTZ;

-- +goose Down
DROP INDEX instance_ports_deployment_idx;
ALTER TABLE instance_ports DROP COLUMN deployment_id;
ALTER TABLE machines DROP COLUMN cached_images, DROP COLUMN cached_images_at;
DROP TABLE app_deploy_events;
DROP TABLE app_deployments;
DROP TABLE app_versions;
DROP TABLE apps;
