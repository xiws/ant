// Package web 实现本地只读 Web 服务（docs/20-web-serve.md §22）。
//
// 它只依赖 session 的只读归档与 config.View；不 import cli/agent/tool，
// 也不写任何 session、meta、events、artifact 或 workspace 文件。
package web

import (
	"encoding/json"
	"time"

	"github.com/xiws/ant/internal/model"
	"github.com/xiws/ant/internal/session"
)

// SessionSummary 是左栏列表项。
type SessionSummary struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Mode      model.Mode      `json:"mode"`
	Status    session.Status  `json:"status"`
	Live      bool            `json:"live"`
	Stale     bool            `json:"stale"`
	Model     string          `json:"model"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	TurnCount int             `json:"turnCount"`
	Tokens    int             `json:"tokens"`
	LastEvent model.EventType `json:"lastEvent,omitempty"`
}

// SessionsResponse 是 GET /api/sessions 的响应。
type SessionsResponse struct {
	Workspace string           `json:"workspace"`
	Home      string           `json:"home"`
	Sessions  []SessionSummary `json:"sessions"`
	Warnings  []string         `json:"warnings,omitempty"`
}

// SessionView 是 GET /api/sessions/{id} 的完整投影。
type SessionView struct {
	ID         string            `json:"id"`
	Meta       session.Meta      `json:"meta"`
	Lock       session.LockState `json:"lock"`
	Warnings   []string          `json:"warnings"`
	Turns      []TurnView        `json:"turns"`
	Trajectory []TrajRow         `json:"trajectory"`
	Approvals  []ApprovalView    `json:"approvals"`
	Artifacts  []ArtifactView    `json:"artifacts"`
	Changes    []ChangedFile     `json:"changes"`
	Plan       *PlanView         `json:"plan"`
	Stats      SessionStats      `json:"stats"`
	LastSeq    int64             `json:"lastSeq"`
	FirstTurn  int               `json:"firstTurn,omitempty"`
	LastTurn   int               `json:"lastTurn,omitempty"`
	HasMore    bool              `json:"hasMore,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"`
	Readonly   bool              `json:"readonly"`

	// unknown 保存不属于已知事件的轨迹行，供分页时重建轨迹。
	unknown []TrajRow
}

// SessionStats 是会话级统计。
type SessionStats struct {
	Turns      int         `json:"turns"`
	Requests   int         `json:"requests"`
	ToolCalls  int         `json:"toolCalls"`
	Usage      model.Usage `json:"usage"`
	DurationMs int64       `json:"durationMs"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	EndedAt    *time.Time  `json:"endedAt,omitempty"`
}

// TurnView 是一轮对话。
type TurnView struct {
	Turn       int           `json:"turn"`
	EventTurn  int           `json:"eventTurn,omitempty"`
	StartSeq   int64         `json:"-"`
	Status     string        `json:"status"`
	Pseudo     bool          `json:"pseudo,omitempty"`
	StartedAt  *time.Time    `json:"startedAt,omitempty"`
	EndedAt    *time.Time    `json:"endedAt,omitempty"`
	DurationMs int64         `json:"durationMs"`
	Input      string        `json:"input,omitempty"`
	Mode       model.Mode    `json:"mode,omitempty"`
	Round      int           `json:"round,omitempty"`
	Usage      *model.Usage  `json:"usage,omitempty"`
	Changed    []string      `json:"changed,omitempty"`
	Checks     []model.Check `json:"checks,omitempty"`
	PlanPath   string        `json:"planPath,omitempty"`
	Summary    string        `json:"summary,omitempty"`
	ExitCode   *int          `json:"exitCode,omitempty"`
	LastSeq    int64         `json:"lastSeq,omitempty"`
	Items      []ItemView    `json:"items"`
	Requests   []RequestView `json:"requests"`
	Nodes      []NodeView    `json:"nodes,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
}

// NodeView 是 node_started/node_completed 的投影。
type NodeView struct {
	Seq           int64              `json:"seq"`
	Node          model.NodeName     `json:"node"`
	Step          string             `json:"step,omitempty"`
	Model         string             `json:"model,omitempty"`
	Provider      string             `json:"provider,omitempty"`
	PromptVersion string             `json:"promptVersion,omitempty"`
	PromptHash    string             `json:"promptHash,omitempty"`
	InputRef      *model.ArtifactRef `json:"inputRef,omitempty"`
	StartedAt     time.Time          `json:"startedAt"`
	EndedAt       *time.Time         `json:"endedAt,omitempty"`
	DurationMs    int64              `json:"durationMs"`
	Status        string             `json:"status,omitempty"`
	Usage         *model.Usage       `json:"usage,omitempty"`
	Pseudo        bool               `json:"pseudo,omitempty"`
}

// ItemView 是对话流中的一条消息/卡片。
type ItemView struct {
	Seq        int64              `json:"seq"`
	TS         time.Time          `json:"ts"`
	Kind       string             `json:"kind"` // user|assistant|tool|approval|compaction|plan|error|notice
	Text       string             `json:"text,omitempty"`
	Ref        *model.ArtifactRef `json:"ref,omitempty"`
	RefMissing bool               `json:"refMissing,omitempty"`
	RefCut     bool               `json:"refCut,omitempty"`
	Tool       *ToolView          `json:"tool,omitempty"`
	Approval   *ApprovalView      `json:"approval,omitempty"`
	Compaction *CompactionView    `json:"compaction,omitempty"`
	Error      *ErrorView         `json:"error,omitempty"`
	Notice     string             `json:"notice,omitempty"`
}

// ToolView 是工具调用卡片。
type ToolView struct {
	ID            string      `json:"id"`
	Seq           int64       `json:"seq"`
	ResultSeq     int64       `json:"resultSeq,omitempty"`
	CallTS        time.Time   `json:"callTs"`
	EndTS         *time.Time  `json:"endTs,omitempty"`
	Name          string      `json:"name"`
	Node          string      `json:"node,omitempty"`
	Step          string      `json:"step,omitempty"`
	Approval      string      `json:"approval,omitempty"`
	Args          any         `json:"args,omitempty"`
	Status        string      `json:"status"` // pending|ok|error|stopped|denied
	OK            *bool       `json:"ok,omitempty"`
	DurationMs    int64       `json:"durationMs"`
	ExitCode      *int        `json:"exitCode,omitempty"`
	Truncated     bool        `json:"truncated,omitempty"`
	OutputRef     string      `json:"outputRef,omitempty"`
	Summary       string      `json:"summary,omitempty"`
	Error         *ErrorView  `json:"error,omitempty"`
	Result        any         `json:"result,omitempty"`
	ResultMissing bool        `json:"resultMissing,omitempty"`
	Diff          *DiffView   `json:"diff,omitempty"`
	Write         *WriteView  `json:"write,omitempty"`
	Read          *ReadView   `json:"read,omitempty"`
	Search        *SearchView `json:"search,omitempty"`
	Bash          *BashView   `json:"bash,omitempty"`
	Ask           *AskView    `json:"ask,omitempty"`
	Warnings      []string    `json:"warnings,omitempty"`
}

// ReadView 是 read 工具的参数与结果。
type ReadView struct {
	Path       string `json:"path,omitempty"`
	StartLine  int    `json:"startLine,omitempty"`
	EndLine    int    `json:"endLine,omitempty"`
	TotalLines int    `json:"totalLines,omitempty"`
	Content    string `json:"content,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Missing    bool   `json:"missing,omitempty"`
}

// SearchView 是 search 工具的参数与结果。
type SearchView struct {
	Query        string   `json:"query,omitempty"`
	Path         string   `json:"path,omitempty"`
	Regex        bool     `json:"regex,omitempty"`
	MatchCount   int      `json:"matchCount"`
	ScannedFiles int      `json:"scannedFiles,omitempty"`
	Results      []string `json:"results,omitempty"`
	Truncated    bool     `json:"truncated,omitempty"`
	Missing      bool     `json:"missing,omitempty"`
}

// BashView 是 bash 工具的参数与结果。
type BashView struct {
	Command   string `json:"command,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMs int    `json:"timeoutMs,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ExitCode  int    `json:"exitCode"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	OutputRef string `json:"outputRef,omitempty"`
	Missing   bool   `json:"missing,omitempty"`
}

// WriteView 是 write 工具的参数与结果。
type WriteView struct {
	Path             string `json:"path,omitempty"`
	Bytes            int    `json:"bytes,omitempty"`
	Lines            int    `json:"lines,omitempty"`
	Created          bool   `json:"created,omitempty"`
	Content          string `json:"content,omitempty"`
	PreviewTruncated bool   `json:"previewTruncated,omitempty"`
	Missing          bool   `json:"missing,omitempty"`
}

// AskView 是 ask 工具的参数与结果。
type AskView struct {
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Selected string   `json:"selected,omitempty"`
}

// RequestView 是一次模型请求。
type RequestView struct {
	Seq                  int64              `json:"seq"`
	Turn                 int                `json:"turn"`
	Node                 model.NodeName     `json:"node,omitempty"`
	Step                 string             `json:"step,omitempty"`
	Model                string             `json:"model,omitempty"`
	Provider             string             `json:"provider,omitempty"`
	Attempt              int                `json:"attempt,omitempty"`
	Status               string             `json:"status"` // pending|ok|failed
	StartedAt            time.Time          `json:"startedAt"`
	EndedAt              *time.Time         `json:"endedAt,omitempty"`
	DurationMs           int64              `json:"durationMs"`
	MessageCount         int                `json:"messageCount,omitempty"`
	EstimatedInputTokens int                `json:"estimatedInputTokens,omitempty"`
	Tools                []string           `json:"tools,omitempty"`
	ToolsUsed            []string           `json:"toolsUsed,omitempty"`
	Stream               bool               `json:"stream,omitempty"`
	FinishReason         string             `json:"finishReason,omitempty"`
	ToolCallCount        int                `json:"toolCallCount"`
	TextHash             string             `json:"textHash,omitempty"`
	Usage                *model.Usage       `json:"usage,omitempty"`
	Error                *ErrorView         `json:"error,omitempty"`
	Fallbacks            []FallbackView     `json:"fallbacks,omitempty"`
	InputRef             *model.ArtifactRef `json:"inputRef,omitempty"`
	RequestData          map[string]any     `json:"request,omitempty"`
	ResponseData         map[string]any     `json:"response,omitempty"`
	Pseudo               bool               `json:"pseudo,omitempty"`
}

// FallbackView 是模型降级记录。
type FallbackView struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// ApprovalView 是审批卡。
type ApprovalView struct {
	Seq        int64               `json:"seq"`
	TS         time.Time           `json:"ts"`
	ResolvedTS *time.Time          `json:"resolvedTs,omitempty"`
	Class      model.ApprovalClass `json:"class"`
	Node       model.NodeName      `json:"node,omitempty"`
	Action     string              `json:"action,omitempty"`
	Command    string              `json:"command,omitempty"`
	Paths      []string            `json:"paths,omitempty"`
	CWD        string              `json:"cwd,omitempty"`
	Reason     string              `json:"reason,omitempty"`
	Decision   string              `json:"decision"` // pending|allow|deny
	Scope      string              `json:"scope,omitempty"`
	Source     string              `json:"source,omitempty"`
}

// CompactionView 是上下文压缩提示。
type CompactionView struct {
	BeforeTokens int                `json:"beforeTokens"`
	AfterTokens  int                `json:"afterTokens"`
	Removed      int                `json:"removed,omitempty"`
	Kept         int                `json:"kept,omitempty"`
	SummaryRef   *model.ArtifactRef `json:"summaryRef,omitempty"`
	Summary      string             `json:"summary,omitempty"`
	Missing      bool               `json:"missing,omitempty"`
}

// ErrorView 是错误卡。
type ErrorView struct {
	Class   string `json:"class,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Step    string `json:"step,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// DiffView 是解析后的 unified diff。
type DiffView struct {
	Path      string     `json:"path,omitempty"`
	Hunks     []DiffHunk `json:"hunks"`
	Added     int        `json:"added"`
	Removed   int        `json:"removed"`
	HunkCount int        `json:"hunkCount"`
	Truncated bool       `json:"truncated,omitempty"`
	Warning   string     `json:"warning,omitempty"`
	Empty     bool       `json:"empty,omitempty"`
	Raw       string     `json:"raw,omitempty"`
}

// DiffHunk 是一个 @@ hunk。
type DiffHunk struct {
	OldStart int        `json:"oldStart"`
	OldCount int        `json:"oldCount"`
	NewStart int        `json:"newStart"`
	NewCount int        `json:"newCount"`
	Header   string     `json:"header,omitempty"`
	Lines    []DiffLine `json:"lines"`
}

// DiffLine 是 diff 中的一行；Kind 为 ctx|add|del|note。
type DiffLine struct {
	Kind  string `json:"kind"`
	Text  string `json:"text"`
	OldNo int    `json:"oldNo,omitempty"`
	NewNo int    `json:"newNo,omitempty"`
}

// ChangedFile 是右栏“改动”的条目。
type ChangedFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // created|modified|deleted|unknown
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Source  string `json:"source,omitempty"` // turn_completed|edit|write
	Turn    int    `json:"turn,omitempty"`
}

// ArtifactView 是 session 产物列表项。
type ArtifactView struct {
	Name        string    `json:"name"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256,omitempty"`
	ModTime     time.Time `json:"modTime"`
	MediaType   string    `json:"mediaType,omitempty"`
	Previewable bool      `json:"previewable"`
}

// PlanView 是计划预览。
type PlanView struct {
	Found   bool   `json:"found"`
	Source  string `json:"source,omitempty"`
	Name    string `json:"name,omitempty"`
	Path    string `json:"path,omitempty"`
	Body    string `json:"body,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// TrajRow 是轨迹表的一行。
type TrajRow struct {
	ID            string          `json:"id"`
	Turn          int             `json:"turn"`
	Kind          string          `json:"kind"` // user|request|tool|node|approval|compaction|error|unknown|turn
	Seq           int64           `json:"seq"`
	Label         string          `json:"label"`
	Input         string          `json:"input,omitempty"`
	Output        string          `json:"output,omitempty"`
	Thinking      string          `json:"thinking,omitempty"`
	DurationLabel string          `json:"durationLabel,omitempty"`
	DurationMs    int64           `json:"durationMs,omitempty"`
	Model         string          `json:"model,omitempty"`
	Tools         string          `json:"tools,omitempty"`
	Status        string          `json:"status,omitempty"`
	Request       *RequestView    `json:"request,omitempty"`
	Tool          *ToolView       `json:"tool,omitempty"`
	Approval      *ApprovalView   `json:"approval,omitempty"`
	Compaction    *CompactionView `json:"compaction,omitempty"`
	Error         *ErrorView      `json:"error,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

// HealthResponse 是 GET /api/health 的响应。
type HealthResponse struct {
	OK        bool   `json:"ok"`
	Version   string `json:"version"`
	Workspace string `json:"workspace"`
	Home      string `json:"home"`
	Readonly  bool   `json:"readonly"`
}

// ConfigResponse 是 GET /api/config 的脱敏投影。
type ConfigResponse struct {
	Available    bool              `json:"available"`
	Error        string            `json:"error,omitempty"`
	Settings     *SettingsView     `json:"settings,omitempty"`
	Models       []ModelView       `json:"models,omitempty"`
	Providers    []ProviderView    `json:"providers,omitempty"`
	Origins      map[string]string `json:"origins,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
	DefaultModel string            `json:"defaultModel,omitempty"`
	DefaultMode  model.Mode        `json:"defaultMode,omitempty"`
}

// SettingsView 是 settings 的安全子集（不含任何密钥）。
type SettingsView struct {
	SchemaVersion  int               `json:"schemaVersion"`
	DefaultModel   string            `json:"defaultModel,omitempty"`
	DefaultMode    model.Mode        `json:"defaultMode,omitempty"`
	ModelPolicy    string            `json:"modelPolicy,omitempty"`
	FallbackModels []string          `json:"fallbackModels,omitempty"`
	Stream         bool              `json:"stream"`
	NodeModels     map[string]string `json:"nodeModels,omitempty"`
	LogLevel       string            `json:"logLevel,omitempty"`
	ApprovalPolicy string            `json:"approvalPolicy,omitempty"`
	ProjectAuto    bool              `json:"projectAutoInit"`
}

// ModelView 是模型元数据（不含 provider 密钥）。
type ModelView struct {
	Name                      string         `json:"name"`
	ID                        string         `json:"id"`
	Provider                  string         `json:"provider"`
	ContextWindow             int            `json:"contextWindow"`
	MaxOutputTokens           int            `json:"maxOutputTokens,omitempty"`
	SupportsTools             bool           `json:"supportsTools"`
	SupportsStreaming         bool           `json:"supportsStreaming"`
	SupportsParallelToolCalls bool           `json:"supportsParallelToolCalls"`
	Reasoning                 bool           `json:"reasoning"`
	DefaultTemperature        float64        `json:"defaultTemperature,omitempty"`
	Pricing                   *model.Pricing `json:"pricing,omitempty"`
	Notes                     string         `json:"notes,omitempty"`
}

// ProviderView 是 provider 元数据（不含 apiKey/headers/环境变量）。
type ProviderView struct {
	Name       string `json:"name"`
	API        string `json:"api"`
	BaseURL    string `json:"baseUrl,omitempty"`
	TimeoutMs  int    `json:"timeoutMs,omitempty"`
	ModelCount int    `json:"modelCount"`
}

// HomeResponse 是 GET /api/home 的响应（只有元数据）。
type HomeResponse struct {
	Path  string         `json:"path"`
	Files []HomeFileView `json:"files"`
}

// HomeFileView 是 home 白名单文件元数据。
type HomeFileView struct {
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	ModTime time.Time `json:"modTime"`
	Exists  bool      `json:"exists"`
}

// HomePromptResponse 是 GET /api/home/prompt 的响应。
type HomePromptResponse struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Bytes   int64  `json:"bytes"`
}

// ProjectResponse 是 GET /api/project 的响应。
type ProjectResponse struct {
	Path        string    `json:"path"`
	Exists      bool      `json:"exists"`
	Bytes       int64     `json:"bytes"`
	ModTime     time.Time `json:"modTime,omitempty"`
	Body        string    `json:"body,omitempty"`
	Hash        string    `json:"hash,omitempty"`
	GeneratedAt string    `json:"generatedAt,omitempty"`
}

// WorkspaceFileResponse 是 GET /api/workspace/file 的响应。
type WorkspaceFileResponse struct {
	Path      string    `json:"path"`
	Bytes     int64     `json:"bytes"`
	ModTime   time.Time `json:"modTime"`
	Content   string    `json:"content"`
	Truncated bool      `json:"truncated,omitempty"`
	Binary    bool      `json:"binary,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
}

// EventsResponse 是 GET /api/sessions/{id}/events 的响应。
type EventsResponse struct {
	ID      string        `json:"id"`
	LastSeq int64         `json:"lastSeq"`
	Events  []model.Event `json:"events"`
}

// APIError 是统一错误响应。
type APIError struct {
	Error APIErrorBody `json:"error"`
}

// APIErrorBody 是错误体。
type APIErrorBody struct {
	Class   string         `json:"class"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}
