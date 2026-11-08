# 项目上下文

<!-- ant:generated:start -->
## 项目用途

ant 需求与设计规格

本项目是 Go 项目，模块 / 包名为 `github.com/xiws/ant`，结构细节见下文各小节。

以下内容由 ant 扫描仓库结构确定性生成（未调用模型）；重新生成只替换 `ant:generated` 标记块。

## 技术栈

- 项目类型：Go
- 模块 / 包名：`github.com/xiws/ant`
- 主要依赖：`github.com/charmbracelet/glamour`、`golang.org/x/term`、`github.com/alecthomas/chroma/v2`、`github.com/aymanbagabas/go-osc52/v2`、`github.com/aymerick/douceur`、`github.com/charmbracelet/colorprofile`、`github.com/charmbracelet/lipgloss`、`github.com/charmbracelet/x/ansi`、`github.com/charmbracelet/x/cellbuf`、`github.com/charmbracelet/x/exp/slice` 等（另有 10 个未列出）
- 语言 / 运行时版本：未解析（以仓库内清单与 CI 配置为准）

## 目录结构

```text
.github/
.github/workflows/
.github/workflows/build.yml
.gitignore
README.md
cmd/
docs/
docs/00-overview.md
docs/01-cli.md
docs/02-config.md
docs/03-models.md
docs/04-prompt.md
docs/05-nodes.md
docs/06-state-machine.md
docs/07-session.md
docs/08-tools.md
docs/09-context.md
docs/10-security.md
docs/11-fault-tolerance.md
docs/12-project-context.md
docs/13-observability.md
docs/14-engineering.md
docs/15-testing.md
docs/16-traceability.md
docs/17-adr.md
docs/18-appendix.md
docs/19-interfaces.md
go.mod
go.sum
internal/
internal/agent/
internal/agent/agent.go
internal/agent/agent_test.go
internal/agent/basenode.go
internal/agent/contracts.go
internal/agent/loop.go
internal/agent/registry.go
internal/approval/
internal/approval/approval.go
internal/approval/approval_test.go
internal/approval/approver.go
internal/cli/
internal/cli/app.go
internal/cli/app_test.go
internal/cli/args.go
internal/cli/args_test.go
internal/cli/block.go
internal/cli/cli.go
internal/cli/export.go
internal/cli/export_test.go
internal/cli/markdown.go
internal/cli/markdown_test.go
internal/cli/prompter.go
internal/cli/render.go
internal/cli/render_integration_test.go
internal/cli/render_test.go
internal/cli/resume_test.go
internal/cli/subcommands.go
internal/cli/trace_render.go
internal/cli/trace_render_test.go
internal/cli/verbose_test.go
internal/config/
internal/config/config.go
internal/config/defaults.go
internal/config/loader.go
internal/config/loader_test.go
internal/config/model_spec.go
internal/config/validate.go
internal/context/
internal/context/context.go
internal/context/context_test.go
internal/context/cost.go
internal/context/estimator.go
internal/context/preparer.go
internal/context/summarizer.go
internal/logx/
internal/logx/aggregator.go
internal/logx/logx.go
internal/logx/logx_test.go
internal/model/
internal/model/artifact.go
internal/model/context.go
internal/model/enums.go
internal/model/error.go
internal/model/event.go
internal/model/hash.go
internal/model/json.go
internal/model/message.go
internal/model/model_test.go
internal/model/pricing.go
internal/model/sink.go
internal/model/token.go
internal/model/tool.go
internal/model/usage.go
internal/model/verdict.go
internal/projectctx/
internal/projectctx/builder.go
internal/projectctx/generator.go
internal/projectctx/git.go
internal/projectctx/hasher.go
internal/projectctx/model_generator.go
internal/projectctx/model_generator_test.go
internal/projectctx/projectctx.go
internal/projectctx/projectctx_test.go
internal/projectctx/scanner.go
internal/prompt/
internal/prompt/assembler.go
internal/prompt/assembler_test.go
internal/prompt/prompt.go
internal/prompt/source.go
internal/provider/
internal/provider/client.go
internal/provider/factory.go
internal/provider/fallback.go
internal/provider/openai.go
internal/provider/openai_test.go
internal/provider/provider.go
internal/provider/provider_test.go
internal/provider/registry.go
internal/provider/retry.go
...（已省略 53 项）
```

## 入口

未检测到明显入口（可检查 cmd/、main.go、package.json scripts）。

## 构建 / 测试 / Lint

- 构建：`make build`
- 测试：`go test ./...`
- Lint：`go vet ./...`

## 约定与风格

- 本文件是确定性骨架：只汇总结构与清单信息，不做风格推断。
- 用户可在 `<!-- ant:generated:start/end -->` 标记块之外追加项目约定，重新生成时原样保留。
- 构建、测试、Lint 命令来自项目类型默认值、Makefile 或 package.json scripts。

## 关键文件

- `README.md`
- `go.mod`
- `makefile`
- `.github/workflows/build.yml`

## 已知坑

- 本小节来自确定性骨架，未做语义推断；实际坑位需人工或模型摘要补充。
- 构建 / 测试 / Lint 命令可能与仓库实际约定不同，请以 CI 配置为准。
- 目录树只展示前 120 项，完整结构请在仓库中查看。

## 验收规则

- 构建通过：`make build`
- 测试通过：`go test ./...`
- Lint 通过：`go vet ./...`
- 项目特有的验收标准可写在 `ant:generated` 标记块之外，verifier 优先使用。

<!-- ant:generated:end -->
