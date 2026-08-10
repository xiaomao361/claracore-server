// Package crypto 是密码哈希的实现。
//
// 独立成一个 adapter 包而不是塞进 platform/：它有外部依赖
// （golang.org/x/crypto/argon2），而 platform 是被领域层 import 的横切包——
// 领域层的依赖越少越好。
package crypto

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"claracore-server/internal/ports"
)

// Argon2idParams 是哈希参数。
//
// 默认值取 OWASP 对 argon2id 的推荐档之一（19 MiB / 2 次迭代 / 1 并行度）。
// 选这一档而不是更狠的：登录是同步请求，参数每翻一倍，一次登录就多占
// 一份内存和时间——而这套系统的威胁模型是「数据库被读走」，
// 不是「有人在线爆破一个封闭部署的管理后台」（无自助注册，账号由管理员分配）。
type Argon2idParams struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

func DefaultParams() Argon2idParams {
	return Argon2idParams{
		Memory:      19 * 1024,
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

type Argon2id struct {
	params Argon2idParams
}

var _ ports.PasswordHasher = (*Argon2id)(nil)

func NewArgon2id(p Argon2idParams) *Argon2id {
	if p.Memory == 0 {
		p = DefaultParams()
	}
	return &Argon2id{params: p}
}

// ErrMalformedHash 表示库里那串 hash 不是本实现能理解的格式。
//
// 单独一个错误而不是当成「密码不对」：前者是数据问题（比如集成测试
// 塞进去的假值、或者从别的系统迁移过来的 bcrypt），后者是用户问题。
// 混在一起会让一次数据迁移事故表现成「所有人突然密码都错了」。
var ErrMalformedHash = errors.New("crypto: 密码哈希格式无法识别")

// Hash 产出标准的 PHC 字符串：
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt-b64>$<hash-b64>
//
// **参数编码在字符串里**，这是 Verify 能验过老 hash 的前提：
// 将来调高参数后，存量用户的 hash 仍然按它自己记的参数校验。
// 不这么做的话，任何一次调参都等于把所有存量用户锁在门外。
func (a *Argon2id) Hash(plaintext string) (string, error) {
	salt := make([]byte, a.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto: 生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt,
		a.params.Iterations, a.params.Memory, a.params.Parallelism, a.params.KeyLength)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, a.params.Memory, a.params.Iterations, a.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify 用 hash 自己记录的参数重算，再做**常数时间比较**。
//
// 用 == 比较会因为提前返回而泄露前缀匹配了多少字节。这个泄露在
// 远程时序攻击里未必好用，但 subtle.ConstantTimeCompare 是免费的，
// 而「这里为什么不能用 ==」是个每次 review 都要重新解释的问题。
func (a *Argon2id) Verify(plaintext, hash string) (bool, error) {
	parts := strings.Split(hash, "$")
	// 形如 ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("%w: %q", ErrMalformedHash, truncate(hash))
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, fmt.Errorf("%w: 版本段无法解析", ErrMalformedHash)
	}
	if version != argon2.Version {
		// 换 argon2 版本是件大事，不能当作「密码错了」悄悄放过
		return false, fmt.Errorf("%w: argon2 版本 %d 与本实现的 %d 不符", ErrMalformedHash, version, argon2.Version)
	}

	var memory, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, fmt.Errorf("%w: 参数段无法解析", ErrMalformedHash)
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("%w: 盐不是合法 base64", ErrMalformedHash)
	}
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("%w: 哈希不是合法 base64", ErrMalformedHash)
	}

	// 用**存下来的**参数重算，不是用当前配置——那正是调参之后
	// 老用户还能登录的原因。
	got := argon2.IDKey([]byte(plaintext), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// truncate 让错误信息里不出现完整的哈希串。
// 它虽然不是明文密码，但也没有任何理由进日志。
func truncate(s string) string {
	if len(s) > 24 {
		return s[:24] + "…"
	}
	return s
}
