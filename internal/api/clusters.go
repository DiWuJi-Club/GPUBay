package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// 计划 13：控制台集群接口（规格 §10.1）。集群视图对用户隐藏机器 ID、fabric（ID / 名称）、子网序号与 VLAN：
// 一律经 clusterConsoleView 输出，不直接序列化 rental.ClusterView。

// clusterMemberView 是集群视图的一个成员（在位成员；已结束集群每个 rank 最后一个实例）。
type clusterMemberView struct {
	Rank         int        `json:"rank"`
	InstanceID   string     `json:"instance_id"`
	State        string     `json:"state"`
	Suspended    bool       `json:"suspended"`
	IP           string     `json:"ip"`
	MachineLabel string     `json:"machine_label"`
	NetStatus    string     `json:"net_status"` // 归一化：ok / pending / error（见 consoleNetStatus）
	NetReason    string     `json:"net_reason"` // error 时的固定文案；其余为 ""
	ExpiresAt    *time.Time `json:"expires_at"`
}

// 控制台的成员集群网络状态（终审 B-2）：Agent 上报的原始 "error: …" 含宿主命令行、VLAN、网卡名与地址，
// 只在后台视图保留；控制台归一为三个值 + 固定原因（平台已对 error 告警，见 rental.AlertClusterNetError）。
const (
	netStatusPending = "pending"
	netStatusError   = "error"
	netErrorReason   = "集群网络异常，平台已告警处理"
)

// consoleNetStatus 把 Agent 原始状态归一：ok → ok；error / error: … → error + 固定原因；
// 未上报（""）、pending 与其它未知值 → pending。
func consoleNetStatus(raw string) (status, reason string) {
	switch {
	case raw == rental.NetStatusOK:
		return rental.NetStatusOK, ""
	case raw == netStatusError || strings.HasPrefix(raw, netStatusError+":"):
		return netStatusError, netErrorReason
	default:
		return netStatusPending, ""
	}
}

// clusterView 是控制台的集群视图。
type clusterView struct {
	ID                    string               `json:"id"`
	Size                  int                  `json:"size"`
	Status                string               `json:"status"`  // forming / ready / degraded / released / failed
	Network               string               `json:"network"` // rdma / ethernet / rdma_degraded
	FabricKind            string               `json:"fabric_kind"`
	Subnet                string               `json:"subnet"`
	HeadIP                string               `json:"head_ip"`
	BatchID               *string              `json:"batch_id"`
	ExpiresAt             *time.Time           `json:"expires_at"`       // 在位成员里最早的到期（对齐后全员相同）
	ExpiresMismatch       bool                 `json:"expires_mismatch"` // 成员到期相差超过 rental.ClusterAlignSlack（控制台提示「成员到期时间不一致」）
	AutoRenew             bool                 `json:"auto_renew"`       // 在位成员全部开启
	AutoRenewDuration     string               `json:"auto_renew_duration"`
	AutoRenewFailedReason string               `json:"auto_renew_failed_reason"`
	RenewableUntil        *time.Time           `json:"renewable_until"` // 成员里最早的可续上限；无预订为 null
	RenewOptions          []rental.RenewOption `json:"renew_options"`   // 各档位全员都放得下的最大单位数；已结束集群为 []
	Members               []clusterMemberView  `json:"members"`
	App                   *clusterAppView      `json:"app"` // 当前集群应用；没有（或未开放应用部署）为 null
	ReadyAt               *time.Time           `json:"ready_at"`
	EndedAt               *time.Time           `json:"ended_at"`
	CreatedAt             time.Time            `json:"created_at"`
}

// clusterAppMemberView 是集群应用的一个成员部署。
type clusterAppMemberView struct {
	Rank         int    `json:"rank"`
	Role         string `json:"role"` // head / worker
	InstanceID   string `json:"instance_id"`
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
	Health       string `json:"health"`
	Phase        string `json:"phase"`
	Progress     int    `json:"progress"`
	Message      string `json:"message"`
	Current      bool   `json:"current"` // 实例仍是集群当前成员（false = 换机前的旧成员，需要重新部署）
}

// clusterAppView 是控制台的集群应用（规格 §10.1 ClusterAppView）；env / secret_env / spec 取 head（rank0）部署，供重新部署表单用。
type clusterAppView struct {
	ID              string                 `json:"id"`
	State           string                 `json:"state"` // active / removed
	AppID           int64                  `json:"app_id"`
	AppSlug         string                 `json:"app_slug"`
	AppName         string                 `json:"app_name"`
	AppIcon         string                 `json:"app_icon"`
	VersionID       int64                  `json:"version_id"`
	Version         string                 `json:"version"`
	LatestVersionID *int64                 `json:"latest_version_id"`
	LatestVersion   string                 `json:"latest_version"`
	UpdateAvailable bool                   `json:"update_available"`
	Status          string                 `json:"status"` // installing / running / failed / stopped / stale / removing / removed
	Health          string                 `json:"health"`
	Phase           string                 `json:"phase"`
	Progress        int                    `json:"progress"`
	URL             *string                `json:"url"` // head 的「打开」地址；端口未登记为 null
	Env             map[string]string      `json:"env"`
	SecretEnv       map[string]bool        `json:"secret_env"`
	Spec            appdeploy.Spec         `json:"spec"`
	Members         []clusterAppMemberView `json:"members"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

// instanceClusterView 是实例视图里的 cluster 段（成员才有）。
type instanceClusterView struct {
	ID     string `json:"id"`
	Size   int    `json:"size"`
	Rank   int    `json:"rank"`
	Status string `json:"status"`
}

// ---- 视图组装 ----

// clusterConsoleView 把 rental.ClusterView 转为控制台视图：到期、自动续费与续费档位按在位成员汇总，并带当前集群应用。
func (s *Server) clusterConsoleView(ctx context.Context, userID int64, cv rental.ClusterView) (clusterView, error) {
	v := clusterView{ID: cv.ID, Size: cv.Size, Status: cv.Status, Network: cv.Network, FabricKind: cv.FabricKind, Subnet: cv.Subnet, HeadIP: cv.HeadIP,
		BatchID: cv.BatchID, ReadyAt: cv.ReadyAt, EndedAt: cv.EndedAt, CreatedAt: cv.CreatedAt,
		Members: make([]clusterMemberView, 0, len(cv.Members)), RenewOptions: []rental.RenewOption{}}
	var earliest, latest *time.Time
	for _, m := range cv.Members {
		net, reason := consoleNetStatus(m.NetStatus)
		v.Members = append(v.Members, clusterMemberView{Rank: m.Rank, InstanceID: m.InstanceID, State: m.State, Suspended: m.Suspended, IP: m.IP,
			MachineLabel: m.MachineLabel, NetStatus: net, NetReason: reason, ExpiresAt: m.ExpiresAt})
		if m.ExpiresAt == nil || cv.State != rental.ClusterActive {
			continue
		}
		if earliest == nil || m.ExpiresAt.Before(*earliest) {
			earliest = m.ExpiresAt
		}
		if latest == nil || m.ExpiresAt.After(*latest) {
			latest = m.ExpiresAt
		}
	}
	v.ExpiresAt = earliest
	v.ExpiresMismatch = earliest != nil && latest.Sub(*earliest) > rental.ClusterAlignSlack
	if cv.State == rental.ClusterActive && len(cv.Members) > 0 {
		v.AutoRenew = true
		var opts []rental.RenewOption
		for i, m := range cv.Members {
			inst, err := s.Rental.GetForUser(ctx, userID, m.InstanceID)
			if err != nil {
				return clusterView{}, err
			}
			v.AutoRenew = v.AutoRenew && inst.AutoRenew
			if i == 0 {
				v.AutoRenewDuration = string(inst.AutoRenewDuration)
			}
			if v.AutoRenewFailedReason == "" {
				v.AutoRenewFailedReason = inst.AutoRenewFailedReason
			}
			info, err := s.Rental.RenewInfoFor(ctx, inst)
			if err != nil {
				return clusterView{}, err
			}
			if info.RenewableUntil != nil && (v.RenewableUntil == nil || info.RenewableUntil.Before(*v.RenewableUntil)) {
				v.RenewableUntil = info.RenewableUntil
			}
			opts = intersectRenewOptions(opts, info.Options, i == 0)
		}
		if opts != nil {
			v.RenewOptions = opts
		}
	}
	if s.Apps != nil {
		acd, err := s.Apps.LiveClusterDeployment(ctx, cv.ID)
		if err != nil {
			return clusterView{}, err
		}
		if acd != nil {
			av, err := s.clusterAppView(ctx, *acd)
			if err != nil {
				return clusterView{}, err
			}
			v.App = &av
		}
	}
	return v, nil
}

// intersectRenewOptions 按档位取全员都放得下的单位数（最小值）；first 时直接采用 next。
func intersectRenewOptions(acc, next []rental.RenewOption, first bool) []rental.RenewOption {
	if first {
		return slices.Clone(next)
	}
	out := acc[:0]
	for _, o := range acc {
		i := slices.IndexFunc(next, func(n rental.RenewOption) bool { return n.Duration == o.Duration })
		if i < 0 {
			continue
		}
		o.QuantityMax = min(o.QuantityMax, next[i].QuantityMax)
		out = append(out, o)
	}
	return out
}

// clusterAppView 组装集群应用视图：成员部署经 appdeploy.Views（不序列化 Deployment），应用 / 版本 / head 地址取 head 的视图。
func (s *Server) clusterAppView(ctx context.Context, acd appdeploy.ClusterDeployment) (clusterAppView, error) {
	v := clusterAppView{ID: acd.ID, State: acd.State, AppID: acd.AppID, VersionID: acd.AppVersionID, Status: acd.Status, Health: acd.Health,
		Phase: acd.Phase, Progress: acd.Progress, CreatedAt: acd.CreatedAt, UpdatedAt: acd.UpdatedAt,
		Members: make([]clusterAppMemberView, 0, len(acd.Members)), Env: map[string]string{}, SecretEnv: map[string]bool{}}
	deps := make([]appdeploy.Deployment, 0, len(acd.Members))
	for _, m := range acd.Members {
		deps = append(deps, m.Deployment)
	}
	views, err := s.Apps.Views(ctx, deps)
	if err != nil {
		return clusterAppView{}, err
	}
	src := -1 // 取当前成员的 head；没有（换机后未重新部署）退回第一个成员部署，此时不给访问地址
	for i, m := range acd.Members {
		dv := views[i]
		v.Members = append(v.Members, clusterAppMemberView{Rank: m.Rank, Role: m.Role, InstanceID: m.InstanceID, DeploymentID: dv.ID, State: dv.State,
			Health: dv.Health, Phase: dv.Phase, Progress: dv.Progress, Message: dv.Message, Current: m.Current})
		if src < 0 && m.Role == appdeploy.RoleHead && m.Current {
			src = i
		}
	}
	head := src >= 0
	if !head && len(views) > 0 {
		src = 0
	}
	if src >= 0 {
		sv := views[src]
		v.AppSlug, v.AppName, v.AppIcon, v.Version = sv.AppSlug, sv.AppName, sv.AppIcon, sv.Version
		v.LatestVersionID, v.LatestVersion, v.UpdateAvailable = sv.LatestVersionID, sv.LatestVersion, sv.UpdateAvailable
		v.Env, v.SecretEnv, v.Spec = sv.Env, sv.SecretEnv, sv.Spec
		if head {
			v.URL = sv.URL
		}
	}
	return v, nil
}

// instanceClusters 一次请求内按集群 ID 去重读集群状态（实例视图的 cluster 段）。
func (s *Server) instanceClusters(ctx context.Context, insts []rental.Instance) (map[string]rental.ClusterView, error) {
	out := map[string]rental.ClusterView{}
	for _, i := range insts {
		if i.ClusterID == nil {
			continue
		}
		if _, ok := out[*i.ClusterID]; ok {
			continue
		}
		cv, err := s.Rental.GetClusterView(ctx, *i.ClusterID)
		if err != nil {
			return nil, err
		}
		out[*i.ClusterID] = cv
	}
	return out, nil
}

// ---- 下单辅助 ----

// parseClusterSize 解析 /availability 的 cluster_size（只接受 rental.ClusterSizes）。
func parseClusterSize(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || !slices.Contains(rental.ClusterSizes, n) {
		return 0, apperr.ErrClusterSizeInvalid
	}
	return n, nil
}

// clusterAvailability 可组成的 n 台集群数（GET /availability?cluster_size=）。
func (s *Server) clusterAvailability(ctx context.Context, productID int64, groupID *int64, n int) (rental.ClusterAvail, error) {
	if groupID != nil {
		if _, err := s.Rental.VisibleGroup(ctx, *groupID); err != nil {
			return rental.ClusterAvail{}, err
		}
	}
	m, err := s.Rental.ClusterAvailability(ctx, productID, groupID)
	if err != nil {
		return rental.ClusterAvail{}, err
	}
	return m[n], nil
}

// productClusters /products 的 clusters 段：{"2": {available, rdma}, "4": {…}}。
func (s *Server) productClusters(ctx context.Context, productID int64) (map[string]rental.ClusterAvail, error) {
	m, err := s.Rental.ClusterAvailability(ctx, productID, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]rental.ClusterAvail, len(rental.ClusterSizes))
	for _, n := range rental.ClusterSizes {
		out[strconv.Itoa(n)] = m[n]
	}
	return out, nil
}

// ---- 处理器 ----

func (s *Server) writeCluster(w http.ResponseWriter, r *http.Request, cv rental.ClusterView, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.clusterConsoleView(r.Context(), currentUser(r).ID, cv)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	cvs, err := s.Rental.ListClustersForUser(r.Context(), uid)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]clusterView, 0, len(cvs))
	for _, cv := range cvs {
		v, err := s.clusterConsoleView(r.Context(), uid, cv)
		if err != nil {
			// 单个集群组装失败（成员实例 / 续费信息 / 集群应用读不到）只跳过该项：列表其余集群照常显示，详情页会给出具体错误
			if r.Context().Err() != nil {
				s.writeError(w, r, err)
				return
			}
			s.Log.Warn("cluster list: skip cluster view", "cluster_id", cv.ID, "user_id", uid, "err", err)
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, list(out))
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	cv, err := s.Rental.ClusterForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	s.writeCluster(w, r, cv, err)
}

// handleRenewCluster 整组续费：必须带 Idempotency-Key（N 张订单一次扣款，重试不能重复扣）。
func (s *Server) handleRenewCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Duration string `json:"duration"`
		Quantity int    `json:"quantity"` // 0 / 缺省 = 1
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		s.writeError(w, r, apperr.BadRequest("集群续费需要 Idempotency-Key 请求头"))
		return
	}
	uid, id := currentUser(r).ID, chi.URLParam(r, "id")
	orders, err := s.Rental.RenewCluster(r.Context(), uid, id, catalog.Duration(req.Duration), req.Quantity, key)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cv, err := s.Rental.ClusterForUser(r.Context(), uid, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.clusterConsoleView(r.Context(), uid, cv)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if orders == nil {
		orders = []rental.Order{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "cluster": v})
}

func (s *Server) handleClusterAutoRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	cv, err := s.Rental.SetClusterAutoRenew(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), req.Enabled, catalog.Duration(req.Duration), clientIP(r))
	s.writeCluster(w, r, cv, err)
}

func (s *Server) handleReleaseCluster(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.ReleaseCluster(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), clientIP(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}

// ---- 集群应用 ----

// ownedCluster 校验集群归属（别人的 / 系统集群 / 不存在 → cluster_not_found）。
func (s *Server) ownedCluster(r *http.Request) (rental.ClusterView, error) {
	return s.Rental.ClusterForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
}

func (s *Server) writeClusterApp(w http.ResponseWriter, r *http.Request, status int, acd appdeploy.ClusterDeployment, err error) {
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.clusterAppView(r.Context(), acd)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

// handleClusterApp 当前集群应用（含成员部署）；没有 → null。
func (s *Server) handleClusterApp(w http.ResponseWriter, r *http.Request) {
	cv, err := s.ownedCluster(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	acd, err := s.Apps.LiveClusterDeployment(r.Context(), cv.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if acd == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	s.writeClusterApp(w, r, http.StatusOK, *acd, nil)
}

func (s *Server) handleCreateClusterApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	var req struct {
		AppID     int64             `json:"app_id"`
		VersionID *int64            `json:"version_id"`
		Env       map[string]string `json:"env"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	acd, err := s.Apps.CreateCluster(r.Context(), appdeploy.ClusterCreateRequest{UserID: currentUser(r).ID, ClusterID: chi.URLParam(r, "id"),
		AppID: req.AppID, VersionID: req.VersionID, Env: req.Env, IP: clientIP(r)})
	s.writeClusterApp(w, r, http.StatusCreated, acd, err)
}

// ownedClusterApp 写操作前确认集群部署在该用户的这个集群上（不在 → not_found）；appdeploy 在事务内再校验归属。
func (s *Server) ownedClusterApp(r *http.Request) (string, error) {
	acd, err := s.Apps.ClusterDeploymentForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), chi.URLParam(r, "cd"))
	if err != nil {
		return "", err
	}
	return acd.ID, nil
}

func (s *Server) handleRedeployClusterApp(w http.ResponseWriter, r *http.Request) {
	if !s.allowAppWrite(w, r) {
		return
	}
	var req struct {
		VersionID *int64          `json:"version_id"`
		Env       json.RawMessage `json:"env"`
		EnvSet    *bool           `json:"env_set"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id, err := s.ownedClusterApp(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 与单实例 redeploy 相同：env 出现（且不是 null）才算改变量；env_set 显式给出时以它为准（true 且不带 env = 普通变量回到默认值、secret 沿用）
	r2 := appdeploy.ClusterRedeployRequest{UserID: currentUser(r).ID, ClusterDeploymentID: id, VersionID: req.VersionID, IP: clientIP(r),
		EnvSet: len(req.Env) > 0 && string(req.Env) != "null"}
	if req.EnvSet != nil {
		r2.EnvSet = *req.EnvSet
	}
	if len(req.Env) > 0 && string(req.Env) != "null" {
		if err := json.Unmarshal(req.Env, &r2.Env); err != nil {
			s.writeError(w, r, apperr.BadRequest("env 格式不正确"))
			return
		}
	}
	acd, err := s.Apps.RedeployCluster(r.Context(), r2)
	s.writeClusterApp(w, r, http.StatusOK, acd, err)
}

// clusterAppOp 停止 / 启动 / 卸载的公共流程。
func (s *Server) clusterAppOp(w http.ResponseWriter, r *http.Request, op func(ctx context.Context, userID int64, id, ip string) (appdeploy.ClusterDeployment, error)) {
	if !s.allowAppWrite(w, r) {
		return
	}
	id, err := s.ownedClusterApp(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	acd, err := op(r.Context(), currentUser(r).ID, id, clientIP(r))
	s.writeClusterApp(w, r, http.StatusOK, acd, err)
}

func (s *Server) handleStopClusterApp(w http.ResponseWriter, r *http.Request) {
	s.clusterAppOp(w, r, s.Apps.StopCluster)
}

func (s *Server) handleStartClusterApp(w http.ResponseWriter, r *http.Request) {
	s.clusterAppOp(w, r, s.Apps.StartCluster)
}

func (s *Server) handleRemoveClusterApp(w http.ResponseWriter, r *http.Request) {
	s.clusterAppOp(w, r, s.Apps.RemoveCluster)
}
