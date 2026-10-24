-- +goose Up
-- 计划 11：ComfyUI 工作流市场。工作流目录（comfy_workflows / comfy_workflow_versions）、
-- 某个 ComfyUI 部署上「应装」的工作流集合（deployment_workflows）、快速出图运行记录（comfy_workflow_runs）。
-- 00021～00023 留给其它计划。
-- 另外放开 uploads：新用途 workflow_cover（工作流封面，D13），所有者是平台（user_id 为 NULL，只允许该用途）。
-- Down 删这四张表并删除封面登记、恢复 uploads 约束；容器内已装的工作流文件、自定义节点与模型（/root/data/comfyui、/root/data/models）
-- 不受影响；封面文件本身留在上传目录（孤儿文件，无害）。

ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('license', 'workflow_cover'));
ALTER TABLE uploads ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE uploads ADD CONSTRAINT uploads_owner_check CHECK ((user_id IS NULL) = (purpose = 'workflow_cover'));

CREATE TABLE comfy_workflows (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,            -- ^[a-z][a-z0-9-]{1,47}$；容器内文件名 <slug>.json
    name        TEXT NOT NULL,                   -- 1～48 字符
    summary     TEXT NOT NULL DEFAULT '',        -- ≤ 120 字符，卡片副标题
    description TEXT NOT NULL DEFAULT '',        -- ≤ 8000 字符，Markdown 子集（同 apps.description）
    category    TEXT NOT NULL CHECK (category IN ('txt2img', 'img2img', 'video', 'upscale', 'other')),
    tags        TEXT[] NOT NULL DEFAULT '{}',    -- ≤ 8 项，每项 ≤ 16 字符
    cover_key   TEXT,                            -- storage 里的封面文件 key；NULL = 占位图
    sort        INT  NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE comfy_workflow_versions (
    id           BIGSERIAL PRIMARY KEY,
    workflow_id  BIGINT NOT NULL REFERENCES comfy_workflows (id),
    version      TEXT NOT NULL,                  -- ^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published', 'retired')),
    graph        JSONB NOT NULL,                 -- ComfyUI 前端保存的 UI 格式工作流（≤ 256KB）
    api_prompt   JSONB,                          -- ComfyUI「Save (API Format)」导出（≤ 256KB）；NULL = 不支持快速出图
    manifest     JSONB NOT NULL,                 -- 规格 §3.2：models / nodes / inputs / outputs
    download_gb  NUMERIC(8,2) NOT NULL DEFAULT 0,-- 发布时由 manifest.models[].size_gb 求和，列表展示用
    manifest_hash TEXT NOT NULL,                 -- sha256(canonical(graph) || canonical(manifest) || canonical(api_prompt))，发布时冻结
    changelog    TEXT NOT NULL DEFAULT '',       -- ≤ 1000 字符
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    UNIQUE (workflow_id, version)
);
CREATE INDEX comfy_workflow_versions_wf_idx ON comfy_workflow_versions (workflow_id, status, published_at DESC);

-- 某个 ComfyUI 部署上「应装」的工作流集合；部署包按它生成。一个部署上一个工作流只有一行（换版本 = 改 version_id）。
CREATE TABLE deployment_workflows (
    deployment_id       TEXT   NOT NULL REFERENCES app_deployments (id),
    workflow_id         BIGINT NOT NULL REFERENCES comfy_workflows (id),
    workflow_version_id BIGINT NOT NULL REFERENCES comfy_workflow_versions (id),
    state               TEXT   NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'installed', 'failed')),
    added_at            TIMESTAMPTZ NOT NULL,
    installed_at        TIMESTAMPTZ,
    PRIMARY KEY (deployment_id, workflow_id)
);
CREATE INDEX deployment_workflows_wf_idx ON deployment_workflows (workflow_id);

-- 快速出图运行记录（只做统计与归属校验；图片不落库）
CREATE TABLE comfy_workflow_runs (
    id                  TEXT PRIMARY KEY,        -- 'run-' + 12 位 [a-z0-9]
    deployment_id       TEXT   NOT NULL REFERENCES app_deployments (id),
    user_id             BIGINT NOT NULL REFERENCES users (id),
    workflow_version_id BIGINT NOT NULL REFERENCES comfy_workflow_versions (id),
    prompt_id           TEXT   NOT NULL,         -- ComfyUI 返回的 prompt_id（UUID）
    seed                BIGINT,                  -- 平台生成或用户填写的种子（D11）
    state               TEXT   NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'done', 'error')),
    created_at          TIMESTAMPTZ NOT NULL,
    finished_at         TIMESTAMPTZ
);
CREATE INDEX comfy_workflow_runs_dep_idx ON comfy_workflow_runs (deployment_id, created_at DESC);
CREATE INDEX comfy_workflow_runs_user_idx ON comfy_workflow_runs (user_id, created_at DESC);

-- +goose Down
DROP TABLE comfy_workflow_runs;
DROP TABLE deployment_workflows;
DROP TABLE comfy_workflow_versions;
DROP TABLE comfy_workflows;
DELETE FROM uploads WHERE purpose = 'workflow_cover';
ALTER TABLE uploads DROP CONSTRAINT uploads_owner_check;
ALTER TABLE uploads ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE uploads DROP CONSTRAINT uploads_purpose_check;
ALTER TABLE uploads ADD CONSTRAINT uploads_purpose_check CHECK (purpose IN ('license'));
