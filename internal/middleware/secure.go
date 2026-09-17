package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
)

/*
 * SecureEqual 是常量时间字符串比较。
 *
 * 用于密钥校验，避免"== 提前返回"泄漏前缀信息。
 *
 * 先 SHA-256 再比较：
 *   - 消除长度差异带来的时间差
 *   - 比较固定 32 字节，天然常量时间
 *
 * 性能可忽略（一次 SHA-256 约 100ns）。
 */
func SecureEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
