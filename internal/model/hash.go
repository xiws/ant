package model

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashHex 返回 SHA-256 的十六进制摘要。
func HashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// HashRef 返回仓库统一的 sha256:<hex> 引用形式。
func HashRef(data []byte) string { return "sha256:" + HashHex(data) }

// HashHexString 是 HashHex 的字符串版本。
func HashHexString(s string) string { return HashHex([]byte(s)) }

// HashRefString 是 HashRef 的字符串版本。
func HashRefString(s string) string { return HashRef([]byte(s)) }
