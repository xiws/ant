// Package config 负责配置装载、逐键深合并、校验与只读视图（docs/19-interfaces.md §21.4）。
//
// 只有 Loader 读盘；其余包只依赖 View。View 不提供 setter，运行时不可变。
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xiws/ant/internal/model"
)

// Origin 标注一个配置键来自哪一层（`ant config` 使用）。
type Origin string

const (
	OriginBuiltin   Origin = "builtin"
	OriginHome      Origin = "home"
	OriginWorkspace Origin = "workspace"
	OriginCLI       Origin = "cli"
	OriginDefault   Origin = "default"
)

// Overrides 是 CLI 参数对配置的覆盖（§3.3）。
type Overrides struct {
	Model          string
	Mode           model.Mode
	Yes            bool
	NonInteractive bool
	LogLevel       string
	NoColor        bool
	// NodeModels 是 --node-model 的解析结果：node → alias。
	NodeModels map[string]string
}

// Options 是 Loader 的输入。
type Options struct {
	WorkDir string    // workspace 绝对路径；空则取当前目录
	HomeDir string    // $ANT_HOME；空则取环境变量或 ~/.ant
	CLI     Overrides // -p/-m/-y/--log-level/--node-model（§3.3）
	Env     []string  // 仅用于 ANT_HOME/NO_COLOR/密钥展开
}

// Loader 读 home/workspace、逐键合并、校验。
type Loader interface {
	Load(ctx context.Context, opts Options) (*Config, error)
}

// View 是合并、校验、打码后的只读面；除 Loader/cli 外，所有包只应依赖 View。
type View interface {
	Settings() Settings
	Models() []ModelSpec
	Providers() []ProviderSpec
	Origin(key string) (Origin, bool) // 供 `ant config` 标注来源（§3.2）
	Warnings() []string
}

// Paths 暴露装载时使用的目录，供 prompt/projectctx/session 等读取文件。
// 它不是 View 的必选部分：测试可以用只实现 View 的 fake。
type Paths interface {
	WorkDir() string
	HomeDir() string
}

// Config 是 View 的生产实现。
type Config struct {
	settings  Settings
	models    []ModelSpec
	providers []ProviderSpec
	origins   map[string]Origin
	warnings  []string
	workDir   string
	homeDir   string
}

// Settings 是 settings.json 的合并结果；运行期覆盖字段用 json:"-" 排除。
type Settings struct {
	SchemaVersion   int               `json:"schemaVersion"`
	DefaultModel    string            `json:"defaultModel,omitempty"`
	DefaultProvider string            `json:"defaultProvider,omitempty"`
	NodeModels      map[string]string `json:"nodeModels,omitempty"`
	ModelPolicy     string            `json:"modelPolicy,omitempty"`
	FallbackModels  []string          `json:"fallbackModels,omitempty"`
	DefaultMode     model.Mode        `json:"defaultMode,omitempty"`
	Stream          bool              `json:"stream"`
	Log             LogSettings       `json:"log"`
	UI              UISettings        `json:"ui"`
	Approval        ApprovalSettings  `json:"approval"`
	Limits          Limits            `json:"limits"`
	Context         ContextSettings   `json:"context"`
	Retry           RetrySettings     `json:"retry"`
	Sandbox         SandboxSettings   `json:"sandbox"`
	Project         ProjectSettings   `json:"project"`

	// 运行期字段：由 Loader 从 CLI 参数写入，不持久化。
	CLIModel       string            `json:"-"`
	CLINodeModels  map[string]string `json:"-"`
	Yes            bool              `json:"-"`
	NonInteractive bool              `json:"-"`
}

type LogSettings struct {
	Level  string `json:"level"`
	Format string `json:"format"`
	File   string `json:"file,omitempty"`
}

type UISettings struct {
	Color string `json:"color"`
}

type ApprovalSettings struct {
	Policy string   `json:"policy"`
	Allow  []string `json:"allow"`
	Deny   []string `json:"deny"`
}

type Limits struct {
	MaxToolTurns          int            `json:"maxToolTurns"`
	MaxRounds             map[string]int `json:"maxRounds"`
	ToolTimeoutMs         int            `json:"toolTimeoutMs"`
	BashTimeoutMs         int            `json:"bashTimeoutMs"`
	BashMaxTimeoutMs      int            `json:"bashMaxTimeoutMs"`
	MaxToolOutputBytes    int            `json:"maxToolOutputBytes"`
	MaxFileReadBytes      int            `json:"maxFileReadBytes"`
	MaxFileReadLines      int            `json:"maxFileReadLines"`
	MaxWriteBytes         int            `json:"maxWriteBytes"`
	MaxIdenticalToolCalls int            `json:"maxIdenticalToolCalls"`
	NodeTimeoutMs         int            `json:"nodeTimeoutMs"`
	MaxSessionTokens      int            `json:"maxSessionTokens"`
}

type ContextSettings struct {
	AutoCompact       bool    `json:"autoCompact"`
	CompactAt         float64 `json:"compactAt"`
	KeepRecentTurns   int     `json:"keepRecentTurns"`
	SummaryMaxTokens  int     `json:"summaryMaxTokens"`
	SummaryModel      string  `json:"summaryModel,omitempty"`
	SafetyMarginRatio float64 `json:"safetyMarginRatio"`
}

type RetrySettings struct {
	Attempts    int     `json:"attempts"`
	BaseDelayMs int     `json:"baseDelayMs"`
	MaxDelayMs  int     `json:"maxDelayMs"`
	JitterRatio float64 `json:"jitterRatio"`
	RetryOn     []any   `json:"retryOn"`
}

type SandboxSettings struct {
	WorkspaceOnly   bool     `json:"workspaceOnly"`
	AllowReadPaths  []string `json:"allowReadPaths"`
	AllowWritePaths []string `json:"allowWritePaths"`
	ProtectedPaths  []string `json:"protectedPaths"`
	Network         string   `json:"network"`
	Shell           string   `json:"shell"`
	HardDenyExtra   []string `json:"hardDenyExtra"`
}

type ProjectSettings struct {
	AutoInit       bool     `json:"autoInit"`
	ProjectModel   string   `json:"projectModel,omitempty"`
	MaxFiles       int      `json:"maxFiles"`
	MaxBytes       int      `json:"maxBytes"`
	MaxOutputBytes int      `json:"maxOutputBytes"`
	Depth          int      `json:"depth"`
	Exclude        []string `json:"exclude"`
}

// ProviderSpec 是 models.json 中的一个 provider。
type ProviderSpec struct {
	Key            string            `json:"-"`
	Name           string            `json:"name,omitempty"`
	API            string            `json:"api"`
	BaseURL        string            `json:"baseUrl,omitempty"`
	APIKey         Secret            `json:"apiKey,omitempty"`
	APIKeyEnv      string            `json:"apiKeyEnv,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	TimeoutMS      int               `json:"timeoutMs,omitempty"`
	MaxConcurrency int               `json:"maxConcurrency,omitempty"`
	Retry          *RetrySettings    `json:"retry,omitempty"`
	Models         []ModelSpec       `json:"models,omitempty"`
}

// DisplayName 返回展示名，缺省用 provider key。
func (p ProviderSpec) DisplayName() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Key
}

// Timeout 返回单次 HTTP 请求超时。
func (p ProviderSpec) Timeout() time.Duration {
	if p.TimeoutMS <= 0 {
		return 120 * time.Second
	}
	return time.Duration(p.TimeoutMS) * time.Millisecond
}

// ModelSpec 是 models[].name 对应的模型别名。
type ModelSpec struct {
	ProviderKey               string         `json:"-"`
	ID                        string         `json:"id"`
	Name                      string         `json:"name"`
	ContextWindow             int            `json:"contextWindow"`
	MaxOutputTokens           int            `json:"maxOutputTokens,omitempty"`
	SupportsTools             bool           `json:"supportsTools,omitempty"`
	SupportsStreaming         bool           `json:"supportsStreaming"`
	SupportsParallelToolCalls bool           `json:"supportsParallelToolCalls"`
	Reasoning                 bool           `json:"reasoning,omitempty"`
	DefaultTemperature        float64        `json:"defaultTemperature,omitempty"`
	Pricing                   *model.Pricing `json:"pricing,omitempty"`
	Notes                     string         `json:"notes,omitempty"`
}

// Capabilities 把配置字段投影为 model.Capabilities（不在 model 里读配置）。
func (m ModelSpec) Capabilities() model.Capabilities {
	return model.Capabilities{
		ContextWindow:             m.ContextWindow,
		MaxOutputTokens:           m.MaxOutputTokens,
		SupportsTools:             m.SupportsTools,
		SupportsStreaming:         m.SupportsStreaming,
		SupportsParallelToolCalls: m.SupportsParallelToolCalls,
		Reasoning:                 m.Reasoning,
	}
}

// Secret 保证密钥不会被 %v / 日志 / JSON 意外打印（§4.6）。
type Secret struct{ v string }

// NewSecret 由明文构造 Secret。
func NewSecret(s string) Secret { return Secret{v: s} }

// String 实现 fmt.Stringer；永远不返回明文。
func (s Secret) String() string { return "<redacted>" }

// GoString 覆盖 %#v。
func (s Secret) GoString() string { return "<redacted>" }

// Reveal 返回明文；仅 provider.Factory 调用。
func (s Secret) Reveal() string { return s.v }

// IsSet 报告是否配置了密钥。
func (s Secret) IsSet() bool { return s.v != "" }

// MarshalJSON 输出打码后的值（`ant config` 使用）。
func (s Secret) MarshalJSON() ([]byte, error) {
	if s.v == "" {
		return []byte(`""`), nil
	}
	return []byte(`"<redacted>"`), nil
}

// UnmarshalJSON 读取明文；空字符串表示未配置。
func (s *Secret) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.v = raw
	return nil
}

// Settings 返回只读副本（含 map/slice 拷贝）。
func (c *Config) Settings() Settings { return cloneSettings(c.settings) }

// Models 返回扁平化后的模型列表（每个都带 ProviderKey）。
func (c *Config) Models() []ModelSpec {
	out := make([]ModelSpec, len(c.models))
	copy(out, c.models)
	return out
}

// Providers 返回 provider 列表副本。
func (c *Config) Providers() []ProviderSpec {
	out := make([]ProviderSpec, len(c.providers))
	copy(out, c.providers)
	return out
}

// Origin 返回某个键的来源。
func (c *Config) Origin(key string) (Origin, bool) {
	if c == nil {
		return "", false
	}
	o, ok := c.origins[key]
	return o, ok
}

// Origins 返回全部来源，供 `ant config` 输出。
func (c *Config) Origins() map[string]Origin {
	out := make(map[string]Origin, len(c.origins))
	for k, v := range c.origins {
		out[k] = v
	}
	return out
}

// Warnings 返回装载期 warning。
func (c *Config) Warnings() []string {
	return append([]string(nil), c.warnings...)
}

// WorkDir 返回解析后的 workspace 绝对路径。
func (c *Config) WorkDir() string { return c.workDir }

// HomeDir 返回 ANT_HOME 绝对路径。
func (c *Config) HomeDir() string { return c.homeDir }

// Validate 重新执行语义校验。
func (c *Config) Validate() error { return c.validate() }

func (c *Config) addWarning(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func cloneSettings(s Settings) Settings {
	out := s
	if s.NodeModels != nil {
		out.NodeModels = make(map[string]string, len(s.NodeModels))
		for k, v := range s.NodeModels {
			out.NodeModels[k] = v
		}
	}
	if s.CLINodeModels != nil {
		out.CLINodeModels = make(map[string]string, len(s.CLINodeModels))
		for k, v := range s.CLINodeModels {
			out.CLINodeModels[k] = v
		}
	}
	if s.Limits.MaxRounds != nil {
		out.Limits.MaxRounds = make(map[string]int, len(s.Limits.MaxRounds))
		for k, v := range s.Limits.MaxRounds {
			out.Limits.MaxRounds[k] = v
		}
	}
	out.FallbackModels = append([]string(nil), s.FallbackModels...)
	out.Sandbox.AllowReadPaths = append([]string(nil), s.Sandbox.AllowReadPaths...)
	out.Sandbox.AllowWritePaths = append([]string(nil), s.Sandbox.AllowWritePaths...)
	out.Sandbox.ProtectedPaths = append([]string(nil), s.Sandbox.ProtectedPaths...)
	out.Sandbox.HardDenyExtra = append([]string(nil), s.Sandbox.HardDenyExtra...)
	out.Project.Exclude = append([]string(nil), s.Project.Exclude...)
	out.Approval.Allow = append([]string(nil), s.Approval.Allow...)
	out.Approval.Deny = append([]string(nil), s.Approval.Deny...)
	out.Retry.RetryOn = append([]any(nil), s.Retry.RetryOn...)
	return out
}
