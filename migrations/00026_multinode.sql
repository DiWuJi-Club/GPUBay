-- +goose Up
-- 计划 13：多机模型。集群网络（cluster_fabrics）与机器的 fabric 归属 / 集群网卡 / Agent 能力位 / 网卡探测；
-- 实例集群（instance_clusters，用户集群与 Token 商城节点组共用）与 instances.cluster_id / cluster_rank；
-- 集群部署（app_cluster_deployments）与 app_deployments.cluster_deployment_id / cluster_role；
-- Token 商城模型的每副本机器数 / TP / PP、多机节点组（llm_nodes.size / cluster_id / app_cluster_id 与 llm_node_members）。
-- Down 在以下情况拒绝：仍有运行中的集群；仍有非终态的多机节点组；仍有集群应用。
-- 存量数据：无 fabric、无集群；存量机器 cluster_ready=false；存量模型 nodes_per_replica=1、TP=PP=1；存量节点 size=1。

-- 集群网络（fabric）
CREATE TABLE cluster_fabrics (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,                       -- 1～32，后台展示
    kind        TEXT NOT NULL CHECK (kind IN ('lan', 'cx7')),
    isolation   TEXT NOT NULL CHECK (isolation IN ('routed', 'direct', 'vlan')),
    rdma        BOOLEAN NOT NULL DEFAULT false,             -- 运维意图：尝试 RDMA（还要成员探测通过）
    mtu         INT NOT NULL DEFAULT 0 CHECK (mtu = 0 OR mtu BETWEEN 1280 AND 9216),   -- 0 = 继承网卡
    vlan_min    INT,
    vlan_max    INT,
    description TEXT NOT NULL DEFAULT '',                   -- ≤ 200
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT cluster_fabrics_kind_isolation CHECK ((kind = 'lan') = (isolation = 'routed')),
    CONSTRAINT cluster_fabrics_rdma CHECK (kind = 'cx7' OR NOT rdma),
    CONSTRAINT cluster_fabrics_vlan CHECK (
        -- 显式 IS NOT NULL：否则范围为空时 BETWEEN 得 NULL，CHECK 视为通过
        (isolation = 'vlan' AND vlan_min IS NOT NULL AND vlan_max IS NOT NULL
            AND vlan_min BETWEEN 2 AND 4094 AND vlan_max BETWEEN vlan_min AND 4094)
        OR (isolation <> 'vlan' AND vlan_min IS NULL AND vlan_max IS NULL))
);

-- 机器：fabric 归属、集群网卡、Agent 能力与网卡探测
ALTER TABLE machines
    ADD COLUMN fabric_id      BIGINT REFERENCES cluster_fabrics (id),
    ADD COLUMN cluster_iface  TEXT NOT NULL DEFAULT '',     -- 后台指定的集群网卡；'' = 自动（lan → lan_ip 所在网卡；cx7 → 第一个 up 的 mlx5 口）
    ADD COLUMN agent_features INT  NOT NULL DEFAULT 0,      -- Hello.features 原值（bit0 SharedSlots、bit1 Cluster）
    ADD COLUMN net_info       JSONB,                        -- 最近一次网卡探测（clusterspec.NetInfo）
    ADD COLUMN net_info_at    TIMESTAMPTZ,
    ADD COLUMN cluster_ready  BOOLEAN NOT NULL DEFAULT false,   -- fleet 按 fabric + 网卡 + 能力位计算，分配只看它
    ADD COLUMN cluster_rdma   BOOLEAN NOT NULL DEFAULT false,   -- 本机在该 fabric 上 RDMA 可用
    ADD COLUMN cluster_net    TEXT NOT NULL DEFAULT '';         -- 集群网段：lan = 集群网卡 IPv4 前缀（如 192.168.1.0/24），cx7 = 'l2'，未就绪 = ''；分配按 (fabric_id, cluster_net) 分组
CREATE INDEX machines_fabric_idx ON machines (fabric_id, id) WHERE fabric_id IS NOT NULL;

-- 实例集群（用户集群与 Token 商城节点组共用）
CREATE TABLE instance_clusters (
    id           TEXT PRIMARY KEY,                          -- 'clu-' + 10 位 [a-z0-9]
    user_id      BIGINT NOT NULL REFERENCES users (id),
    system       BOOLEAN NOT NULL DEFAULT false,            -- 平台节点组（成员是系统实例）
    size         INT NOT NULL CHECK (size BETWEEN 2 AND 8),
    fabric_id    BIGINT NOT NULL REFERENCES cluster_fabrics (id),
    mode         TEXT NOT NULL CHECK (mode IN ('routed', 'macvlan')),
    vlan         INT CHECK (vlan BETWEEN 2 AND 4094),
    rdma         BOOLEAN NOT NULL,                          -- 开通时判定的有效 RDMA（fabric.rdma ∧ 全员 cluster_rdma）
    mtu          INT NOT NULL DEFAULT 0,
    subnet_index INT NOT NULL CHECK (subnet_index BETWEEN 0 AND 255),
    version      INT NOT NULL DEFAULT 1,                    -- ClusterSpec 版本，成员变更 +1
    state        TEXT NOT NULL CHECK (state IN ('active', 'released', 'failed')),
    batch_id     TEXT REFERENCES rental_batches (id),
    ready_at     TIMESTAMPTZ,                               -- 全员首次 running（到期对齐、开通期判定）
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    ended_at     TIMESTAMPTZ
);
CREATE UNIQUE INDEX instance_clusters_subnet_live_uniq ON instance_clusters (subnet_index) WHERE state = 'active';
CREATE UNIQUE INDEX instance_clusters_vlan_live_uniq ON instance_clusters (fabric_id, vlan) WHERE state = 'active' AND vlan IS NOT NULL;
CREATE INDEX instance_clusters_user_idx ON instance_clusters (user_id, created_at DESC);

ALTER TABLE instances
    ADD COLUMN cluster_id   TEXT REFERENCES instance_clusters (id),
    ADD COLUMN cluster_rank INT CHECK (cluster_rank BETWEEN 0 AND 7),
    ADD CONSTRAINT instances_cluster_pair CHECK ((cluster_id IS NULL) = (cluster_rank IS NULL));
CREATE INDEX instances_cluster_idx ON instances (cluster_id, cluster_rank) WHERE cluster_id IS NOT NULL;
-- 不建 (cluster_id, rank) 的唯一索引：换机时新旧成员短暂同时活动；rank 唯一由集群行锁保证、CheckClusterInvariants 核对

-- 集群部署（一次在全部成员上部署同一应用）
CREATE TABLE app_cluster_deployments (
    id             TEXT PRIMARY KEY,                        -- 'acd-' + 12 位
    cluster_id     TEXT NOT NULL REFERENCES instance_clusters (id),
    user_id        BIGINT NOT NULL REFERENCES users (id),
    app_id         BIGINT NOT NULL REFERENCES apps (id),
    app_version_id BIGINT NOT NULL REFERENCES app_versions (id),
    state          TEXT NOT NULL CHECK (state IN ('active', 'removed')),   -- 运行态由成员部署聚合
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX app_cluster_deployments_live_uniq ON app_cluster_deployments (cluster_id) WHERE state = 'active';
ALTER TABLE app_deployments
    ADD COLUMN cluster_deployment_id TEXT REFERENCES app_cluster_deployments (id),
    ADD COLUMN cluster_role          TEXT CHECK (cluster_role IN ('head', 'worker')),
    ADD CONSTRAINT app_deployments_cluster_pair CHECK ((cluster_deployment_id IS NULL) = (cluster_role IS NULL));
CREATE INDEX app_deployments_cluster_idx ON app_deployments (cluster_deployment_id) WHERE cluster_deployment_id IS NOT NULL;

-- Token 商城
ALTER TABLE llm_models
    ADD COLUMN nodes_per_replica INT NOT NULL DEFAULT 1 CHECK (nodes_per_replica IN (1, 2, 4)),
    ADD COLUMN tensor_parallel   INT NOT NULL DEFAULT 1 CHECK (tensor_parallel BETWEEN 1 AND 8),
    ADD COLUMN pipeline_parallel INT NOT NULL DEFAULT 1 CHECK (pipeline_parallel BETWEEN 1 AND 8),
    ADD CONSTRAINT llm_models_parallel CHECK (tensor_parallel * pipeline_parallel = nodes_per_replica);
ALTER TABLE llm_nodes
    ADD COLUMN size           INT NOT NULL DEFAULT 1 CHECK (size BETWEEN 1 AND 8),
    ADD COLUMN cluster_id     TEXT REFERENCES instance_clusters (id),
    ADD COLUMN app_cluster_id TEXT REFERENCES app_cluster_deployments (id),
    ADD CONSTRAINT llm_nodes_group CHECK ((size = 1) = (cluster_id IS NULL));
CREATE TABLE llm_node_members (
    node_id     TEXT NOT NULL REFERENCES llm_nodes (id),
    rank        INT  NOT NULL CHECK (rank BETWEEN 0 AND 7),
    machine_id  BIGINT NOT NULL REFERENCES machines (id),
    instance_id TEXT NOT NULL REFERENCES instances (id),
    live        BOOLEAN NOT NULL DEFAULT true,              -- 节点进入终态时同事务置 false
    PRIMARY KEY (node_id, rank)
);
CREATE UNIQUE INDEX llm_node_members_machine_live_uniq ON llm_node_members (machine_id) WHERE live;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM instance_clusters WHERE state = 'active') THEN
        RAISE EXCEPTION '仍有运行中的集群，请先释放';
    END IF;
    IF EXISTS (SELECT 1 FROM llm_nodes WHERE size > 1 AND state NOT IN ('removed', 'failed')) THEN
        RAISE EXCEPTION '仍有多机节点组，请先把副本数调为 0';
    END IF;
    IF EXISTS (SELECT 1 FROM app_cluster_deployments WHERE state = 'active') THEN
        RAISE EXCEPTION '仍有集群应用，请先卸载';
    END IF;
END $$;
-- +goose StatementEnd
DROP TABLE llm_node_members;
ALTER TABLE llm_nodes
    DROP CONSTRAINT llm_nodes_group,
    DROP COLUMN app_cluster_id,
    DROP COLUMN cluster_id,
    DROP COLUMN size;
ALTER TABLE llm_models
    DROP CONSTRAINT llm_models_parallel,
    DROP COLUMN pipeline_parallel,
    DROP COLUMN tensor_parallel,
    DROP COLUMN nodes_per_replica;
DROP INDEX app_deployments_cluster_idx;
ALTER TABLE app_deployments
    DROP CONSTRAINT app_deployments_cluster_pair,
    DROP COLUMN cluster_role,
    DROP COLUMN cluster_deployment_id;
DROP TABLE app_cluster_deployments;
DROP INDEX instances_cluster_idx;
ALTER TABLE instances
    DROP CONSTRAINT instances_cluster_pair,
    DROP COLUMN cluster_rank,
    DROP COLUMN cluster_id;
DROP TABLE instance_clusters;
DROP INDEX machines_fabric_idx;
ALTER TABLE machines
    DROP COLUMN cluster_net,
    DROP COLUMN cluster_rdma,
    DROP COLUMN cluster_ready,
    DROP COLUMN net_info_at,
    DROP COLUMN net_info,
    DROP COLUMN agent_features,
    DROP COLUMN cluster_iface,
    DROP COLUMN fabric_id;
DROP TABLE cluster_fabrics;
