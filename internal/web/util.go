package web

import (
	"bytes"
	"crypto/rand"
	"math/big"
)

func newBuffer() *bytes.Buffer { return &bytes.Buffer{} }

// passwordAlphabet 刻意去掉了容易看错的字符：0/O、1/l/I。
//
// 初始密码要被人从屏幕上读出来、发到聊天工具里、再手输一次。
// 一个 l 和 1 的混淆换来的是一次「你是不是密码发错了」的往返。
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// generatedPasswordLen 是服务端生成的初始密码长度。
//
// 16 位，远高于 MinPasswordLen（那是人自己设密码时的下限）。
// 它是机器生成的，长一点没有记忆成本——而它会经过聊天工具传递，
// 值得多几位熵。
const generatedPasswordLen = 16

// generatePassword 生成初始密码。
//
// 用 crypto/rand 而不是 math/rand：后者的默认种子在同一秒启动的
// 两个进程里可能一致，那会让两个新账号拿到同一个初始密码。
// 这类 bug 不会有任何症状，直到有人发现自己能登进别人的账号。
func generatePassword() (string, error) {
	out := make([]byte, generatedPasswordLen)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}
