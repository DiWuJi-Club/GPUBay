package appdeploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"spark-cloud/internal/platform/apperr"
)

var (
	slugRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$`)
	envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	// reservedNames 会影响 run.sh 自身或 sh 的行为，不允许作为用户变量。
	reservedNames = map[string]bool{"PATH": true, "LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "HOME": true, "SHELL": true, "IFS": true, "ENV": true, "BASH_ENV": true}
	envTypes      = map[string]bool{"string": true, "int": true, "bool": true, "enum": true, "secret": true}
)

func ValidSlug(s string) bool    { return slugRe.MatchString(s) }
func ValidVersion(s string) bool { return versionRe.MatchString(s) }

func invalid(path, reason string) error { return apperr.AppVersionInvalid(path, reason) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// fullMatchRe 把 pattern 编译成全匹配（规格 §3.3「pattern 全匹配」）；pattern 须先单独通过 regexp.Compile。
func fullMatchRe(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(`^(?:` + pattern + `)$`)
}

func validPath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) <= 128 && strings.IndexFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }) < 0
}

// ValidateSpec 规范化并校验版本 spec（规格 §3.2）。错误码 app_version_invalid，消息以字段路径开头。
func ValidateSpec(s *Spec) error {
	if len(s.Ports) < 1 || len(s.Ports) > 2 {
		return invalid("ports", "需要 1～2 个端口")
	}
	for i := range s.Ports {
		p := &s.Ports[i]
		path := fmt.Sprintf("ports[%d]", i)
		if p.Port < 1024 || p.Port > 65535 || p.Port == 2222 {
			return invalid(path+".port", "端口必须在 1024～65535 之间且不为 22、2222")
		}
		for j := 0; j < i; j++ {
			if s.Ports[j].Port == p.Port {
				return invalid(path+".port", "端口重复")
			}
		}
		p.Label = strings.TrimSpace(p.Label)
		if runeLen(p.Label) > 32 {
			return invalid(path+".label", "最多 32 个字符")
		}
		if p.Path != "" && !validPath(p.Path) {
			return invalid(path+".path", "必须以 / 开头、不含空白，最多 128 个字符")
		}
		if p.HealthPath != "" && !validPath(p.HealthPath) {
			return invalid(path+".health_path", "必须以 / 开头、不含空白，最多 128 个字符")
		}
		if i > 0 {
			p.HealthPath = "" // 只有主端口做 HTTP 健康检查
		}
	}
	if s.TimeoutMinutes == 0 {
		s.TimeoutMinutes = DefaultTimeoutMinutes
	}
	if s.TimeoutMinutes < 1 || s.TimeoutMinutes > MaxTimeoutMinutes {
		return invalid("timeout_minutes", "需在 1～180 之间")
	}
	if s.MinDataFreeGiB < 0 || s.MinDataFreeGiB > 2048 {
		return invalid("min_data_free_gib", "需在 0～2048 之间")
	}
	if len(s.Env) > 16 {
		return invalid("env", "最多 16 个变量")
	}
	for i := range s.Env {
		if err := validateEnvSpec(&s.Env[i], fmt.Sprintf("env[%d]", i)); err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			if s.Env[j].Name == s.Env[i].Name {
				return invalid(fmt.Sprintf("env[%d].name", i), "变量名重复")
			}
		}
	}
	if len(s.Links) > 4 {
		return invalid("links", "最多 4 个链接")
	}
	for i := range s.Links {
		l := &s.Links[i]
		l.Label = strings.TrimSpace(l.Label)
		if l.Label == "" || runeLen(l.Label) > 32 {
			return invalid(fmt.Sprintf("links[%d].label", i), "1～32 个字符")
		}
		if !validPath(l.Path) {
			return invalid(fmt.Sprintf("links[%d].path", i), "必须以 / 开头、不含空白，最多 128 个字符")
		}
	}
	return nil
}

func validateEnvSpec(e *EnvSpec, path string) error {
	e.Name = strings.TrimSpace(e.Name)
	e.Label = strings.TrimSpace(e.Label)
	if !envNameRe.MatchString(e.Name) || strings.HasPrefix(e.Name, "SPARK_") || reservedNames[e.Name] {
		return invalid(path+".name", "变量名须为大写字母开头的 [A-Z0-9_]，不得以 SPARK_ 开头，不得是 PATH / HOME / LD_* 等保留名")
	}
	if e.Label == "" {
		e.Label = e.Name
	}
	if runeLen(e.Label) > 32 {
		return invalid(path+".label", "最多 32 个字符")
	}
	if runeLen(e.Help) > 200 {
		return invalid(path+".help", "最多 200 个字符")
	}
	if !envTypes[e.Type] {
		return invalid(path+".type", "类型只能是 string / int / bool / enum / secret")
	}
	if e.MaxLen == 0 {
		e.MaxLen = 256
	}
	if e.MaxLen < 1 || e.MaxLen > 1024 {
		return invalid(path+".max_len", "需在 1～1024 之间")
	}
	switch e.Type {
	case "enum":
		if len(e.Options) < 1 || len(e.Options) > 32 {
			return invalid(path+".options", "enum 需要 1～32 个选项")
		}
		for _, o := range e.Options {
			if o == "" || runeLen(o) > 128 {
				return invalid(path+".options", "每个选项 1～128 个字符")
			}
		}
	case "int":
		if e.Min != nil && e.Max != nil && *e.Min > *e.Max {
			return invalid(path+".min", "min 不能大于 max")
		}
	case "string", "secret":
		if len(e.Pattern) > 256 {
			return invalid(path+".pattern", "最多 256 个字符")
		}
		if e.Pattern != "" {
			if _, err := regexp.Compile(e.Pattern); err != nil {
				return invalid(path+".pattern", "不是合法的 RE2 正则："+err.Error())
			}
			re, err := fullMatchRe(e.Pattern)
			if err != nil {
				return invalid(path+".pattern", "不是合法的 RE2 正则："+err.Error())
			}
			// secret 最短 8：pattern 不能放行更短的值（校验层对 secret 另有长度下限，这里提前拒绝自相矛盾的配置）
			if e.Type == "secret" && re.MatchString("abcd") {
				return invalid(path+".pattern", "secret 的正则不得允许少于 8 个字符的值")
			}
		}
	}
	if e.Default != "" {
		if reason := checkValue(*e, e.Default); reason != "" {
			return invalid(path+".default", "默认值不符合自身规则："+reason)
		}
	}
	return nil
}

// checkValue 按类型校验单个值；返回空串表示合法，否则返回原因。
func checkValue(e EnvSpec, v string) string {
	if !utf8.ValidString(v) {
		return "不是合法的 UTF-8"
	}
	if strings.ContainsAny(v, "\n\r\x00") {
		return "不能包含换行或 NUL"
	}
	if len(v) > e.MaxLen {
		return fmt.Sprintf("最多 %d 字节", e.MaxLen)
	}
	switch e.Type {
	case "int":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return "必须是整数"
		}
		if (e.Min != nil && n < *e.Min) || (e.Max != nil && n > *e.Max) {
			return "超出允许范围"
		}
	case "bool":
		if v != "true" && v != "false" {
			return "只能是 true 或 false"
		}
	case "enum":
		found := false
		for _, o := range e.Options {
			found = found || o == v
		}
		if !found {
			return "不在可选项内"
		}
	case "secret":
		if len(v) < MinSecretLen {
			return fmt.Sprintf("至少 %d 个字符", MinSecretLen)
		}
		fallthrough
	case "string":
		if e.Pattern != "" {
			if re, err := fullMatchRe(e.Pattern); err != nil || !re.MatchString(v) {
				return "格式不正确"
			}
		}
	}
	return ""
}

// ValidateEnv 校验用户提供的变量值（规格 §3.3），补默认值，按 type 拆成普通 / 敏感两张表。spec 须已通过 ValidateSpec。
func ValidateEnv(s Spec, values map[string]string) (plain, secret map[string]string, err error) {
	known := map[string]EnvSpec{}
	for _, e := range s.Env {
		known[e.Name] = e
	}
	for name := range values {
		if _, ok := known[name]; !ok {
			return nil, nil, apperr.AppEnvInvalid(name, "不是该应用声明的变量")
		}
	}
	plain, secret = map[string]string{}, map[string]string{}
	for _, e := range s.Env {
		v, set := values[e.Name]
		if !set || v == "" {
			if e.Default != "" {
				v, set = e.Default, true
			} else if e.Required {
				return nil, nil, apperr.AppEnvRequired(e.Label)
			} else {
				continue
			}
		}
		if reason := checkValue(e, v); reason != "" {
			return nil, nil, apperr.AppEnvInvalid(e.Label, reason)
		}
		if e.Type == "secret" {
			secret[e.Name] = v
		} else {
			plain[e.Name] = v
		}
	}
	return plain, secret, nil
}

// SpecHash = sha256(canonical(spec) || "\n" || script) 的十六进制。encoding/json 对 struct 按字段顺序、map 按键排序，输入键序不影响。
func SpecHash(s Spec, script string) string {
	b, _ := json.Marshal(s)
	h := sha256.New()
	h.Write(b)
	h.Write([]byte("\n"))
	h.Write([]byte(script))
	return hex.EncodeToString(h.Sum(nil))
}
