package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"spark-cloud/internal/platform/apperr"
)

// 对外视图：Deployment 带 SecretEnvEnc / LogTail 等内部字段，从不直接序列化；控制台与后台一律经这里的视图输出。

// LinkView 是部署链接（主端口 URL + spec.links[i].path）。
type LinkView struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// View 与前端 Deployment（web/shared/src/types/apps.ts）一一对应。敏感变量只给「已设置」：secret_env = {NAME: true}。
type View struct {
	ID              string            `json:"id"`
	InstanceID      string            `json:"instance_id"`
	AppID           int64             `json:"app_id"`
	AppSlug         string            `json:"app_slug"`
	AppName         string            `json:"app_name"`
	AppIcon         string            `json:"app_icon"`
	VersionID       int64             `json:"version_id"`
	Version         string            `json:"version"`
	LatestVersionID *int64            `json:"latest_version_id"`
	LatestVersion   string            `json:"latest_version"`
	UpdateAvailable bool              `json:"update_available"`
	State           string            `json:"state"`
	Desired         string            `json:"desired"`
	Phase           string            `json:"phase"`
	Progress        int               `json:"progress"`
	Message         string            `json:"message"`
	Health          string            `json:"health"`
	ErrorCode       string            `json:"error_code"`
	Env             map[string]string `json:"env"`
	SecretEnv       map[string]bool   `json:"secret_env"`
	Spec            Spec              `json:"spec"`
	Port            int               `json:"port"`
	URL             *string           `json:"url"`
	Links           []LinkView        `json:"links"`
	Attempt         int               `json:"attempt"`
	CleanupPending  bool              `json:"cleanup_pending"`
	StartedAt       *time.Time        `json:"started_at"`
	FinishedAt      *time.Time        `json:"finished_at"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Summary 是实例列表 / 详情里 apps_summary 的一项。
type Summary struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress int     `json:"progress"`
	URL      *string `json:"url"`
}

// AdminView 后台视图：多 log_tail（回写时已脱敏）与 user_id。
type AdminView struct {
	View
	LogTail string `json:"log_tail"`
	UserID  int64  `json:"user_id"`
}

type appBrief struct{ slug, name, icon string }

type verBrief struct {
	version string
	spec    Spec
}

type latestBrief struct {
	id      int64
	version string
}

// Views 批量组装部署视图：应用、版本、各应用最新已发布版本、部署名下的端口行各一次查询。
func (s *Service) Views(ctx context.Context, deps []Deployment) ([]View, error) {
	out := make([]View, 0, len(deps))
	if len(deps) == 0 {
		return out, nil
	}
	appIDs, verIDs, depIDs := make([]int64, 0, len(deps)), make([]int64, 0, len(deps)), make([]string, 0, len(deps))
	for _, d := range deps {
		appIDs = append(appIDs, d.AppID)
		verIDs = append(verIDs, d.AppVersionID)
		depIDs = append(depIDs, d.ID)
	}
	apps := map[int64]appBrief{}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, slug, name, icon FROM apps WHERE id = ANY($1)`, appIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var a appBrief
		if err := rows.Scan(&id, &a.slug, &a.name, &a.icon); err != nil {
			rows.Close()
			return nil, err
		}
		apps[id] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	vers := map[int64]verBrief{}
	rows, err = s.d.Pool.Query(ctx, `SELECT id, version, spec FROM app_versions WHERE id = ANY($1)`, verIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var v verBrief
		var spec []byte
		if err := rows.Scan(&id, &v.version, &spec); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(spec, &v.spec); err != nil {
			rows.Close()
			return nil, fmt.Errorf("app_versions %d spec: %w", id, err)
		}
		vers[id] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 与 Catalog.LatestPublished 同序：published_at 最新，同时刻取 id 大的
	latest := map[int64]latestBrief{}
	rows, err = s.d.Pool.Query(ctx, `SELECT DISTINCT ON (app_id) app_id, id, version FROM app_versions
		WHERE app_id = ANY($1) AND status = 'published' ORDER BY app_id, published_at DESC, id DESC`, appIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var appID int64
		var l latestBrief
		if err := rows.Scan(&appID, &l.id, &l.version); err != nil {
			rows.Close()
			return nil, err
		}
		latest[appID] = l
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	type depPort struct {
		dep  string
		port int
	}
	ports := map[depPort]bool{}
	rows, err = s.d.Pool.Query(ctx, `SELECT deployment_id, port FROM instance_ports WHERE deployment_id = ANY($1)`, depIDs)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k depPort
		if err := rows.Scan(&k.dep, &k.port); err != nil {
			rows.Close()
			return nil, err
		}
		ports[k] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, d := range deps {
		a, ver := apps[d.AppID], vers[d.AppVersionID]
		secrets, err := openSecrets(s.d.Box, d.SecretEnvEnc)
		if err != nil {
			return nil, err
		}
		v := View{ID: d.ID, InstanceID: d.InstanceID, AppID: d.AppID, AppSlug: a.slug, AppName: a.name, AppIcon: a.icon,
			VersionID: d.AppVersionID, Version: ver.version, State: d.State, Desired: d.Desired, Phase: d.Phase, Progress: d.Progress,
			Message: d.Message, Health: d.Health, ErrorCode: d.ErrorCode, Env: map[string]string{}, SecretEnv: map[string]bool{},
			Spec: ver.spec, Port: d.Port, Links: []LinkView{}, Attempt: d.Attempt, CleanupPending: d.CleanupPending,
			StartedAt: d.StartedAt, FinishedAt: d.FinishedAt, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
		for k, val := range d.Env {
			v.Env[k] = val
		}
		for k := range secrets {
			v.SecretEnv[k] = true
		}
		if l, ok := latest[d.AppID]; ok {
			id := l.id
			v.LatestVersionID, v.LatestVersion, v.UpdateAvailable = &id, l.version, l.id != d.AppVersionID
		}
		if s.d.PortURL != nil && ports[depPort{d.ID, d.Port}] {
			u := s.d.PortURL(d.InstanceID, d.Port)
			v.URL = &u
			for _, l := range ver.spec.Links {
				v.Links = append(v.Links, LinkView{Label: l.Label, URL: strings.TrimSuffix(u, "/") + l.Path})
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// Summaries 返回各实例非终态部署的摘要（实例列表一行显示用）；没有部署的实例不在 map 里。
func (s *Service) Summaries(ctx context.Context, instanceIDs []string) (map[string][]Summary, error) {
	byInst, err := s.LiveByInstances(ctx, instanceIDs)
	if err != nil {
		return nil, err
	}
	all := []Deployment{}
	for _, id := range instanceIDs {
		all = append(all, byInst[id]...)
		delete(byInst, id) // 重复的实例 ID 只取一次
	}
	views, err := s.Views(ctx, all)
	if err != nil {
		return nil, err
	}
	out := map[string][]Summary{}
	for _, v := range views {
		out[v.InstanceID] = append(out[v.InstanceID], Summary{ID: v.ID, Name: v.AppName, State: v.State, Progress: v.Progress, URL: v.URL})
	}
	return out, nil
}

// AdminViews 后台视图（实例详情 / 部署列表）。
func (s *Service) AdminViews(ctx context.Context, deps []Deployment) ([]AdminView, error) {
	views, err := s.Views(ctx, deps)
	if err != nil {
		return nil, err
	}
	out := make([]AdminView, len(views))
	for i, v := range views {
		out[i] = AdminView{View: v, LogTail: deps[i].LogTail, UserID: deps[i].UserID}
	}
	return out, nil
}

// ---- 用户侧目录 ----

// CatalogVersionView 是目录条目里的版本：只含部署表单需要的部分（没有脚本、镜像 ID、hash）。
type CatalogVersionView struct {
	ID             int64      `json:"id"`
	Version        string     `json:"version"`
	Ports          []PortSpec `json:"ports"`
	Env            []EnvSpec  `json:"env"`
	Links          []LinkSpec `json:"links"`
	TimeoutMinutes int        `json:"timeout_minutes"`
}

// CatalogView 与前端 AppCatalogItem 对应；Description 只在单个应用接口里给。
type CatalogView struct {
	ID          int64              `json:"id"`
	Slug        string             `json:"slug"`
	Name        string             `json:"name"`
	Summary     string             `json:"summary"`
	Description string             `json:"description,omitempty"`
	Category    string             `json:"category"`
	Icon        string             `json:"icon"`
	Version     CatalogVersionView `json:"version"`
	Images      []ImageBrief       `json:"images"`
}

func catalogView(a App, v Version, images []ImageBrief, withDescription bool) CatalogView {
	cv := CatalogView{ID: a.ID, Slug: a.Slug, Name: a.Name, Summary: a.Summary, Category: a.Category, Icon: a.Icon,
		Version: CatalogVersionView{ID: v.ID, Version: v.Version, Ports: v.Spec.Ports, Env: v.Spec.Env, Links: v.Spec.Links,
			TimeoutMinutes: v.Spec.TimeoutMinutes},
		Images: images}
	if withDescription {
		cv.Description = a.Description
	}
	if cv.Version.Ports == nil {
		cv.Version.Ports = []PortSpec{}
	}
	if cv.Version.Env == nil {
		cv.Version.Env = []EnvSpec{}
	}
	if cv.Version.Links == nil {
		cv.Version.Links = []LinkSpec{}
	}
	if cv.Version.TimeoutMinutes == 0 {
		cv.Version.TimeoutMinutes = DefaultTimeoutMinutes
	}
	if cv.Images == nil {
		cv.Images = []ImageBrief{}
	}
	return cv
}

// CatalogViews 已上架应用及其最新已发布版本（市场页 / 添加应用弹窗）。
func (s *Service) CatalogViews(ctx context.Context, withDescription bool) ([]CatalogView, error) {
	items, err := s.d.Catalog.ListPublished(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CatalogView, 0, len(items))
	for _, it := range items {
		out = append(out, catalogView(it.App, it.Version, it.Images, withDescription))
	}
	return out, nil
}

// CatalogViewBySlug 单个已上架应用（带说明）；不存在、未上架或没有已发布版本 → app_not_found（与列表口径一致）。
func (s *Service) CatalogViewBySlug(ctx context.Context, slug string) (CatalogView, error) {
	a, err := s.d.Catalog.AppBySlug(ctx, slug)
	if errors.Is(err, apperr.ErrNotFound) || (err == nil && a.Status != StatusPublished) {
		return CatalogView{}, apperr.ErrAppNotFound
	}
	if err != nil {
		return CatalogView{}, err
	}
	v, err := s.d.Catalog.LatestPublished(ctx, s.d.Pool, a.ID)
	if errors.Is(err, apperr.ErrAppUnavailable) {
		return CatalogView{}, apperr.ErrAppNotFound
	}
	if err != nil {
		return CatalogView{}, err
	}
	images, err := s.d.Catalog.imageBriefs(ctx, v.ImageIDs)
	if err != nil {
		return CatalogView{}, err
	}
	return catalogView(a, v, images, true), nil
}
