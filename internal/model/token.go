package model

// EstimateTextTokens 是保守 token 估算：ASCII 约 4 字符/token，CJK 约 1 字符/token（§11.2）。
func EstimateTextTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}

// EstimateMessagesTokens 估算消息历史的输入 token（含 tool calls）。
func EstimateMessagesTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTextTokens(m.Content)
		for _, tc := range m.ToolCalls {
			total += EstimateTextTokens(tc.Name) + EstimateTextTokens(string(tc.Arguments))
		}
	}
	return total
}

// EstimateToolsTokens 估算工具定义的输入 token。
func EstimateToolsTokens(tools []ToolSpec) int {
	total := 0
	for _, t := range tools {
		total += EstimateTextTokens(t.Name) + EstimateTextTokens(t.Description) + EstimateTextTokens(string(t.Schema))
	}
	return total
}
