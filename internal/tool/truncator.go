package tool

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

const artifactMediaType = "text/plain; charset=utf-8"

// truncatorImpl 按 limits.maxToolOutputBytes 做 head+tail 截断，
// 完整内容写入 artifact（§10.3）。未超限时不写 artifact。
type truncatorImpl struct {
	limit int
	store model.ArtifactStore
	seq   atomic.Uint64
}

// NewTruncator 构造截断器；store 为 nil 时只截断不落盘。
func NewTruncator(cfg config.View, store model.ArtifactStore) Truncator {
	return &truncatorImpl{limit: maxOutputBytesOf(cfg), store: store}
}

// Clip 实现 Truncator。
func (t *truncatorImpl) Clip(ctx context.Context, callID string, body []byte) ([]byte, model.ArtifactRef, bool, error) {
	if t.limit <= 0 || len(body) <= t.limit {
		return body, model.ArtifactRef{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, model.ArtifactRef{}, false, err
	}
	name := artifactName(t.seq.Add(1), callID)
	var ref model.ArtifactRef
	if t.store != nil {
		written, err := t.store.PutArtifact(ctx, name, body, artifactMediaType)
		if err != nil {
			return nil, model.ArtifactRef{}, false, fmt.Errorf("写 artifact %s 失败: %w", name, err)
		}
		ref = written
		if ref.Name == "" {
			ref.Name = name
		}
	}
	marker, budget := truncationMarker(t.limit, len(body), ref.Name)
	head := budget / 2
	tail := budget - head
	if head > len(body) {
		head = len(body)
	}
	if tail > len(body)-head {
		tail = len(body) - head
	}
	// 不要把 UTF-8 字符劈开。
	for head > 0 && !utf8.RuneStart(body[head]) {
		head--
	}
	start := len(body) - tail
	for start < len(body) && !utf8.RuneStart(body[start]) {
		start++
	}

	var b strings.Builder
	b.Grow(head + len(marker) + 2 + tail)
	b.Write(body[:head])
	b.WriteString("\n")
	b.WriteString(marker)
	b.WriteString("\n")
	b.Write(body[start:])
	return []byte(b.String()), ref, true, nil
}

// truncationMarker 生成中间标注，并返回允许保留的正文预算。
func truncationMarker(limit, total int, refName string) (string, int) {
	budget := limit
	var marker string
	for i := 0; i < 4; i++ {
		omitted := total - budget
		if omitted < 0 {
			omitted = 0
		}
		if refName != "" {
			marker = fmt.Sprintf("... [已截断 %d 字节，完整内容见 %s] ...", omitted, refName)
		} else {
			marker = fmt.Sprintf("... [已截断 %d 字节] ...", omitted)
		}
		if budget+len(marker)+2 <= limit {
			break
		}
		budget = limit - len(marker) - 2
		if budget < 0 {
			budget = 0
		}
	}
	return marker, budget
}

// artifactName 生成稳定的 artifact 名：artifacts/tool-<序号>-<callID>.out。
func artifactName(seq uint64, callID string) string {
	return fmt.Sprintf("artifacts/tool-%04d-%s.out", seq, sanitizeArtifactID(callID))
}

func sanitizeArtifactID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "output"
	}
	return b.String()
}
