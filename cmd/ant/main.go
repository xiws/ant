// Command ant 是本地 AI 编码代理的薄入口：解析参数、调用 internal/cli。
package main

import (
	"context"
	"os"

	"github.com/xiws/ant/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Stdio{
		In:    os.Stdin,
		Out:   os.Stdout,
		Err:   os.Stderr,
		IsTTY: isTTY(os.Stdout),
		Env:   os.Environ(),
	}))
}

// isTTY 用字符设备判断是否为终端；避免为一次检测引入外部依赖。
func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
