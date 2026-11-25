package tool

// 本文件是 read_code 的人工查看入口：默认跳过，不影响 CI。
// 它直接调用工具执行体，把模型会收到的内容打印到终端。
//
// 用法（在仓库根目录执行）：
//
//	READCODE_VIEW='internal/readcode/index.go' \
//	  go test ./internal/tool -run TestReadCodeView -v
//
// 支持目标形态与参数（用 ? 追加）：
//
//	READCODE_VIEW='internal/readcode/index.go#Read'
//	READCODE_VIEW='internal/readcode/index.go?depth=3&maxSymbols=5'
//	READCODE_VIEW='cmd/ant'
//	READCODE_VIEW='Resolve?depth=1'
//
// 默认以仓库根（本包上两级）为 workspace，可用 READCODE_WORKDIR 覆盖：
//
//	READCODE_WORKDIR=/path/to/repo READCODE_VIEW='pkg/x.go' go test ./internal/tool -run TestReadCodeView -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xiws/ant/internal/config"
	"github.com/xiws/ant/internal/readcode"
)

func TestReadCodeView(t *testing.T) {
	spec := strings.TrimSpace(os.Getenv("READCODE_VIEW"))
	if spec == "" {
		t.Skip("设置 READCODE_VIEW=<target>[?depth=N&maxSymbols=M] 后运行，用法见文件头")
	}
	work := strings.TrimSpace(os.Getenv("READCODE_WORKDIR"))
	if work == "" {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		work = filepath.Dir(filepath.Dir(wd)) // internal/tool → 仓库根
	}
	work, err := filepath.Abs(work)
	if err != nil {
		t.Fatal(err)
	}

	args := map[string]any{}
	target := spec
	if i := strings.IndexByte(spec, '?'); i >= 0 {
		target = spec[:i]
		query, err := url.ParseQuery(spec[i+1:])
		if err != nil {
			t.Fatalf("参数解析失败: %v", err)
		}
		for _, key := range []string{"depth", "maxSymbols"} {
			if v := query.Get(key); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					t.Fatalf("%s 必须是整数: %v", key, err)
				}
				args[key] = n
			}
		}
	}
	args["target"] = target

	view := &fakeView{
		settings: config.Settings{Limits: config.Limits{MaxToolOutputBytes: 16384}},
		workDir:  work,
	}
	exec := readCodeExec(view, Deps{Index: readcode.NewIndex(view)})
	res, err := exec(context.Background(), Invocation{Args: args})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	fmt.Printf("\n========== read_code %s（workspace=%s） ==========\n", spec, work)
	if !res.OK {
		body, _ := json.MarshalIndent(res.Err, "", "  ")
		fmt.Printf("失败：%s\n", body)
		return
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data type %T", res.Data)
	}
	content, _ := data["content"].(string)
	fmt.Printf("---------- mode=%v count=%v truncated=%v ----------\n",
		data["mode"], data["count"], data["truncated"])
	if strings.TrimSpace(content) == "" {
		fmt.Println("（空）")
	} else {
		fmt.Println(content)
	}
	delete(data, "content")
	if meta, err := json.MarshalIndent(data, "", "  "); err == nil {
		fmt.Printf("---------- 元数据 ----------\n%s\n", meta)
	}
}
