package projectctx

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/xiws/ant/internal/model"
)

// generatorVersion 参与失效 hash（docs/12-project-context.md §14.2）：改变骨架
// 格式、小节结构或扫描语义时必须递增，让旧的 project.md 自动失效。
const generatorVersion = "projectctx.v1"

// 生成内容的大小控制：目录树与关键文件只列出前若干项，避免 project.md 无限膨胀。
const (
	maxTreeLines     = 120
	maxKeyFileLines  = 12
	maxTechStackDeps = 10
)

// generatedSections 是 project.md 必须包含的小节（顺序固定，§14.4）。
var generatedSections = []string{
	"## 项目用途",
	"## 技术栈",
	"## 目录结构",
	"## 入口",
	"## 构建 / 测试 / Lint",
	"## 约定与风格",
	"## 关键文件",
	"## 已知坑",
	"## 验收规则",
}

type deterministicGenerator struct{}

// NewDeterministicGenerator 返回确定性骨架生成器：不调用模型、不联网，只把扫描
// 结果整理为固定小节（§14.3 第 2 步）。当前 Ensure 只使用该生成器，FromModel=false。
func NewDeterministicGenerator() Generator { return deterministicGenerator{} }

// Generate 渲染骨架；usage 恒为零值（确定性骨架不消耗 token）。
func (deterministicGenerator) Generate(ctx context.Context, s Skeleton) (string, model.Usage, error) {
	if err := ctx.Err(); err != nil {
		return "", model.Usage{}, err
	}
	md := renderDeterministic(s)
	if err := validateMarkdown(md); err != nil {
		return "", model.Usage{}, err
	}
	return md, model.Usage{}, nil
}

func renderDeterministic(s Skeleton) string {
	var b strings.Builder
	b.WriteString("# 项目上下文\n\n")
	b.WriteString(generatedStartMarker + "\n")
	writeSection(&b, "项目用途", purposeText(s))
	writeSection(&b, "技术栈", techStackText(s))
	writeSection(&b, "目录结构", treeText(s.Tree))
	writeSection(&b, "入口", listText(s.Entries, "未检测到明显入口（可检查 cmd/、main.go、package.json scripts）。"))
	writeSection(&b, "构建 / 测试 / Lint", commandsText(s.Commands))
	writeSection(&b, "约定与风格", conventionsText())
	writeSection(&b, "关键文件", keyFilesText(s.KeyFiles))
	writeSection(&b, "已知坑", pitfallsText(s))
	writeSection(&b, "验收规则", acceptanceText(s.Commands))
	b.WriteString(generatedEndMarker + "\n")
	return b.String()
}

func writeSection(b *strings.Builder, title, body string) {
	b.WriteString("## ")
	b.WriteString(title)
	b.WriteString("\n\n")
	body = strings.TrimSpace(body)
	if body == "" {
		body = "（未扫描到相关信息）"
	}
	b.WriteString(body)
	b.WriteString("\n\n")
}

func purposeText(s Skeleton) string {
	parts := make([]string, 0, 3)
	if label := typeLabel(s.ProjectType); label != "" {
		parts = append(parts, "本项目是 "+label+" 项目")
	} else {
		parts = append(parts, "未能从 go.mod/package.json/pyproject.toml/Cargo.toml 识别项目类型")
	}
	if s.ModulePath != "" {
		parts = append(parts, "模块 / 包名为 `"+s.ModulePath+"`")
	}
	parts = append(parts, "结构细节见下文各小节")

	var out []string
	if title := readmeTitle(s.KeyFiles); title != "" {
		out = append(out, title)
	}
	out = append(out, strings.Join(parts, "，")+"。")
	out = append(out, "以下内容由 ant 扫描仓库结构确定性生成（未调用模型）；重新生成只替换 `ant:generated` 标记块。")
	return strings.Join(out, "\n\n")
}

// readmeTitle 取 README 的首个非空、非徽章行，作为确定性的项目用途描述。
func readmeTitle(kfs []KeyFile) string {
	for _, kf := range kfs {
		base := strings.ToLower(path.Base(kf.Path))
		if base != "readme.md" && base != "readme.markdown" && base != "readme" && base != "readme.txt" {
			continue
		}
		for _, raw := range strings.Split(kf.Body, "\n") {
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "![") || strings.HasPrefix(line, "[![") || strings.HasPrefix(line, "<!--") {
				continue
			}
			line = strings.TrimSpace(strings.TrimLeft(line, "#"))
			if line == "" {
				continue
			}
			if len(line) > 160 {
				line = line[:160] + "…"
			}
			return line
		}
	}
	return ""
}

func techStackText(s Skeleton) string {
	var b strings.Builder
	label := typeLabel(s.ProjectType)
	if label == "" {
		label = "未知（未找到常见清单文件）"
	}
	b.WriteString("- 项目类型：" + label + "\n")
	if s.ModulePath != "" {
		b.WriteString("- 模块 / 包名：`" + s.ModulePath + "`\n")
	}
	if len(s.Deps) > 0 {
		shown := s.Deps
		omitted := 0
		if len(shown) > maxTechStackDeps {
			omitted = len(shown) - maxTechStackDeps
			shown = shown[:maxTechStackDeps]
		}
		b.WriteString("- 主要依赖：" + inlineCodeList(shown))
		if omitted > 0 {
			b.WriteString(fmt.Sprintf(" 等（另有 %d 个未列出）", omitted))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("- 主要依赖：未从清单文件中解析到依赖\n")
	}
	b.WriteString("- 语言 / 运行时版本：未解析（以仓库内清单与 CI 配置为准）")
	return b.String()
}

func treeText(tree []string) string {
	lines := make([]string, 0, len(tree))
	for _, entry := range tree {
		p := entry
		// Scanner 在文件条目上用 "\t<size>" 承载大小（供 Hasher 使用），展示时去掉。
		if i := strings.IndexByte(p, '\t'); i >= 0 {
			p = p[:i]
		}
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		lines = append(lines, p)
	}
	if len(lines) == 0 {
		return "（未扫描到文件：workspace 为空或全部被排除）"
	}
	omitted := 0
	if len(lines) > maxTreeLines {
		omitted = len(lines) - maxTreeLines
		lines = lines[:maxTreeLines]
	}
	var b strings.Builder
	b.WriteString("```text\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "...（已省略 %d 项）\n", omitted)
	}
	b.WriteString("```")
	return b.String()
}

func listText(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		lines = append(lines, "- `"+item+"`")
	}
	if len(lines) == 0 {
		return fallback
	}
	return strings.Join(lines, "\n")
}

func commandsText(c Commands) string {
	var b strings.Builder
	write := func(label string, cmds []string) {
		if len(cmds) == 0 {
			b.WriteString("- " + label + "：未检测到（可在标记块之外补充）\n")
			return
		}
		b.WriteString("- " + label + "：" + inlineCodeList(cmds) + "\n")
	}
	write("构建", c.Build)
	write("测试", c.Test)
	write("Lint", c.Lint)
	return strings.TrimRight(b.String(), "\n")
}

func conventionsText() string {
	return strings.Join([]string{
		"- 本文件是确定性骨架：只汇总结构与清单信息，不做风格推断。",
		"- 用户可在 `<!-- ant:generated:start/end -->` 标记块之外追加项目约定，重新生成时原样保留。",
		"- 构建、测试、Lint 命令来自项目类型默认值、Makefile 或 package.json scripts。",
	}, "\n")
}

func keyFilesText(kfs []KeyFile) string {
	if len(kfs) == 0 {
		return "（未选中关键文件）"
	}
	shown := kfs
	omitted := 0
	if len(shown) > maxKeyFileLines {
		omitted = len(shown) - maxKeyFileLines
		shown = shown[:maxKeyFileLines]
	}
	var b strings.Builder
	for _, kf := range shown {
		b.WriteString("- `" + kf.Path + "`\n")
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "- 另有 %d 个关键文件未列出\n", omitted)
	}
	return strings.TrimRight(b.String(), "\n")
}

func pitfallsText(s Skeleton) string {
	lines := []string{
		"- 本小节来自确定性骨架，未做语义推断；实际坑位需人工或模型摘要补充。",
		"- 构建 / 测试 / Lint 命令可能与仓库实际约定不同，请以 CI 配置为准。",
	}
	if s.ProjectType == "" {
		lines = append(lines, "- 未找到常见清单文件，项目类型与依赖判断可能不准确。")
	}
	if len(s.Deps) == 0 {
		lines = append(lines, "- 未解析到依赖清单，技术栈信息可能不完整。")
	}
	if len(s.Tree) > maxTreeLines {
		lines = append(lines, fmt.Sprintf("- 目录树只展示前 %d 项，完整结构请在仓库中查看。", maxTreeLines))
	}
	return strings.Join(lines, "\n")
}

func acceptanceText(c Commands) string {
	lines := make([]string, 0, len(c.Build)+len(c.Test)+len(c.Lint)+1)
	for _, cmd := range c.Build {
		lines = append(lines, "- 构建通过：`"+cmd+"`")
	}
	for _, cmd := range c.Test {
		lines = append(lines, "- 测试通过：`"+cmd+"`")
	}
	for _, cmd := range c.Lint {
		lines = append(lines, "- Lint 通过：`"+cmd+"`")
	}
	lines = append(lines, "- 项目特有的验收标准可写在 `ant:generated` 标记块之外，verifier 优先使用。")
	return strings.Join(lines, "\n")
}

func typeLabel(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "go":
		return "Go"
	case "node", "node.js", "javascript", "typescript":
		return "Node.js"
	case "python":
		return "Python"
	case "rust":
		return "Rust"
	default:
		return strings.TrimSpace(t)
	}
}

func inlineCodeList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts = append(parts, "`"+item+"`")
	}
	return strings.Join(parts, "、")
}

// validateMarkdown 校验小节齐全、顺序正确、标记成对（§14.3 第 5 步）。
func validateMarkdown(md string) error {
	if strings.TrimSpace(md) == "" {
		return model.NewError(model.ErrInternal, "projectctx.empty_output", "生成内容为空")
	}
	start, end, ok := markerRange(md)
	if !ok {
		return model.NewError(model.ErrInternal, "projectctx.markers", "生成内容缺少 ant:generated 标记")
	}
	block := md[start:end]
	pos := 0
	for _, section := range generatedSections {
		idx := strings.Index(block, section)
		if idx < 0 {
			return model.NewError(model.ErrInternal, "projectctx.section", "生成内容缺少小节 "+section)
		}
		if idx < pos {
			return model.NewError(model.ErrInternal, "projectctx.section", "生成内容小节顺序错误："+section)
		}
		pos = idx
	}
	return nil
}
