package crypto

import (
	"errors"
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	h := NewArgon2id(testParams())

	hash, err := h.Hash("正确的密码")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := h.Verify("正确的密码", hash)
	if err != nil || !ok {
		t.Fatalf("正确的密码应当验过: ok=%v err=%v", ok, err)
	}
	ok, err = h.Verify("错误的密码", hash)
	if err != nil {
		t.Fatalf("密码不对不是错误，只是 false: %v", err)
	}
	if ok {
		t.Fatal("错误的密码不该验过")
	}
}

// TestHashIsSalted 盯住「两次哈希同一个密码必须不同」。
//
// 没有盐的话，库里两行相同的 hash 就直接告诉读到数据库的人
// 「这两个账号用了同一个密码」——而那通常是初始密码。
func TestHashIsSalted(t *testing.T) {
	h := NewArgon2id(testParams())

	a, err := h.Hash("同一个密码")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("同一个密码")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次哈希必须不同——相同的 hash 会泄露「这两个账号密码一样」")
	}
	for _, hash := range []string{a, b} {
		if ok, err := h.Verify("同一个密码", hash); err != nil || !ok {
			t.Fatalf("两个 hash 都该验过: %v %v", ok, err)
		}
	}
}

// TestVerifyUsesEmbeddedParams 是这一层最要紧的一条。
//
// 用**当前配置**去验老 hash 的话，任何一次调高参数都会把所有存量用户
// 锁在门外——而且是静默的：他们只会看到「密码错误」。
func TestVerifyUsesEmbeddedParams(t *testing.T) {
	weak := NewArgon2id(Argon2idParams{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	hash, err := weak.Hash("旧参数下设的密码")
	if err != nil {
		t.Fatal(err)
	}

	// 运维调高了参数
	strong := NewArgon2id(Argon2idParams{Memory: 32 * 1024, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32})
	ok, err := strong.Verify("旧参数下设的密码", hash)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("调高参数之后，存量用户必须仍能登录——参数编码在 hash 串里就是为了这个")
	}
}

func TestHashEncodesParams(t *testing.T) {
	h := NewArgon2id(Argon2idParams{Memory: 19456, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	hash, err := h.Hash("x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("应是标准 PHC 串，实际: %s", hash)
	}
}

// TestMalformedHashIsNotSilentlyWrongPassword 区分数据问题与用户问题。
//
// 混为一谈的话，一次数据迁移事故会表现成「所有人突然密码都错了」，
// 而运维会去查登录逻辑而不是去查数据。
func TestMalformedHashIsNotSilentlyWrongPassword(t *testing.T) {
	h := NewArgon2id(testParams())

	for name, bad := range map[string]string{
		"集成测试塞的假值":   "$argon2id$e2e$fake",
		"别的算法":       "$2y$10$abcdefghijklmnopqrstuv",
		"空串":         "",
		"段数不对":       "$argon2id$v=19$m=1,t=1,p=1$onlysalt",
		"盐不是 base64": "$argon2id$v=19$m=19456,t=2,p=1$!!!!$AAAA",
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := h.Verify("任意密码", bad)
			if ok {
				t.Fatal("畸形 hash 绝不能验过")
			}
			if !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("应报 ErrMalformedHash 而不是当成密码错误，实际: %v", err)
			}
		})
	}
}

func TestErrorDoesNotEchoWholeHash(t *testing.T) {
	h := NewArgon2id(testParams())
	long := "$notargon2$" + strings.Repeat("s", 200)

	_, err := h.Verify("x", long)
	if err == nil {
		t.Fatal("应当报错")
	}
	// hash 不是明文密码，但也没有任何理由整串进日志
	if strings.Contains(err.Error(), strings.Repeat("s", 100)) {
		t.Fatalf("错误信息不该带上完整哈希串: %v", err)
	}
}

// testParams 用最低档参数，让测试跑得快。
// 生产用 DefaultParams()——两者的差别正是「参数属于配置」的例证。
func testParams() Argon2idParams {
	return Argon2idParams{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}
