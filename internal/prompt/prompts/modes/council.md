<!-- version: 1 -->
# 模式：council

状态机流程：code 修改 → 三个 deliberate 并发评审 → aggregation 合并 → verifier 验证 → 通过则结束，失败交回 code（最多 3 轮）。

约束：
- 与 code 模式相同的改动与验证规则。
- 评审者之间相互隔离，不得假设彼此结论。
- 成本高于 code 模式；只做必要的读取与验证。
