package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiws/ant/internal/model"
)

const maxConfigFileBytes = 1 << 20 // 1 MB（§4.3）

type fileLoader struct{}

// NewLoader 返回生产 Loader：home/.ant + workspace/.ant 逐键深合并。
func NewLoader() Loader { return fileLoader{} }

// Load 实现 Loader。
func (fileLoader) Load(ctx context.Context, opts Options) (*Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env := envMap(opts.Env)
	workDir, err := resolveWorkDir(opts.WorkDir)
	if err != nil {
		return nil, err
	}
	homeDir, err := resolveHomeDir(opts.HomeDir, env)
	if err != nil {
		return nil, err
	}

	c := &Config{
		origins: map[string]Origin{},
		workDir: workDir,
		homeDir: homeDir,
	}

	// 1. 内置默认值（最低层）。
	rawSettings := defaultSettingsMap()
	markLeaves(rawSettings, OriginBuiltin, "", c.origins)
	defaultsSnapshot := deepCopyMap(rawSettings)
	defaultsOrigins := copyOrigins(c.origins)

	// 2. home → workspace 逐键合并；数组整体替换。
	// null 表示「删除该键并回落到更低层」，因此每层备份合并前快照作为 fallback。
	homePath := filepath.Join(homeDir, "settings.json")
	homeMap, err := readSettingsFile(homePath)
	if err != nil {
		return nil, err
	}
	if homeMap != nil {
		c.warnSettingsSchema(homePath, homeMap)
		mergeWithFallback(rawSettings, homeMap, defaultsSnapshot, defaultsOrigins, OriginHome, "", c.origins)
	}
	afterHome := deepCopyMap(rawSettings)
	afterHomeOrigins := copyOrigins(c.origins)

	workspacePath := filepath.Join(workDir, ".ant", "settings.json")
	workspaceMap, err := readSettingsFile(workspacePath)
	if err != nil {
		return nil, err
	}
	if workspaceMap != nil {
		c.warnSettingsSchema(workspacePath, workspaceMap)
		mergeWithFallback(rawSettings, workspaceMap, afterHome, afterHomeOrigins, OriginWorkspace, "", c.origins)
	}

	// 3. 解码 settings。
	if err := decodeInto(rawSettings, &c.settings); err != nil {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "settings_type",
			Message: fmt.Sprintf("settings.json 类型错误：%v", err),
			Hint:    "检查 .ant/settings.json 与 $ANT_HOME/settings.json 的字段类型",
		}
	}
	c.warnUnknownSettings(rawSettings)
	c.settings.SchemaVersion = 1
	c.settings.CLINodeModels = map[string]string{}

	// 4. models.json：providers 对象深合并，models 数组整体替换。
	rawModels := map[string]any{"schemaVersion": float64(1), "providers": map[string]any{}}
	modelsFiles := []struct {
		path   string
		origin Origin
	}{
		{filepath.Join(homeDir, "models.json"), OriginHome},
		{filepath.Join(workDir, ".ant", "models.json"), OriginWorkspace},
	}
	for _, f := range modelsFiles {
		m, err := readJSONObject(f.path)
		if err != nil {
			return nil, err
		}
		if m == nil {
			continue
		}
		if _, ok := m["schemaVersion"]; !ok {
			c.addWarning("%s 缺少 schemaVersion，按 1 处理", f.path)
		} else if !isOne(m["schemaVersion"]) {
			c.addWarning("%s 的 schemaVersion 不是 1，按 1 读取", f.path)
		}
		mergeMaps(rawModels, m, f.origin, "", c.origins)
	}
	providers, models, err := decodeModels(rawModels)
	if err != nil {
		return nil, err
	}
	c.providers, c.models = providers, models
	c.warnUnknownModels(rawModels)

	// 5. 环境变量展开与 apiKeyEnv 解析。
	c.expandProviders(env)

	// 6. CLI 覆盖（最高优先级，运行期不可变）。
	c.applyCLI(opts.CLI)

	// 7. 兼容旧键 defaultProvider（D9）。
	if c.settings.DefaultModel == "" && c.settings.DefaultProvider != "" {
		c.settings.DefaultModel = c.settings.DefaultProvider
		c.origins["defaultModel"] = c.origins["defaultProvider"]
		c.addWarning("defaultProvider 已按 defaultModel 处理；建议改用 defaultModel")
	}

	// 8. 语义校验。
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func resolveWorkDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", &model.Error{Class: model.ErrConfigInvalid, Code: "workdir", Message: err.Error()}
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", &model.Error{Class: model.ErrConfigInvalid, Code: "workdir", Message: err.Error()}
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

func resolveHomeDir(home string, env map[string]string) (string, error) {
	if home == "" {
		home = env["ANT_HOME"]
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", &model.Error{Class: model.ErrConfigInvalid, Code: "home", Message: err.Error()}
		}
		home = filepath.Join(userHome, ".ant")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", &model.Error{Class: model.ErrConfigInvalid, Code: "home", Message: err.Error()}
	}
	return abs, nil
}

func envMap(env []string) map[string]string {
	if env == nil {
		env = os.Environ()
	}
	out := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// readSettingsFile 读取 settings.json；log 允许字符串简写（§4.5）。
func readSettingsFile(path string) (map[string]any, error) {
	m, err := readJSONObject(path)
	if err != nil || m == nil {
		return m, err
	}
	if v, ok := m["log"]; ok {
		switch tv := v.(type) {
		case string:
			m["log"] = map[string]any{"level": tv}
		case nil:
			delete(m, "log")
		}
	}
	return m, nil
}

func readJSONObject(path string) (map[string]any, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 文件不存在不是错误（§4.3）
		}
		return nil, &model.Error{Class: model.ErrConfigInvalid, Code: "read", Message: fmt.Sprintf("%s: %v", path, err)}
	}
	if info.Size() > maxConfigFileBytes {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "too_large",
			Message: fmt.Sprintf("%s 超过 1 MB", path),
			Hint:    "配置只应包含设置与模型定义",
			Details: map[string]any{"path": path, "bytes": info.Size()},
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &model.Error{Class: model.ErrConfigInvalid, Code: "read", Message: fmt.Sprintf("%s: %v", path, err)}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, &model.Error{
			Class:   model.ErrConfigInvalid,
			Code:    "json",
			Message: fmt.Sprintf("%s 不是合法 JSON 对象：%v", path, err),
			Hint:    "使用严格 JSON（无注释、无尾随逗号）",
			Details: map[string]any{"path": path},
		}
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *Config) warnSettingsSchema(path string, m map[string]any) {
	if _, ok := m["schemaVersion"]; !ok {
		c.addWarning("%s 缺少 schemaVersion，按 1 处理", path)
	} else if !isOne(m["schemaVersion"]) {
		c.addWarning("%s 的 schemaVersion 不是 1，按 1 读取", path)
	}
}

// mergeWithFallback 在 null 处恢复 fallback 层（更低优先级）的值；
// 若兜底层也没有该键，才真正删除。
func mergeWithFallback(dst, src, fallback map[string]any, fallbackOrigins map[string]Origin, origin Origin, prefix string, origins map[string]Origin) {
	for k, v := range src {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if v == nil {
			if fv, ok := fallback[k]; ok {
				dst[k] = deepCopyValue(fv)
				if o, ok := fallbackOrigins[path]; ok {
					origins[path] = o
				} else {
					markLeaves(map[string]any{k: fv}, origin, prefix, origins)
				}
			} else {
				delete(dst, k)
				delete(origins, path)
			}
			continue
		}
		if srcMap, ok := v.(map[string]any); ok {
			fbMap, _ := fallback[k].(map[string]any)
			dstMap, ok := dst[k].(map[string]any)
			if !ok {
				dstMap = deepCopyMap(fbMap)
				if dstMap == nil {
					dstMap = map[string]any{}
				}
				dst[k] = dstMap
			}
			mergeWithFallback(dstMap, srcMap, fbMap, fallbackOrigins, origin, path, origins)
			continue
		}
		dst[k] = v
		origins[path] = origin
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return v
	}
}

func copyOrigins(m map[string]Origin) map[string]Origin {
	out := make(map[string]Origin, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeMaps 实现逐键深合并：对象递归、标量覆盖、数组替换、null 删除键（§4.2）。
func mergeMaps(dst, src map[string]any, origin Origin, prefix string, origins map[string]Origin) {
	for k, v := range src {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if v == nil {
			delete(dst, k)
			delete(origins, path)
			continue
		}
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				mergeMaps(dstMap, srcMap, origin, path, origins)
				continue
			}
			dst[k] = srcMap
			markLeaves(srcMap, origin, path, origins)
			continue
		}
		dst[k] = v
		origins[path] = origin
	}
}

func markLeaves(m map[string]any, origin Origin, prefix string, origins map[string]Origin) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			markLeaves(child, origin, path, origins)
			continue
		}
		origins[path] = origin
	}
}

func decodeInto(src map[string]any, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func isOne(v any) bool {
	switch n := v.(type) {
	case float64:
		return n == 1
	case int:
		return n == 1
	case json.Number:
		return n.String() == "1"
	}
	return false
}

func (c *Config) warnUnknownSettings(raw map[string]any) {
	for group, known := range knownSettingsKeys {
		var node any = raw
		if group != "" {
			child, ok := raw[group].(map[string]any)
			if !ok {
				continue
			}
			node = child
		}
		m, ok := node.(map[string]any)
		if !ok {
			continue
		}
		for k := range m {
			if !known[k] {
				prefix := "settings"
				if group != "" {
					prefix = "settings." + group
				}
				c.addWarning("%s 存在未知字段 %s", prefix, k)
			}
		}
	}
}

func (c *Config) warnUnknownModels(raw map[string]any) {
	providers, _ := raw["providers"].(map[string]any)
	for key, pv := range providers {
		pm, ok := pv.(map[string]any)
		if !ok {
			continue
		}
		for k := range pm {
			if !knownProviderKeys[k] {
				c.addWarning("models.providers.%s 存在未知字段 %s", key, k)
			}
		}
		models, _ := pm["models"].([]any)
		for i, mv := range models {
			mm, ok := mv.(map[string]any)
			if !ok {
				continue
			}
			for k := range mm {
				if !knownModelKeys[k] {
					c.addWarning("models.providers.%s.models[%d] 存在未知字段 %s", key, i, k)
				}
			}
		}
	}
}

func (c *Config) expandProviders(env map[string]string) {
	for i := range c.providers {
		p := &c.providers[i]
		p.BaseURL = expandEnv(p.BaseURL, env)
		if p.Headers != nil {
			for k, v := range p.Headers {
				p.Headers[k] = expandEnv(v, env)
			}
		}
		if p.APIKeyEnv != "" {
			if v, ok := env[p.APIKeyEnv]; ok && v != "" {
				p.APIKey = NewSecret(v)
			} else {
				c.addWarning("provider %s 的 apiKeyEnv %s 未设置", p.DisplayName(), p.APIKeyEnv)
			}
		} else if p.APIKey.IsSet() {
			p.APIKey = NewSecret(expandEnv(p.APIKey.Reveal(), env))
			c.addWarning("provider %s 使用字面 apiKey；建议改为 apiKeyEnv（§4.6）", p.DisplayName())
		}
	}
	// 用展开后的 provider 重建扁平模型列表。
	c.models = make([]ModelSpec, 0, len(c.models))
	for _, p := range c.providers {
		c.models = append(c.models, p.Models...)
	}
}

func expandEnv(s string, env map[string]string) string {
	if s == "" || !strings.Contains(s, "${") {
		return s
	}
	var b strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			b.WriteString(s)
			break
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:start])
		name := s[start+2 : start+end]
		b.WriteString(env[name])
		s = s[start+end+1:]
	}
	return b.String()
}

func (c *Config) applyCLI(o Overrides) {
	if o.Model != "" {
		c.settings.CLIModel = o.Model
		c.origins["cli.model"] = OriginCLI
	}
	if len(o.NodeModels) > 0 {
		c.settings.CLINodeModels = map[string]string{}
		for k, v := range o.NodeModels {
			c.settings.CLINodeModels[k] = v
			c.origins["cli.nodeModel."+k] = OriginCLI
		}
	}
	if o.Mode != "" {
		c.settings.DefaultMode = o.Mode
		c.origins["defaultMode"] = OriginCLI
	}
	if o.LogLevel != "" {
		c.settings.Log.Level = o.LogLevel
		c.origins["log.level"] = OriginCLI
	}
	if o.NoColor {
		c.settings.UI.Color = "never"
		c.origins["ui.color"] = OriginCLI
	}
	c.settings.Yes = o.Yes
	c.settings.NonInteractive = o.NonInteractive
}
