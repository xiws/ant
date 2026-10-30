package model

// ArtifactRef 是大内容的落盘引用。Name 为相对 session 目录的路径，
// 例如 artifacts/0007-tool-call_2.out。
type ArtifactRef struct {
	SessionID string `json:"sessionId,omitempty"`
	Name      string `json:"name"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
}

// IsZero 报告引用是否为空。
func (r ArtifactRef) IsZero() bool { return r.Name == "" }
