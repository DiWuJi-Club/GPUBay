package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/butler/guard"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

// 8 个写工具（规格 §4.2）：模型调用时只生成确认卡（Summary，在 pending 落库前调用），用户点确认后才 Exec。
//
// Summary：
//   - 文案与金额全部来自真实 args 与服务端数据（套餐 / 镜像 / 应用名、价格、实例 ID），不取模型的任何文字；
//   - 所有实例 / 部署 ID 先经 GetForUser 校验归属（他人 → not_found，不生成卡）；能提前发现的失败（状态不对、租期超限、
//     变量缺失、镜像不兼容）在这里就以错误返回，由 Task 7 用 ErrorResult 回填模型；
//   - env / app_env 里 type = secret 的变量：Card.SecretEnv 放真实值（Task 7 用 SealSecretEnv 加密进 secret_env_enc），
//     Card.ArgsStored 与 Card.StoredSummary 里是 "***"，Card.Summary 显示真实值（只进 SSE confirm 事件与 GET 会话的 pending_tool_call）。
//
// Exec（Confirm 时调用）：
//   - args 必须是 MergeSecretEnv(ArgsStored, 解密后的 SecretEnv) 的结果；仍含 "***" 的 secret 变量 → 拒绝执行（不会把 *** 当密码部署）；
//   - 再次 GetForUser 校验归属（服务层也会复核）；
//   - 下单 / 续费把 Caller.IdemKey（"butler:" + tc.ID）作为幂等键传给 PlaceBatch / RenewQuantity，重复确认只产生一笔订单；
//     部署类接口本身幂等（重复 Create → app_exists、重复 Remove 返回当前行）；
//   - 业务错误（apperr / 参数错误）不返回 error，而是 Result{Content: {"ok":false,"code","message"}}（与 ErrorResult 相同的裸 JSON）；
//     其它错误原样返回（Task 7 记 failed + 通用文案）；
//   - 成功结果与读工具同样 guard.JSONWrap(<工具名>, {user}, {"ok": true, …})。
//
// 审计 butler.tool_execute 由 Task 7 在执行后记录，这里不写。

// SecretMask 是 secret 变量在落库 args / summary 里的占位值。
const SecretMask = "***"

// ErrCardInvalid：MergeSecretEnv / OpenSecretEnv / OpenCard 的失败（密文损坏、args 与密文对不上）都包这个错误
// （errors.Is 可判断）。它不是业务错误（IsBusinessError = false）；Run 在 Confirm 时把它映射为 apperr.ErrButlerConfirmInvalid，
// 并照常清空 secret_env_enc。
var ErrCardInvalid = errors.New("butler tools: confirm card secrets invalid")

// 可选能力（真实服务都有；Task 7 等处的 fake 不必实现）：有就在确认卡前多做一步预检，没有就跳过。
type groupLookup interface {
	VisibleGroup(ctx context.Context, id int64) (fleet.Group, error)
}

type rentChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

// quoteChanged：确认时价格 / 退款已与卡片不同（对用户不利的方向），要求重新发起。
func quoteChanged(what string, now int64) error {
	return apperr.New("quote_changed", what+"已变化（现为 "+yuan(now)+"），请重新发起", 409)
}

const propEnv = `{"type": "object", "additionalProperties": {"type": "string", "maxLength": 1024}, "description": "%s"}`

func writeTools(d Deps) []Tool {
	h := writeHandlers{readHandlers{d: d}}
	return []Tool{
		{Name: "place_order", Kind: Write, Summary: h.placeOrderCard, Exec: h.guarded(h.placeOrder),
			Schema: objSchema([]string{"product_id", "duration", "image_id"},
				`"product_id": {"type": "integer", "minimum": 1, "description": "套餐 ID，来自 list_products"}`,
				propDuration,
				`"quantity": {"type": "integer", "minimum": 1, "maximum": 90, "default": 1, "description": "租期单位数，如 3 个 1d = 3 天（总时长不超过 90 天）"}`,
				`"image_id": {"type": "integer", "minimum": 1, "description": "镜像 ID，来自 list_products 的 images"}`,
				`"count": {"type": "integer", "minimum": 1, "maximum": 8, "default": 1, "description": "台数"}`,
				`"start_at": {"type": "string", "maxLength": 40, "description": "预订开始时间（RFC3339，北京时间整点，至少 2 小时后、60 天内，如 2026-10-20T10:00:00+08:00）；不填表示立即租用"}`,
				`"group_id": {"type": "integer", "minimum": 1, "description": "机组 ID（可选）"}`,
				`"auto_renew": {"type": "boolean", "default": false, "description": "到期自动续费（按同一租期档位）"}`,
				`"app_id": {"type": "integer", "minimum": 1, "description": "开通后预装的应用 ID（来自 list_apps，可选）"}`,
				`"app_env": `+fmt.Sprintf(propEnv, "预装应用的变量（变量名 → 值，按 get_app 的 version.env 填写）")),
			Description: "租用实例（立即或预订，可一次多台、可预装应用）。只生成确认卡，用户在卡片上确认后才下单扣款。"},
		{Name: "renew_instance", Kind: Write, Summary: h.renewCard, Exec: h.guarded(h.renew),
			Schema: objSchema([]string{"instance_id", "duration"}, propInstanceID, propDuration,
				`"quantity": {"type": "integer", "minimum": 1, "maximum": 90, "default": 1, "description": "租期单位数，如 3 个 1d = 3 天；上限见 get_instance 的 renew_options.quantity_max"}`),
			Description: "为实例续费。只生成确认卡，用户确认后才扣款。"},
		{Name: "release_instance", Kind: Write, Danger: true, Summary: h.releaseCard, Exec: h.guarded(h.release),
			Schema:      objSchema([]string{"instance_id"}, propInstanceID),
			Description: "释放实例（立即清除数据且不可恢复，已付费用不退）。只生成确认卡，用户确认后才执行。只有用户明确要求释放时才调用。"},
		{Name: "cancel_booking", Kind: Write, Danger: true, Summary: h.cancelCard, Exec: h.guarded(h.cancel),
			Schema:      objSchema([]string{"instance_id"}, propInstanceID),
			Description: "取消一个未开通的预订并按规则退款（先用 get_cancel_quote 查退款）。只生成确认卡，用户确认后才执行。"},
		{Name: "deploy_app", Kind: Write, Summary: h.deployCard, Exec: h.guarded(h.deploy),
			Schema: objSchema([]string{"instance_id", "app_slug"}, propInstanceID,
				`"app_slug": {"type": "string", "pattern": "`+slugPattern+`", "description": "应用 slug，来自 list_apps"}`,
				`"env": `+fmt.Sprintf(propEnv, "变量（变量名 → 值，按 get_app 的 version.env 填写；secret 类变量可用 generate_secret 生成）")),
			Description: "在实例上部署一个应用。只生成确认卡，用户确认后才执行。"},
		{Name: "redeploy_app", Kind: Write, Summary: h.redeployCard, Exec: h.guarded(h.redeploy),
			Schema: objSchema([]string{"instance_id", "deployment_id"}, propInstanceID, propDeploymentID,
				`"env": `+fmt.Sprintf(propEnv, "不填表示全部变量沿用；填写时普通变量以此为准（未填的恢复默认），secret 变量未填沿用、填空串清除")),
			Description: "把一个部署重新部署为应用的最新版本（可同时修改变量）。只生成确认卡，用户确认后才执行。"},
		{Name: "set_app_state", Kind: Write, Summary: h.appStateCard, Exec: h.guarded(h.appState),
			Schema: objSchema([]string{"instance_id", "deployment_id", "action"}, propInstanceID, propDeploymentID,
				`"action": {"type": "string", "enum": ["stop", "start"], "description": "stop = 停止，start = 启动"}`),
			Description: "停止或启动实例上的一个应用。只生成确认卡，用户确认后才执行。"},
		{Name: "remove_app", Kind: Write, Danger: true, Summary: h.removeCard, Exec: h.guarded(h.remove),
			Schema:      objSchema([]string{"instance_id", "deployment_id"}, propInstanceID, propDeploymentID),
			Description: "卸载实例上的一个应用（删除应用文件，模型缓存保留）。只生成确认卡，用户确认后才执行。"},
	}
}

type writeHandlers struct{ readHandlers }

type execFunc func(ctx context.Context, c Caller, args json.RawMessage) (Result, error)

// guarded 把业务错误转成 {"ok":false,…} 结果（err = nil），其它错误原样返回。
func (h writeHandlers) guarded(fn execFunc) execFunc {
	return func(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
		res, err := fn(ctx, c, args)
		if err != nil {
			if IsBusinessError(err) {
				return ErrorResult(err), nil
			}
			return Result{}, err
		}
		return res, nil
	}
}

func okResult(name string, c Caller, v map[string]any, ref string) (Result, error) {
	v["ok"] = true
	return wrapOne(name, c, v, smallLimit, ref)
}

// ---- 格式化 ----

// yuan 把分格式化为「¥X.XX」。
func yuan(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("¥%s%d.%02d", sign, cents/100, cents%100)
}

// durationText 是总时长的中文：1d × 3 → 「3 天」，1w × 2 → 「2 周」，1m × 1 → 「1 个月」。
func durationText(d catalog.Duration, quantity int) string {
	if quantity < 1 {
		quantity = 1
	}
	unit := string(d)
	switch d {
	case catalog.D1d:
		unit = "天"
	case catalog.D1w:
		unit = "周"
	case catalog.D1m:
		unit = "个月"
	}
	return fmt.Sprintf("%d %s", quantity, unit)
}

// display 是卡片里的一段名称 / 变量值：净化、截断、压成一行。
func display(s string) string {
	s = clean(s, 200)
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s)
}

// displayValue 是卡片里的变量值：不截断（值已由 schema 限 1024 字节、splitSecrets / ValidateEnv 保证无控制字符与换行；
// 默认值来自版本 spec，同样经过校验），只做净化兜底。secret 必须完整显示，用户要照着记下来。
func displayValue(s string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(guard.Sanitize(s))
}

// ---- secret 变量 ----

func secretNames(env []appdeploy.EnvSpec) map[string]bool {
	out := map[string]bool{}
	for _, e := range env {
		if e.Type == "secret" {
			out[e.Name] = true
		}
	}
	return out
}

// splitSecrets 把 env 里 secret 变量的非空值换成 ***，真实值放进 secrets。模型直接写 *** 视为参数错误
// （它多半是从历史里抄来的占位值，不是真实密钥）。
func splitSecrets(env map[string]string, secret map[string]bool, field string) (stored, secrets map[string]string, err error) {
	if env == nil {
		return nil, nil, nil
	}
	stored, secrets = map[string]string{}, map[string]string{}
	for k, v := range env {
		if guard.Sanitize(v) != v {
			return nil, nil, &ValidationError{Path: field + "." + k, Reason: "不能包含控制字符、格式字符或换行"}
		}
		if secret[k] && v == SecretMask {
			return nil, nil, &ValidationError{Path: field + "." + k, Reason: "不能是 ***（那是已隐藏的旧值）；需要新密钥时请先调用 generate_secret，沿用旧值时不要填写该变量"}
		}
		if secret[k] && v != "" {
			stored[k], secrets[k] = SecretMask, v
			continue
		}
		stored[k] = v
	}
	if len(secrets) == 0 {
		secrets = nil
	}
	return stored, secrets, nil
}

// maskedArgs 返回把 args[field] 换成 stored 后的 args（其它字段原样）。
func maskedArgs(args json.RawMessage, field string, stored map[string]string) (json.RawMessage, error) {
	if stored == nil {
		return args, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, &ValidationError{Reason: "不是合法的 JSON"}
	}
	b, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	m[field] = b
	return json.Marshal(m)
}

// checkNoMask：执行前 secret 变量不得仍是 ***（Task 7 未合并 secret_env_enc，或密文已被清掉）。
func checkNoMask(env map[string]string, secret map[string]bool) error {
	for k, v := range env {
		if secret[k] && v == SecretMask {
			return apperr.ErrButlerConfirmInvalid
		}
	}
	return nil
}

// MergeSecretEnv 是 Task 7 在 Confirm 执行写工具前调用的：把 secret_env_enc 解密出的真实值（Card.SecretEnv 原样）
// 合并回落库的 args（Card.ArgsStored）。每个键必须在 args 的 env 或 app_env 里且当前值为 ***，否则返回包了 ErrCardInvalid 的错误
// （防止串卡 / 篡改；Run 映射为 butler_confirm_invalid）；secrets 为空时原样返回 args。返回值直接传给 Tool.Exec。
func MergeSecretEnv(args json.RawMessage, secrets map[string]string) (json.RawMessage, error) {
	if len(secrets) == 0 {
		return args, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, fmt.Errorf("%w: args: %v", ErrCardInvalid, err)
	}
	envs := map[string]map[string]string{}
	for _, field := range []string{"env", "app_env"} {
		raw, ok := m[field]
		if !ok {
			continue
		}
		var e map[string]string
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("%w: args.%s: %v", ErrCardInvalid, field, err)
		}
		envs[field] = e
	}
	for k, v := range secrets {
		placed := false
		for _, field := range []string{"env", "app_env"} {
			if e := envs[field]; e != nil && e[k] == SecretMask {
				e[k], placed = v, true
				break
			}
		}
		if !placed {
			return nil, fmt.Errorf("%w: %q is not a masked variable in args", ErrCardInvalid, k)
		}
	}
	for field, e := range envs {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		m[field] = b
	}
	return json.Marshal(m)
}

// sealedCard 是 secret_env_enc 的新格式：secret 真实值 + 含真实值的卡片文案（GET 会话的 pending_tool_call 显示它）。
type sealedCard struct {
	Env     map[string]string `json:"env"`
	Summary string            `json:"summary,omitempty"`
}

// SealCard 把 Card.SecretEnv 与 Card.Summary（含真实值）一起加密为 secret_env_enc 列的值；没有 secret → nil（列为 NULL，
// 此时 StoredSummary == Summary，不需要另存）。
func SealCard(box *secretbox.Box, secrets map[string]string, summary string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if box == nil {
		return nil, errors.New("butler tools: secretbox not configured")
	}
	b, err := json.Marshal(sealedCard{Env: secrets, Summary: summary})
	if err != nil {
		return nil, err
	}
	return box.Seal(b), nil
}

// OpenCard 解密 SealCard（也接受 SealSecretEnv 的旧格式，summary 为空）；nil / 空 → nil, "", nil。
// 解密 / 解析失败返回包了 ErrCardInvalid 的错误。
func OpenCard(box *secretbox.Box, enc []byte) (secrets map[string]string, summary string, err error) {
	if len(enc) == 0 {
		return nil, "", nil
	}
	if box == nil {
		return nil, "", errors.New("butler tools: secretbox not configured")
	}
	b, err := box.Open(enc)
	if err != nil {
		return nil, "", fmt.Errorf("%w: open: %v", ErrCardInvalid, err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, "", fmt.Errorf("%w: decode: %v", ErrCardInvalid, err)
	}
	// 新格式的 env 是对象；旧格式（变量名 → 字符串）里不会出现对象值，两者不会混淆
	if raw, ok := probe["env"]; ok && len(raw) > 0 && raw[0] == '{' {
		var c sealedCard
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, "", fmt.Errorf("%w: decode: %v", ErrCardInvalid, err)
		}
		return c.Env, c.Summary, nil
	}
	var old map[string]string
	if err := json.Unmarshal(b, &old); err != nil {
		return nil, "", fmt.Errorf("%w: decode: %v", ErrCardInvalid, err)
	}
	return old, "", nil
}

// SealSecretEnv 只加密 secret 值（旧格式，不含卡片文案）；新代码用 SealCard。空 map → nil（列为 NULL）。
func SealSecretEnv(box *secretbox.Box, secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	if box == nil {
		return nil, errors.New("butler tools: secretbox not configured")
	}
	b, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	return box.Seal(b), nil
}

// OpenSecretEnv 解密 secret_env_enc 里的 secret 值，SealCard 与 SealSecretEnv 两种格式都接受；nil / 空 → nil map。
// 失败返回包了 ErrCardInvalid 的错误。
func OpenSecretEnv(box *secretbox.Box, sealed []byte) (map[string]string, error) {
	secrets, _, err := OpenCard(box, sealed)
	return secrets, err
}

// envLines 按变量声明顺序列出将生效的变量（ValidateEnv 的结果，含默认值）；maskSecret 时 secret 显示 ***。
func envLines(specEnv []appdeploy.EnvSpec, plain, secret map[string]string, maskSecret bool) string {
	var b strings.Builder
	for _, e := range specEnv {
		v, ok := plain[e.Name]
		if e.Type == "secret" {
			v, ok = secret[e.Name]
			if ok && maskSecret {
				v = SecretMask
			}
		}
		if !ok {
			continue
		}
		b.WriteString("\n- " + envLabel(e) + "：" + displayValue(v))
	}
	if b.Len() == 0 {
		return "无"
	}
	return b.String()
}

func envLabel(e appdeploy.EnvSpec) string {
	if e.Label != "" {
		return display(e.Label)
	}
	return e.Name
}

func versionSpec(v appdeploy.CatalogView) appdeploy.Spec {
	return appdeploy.Spec{Ports: v.Version.Ports, TimeoutMinutes: v.Version.TimeoutMinutes, Env: v.Version.Env, Links: v.Version.Links}
}

// imageCompatible 与 appdeploy.checkImage 同口径：版本的兼容镜像里须有该镜像。
func imageCompatible(v appdeploy.CatalogView, imageID int64, imageName string) error {
	need := make([]string, 0, len(v.Images))
	for _, im := range v.Images {
		if im.ID == imageID {
			return nil
		}
		need = append(need, im.Name)
	}
	if imageName == "" {
		imageName = fmt.Sprintf("#%d", imageID)
	}
	return apperr.AppImageMismatch(need, imageName)
}

// ---- place_order ----

type placeArgs struct {
	ProductID int64             `json:"product_id"`
	Duration  string            `json:"duration"`
	Quantity  int               `json:"quantity"`
	ImageID   int64             `json:"image_id"`
	Count     int               `json:"count"`
	StartAt   string            `json:"start_at"`
	GroupID   *int64            `json:"group_id"`
	AutoRenew bool              `json:"auto_renew"`
	AppID     *int64            `json:"app_id"`
	AppEnv    map[string]string `json:"app_env"`
}

func (a *placeArgs) normalize() {
	if a.Quantity < 1 {
		a.Quantity = 1
	}
	if a.Count < 1 {
		a.Count = 1
	}
}

func (h writeHandlers) appByID(ctx context.Context, id int64) (appdeploy.CatalogView, error) {
	views, err := h.d.Apps.CatalogViews(ctx, false)
	if err != nil {
		return appdeploy.CatalogView{}, err
	}
	for _, v := range views {
		if v.ID == id {
			return v, nil
		}
	}
	return appdeploy.CatalogView{}, apperr.ErrAppNotFound
}

func (h writeHandlers) placeOrderCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a placeArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	a.normalize()
	d := catalog.Duration(a.Duration)
	if a.Quantity > rental.MaxQuantity(d) {
		return Card{}, apperr.ErrBookingTooLong
	}
	if rc, ok := h.d.Users.(rentChecker); ok { // 封禁 / 未实名（与 PlaceBatch 的 CanRent 相同）
		if err := rc.CanRent(ctx, c.UserID); err != nil {
			return Card{}, err
		}
	}
	now := h.now(c)
	var start *time.Time
	if a.StartAt != "" {
		t, err := time.Parse(time.RFC3339, a.StartAt)
		if err != nil {
			return Card{}, &ValidationError{Path: "start_at", Reason: "格式应为 RFC3339，例如 2026-10-20T10:00:00+08:00"}
		}
		t = t.UTC()
		switch {
		case !tz.IsHour(t):
			return Card{}, apperr.ErrBookingNotHour
		case t.Before(now.Add(rental.MinBookingLead - rental.BookingLeadTolerance)):
			return Card{}, apperr.ErrBookingTooSoon
		case t.After(now.Add(rental.MaxBookingAhead)):
			return Card{}, apperr.ErrBookingTooFar
		}
		start = &t
	}
	unit, err := h.d.Catalog.Quote(ctx, h.d.Pool, a.ProductID, d)
	if err != nil {
		return Card{}, err
	}
	product, err := h.d.Catalog.ProductName(ctx, a.ProductID)
	if err != nil {
		return Card{}, err
	}
	images, err := h.d.Catalog.ListImages(ctx)
	if err != nil {
		return Card{}, err
	}
	idx := slices.IndexFunc(images, func(im catalog.Image) bool { return im.ID == a.ImageID })
	if idx < 0 {
		return Card{}, apperr.BadRequest("镜像不存在或已下架")
	}
	imageName := images[idx].Name

	var app *appdeploy.CatalogView
	var stored, secrets map[string]string
	envPlain, envSecret := map[string]string{}, map[string]string{}
	if a.AppID == nil && len(a.AppEnv) > 0 {
		return Card{}, apperr.BadRequest("未选择预装应用时不能填写应用变量")
	}
	if a.AppID != nil {
		v, err := h.appByID(ctx, *a.AppID)
		if err != nil {
			return Card{}, err
		}
		if err := imageCompatible(v, a.ImageID, imageName); err != nil {
			return Card{}, err
		}
		if stored, secrets, err = splitSecrets(a.AppEnv, secretNames(v.Version.Env), "app_env"); err != nil {
			return Card{}, err
		}
		if envPlain, envSecret, err = appdeploy.ValidateEnv(versionSpec(v), a.AppEnv); err != nil {
			return Card{}, err
		}
		app = &v
	}

	groupName := ""
	if a.GroupID != nil {
		groupName = fmt.Sprintf("#%d", *a.GroupID)
		if gl, ok := h.d.Rental.(groupLookup); ok {
			g, err := gl.VisibleGroup(ctx, *a.GroupID) // 不存在 / 不可见 → group_not_found
			if errors.Is(err, apperr.ErrNotFound) {
				err = apperr.ErrGroupNotFound
			}
			if err != nil {
				return Card{}, err
			}
			groupName = g.Name
		}
	}

	av, err := h.d.Rental.Availability(ctx, rental.AvailabilityQuery{ProductID: a.ProductID, GroupID: a.GroupID, StartAt: start,
		Duration: d, Quantity: a.Quantity})
	if err != nil {
		return Card{}, err
	}
	if av.Available < a.Count {
		if start == nil {
			return Card{}, apperr.New("no_capacity", fmt.Sprintf("现在只有 %d 台可立即租用", av.Available), 409)
		}
		return Card{}, apperr.New("no_capacity", fmt.Sprintf("所选时段只有 %d 台可预订", av.Available), 409)
	}

	total := unit * int64(a.Quantity) * int64(a.Count)
	if h.d.Wallet != nil {
		bal, err := h.d.Wallet.Balance(ctx, c.UserID)
		if err != nil {
			return Card{}, err
		}
		if bal < total {
			return Card{}, apperr.New("insufficient_balance", fmt.Sprintf("余额不足：需要 %s，当前余额 %s，请先充值", yuan(total), yuan(bal)), 402)
		}
	}
	head := fmt.Sprintf("租用 %s × %d，%s，镜像 %s", display(product), a.Count, durationText(d, a.Quantity), display(imageName))
	if a.GroupID != nil {
		head += "，机组 " + display(groupName)
	}
	if app != nil {
		head += "，预装 " + display(app.Name)
	}
	if start != nil {
		head += "，预订 " + tz.Format(*start) + " 开始"
	}
	if a.AutoRenew {
		head += "，到期自动续费"
	}
	head += "，预计扣款 " + yuan(total)
	card := Card{Summary: head, StoredSummary: head, Quote: &total, ArgsStored: args, SecretEnv: secrets}
	if app != nil && len(envPlain)+len(envSecret) > 0 {
		spec := app.Version.Env
		card.Summary += "\n预装应用变量：" + envLines(spec, envPlain, envSecret, false)
		card.StoredSummary += "\n预装应用变量：" + envLines(spec, envPlain, envSecret, true)
	}
	if card.ArgsStored, err = maskedArgs(args, "app_env", stored); err != nil {
		return Card{}, err
	}
	return card, nil
}

func (h writeHandlers) placeOrder(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a placeArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	a.normalize()
	if c.IdemKey == "" {
		return Result{}, errors.New("butler tools: place_order requires an idempotency key")
	}
	if a.AppID != nil && len(a.AppEnv) > 0 {
		v, err := h.appByID(ctx, *a.AppID)
		if err != nil {
			return Result{}, err
		}
		if err := checkNoMask(a.AppEnv, secretNames(v.Version.Env)); err != nil {
			return Result{}, err
		}
	}
	var start *time.Time
	if a.StartAt != "" {
		t, err := time.Parse(time.RFC3339, a.StartAt)
		if err != nil {
			return Result{}, &ValidationError{Path: "start_at", Reason: "格式应为 RFC3339"}
		}
		t = t.UTC()
		start = &t
	}
	if c.QuotedCents != nil {
		unit, err := h.d.Catalog.Quote(ctx, h.d.Pool, a.ProductID, catalog.Duration(a.Duration))
		if err != nil {
			return Result{}, err
		}
		if now := unit * int64(a.Quantity) * int64(a.Count); now > *c.QuotedCents {
			return Result{}, quoteChanged("价格", now)
		}
	}
	res, err := h.d.Rental.PlaceBatch(ctx, rental.BatchRequest{UserID: c.UserID, ProductID: a.ProductID, ImageID: a.ImageID, GroupID: a.GroupID,
		Count: a.Count, Duration: catalog.Duration(a.Duration), Quantity: a.Quantity, StartAt: start, AutoRenew: a.AutoRenew,
		IdempotencyKey: c.IdemKey, AppID: a.AppID, AppEnv: a.AppEnv})
	if err != nil {
		return Result{}, err
	}
	batchID := ""
	if res.Batch != nil {
		batchID = res.Batch.ID
	}
	ids, orderIDs := []string{}, []string{}
	for _, i := range res.Instances {
		ids = append(ids, i.ID)
	}
	var amount int64
	for _, o := range res.Orders {
		amount += o.Amount
		orderIDs = append(orderIDs, o.ID)
	}
	ref := ""
	if len(ids) > 0 {
		ref = ids[0]
	}
	return okResult("place_order", c, map[string]any{"batch_id": batchID, "instance_ids": ids, "order_ids": orderIDs,
		"amount_cents": amount, "scheduled_start_at": tsp(start)}, ref)
}

// ---- renew_instance ----

type renewArgs struct {
	InstanceID string `json:"instance_id"`
	Duration   string `json:"duration"`
	Quantity   int    `json:"quantity"`
}

func (h writeHandlers) renewCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a renewArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	if a.Quantity < 1 {
		a.Quantity = 1
	}
	inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID)
	if err != nil {
		return Card{}, err
	}
	if !renewable(inst.State) {
		return Card{}, apperr.ErrInvalidState
	}
	info, err := h.d.Rental.RenewInfoFor(ctx, inst)
	if err != nil {
		return Card{}, err
	}
	d := catalog.Duration(a.Duration)
	idx := slices.IndexFunc(info.Options, func(o rental.RenewOption) bool { return o.Duration == d })
	if idx < 0 {
		return Card{}, apperr.BadRequest("该实例不支持这个续费档位")
	}
	if qmax := info.Options[idx].QuantityMax; a.Quantity > qmax {
		switch {
		case qmax <= 0 && info.RenewableUntil != nil:
			return Card{}, apperr.New("renew_blocked", "该机器已有后续预订，本次最多可续至 "+tz.Format(*info.RenewableUntil)+"，这个档位放不下", 409)
		case info.RenewableUntil != nil:
			return Card{}, apperr.New("renew_blocked", fmt.Sprintf("该机器已有后续预订，这个档位最多续 %d 个单位", qmax), 409)
		default:
			return Card{}, apperr.ErrBookingTooLong
		}
	}
	unit, err := h.d.Catalog.Quote(ctx, h.d.Pool, inst.ProductID, d)
	if err != nil {
		return Card{}, err
	}
	product, err := newNameCache(h.d).product(ctx, inst.ProductID)
	if err != nil {
		return Card{}, err
	}
	total := unit * int64(a.Quantity)
	s := fmt.Sprintf("续费实例 %s（%s）%s，预计扣款 %s", inst.ID, display(product), durationText(d, a.Quantity), yuan(total))
	now := h.now(c)
	if inst.State == rental.StateRunning && !inst.Suspended() && inst.ExpiresAt != nil {
		base := *inst.ExpiresAt
		if base.Before(now) {
			base = now
		}
		s += "，到期延至 " + tz.Format(base.Add(d.Length()*time.Duration(a.Quantity)))
	}
	return Card{Summary: s, StoredSummary: s, Quote: &total, ArgsStored: args}, nil
}

func (h writeHandlers) renew(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a renewArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	if a.Quantity < 1 {
		a.Quantity = 1
	}
	if c.IdemKey == "" {
		return Result{}, errors.New("butler tools: renew_instance requires an idempotency key")
	}
	inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID)
	if err != nil {
		return Result{}, err
	}
	if c.QuotedCents != nil {
		unit, err := h.d.Catalog.Quote(ctx, h.d.Pool, inst.ProductID, catalog.Duration(a.Duration))
		if err != nil {
			return Result{}, err
		}
		if now := unit * int64(a.Quantity); now > *c.QuotedCents {
			return Result{}, quoteChanged("价格", now)
		}
	}
	ord, err := h.d.Rental.RenewQuantity(ctx, c.UserID, a.InstanceID, catalog.Duration(a.Duration), a.Quantity, c.IdemKey)
	if err != nil {
		return Result{}, err
	}
	out := map[string]any{"order_id": ord.ID, "instance_id": a.InstanceID, "amount_cents": ord.Amount}
	if inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID); err == nil {
		out["state"], out["expires_at"] = inst.State, tsp(inst.ExpiresAt)
	}
	return okResult("renew_instance", c, out, a.InstanceID)
}

// ---- release_instance / cancel_booking ----

type instanceArgs struct {
	InstanceID string `json:"instance_id"`
}

func (h writeHandlers) releaseCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a instanceArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID)
	if err != nil {
		return Card{}, err
	}
	if inst.State != rental.StateRunning && inst.State != rental.StateStopped { // 与 rental.Release 一致
		return Card{}, apperr.ErrInvalidState
	}
	product, err := newNameCache(h.d).product(ctx, inst.ProductID)
	if err != nil {
		return Card{}, err
	}
	s := fmt.Sprintf("释放实例 %s（%s），数据将被清除且不可恢复，已付费用不退", inst.ID, display(product))
	return Card{Summary: s, StoredSummary: s, ArgsStored: args}, nil
}

func (h writeHandlers) release(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a instanceArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	if _, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID); err != nil {
		return Result{}, err
	}
	if err := h.d.Rental.Release(ctx, c.UserID, a.InstanceID); err != nil {
		return Result{}, err
	}
	out := map[string]any{"instance_id": a.InstanceID}
	if inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID); err == nil {
		out["state"] = inst.State
	}
	return okResult("release_instance", c, out, a.InstanceID)
}

func (h writeHandlers) cancelCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a instanceArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID)
	if err != nil {
		return Card{}, err
	}
	q, err := h.d.Rental.CancelQuote(ctx, c.UserID, inst.ID) // 只有未开通的预订能取消（CancelQuote 内校验）
	if err != nil {
		return Card{}, err
	}
	product, err := newNameCache(h.d).product(ctx, inst.ProductID)
	if err != nil {
		return Card{}, err
	}
	what := display(product)
	if inst.ScheduledStartAt != nil {
		what += "，" + tz.Format(*inst.ScheduledStartAt) + " 开始"
	}
	refund := q.Refund
	s := fmt.Sprintf("取消预订 %s（%s），退款 %s（%d%%）", inst.ID, what, yuan(refund), q.Percent)
	return Card{Summary: s, StoredSummary: s, Quote: &refund, ArgsStored: args}, nil
}

func (h writeHandlers) cancel(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a instanceArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	if _, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID); err != nil {
		return Result{}, err
	}
	if c.QuotedCents != nil {
		q, err := h.d.Rental.CancelQuote(ctx, c.UserID, a.InstanceID)
		if err != nil {
			return Result{}, err
		}
		if q.Refund < *c.QuotedCents {
			return Result{}, quoteChanged("退款金额", q.Refund)
		}
	}
	inst, err := h.d.Rental.Cancel(ctx, c.UserID, a.InstanceID, c.IP)
	if err != nil {
		return Result{}, err
	}
	return okResult("cancel_booking", c, map[string]any{"instance_id": a.InstanceID, "state": inst.State}, a.InstanceID)
}

// ---- deploy_app ----

type deployArgs struct {
	InstanceID string            `json:"instance_id"`
	AppSlug    string            `json:"app_slug"`
	Env        map[string]string `json:"env"`
}

func (h writeHandlers) deployCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a deployArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	inst, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID)
	if err != nil {
		return Card{}, err
	}
	switch inst.State { // 与 appdeploy.Create 一致（D18：预订 / 开通中也可以先排队部署）
	case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
	default:
		return Card{}, apperr.ErrInvalidState
	}
	v, err := h.d.Apps.CatalogViewBySlug(ctx, a.AppSlug)
	if err != nil {
		return Card{}, err
	}
	imageName, err := newNameCache(h.d).image(ctx, inst.ImageID)
	if err != nil {
		return Card{}, err
	}
	if err := imageCompatible(v, inst.ImageID, imageName); err != nil {
		return Card{}, err
	}
	if err := h.checkSlots(ctx, inst.ID, v); err != nil {
		return Card{}, err
	}
	stored, secrets, err := splitSecrets(a.Env, secretNames(v.Version.Env), "env")
	if err != nil {
		return Card{}, err
	}
	plain, secret, err := appdeploy.ValidateEnv(versionSpec(v), a.Env)
	if err != nil {
		return Card{}, err
	}
	head := fmt.Sprintf("在实例 %s 上部署 %s %s（镜像兼容：是）", inst.ID, display(v.Name), display(v.Version.Version))
	if inst.State != rental.StateRunning {
		head += "，实例运行后自动开始"
	}
	head += "，变量："
	card := Card{Summary: head + envLines(v.Version.Env, plain, secret, false), StoredSummary: head + envLines(v.Version.Env, plain, secret, true),
		SecretEnv: secrets}
	if card.ArgsStored, err = maskedArgs(args, "env", stored); err != nil {
		return Card{}, err
	}
	return card, nil
}

// checkSlots 与 appdeploy.Create 同序同口径的预检：实例上非终态部署 ≥ MaxAppsPerInstance → app_limit；同一应用已部署 → app_exists；
// 应用端口与实例现有端口（用户端口与部署登记的）或其它非终态部署声明的端口冲突 → app_port_conflict；登记后超过 5 个 → port_limit。
func (h writeHandlers) checkSlots(ctx context.Context, instanceID string, v appdeploy.CatalogView) error {
	deps, err := h.d.Apps.ListForInstance(ctx, instanceID) // 调用方已 GetForUser
	if err != nil {
		return err
	}
	live := make([]appdeploy.Deployment, 0, len(deps))
	for _, d := range deps {
		if d.Live() {
			live = append(live, d)
		}
	}
	if len(live) >= appdeploy.MaxAppsPerInstance {
		return apperr.ErrAppLimit
	}
	if slices.ContainsFunc(live, func(d appdeploy.Deployment) bool { return d.AppID == v.ID }) {
		return apperr.ErrAppExists
	}
	ports, err := h.d.Rental.ListPorts(ctx, instanceID)
	if err != nil {
		return err
	}
	taken := map[int]bool{}
	for _, p := range ports {
		taken[p.Port] = true
	}
	if len(live) > 0 {
		views, err := h.d.Apps.Views(ctx, live)
		if err != nil {
			return err
		}
		for _, lv := range views {
			for _, p := range lv.Spec.Ports {
				taken[p.Port] = true
			}
		}
	}
	for _, p := range v.Version.Ports {
		if taken[p.Port] {
			return apperr.AppPortConflict(p.Port)
		}
	}
	if len(taken)+len(v.Version.Ports) > rental.MaxPortsPerInstance {
		return apperr.ErrPortLimit
	}
	return nil
}

func (h writeHandlers) deploy(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a deployArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	if _, err := h.d.Rental.GetForUser(ctx, c.UserID, a.InstanceID); err != nil {
		return Result{}, err
	}
	v, err := h.d.Apps.CatalogViewBySlug(ctx, a.AppSlug)
	if err != nil {
		return Result{}, err
	}
	if err := checkNoMask(a.Env, secretNames(v.Version.Env)); err != nil {
		return Result{}, err
	}
	dep, err := h.d.Apps.Create(ctx, appdeploy.CreateRequest{UserID: c.UserID, InstanceID: a.InstanceID, AppID: v.ID, Env: a.Env, IP: c.IP})
	if err != nil {
		return Result{}, err
	}
	return okResult("deploy_app", c, map[string]any{"instance_id": a.InstanceID,
		"deployment": map[string]any{"id": dep.ID, "state": dep.State}}, dep.ID)
}

// ---- redeploy_app / set_app_state / remove_app ----

type depArgs struct {
	InstanceID   string            `json:"instance_id"`
	DeploymentID string            `json:"deployment_id"`
	Env          map[string]string `json:"env"` // nil = 变量全部沿用（EnvSet = false）；{} = 普通变量恢复默认
	Action       string            `json:"action"`
}

// deployment 校验归属并取部署视图（应用名、当前版本、当前变量）。
func (h writeHandlers) deployment(ctx context.Context, c Caller, a depArgs) (appdeploy.Deployment, appdeploy.View, error) {
	dep, err := h.d.Apps.GetForUser(ctx, c.UserID, a.InstanceID, a.DeploymentID)
	if err != nil {
		return appdeploy.Deployment{}, appdeploy.View{}, err
	}
	views, err := h.d.Apps.Views(ctx, []appdeploy.Deployment{dep})
	if err != nil {
		return appdeploy.Deployment{}, appdeploy.View{}, err
	}
	if len(views) != 1 {
		return appdeploy.Deployment{}, appdeploy.View{}, fmt.Errorf("butler tools: no view for deployment %s", dep.ID)
	}
	return dep, views[0], nil
}

func appTitle(v appdeploy.View) string {
	switch {
	case v.AppName != "":
		return display(v.AppName)
	case v.AppSlug != "":
		return v.AppSlug
	}
	return v.ID
}

func (h writeHandlers) redeployCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	dep, view, err := h.deployment(ctx, c, a)
	if err != nil {
		return Card{}, err
	}
	switch dep.State { // 与 appdeploy.Redeploy 一致
	case appdeploy.StateQueued, appdeploy.StateRunning, appdeploy.StateStopped, appdeploy.StateFailed:
	default:
		return Card{}, apperr.ErrAppInvalidState
	}
	latest, err := h.d.Apps.CatalogViewBySlug(ctx, view.AppSlug)
	if err != nil {
		return Card{}, err
	}
	spec := latest.Version.Env
	if dep.State == appdeploy.StateQueued { // 排队中只更新变量，版本不变
		if a.Env == nil {
			return Card{}, apperr.New("app_invalid_state", "该部署正在排队，不改变量时无需重新部署", 409)
		}
		spec = view.Spec.Env
	}
	secretSet := secretNames(spec)
	stored, secrets, err := splitSecrets(a.Env, secretSet, "env")
	if err != nil {
		return Card{}, err
	}
	var changes string
	if a.Env != nil {
		// 预校验（与 appdeploy.mergeEnv + ValidateEnv 同口径）：已设置且本次未填的 secret 沿用，不参与必填检查
		reduced := make([]appdeploy.EnvSpec, 0, len(spec))
		for _, e := range spec {
			if _, given := a.Env[e.Name]; e.Type == "secret" && !given && view.SecretEnv[e.Name] {
				continue
			}
			reduced = append(reduced, e)
		}
		plain, secret, err := appdeploy.ValidateEnv(appdeploy.Spec{Env: reduced}, a.Env)
		if err != nil {
			return Card{}, err
		}
		changes = envChanges(spec, view, a.Env, plain, secret)
	} else {
		// 变量全部沿用到最新版本（mergeEnv envSet = false）：旧普通值只进仍是普通类型的变量、已设置的 secret 沿用；
		// 新版本新增的必填变量、旧值不再合法等可预见的失败在出卡前报出
		keep := map[string]string{}
		reduced := make([]appdeploy.EnvSpec, 0, len(spec))
		for _, e := range spec {
			if e.Type == "secret" {
				if view.SecretEnv[e.Name] {
					continue
				}
			} else if v, ok := view.Env[e.Name]; ok {
				keep[e.Name] = v
			}
			reduced = append(reduced, e)
		}
		if _, _, err := appdeploy.ValidateEnv(appdeploy.Spec{Env: reduced}, keep); err != nil {
			return Card{}, err
		}
	}
	head := fmt.Sprintf("重新部署 %s（实例 %s，", appTitle(view), dep.InstanceID)
	if dep.State == appdeploy.StateQueued {
		head += "排队中，只更新变量）"
	} else {
		head += display(view.Version) + " → 最新版本 " + display(latest.Version.Version) + "）"
	}
	card := Card{SecretEnv: secrets}
	if changes == "" {
		card.Summary = head + "，沿用现有变量"
		card.StoredSummary = card.Summary
	} else {
		card.Summary = head + "，变量变化：" + strings.ReplaceAll(changes, "\x00", "")
		card.StoredSummary = head + "，变量变化：" + maskChanges(changes)
	}
	if card.ArgsStored, err = maskedArgs(args, "env", stored); err != nil {
		return Card{}, err
	}
	return card, nil
}

// envChanges 列出变化的变量（行内 secret 新值用 \x00 包住，maskChanges 换成 ***）。
func envChanges(spec []appdeploy.EnvSpec, view appdeploy.View, req, plain, secret map[string]string) string {
	var b strings.Builder
	for _, e := range spec {
		if e.Type == "secret" {
			nv, given := req[e.Name]
			if !given {
				continue // 沿用
			}
			old := "未设置"
			if view.SecretEnv[e.Name] {
				old = "已设置"
			}
			if nv == "" { // 空串 = 清除（secret 不能有默认值，ValidateSpec 保证）；本来就没设置 → 无变化，不列
				if view.SecretEnv[e.Name] {
					b.WriteString("\n- " + envLabel(e) + "：已设置 → 清除")
				}
				continue
			}
			b.WriteString("\n- " + envLabel(e) + "：" + old + " → \x00" + displayValue(nv) + "\x00")
			continue
		}
		oldV, hadOld := view.Env[e.Name]
		newV, hasNew := plain[e.Name]
		if hadOld == hasNew && oldV == newV {
			continue
		}
		from, to := "（空）", "（空）"
		if hadOld {
			from = display(oldV)
		}
		if hasNew {
			to = displayValue(newV)
		}
		b.WriteString("\n- " + envLabel(e) + "：" + from + " → " + to)
	}
	return b.String()
}

func maskChanges(s string) string {
	parts := strings.Split(s, "\x00")
	for i := 1; i < len(parts); i += 2 {
		parts[i] = SecretMask
	}
	return strings.Join(parts, "")
}

func (h writeHandlers) redeploy(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	dep, view, err := h.deployment(ctx, c, a)
	if err != nil {
		return Result{}, err
	}
	if a.Env != nil {
		secretSet := secretNames(view.Spec.Env)
		if latest, err := h.d.Apps.CatalogViewBySlug(ctx, view.AppSlug); err == nil {
			for k := range secretNames(latest.Version.Env) {
				secretSet[k] = true
			}
		}
		if err := checkNoMask(a.Env, secretSet); err != nil {
			return Result{}, err
		}
	}
	out, err := h.d.Apps.Redeploy(ctx, appdeploy.RedeployRequest{UserID: c.UserID, InstanceID: a.InstanceID, DeploymentID: dep.ID,
		Env: a.Env, EnvSet: a.Env != nil, IP: c.IP})
	if err != nil {
		return Result{}, err
	}
	return okResult("redeploy_app", c, map[string]any{"instance_id": a.InstanceID,
		"deployment": map[string]any{"id": out.ID, "state": out.State}}, out.ID)
}

func (h writeHandlers) appStateCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	dep, view, err := h.deployment(ctx, c, a)
	if err != nil {
		return Card{}, err
	}
	verb := "停止"
	switch a.Action { // 与 appdeploy.Stop / Start 的状态要求一致
	case "stop":
		if dep.State != appdeploy.StateInstalling && dep.State != appdeploy.StateRunning {
			return Card{}, apperr.ErrAppInvalidState
		}
	case "start":
		verb = "启动"
		if dep.State != appdeploy.StateStopped {
			return Card{}, apperr.ErrAppInvalidState
		}
	default:
		return Card{}, &ValidationError{Path: "action", Reason: "应为 stop 或 start"}
	}
	s := fmt.Sprintf("%s应用 %s（实例 %s）", verb, appTitle(view), dep.InstanceID)
	return Card{Summary: s, StoredSummary: s, ArgsStored: args}, nil
}

func (h writeHandlers) appState(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	dep, err := h.d.Apps.GetForUser(ctx, c.UserID, a.InstanceID, a.DeploymentID)
	if err != nil {
		return Result{}, err
	}
	var out appdeploy.Deployment
	switch a.Action {
	case "stop":
		out, err = h.d.Apps.Stop(ctx, c.UserID, a.InstanceID, dep.ID, c.IP)
	case "start":
		out, err = h.d.Apps.Start(ctx, c.UserID, a.InstanceID, dep.ID, c.IP)
	default:
		return Result{}, &ValidationError{Path: "action", Reason: "应为 stop 或 start"}
	}
	if err != nil {
		return Result{}, err
	}
	return okResult("set_app_state", c, map[string]any{"instance_id": a.InstanceID, "action": a.Action,
		"deployment": map[string]any{"id": out.ID, "state": out.State, "desired": out.Desired}}, out.ID)
}

func (h writeHandlers) removeCard(ctx context.Context, c Caller, args json.RawMessage) (Card, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Card{}, err
	}
	dep, view, err := h.deployment(ctx, c, a)
	if err != nil {
		return Card{}, err
	}
	if !dep.Live() {
		return Card{}, apperr.ErrAppInvalidState
	}
	s := fmt.Sprintf("卸载应用 %s（实例 %s），应用文件将被删除，模型缓存保留在 /root/data/models", appTitle(view), dep.InstanceID)
	return Card{Summary: s, StoredSummary: s, ArgsStored: args}, nil
}

func (h writeHandlers) remove(ctx context.Context, c Caller, args json.RawMessage) (Result, error) {
	var a depArgs
	if err := decodeArgs(args, &a); err != nil {
		return Result{}, err
	}
	dep, err := h.d.Apps.GetForUser(ctx, c.UserID, a.InstanceID, a.DeploymentID)
	if err != nil {
		return Result{}, err
	}
	out, err := h.d.Apps.Remove(ctx, c.UserID, a.InstanceID, dep.ID, c.IP)
	if err != nil {
		return Result{}, err
	}
	return okResult("remove_app", c, map[string]any{"instance_id": a.InstanceID,
		"deployment": map[string]any{"id": out.ID, "state": out.State}}, out.ID)
}
