package governance

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// token 明文格式：ccs_<43 字符 base64url>，共 47 字符。
//
//	tokenPrefixLen 决定明文中有多少位可以安全地明文留存——
//	它用于列表展示与日志脱敏，短到无法被穷举反推，长到足以让人认出是哪一个。
//	32 字节熵取前 8 个 base64 字符（48 bit）作为展示位，剩余 220 bit 仍是秘密。
const (
	tokenScheme    = "ccs_"
	tokenEntropy   = 32 // 字节
	tokenPrefixLen = len(tokenScheme) + 8
)

// NewTokenSecret 生成一个新的 token 明文及其派生值。
//
// 明文只在此刻存在一次：库里只存 SHA-256，调用方必须立即把明文交给用户，
// 之后无任何途径可以取回（docs/02-data-model.md §2.3）。
func NewTokenSecret() (plaintext, prefix, hash string, err error) {
	buf := make([]byte, tokenEntropy)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败是系统级故障，绝不降级为可预测的 token
		return "", "", "", fmt.Errorf("governance: 生成 token 随机数失败: %w", err)
	}

	plaintext = tokenScheme + base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, plaintext[:tokenPrefixLen], HashToken(plaintext), nil
}

// HashToken 计算 token 明文的存储形态。
//
// 不加盐是有意的：token 本身就是 32 字节的高熵随机串，不存在字典攻击面，
// 而加盐会让「按 hash 查 token」变成全表扫描——每次 MCP 调用都要走这条路径。
// 这与密码存储（低熵、必须 argon2id 加盐）是完全不同的场景。
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParseToken 校验明文的形状并返回其 hash。
//
// 形状不对的直接拒绝，不查库：这能挡掉绝大多数扫描器流量，
// 也避免把随机字符串喂给数据库查询。
func ParseToken(plaintext string) (hash string, err error) {
	plaintext = strings.TrimSpace(plaintext)
	if !strings.HasPrefix(plaintext, tokenScheme) {
		return "", ErrTokenMalformed
	}
	body := plaintext[len(tokenScheme):]
	if len(body) != base64.RawURLEncoding.EncodedLen(tokenEntropy) {
		return "", ErrTokenMalformed
	}
	if _, err := base64.RawURLEncoding.DecodeString(body); err != nil {
		return "", ErrTokenMalformed
	}
	return HashToken(plaintext), nil
}
