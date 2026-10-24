package comfywf

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"

	"spark-cloud/internal/platform/apperr"
)

// BuildPrompt 按快速出图表单生成要提交给 ComfyUI /prompt 的 prompt（规格 §6.1）：深拷贝 apiPrompt（原字节不变），
// 逐个 input 取值（缺值用 Default；seed 无默认 / -1 / 空 → 随机 [0, 2^53)；其它无默认 → 必填）、按类型校验后写入
// prompt[node].inputs[input]。values 里有表单之外的 key → 错误。返回实际使用的种子（第一个 seed 类型输入；没有则 nil）。
// rnd 为 nil 时用 math/rand/v2；错误为 apperr.WorkflowInputInvalid(label, reason)。
func BuildPrompt(apiPrompt json.RawMessage, inputs []InputSpec, values map[string]any, rnd func() int64) (json.RawMessage, *int64, error) {
	var prompt map[string]map[string]any
	if err := decodeNumber(apiPrompt, &prompt); err != nil || prompt == nil {
		return nil, nil, fmt.Errorf("comfywf: bad api_prompt: %v", err)
	}
	if rnd == nil {
		rnd = func() int64 { return rand.Int64N(maxSeed) }
	}
	known := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		known[in.Key] = true
	}
	for k := range values {
		if !known[k] {
			return nil, nil, apperr.WorkflowInputInvalid(k, "未知参数")
		}
	}
	var seed *int64
	for _, in := range inputs {
		node, ok := prompt[in.Node]
		if !ok {
			return nil, nil, fmt.Errorf("comfywf: input %s: node %s not in api_prompt", in.Key, in.Node)
		}
		nodeInputs, ok := node["inputs"].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("comfywf: input %s: node %s has no inputs", in.Key, in.Node)
		}
		if _, ok := nodeInputs[in.Input]; !ok {
			return nil, nil, fmt.Errorf("comfywf: input %s: node %s has no input %s", in.Key, in.Node, in.Input)
		}
		v, given := values[in.Key]
		if !given || v == nil {
			v = in.Default
		}
		var val any
		if in.Type == InputSeed {
			s, reason := seedValue(v, rnd)
			if reason != "" {
				return nil, nil, apperr.WorkflowInputInvalid(in.Label, reason)
			}
			if seed == nil {
				seed = &s
			}
			val = s
		} else {
			if v == nil {
				return nil, nil, apperr.WorkflowInputInvalid(in.Label, "必填")
			}
			var reason string
			if val, reason = coerceValue(in, v); reason != "" {
				return nil, nil, apperr.WorkflowInputInvalid(in.Label, reason)
			}
		}
		nodeInputs[in.Input] = val
	}
	out, err := json.Marshal(prompt)
	if err != nil {
		return nil, nil, err
	}
	return out, seed, nil
}

// seedValue D11：nil / "" / -1 → 随机；否则须是 0～2^53-1 的整数。
func seedValue(v any, rnd func() int64) (int64, string) {
	if v == nil || v == "" {
		return randomSeed(rnd), ""
	}
	n, ok := toInt(v)
	if !ok || n < -1 || n >= maxSeed {
		return 0, "种子应为 -1（随机）或 0～9007199254740991 的整数"
	}
	if n == -1 {
		return randomSeed(rnd), ""
	}
	return n, ""
}

func randomSeed(rnd func() int64) int64 { return ((rnd() % maxSeed) + maxSeed) % maxSeed }
