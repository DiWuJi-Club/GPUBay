package llm

import "unicode/utf8"

// EstimateTokens 粗估一段文本的 token 数（中文约 2 字 / token，另加每条消息的固定开销 8）。
// 只用于历史裁剪与预算预估，真实用量以 Usage 为准。
func EstimateTokens(s string) int {
	return utf8.RuneCountInString(s)/2 + 8
}
