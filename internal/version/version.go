// Package version 提供构建期注入的版本信息。
package version

import "fmt"

// 由 -ldflags "-X .../version.Version=<tag>" 注入。
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

// String 返回 `version commit buildDate`。
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, BuildDate)
}
