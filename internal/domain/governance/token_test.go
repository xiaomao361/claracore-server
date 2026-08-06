package governance

import (
	"errors"
	"strings"
	"testing"
)

func TestNewTokenSecret(t *testing.T) {
	plaintext, prefix, hash, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("明文能被 ParseToken 接受且 hash 一致", func(t *testing.T) {
		got, err := ParseToken(plaintext)
		if err != nil {
			t.Fatalf("自己生成的 token 应当能被自己解析: %v", err)
		}
		if got != hash {
			t.Error("ParseToken 与 NewTokenSecret 必须产出同一个 hash，否则签发的 token 永远认证不过")
		}
	})

	t.Run("prefix 是明文的前缀", func(t *testing.T) {
		if !strings.HasPrefix(plaintext, prefix) {
			t.Errorf("prefix %q 不是明文的前缀", prefix)
		}
		if len(prefix) != tokenPrefixLen {
			t.Errorf("prefix 长度应为 %d，实际 %d", tokenPrefixLen, len(prefix))
		}
	})

	t.Run("hash 是 64 位 hex", func(t *testing.T) {
		if len(hash) != 64 {
			t.Errorf("SHA-256 hex 应为 64 字符，实际 %d——库里的列是 CHAR(64)", len(hash))
		}
	})

	t.Run("明文里保留的秘密远多于 prefix 暴露的", func(t *testing.T) {
		secret := plaintext[len(prefix):]
		if len(secret) < 32 {
			t.Errorf("prefix 之后剩余 %d 字符，太短——prefix 会出现在列表和日志里", len(secret))
		}
	})
}

func TestTokenSecretsAreUnique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		plaintext, _, _, err := NewTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		if seen[plaintext] {
			t.Fatal("生成了重复的 token——随机源出问题了")
		}
		seen[plaintext] = true
	}
}

func TestParseTokenRejectsMalformed(t *testing.T) {
	valid, _, _, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		input string
	}{
		{"空串", ""},
		{"只有前缀", tokenScheme},
		{"缺少 scheme", strings.TrimPrefix(valid, tokenScheme)},
		{"scheme 不对", "ccx_" + strings.TrimPrefix(valid, tokenScheme)},
		{"太短", valid[:len(valid)-1]},
		{"太长", valid + "A"},
		{"含非法 base64 字符", valid[:len(valid)-1] + "!"},
		{"整串是别的东西", "Bearer abcdefg"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseToken(tc.input); !errors.Is(err, ErrTokenMalformed) {
				t.Errorf("应拒绝畸形 token（挡掉扫描器流量，不喂给数据库），得到 %v", err)
			}
		})
	}
}

// 首尾空白要容忍：从 header 或配置文件里复制 token 时极易带上。
func TestParseTokenTrimsWhitespace(t *testing.T) {
	valid, _, hash, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseToken("  " + valid + "\n")
	if err != nil {
		t.Fatalf("带首尾空白的 token 应当被接受: %v", err)
	}
	if got != hash {
		t.Error("去空白后应得到同一个 hash")
	}
}

func TestHashTokenIsDeterministic(t *testing.T) {
	const sample = "ccs_whatever"
	first, second := HashToken(sample), HashToken(sample)
	if first != second {
		t.Fatal("同一明文必须得到同一 hash")
	}
	if HashToken("ccs_a") == HashToken("ccs_b") {
		t.Fatal("不同明文得到了相同 hash")
	}
}
