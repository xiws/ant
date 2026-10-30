package readcode

import (
	"context"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/model"
)

type stubIndex struct{}

// NewIndex 构造会话内索引。抽象层阶段明确返回 unsupported_language，
// 保留报错语义（§5.6、§21.19）。
func NewIndex(cfg config.View) Index { return stubIndex{} }

func (stubIndex) Resolve(ctx context.Context, target string) (Symbol, error) {
	return Symbol{}, &model.Error{
		Class:   model.ErrToolFailed,
		Code:    "unsupported_language",
		Message: "read_code 索引尚未实现（抽象层阶段）",
		Hint:    "暂时使用 read / search",
	}
}

func (stubIndex) Closure(ctx context.Context, root Symbol, opt Options) ([]Snippet, error) {
	return nil, &model.Error{
		Class:   model.ErrToolFailed,
		Code:    "unsupported_language",
		Message: "read_code 依赖闭包尚未实现（抽象层阶段）",
	}
}

func (stubIndex) Invalidate(paths []string) {}
