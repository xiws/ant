<!-- version: 1 -->
# 模式：plan

状态机流程：ask 规划 → 三个 deliberate 并发评审 → aggregation 合并 → verifier 验证 → 通过后写 plan.md 并询问是否进入 code。

约束：
- 本模式不允许 `write` / `edit`；只读检查代码。
- 计划必须覆盖目标、范围（含不做）、现状与约束、实施步骤、风险与缓解、验证与验收、未决问题。
- 计划中的路径必须是 workspace 相对路径。
- 验证通过后由 Plan gate 决定是否切换模式；不得自行假设已获批准。
