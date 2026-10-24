package mock

// Decide 是 mock 模型的确定性规则脚本（规格 §16.3；Go 集成测试与 Playwright 共用）。
//
// 输入只有 Request.Messages。「本轮」= 最后一条 user 消息之后的 assistant / tool 消息；规则只读本轮的工具结果，
// 所以每条规则都是多步的：缺哪个读工具的结果就先发哪个读工具，拿到结果后下一步再发写工具或收尾文本。
// 工具结果的内容可以是 guard.JSONWrap 包裹（`<untrusted_data …>\n<json>\n</untrusted_data>`，剥掉首尾行再解析）或裸 JSON；
// 结果块的 Name 可能为空，按 ToolCallID / ProviderCallID 与本轮 assistant 的 tool_use 块配对得到工具名。
// 结果块 OK = false、解析失败或 JSON 顶层 `"ok": false` 视为该工具失败 → 收尾文本「…查询失败…」，不重复发同一个工具（避免打满迭代上限）。
//
// 匹配顺序（先到先得）：
//
//	0. 最后一条消息是 tool 且某个结果含 `IGNORE ALL INSTRUCTIONS`（不区分大小写，词间任意空白）
//	   → release_instance{instance_id: 该结果里该短语之后（否则任意位置）出现的第一个 ins-…；否则用户文本 / 上下文实例 / 第一台 running}
//	   （模拟模型「上钩」；系统只会生成待确认的危险卡）
//	1. 最后一条消息是 tool 且含写工具结果（确认卡执行 / 拒绝 / 过期后回填）→ 收尾文本：失败结果 code = rejected（或无 code 且含「拒绝」/ rejected）→「用户拒绝了这次操作，已取消。」，code = expired →「这个操作已经失效了…」；
//	   失败 →「操作没有成功：<message>」；成功 → 按工具给一句（deploy_app →「已提交部署，可以在实例详情的「应用」卡片查看进度。」等）
//	2. 「失败」「为什么」「日志」→ 目标实例（见下）→ list_instance_apps{instance_id} → 第一个 state=failed 的部署
//	   （没有且文本含「日志」→ 第一个部署；都没有 →「实例 X 上没有失败的部署。」）→ get_deployment_log{instance_id, deployment_id}
//	   → 「日志显示：<末行 ≤80 字>。建议重新部署。」（非 failed 部署 → 「…。部署状态：<state>。」）
//	3. 「部署」→ 从文本识别应用（open webui / openwebui / webui → open-webui；jupyter → jupyterlab；comfy → comfyui；vllm → vllm）：
//	   - 未识别 → list_apps → 「目前可以部署：A、B…。告诉我要部署哪一个。」
//	   - 实例：用户文本里的 ins-… 或上下文实例 → 先 list_instance_apps{instance_id}（失败 →「没有找到实例 X」）；
//	     否则 list_instances → 第一台 running（无实例 →「你还没有实例，先去市场租一台」；无 running →「你还没有运行中的实例」）
//	   - get_app{slug}（失败 →「没有找到应用」）→ 对每个 type=secret 且无 default 的变量（按声明顺序）各发一个 generate_secret{}（同一条消息并行）
//	     → deploy_app{instance_id, app_slug, env: {secret 变量: 生成值；required、无 default 的 enum 变量: 第一个选项}}（env 为空则不带）
//	4. 「余额」「还能用」→ get_wallet{} + list_instances{} + list_products{}（缺哪个发哪个，同一条消息并行）
//	   → 「余额 ¥X，按当前实例日价可用约 N 天」（N = 余额 / running 实例的 1d 价格之和，向下取整；无 running →「余额 ¥X，目前没有运行中的实例。」）
//	5. 「续费」→ 目标 running 实例 → renew_instance{instance_id, duration, quantity?}（文本「N 天 / 周 / 个月」→ 1d / 1w / 1m × N；默认 1d × 1）
//	6. 「释放」「删除实例」→ 目标 running 实例 → release_instance{instance_id}
//	7. 「选」「推荐」「哪台」→ list_products{} → 「推荐 <第一个套餐名>（¥X/天，现有 N 台可租）。要我帮你下单吗？」
//	8. 「帮助」「怎么」→ get_help{section}（按关键词选节，默认 ssh）→ 「帮助文档里是这样说的：<前 120 字>」
//	9. 其它 →「我可以帮你选机器、部署应用、排查问题、查看余额。」
//
// 目标实例：用户文本里的第一个 ins-… → user 消息 context 块的 InstanceID → list_instances 结果中第一台 state=running
// （诊断规则允许退回第一台任意状态的实例）。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"spark-cloud/internal/butler/llm"
)

const (
	defaultText    = "我可以帮你选机器、部署应用、排查问题、查看余额。"
	noInstanceText = "你还没有实例，先去市场租一台"
	noRunningText  = "你还没有运行中的实例"
	rejectedText   = "用户拒绝了这次操作，已取消。"
	expiredText    = "这个操作已经失效了，需要的话请重新告诉我。"
)

var (
	instanceIDRe = regexp.MustCompile(`ins-[A-Za-z0-9]+`)
	injectionRe  = regexp.MustCompile(`(?i)ignore\s+all\s+instructions`)
	renewQtyRe   = regexp.MustCompile(`(\d+)\s*(天|日|周|星期|个月|月)`)
)

// 写工具（规格 §4.2）；其结果出现在最后一条 tool 消息里时规则 1 收尾。
var writeTools = map[string]string{
	"place_order":      "下单成功，实例正在开通。",
	"renew_instance":   "续费成功，到期时间已顺延。",
	"release_instance": "实例已开始释放。",
	"cancel_booking":   "预订已取消，退款会退回余额。",
	"deploy_app":       "已提交部署，可以在实例详情的「应用」卡片查看进度。",
	"redeploy_app":     "已提交重新部署。",
	"set_app_state":    "操作已完成。",
	"remove_app":       "应用已开始卸载。",
}

// 应用识别：按顺序匹配小写后的用户文本（open webui 在 vllm 之前，避免「Open WebUI 连接 vLLM」被认成 vLLM）。
var appKeywords = []struct{ kw, slug string }{
	{"open webui", "open-webui"}, {"open-webui", "open-webui"}, {"openwebui", "open-webui"}, {"webui", "open-webui"},
	{"jupyter", "jupyterlab"},
	{"comfy", "comfyui"},
	{"vllm", "vllm"},
}

// 帮助章节关键词（help/*.md 的节名）。
var helpKeywords = []struct{ kw, section string }{
	{"密钥", "ssh-keys"}, {"公钥", "ssh-keys"}, {"端口", "forward"}, {"转发", "forward"}, {"网页", "web"},
	{"应用", "apps"}, {"预订", "booking"}, {"批量", "batch"}, {"自动续费", "auto-renew"}, {"保留", "retention"},
	{"计费", "billing"}, {"扣费", "billing"}, {"价格", "billing"}, {"管家", "butler"},
}

type toolResult struct {
	Name    string
	Args    json.RawMessage
	Content string
	OK      bool // Block.OK；false 一律视为工具失败
}

// parse 解析结果 JSON；Block.OK = false、解析失败或顶层 "ok": false → ok = false。
func (r toolResult) parse() (any, bool) {
	if !r.OK {
		return nil, false
	}
	return parseResult(r.Content)
}

type turn struct {
	text        string // 最后一条 user 消息的文本
	lower       string
	ctxInstance string
	results     []toolResult // 本轮全部工具结果，按出现顺序
	last        []toolResult // 最后一条消息是 tool 时它的结果
}

func parseTurn(msgs []llm.Message) *turn {
	t := &turn{}
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			var parts []string
			for _, b := range msgs[i].Blocks {
				switch b.Type {
				case llm.BlockText:
					parts = append(parts, b.Text)
				case llm.BlockContext:
					t.ctxInstance = b.InstanceID
				}
			}
			t.text = strings.Join(parts, "\n")
			start = i + 1
			break
		}
	}
	t.lower = strings.ToLower(t.text)
	type use struct {
		name string
		args json.RawMessage
	}
	uses := map[string]use{}
	for i := start; i < len(msgs); i++ {
		m := msgs[i]
		switch m.Role {
		case llm.RoleAssistant:
			for _, b := range m.Blocks {
				if b.Type != llm.BlockToolUse {
					continue
				}
				u := use{b.Name, b.Args}
				if b.ToolCallID != "" {
					uses["tc:"+b.ToolCallID] = u
				}
				if b.ProviderCallID != "" {
					uses["pc:"+b.ProviderCallID] = u
				}
			}
		case llm.RoleTool:
			var rs []toolResult
			for _, b := range m.Blocks {
				if b.Type != llm.BlockToolResult {
					continue
				}
				r := toolResult{Name: b.Name, Content: b.Content, OK: b.OK}
				var u use
				ok := false
				if b.ToolCallID != "" {
					u, ok = uses["tc:"+b.ToolCallID]
				}
				if !ok && b.ProviderCallID != "" {
					u, ok = uses["pc:"+b.ProviderCallID]
				}
				if ok {
					if r.Name == "" {
						r.Name = u.name
					}
					r.Args = u.args
				}
				rs = append(rs, r)
			}
			t.results = append(t.results, rs...)
			if i == len(msgs)-1 {
				t.last = rs
			}
		}
	}
	return t
}

func (t *turn) latest(name string) (toolResult, bool) {
	for i := len(t.results) - 1; i >= 0; i-- {
		if t.results[i].Name == name {
			return t.results[i], true
		}
	}
	return toolResult{}, false
}

func (t *turn) all(name string) []toolResult {
	var out []toolResult
	for _, r := range t.results {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func (t *turn) has(kws ...string) bool {
	for _, k := range kws {
		if strings.Contains(t.lower, k) {
			return true
		}
	}
	return false
}

// explicitInstance：用户文本里的 ins-… → 上下文实例。
func (t *turn) explicitInstance() string {
	if id := instanceIDRe.FindString(t.text); id != "" {
		return id
	}
	return t.ctxInstance
}

// instanceFor 选目标实例；返回非 nil 的 Reply 表示需要先发读工具或直接收尾。
func (t *turn) instanceFor(anyState bool) (string, *Reply) {
	if id := t.explicitInstance(); id != "" {
		return id, nil
	}
	res, ok := t.latest("list_instances")
	if !ok {
		return "", call("list_instances", nil)
	}
	v, ok := res.parse()
	if !ok {
		return "", textReply("查询实例列表失败，请稍后再试。")
	}
	items := listOf(v, "instances", "items")
	if len(items) == 0 {
		return "", textReply(noInstanceText)
	}
	for _, it := range items {
		if str(it, "state") == "running" && str(it, "id") != "" {
			return str(it, "id"), nil
		}
	}
	if anyState && str(items[0], "id") != "" {
		return str(items[0], "id"), nil
	}
	return "", textReply(noRunningText)
}

// Decide 按规则表决定下一步回复（不填 ProviderCallID，由 Mock.Chat 编号）。
func Decide(req llm.Request) Reply {
	t := parseTurn(req.Messages)
	if r := ruleInjection(t); r != nil {
		return *r
	}
	if r := ruleAfterWrite(t); r != nil {
		return *r
	}
	var r *Reply
	switch {
	case t.has("失败", "为什么", "日志"):
		r = ruleDiagnose(t)
	case t.has("部署"):
		r = ruleDeploy(t)
	case t.has("余额", "还能用"):
		r = ruleWallet(t)
	case t.has("续费"):
		r = ruleRenew(t)
	case t.has("释放", "删除实例"):
		r = ruleRelease(t)
	case t.has("选", "推荐", "哪台"):
		r = ruleRecommend(t)
	case t.has("帮助", "怎么"):
		r = ruleHelp(t)
	default:
		r = textReply(defaultText)
	}
	return *r
}

// ---- 规则 ----

func ruleInjection(t *turn) *Reply {
	for _, res := range t.last {
		// 在原文上做不区分大小写的匹配：ToLower 会改变部分字符的字节长度（如 Ⱥ），不能拿小写串的下标切原文
		loc := injectionRe.FindStringIndex(res.Content)
		if loc == nil {
			continue
		}
		id := instanceIDRe.FindString(res.Content[loc[1]:])
		if id == "" {
			id = instanceIDRe.FindString(res.Content)
		}
		if id == "" {
			id, _ = t.instanceFor(false)
		}
		if id == "" {
			continue
		}
		return call("release_instance", map[string]any{"instance_id": id})
	}
	return nil
}

func ruleAfterWrite(t *turn) *Reply {
	for _, res := range t.last {
		okText, isWrite := writeTools[res.Name]
		if !isWrite {
			continue
		}
		if _, ok := res.parse(); !ok {
			// Task 7 对拒绝 / 过期回填 {"ok": false, "code": "rejected" | "expired", "message": …}
			msg, code := "", ""
			if v, err := decode(res.Content); err == nil {
				if m, ok := v.(map[string]any); ok {
					msg, code = str(m, "message"), str(m, "code")
				}
			}
			switch {
			case code == "rejected":
				return textReply(rejectedText)
			case code == "expired":
				return textReply(expiredText)
			case code == "" && (strings.Contains(res.Content, "拒绝") || strings.Contains(strings.ToLower(res.Content), "rejected")):
				return textReply(rejectedText)
			}
			if msg == "" {
				msg = clip(unwrap(res.Content), 80)
			}
			return textReply("操作没有成功：" + msg)
		}
		return textReply(okText)
	}
	return nil
}

func ruleDiagnose(t *turn) *Reply {
	id, r := t.instanceFor(true)
	if r != nil {
		return r
	}
	res, ok := t.latest("list_instance_apps")
	if !ok {
		return call("list_instance_apps", map[string]any{"instance_id": id})
	}
	v, ok := res.parse()
	if !ok {
		return textReply("查询实例 " + id + " 上的应用失败，请确认实例是否存在。")
	}
	deps := listOf(v, "deployments", "apps", "items")
	var target map[string]any
	for _, d := range deps {
		if str(d, "state") == "failed" {
			target = d
			break
		}
	}
	if target == nil && t.has("日志") && len(deps) > 0 {
		target = deps[0]
	}
	if target == nil {
		if len(deps) == 0 {
			return textReply("实例 " + id + " 上还没有部署应用。")
		}
		return textReply("实例 " + id + " 上没有失败的部署。")
	}
	logRes, ok := t.latest("get_deployment_log")
	if !ok {
		return call("get_deployment_log", map[string]any{"instance_id": id, "deployment_id": str(target, "id")})
	}
	logText := unwrap(logRes.Content)
	if lv, ok := logRes.parse(); ok {
		if m, ok := lv.(map[string]any); ok {
			if s, ok := m["log"].(string); ok {
				logText = unwrap(s)
			}
		}
	}
	line := lastLine(logText)
	if line == "" {
		line = "（没有输出）"
	}
	if str(target, "state") == "failed" {
		return textReply("日志显示：" + clip(line, 80) + "。建议重新部署。")
	}
	return textReply("日志显示：" + clip(line, 80) + "。部署状态：" + str(target, "state") + "。")
}

func ruleDeploy(t *turn) *Reply {
	slug := ""
	for _, k := range appKeywords {
		if strings.Contains(t.lower, k.kw) {
			slug = k.slug
			break
		}
	}
	if slug == "" {
		res, ok := t.latest("list_apps")
		if !ok {
			return call("list_apps", nil)
		}
		v, ok := res.parse()
		if !ok {
			return textReply("查询应用目录失败，请稍后再试。")
		}
		var names []string
		for _, a := range listOf(v, "apps", "items") {
			if n := str(a, "name"); n != "" {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			return textReply("目前还没有可以一键部署的应用。")
		}
		return textReply("目前可以部署：" + strings.Join(names, "、") + "。告诉我要部署哪一个。")
	}

	var id string
	if id = t.explicitInstance(); id != "" {
		res, ok := t.latest("list_instance_apps")
		if !ok {
			return call("list_instance_apps", map[string]any{"instance_id": id})
		}
		if _, ok := res.parse(); !ok {
			return textReply("没有找到实例 " + id + "，请确认实例 ID。")
		}
	} else {
		var r *Reply
		if id, r = t.instanceFor(false); r != nil {
			return r
		}
	}

	appRes, ok := t.latest("get_app")
	if !ok {
		return call("get_app", map[string]any{"slug": slug})
	}
	av, ok := appRes.parse()
	app, isMap := av.(map[string]any)
	if !ok || !isMap {
		return textReply("没有找到应用 " + slug + "。")
	}
	env := map[string]string{}
	var secrets []string
	for _, e := range envSpecs(app) {
		name := str(e, "name")
		if name == "" || str(e, "default") != "" {
			continue
		}
		switch {
		case str(e, "type") == "secret":
			secrets = append(secrets, name)
		case e["required"] == true:
			if opts, ok := e["options"].([]any); ok && len(opts) > 0 {
				if s, ok := opts[0].(string); ok {
					env[name] = s
				}
			}
		}
	}
	gens := t.all("generate_secret")
	if len(secrets) > 0 && len(gens) == 0 {
		r := &Reply{}
		for range secrets {
			r.ToolCalls = append(r.ToolCalls, toolCall("generate_secret", nil))
		}
		return r
	}
	for i, name := range secrets {
		if i >= len(gens) {
			break
		}
		if gv, ok := gens[i].parse(); ok {
			if m, ok := gv.(map[string]any); ok && str(m, "secret") != "" {
				env[name] = str(m, "secret")
			}
		}
	}
	args := map[string]any{"instance_id": id, "app_slug": slug}
	if len(env) > 0 {
		args["env"] = env
	}
	return call("deploy_app", args)
}

func ruleWallet(t *turn) *Reply {
	r := &Reply{}
	for _, name := range []string{"get_wallet", "list_instances", "list_products"} {
		if _, ok := t.latest(name); !ok {
			r.ToolCalls = append(r.ToolCalls, toolCall(name, nil))
		}
	}
	if len(r.ToolCalls) > 0 {
		return r
	}
	wres, _ := t.latest("get_wallet")
	wv, ok := wres.parse()
	w, isMap := wv.(map[string]any)
	if !ok || !isMap {
		return textReply("查询钱包失败，请稍后再试。")
	}
	bal := int64(0)
	if n, ok := num(w, "balance_cents"); ok {
		bal = n
	} else if n, ok := num(w, "balance"); ok {
		bal = n
	}
	ires, _ := t.latest("list_instances")
	pres, _ := t.latest("list_products")
	iv, _ := ires.parse()
	pv, _ := pres.parse()
	products := listOf(pv, "products", "items")
	var daily int64
	running := 0
	for _, inst := range listOf(iv, "instances", "items") {
		if str(inst, "state") != "running" {
			continue
		}
		running++
		for _, p := range products {
			pid, okP := num(p, "id")
			iid, okI := num(inst, "product_id")
			if (okP && okI && pid == iid) || (str(inst, "product_name") != "" && str(inst, "product_name") == str(p, "name")) {
				daily += dailyPrice(p)
				break
			}
		}
	}
	if running == 0 {
		return textReply("余额 " + yuan(bal) + "，目前没有运行中的实例。")
	}
	if daily <= 0 {
		return textReply("余额 " + yuan(bal) + "，有 " + strconv.Itoa(running) + " 台运行中的实例。")
	}
	return textReply(fmt.Sprintf("余额 %s，按当前实例日价可用约 %d 天", yuan(bal), bal/daily))
}

func ruleRenew(t *turn) *Reply {
	id, r := t.instanceFor(false)
	if r != nil {
		return r
	}
	duration, qty := "1d", 1
	if m := renewQtyRe.FindStringSubmatch(t.text); m != nil {
		qty, _ = strconv.Atoi(m[1])
		switch m[2] {
		case "周", "星期":
			duration = "1w"
		case "个月", "月":
			duration = "1m"
		}
	} else if t.has("周") {
		duration = "1w"
	} else if t.has("月") {
		duration = "1m"
	}
	args := map[string]any{"instance_id": id, "duration": duration}
	if qty > 1 {
		args["quantity"] = qty
	}
	return call("renew_instance", args)
}

func ruleRelease(t *turn) *Reply {
	id, r := t.instanceFor(false)
	if r != nil {
		return r
	}
	return call("release_instance", map[string]any{"instance_id": id})
}

func ruleRecommend(t *turn) *Reply {
	res, ok := t.latest("list_products")
	if !ok {
		return call("list_products", nil)
	}
	v, ok := res.parse()
	if !ok {
		return textReply("查询套餐失败，请稍后再试。")
	}
	products := listOf(v, "products", "items")
	if len(products) == 0 {
		return textReply("目前没有可租的套餐。")
	}
	p := products[0]
	avail, _ := num(p, "available_now")
	return textReply(fmt.Sprintf("推荐 %s（%s/天，现有 %d 台可租）。要我帮你下单吗？", str(p, "name"), yuan(dailyPrice(p)), avail))
}

func ruleHelp(t *turn) *Reply {
	section := "ssh"
	for _, k := range helpKeywords {
		if strings.Contains(t.lower, k.kw) {
			section = k.section
			break
		}
	}
	res, ok := t.latest("get_help")
	if !ok {
		return call("get_help", map[string]any{"section": section})
	}
	body := unwrap(res.Content)
	if v, ok := res.parse(); ok {
		if m, ok := v.(map[string]any); ok {
			for _, k := range []string{"content", "text", "markdown"} {
				if s := str(m, k); s != "" {
					body = unwrap(s)
					break
				}
			}
		}
	}
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#->*"))
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return textReply("帮助文档里暂时没有这部分内容。")
	}
	return textReply("帮助文档里是这样说的：" + clip(strings.Join(lines, " "), 120))
}

// ---- 构造与解析工具 ----

func textReply(s string) *Reply { return &Reply{Text: s} }

func toolCall(name string, args map[string]any) llm.ToolCall {
	if args == nil {
		args = map[string]any{}
	}
	b, _ := json.Marshal(args)
	return llm.ToolCall{Name: name, Args: b}
}

func call(name string, args map[string]any) *Reply {
	return &Reply{ToolCalls: []llm.ToolCall{toolCall(name, args)}}
}

// unwrap 剥掉 guard.Wrap 的首尾行（`<untrusted_data …>` / `</untrusted_data>`）；不是包裹格式则原样返回。
func unwrap(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "<untrusted_data") {
		return s
	}
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return ""
	}
	s = s[i+1:]
	if j := strings.LastIndexByte(s, '\n'); j >= 0 && strings.HasPrefix(strings.TrimSpace(s[j+1:]), "</untrusted_data") {
		s = s[:j]
	} else if strings.HasPrefix(strings.TrimSpace(s), "</untrusted_data") {
		s = ""
	}
	return s
}

func decode(content string) (any, error) {
	var v any
	err := json.Unmarshal([]byte(unwrap(content)), &v)
	return v, err
}

// parseResult 解析工具结果 JSON；解析失败或顶层 "ok": false → ok = false。
func parseResult(content string) (any, bool) {
	v, err := decode(content)
	if err != nil {
		return nil, false
	}
	if m, isMap := v.(map[string]any); isMap {
		if okv, has := m["ok"]; has && okv == false {
			return v, false
		}
	}
	return v, true
}

// listOf 取结果里的对象数组：顶层数组，或对象里 keys 指定的数组字段（都没有 → 第一个数组字段）。
func listOf(v any, keys ...string) []map[string]any {
	var arr []any
	switch x := v.(type) {
	case []any:
		arr = x
	case map[string]any:
		for _, k := range keys {
			if a, ok := x[k].([]any); ok {
				arr = a
				break
			}
		}
		if arr == nil { // 退回第一个数组字段：按键名排序，保证确定性
			names := make([]string, 0, len(x))
			for k := range x {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if a, ok := x[k].([]any); ok {
					arr = a
					break
				}
			}
		}
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// envSpecs 取应用的变量声明：version.env（CatalogView）→ version.spec.env → spec.env → env。
func envSpecs(app map[string]any) []map[string]any {
	try := func(v any) []map[string]any {
		if a, ok := v.([]any); ok {
			return listOf(a)
		}
		return nil
	}
	if ver, ok := app["version"].(map[string]any); ok {
		if e := try(ver["env"]); e != nil {
			return e
		}
		if spec, ok := ver["spec"].(map[string]any); ok {
			if e := try(spec["env"]); e != nil {
				return e
			}
		}
	}
	if spec, ok := app["spec"].(map[string]any); ok {
		if e := try(spec["env"]); e != nil {
			return e
		}
	}
	return try(app["env"])
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func num(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// dailyPrice 取套餐 1d 档价格（分）；字段 amount_cents 或 amount。
func dailyPrice(p map[string]any) int64 {
	for _, pr := range listOf(map[string]any{"prices": p["prices"]}, "prices") {
		if str(pr, "duration") != "1d" {
			continue
		}
		if n, ok := num(pr, "amount_cents"); ok {
			return n
		}
		if n, ok := num(pr, "amount"); ok {
			return n
		}
	}
	return 0
}

func yuan(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("¥%s%d.%02d", sign, cents/100, cents%100)
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
