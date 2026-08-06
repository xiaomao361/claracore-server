package platform

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// ID 是 26 字符的 ULID（Crockford Base32）。
//
// 选择 ULID 而非 UUID v4 的原因（docs/02-data-model.md §7）：
//   - 时间有序：作为 InnoDB 主键时顺序插入，避免随机 UUID 的页分裂
//   - 可读：前缀即时间，排查问题时肉眼可比先后
//   - 分布式友好：无需协调即可生成
type ID string

const (
	ulidLen      = 26
	crockfordAlp = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // 去掉 I L O U，避免混淆
)

// IDGenerator 生成 ULID。同一毫秒内保证单调递增，
// 使同一批次创建的记录顺序稳定。
type IDGenerator struct {
	clock Clock

	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
}

func NewIDGenerator(clock Clock) *IDGenerator {
	return &IDGenerator{clock: clock}
}

func (g *IDGenerator) New() ID {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(g.clock.Now().UnixMilli())

	if ms == g.lastMS {
		// 同毫秒：随机部分 +1，保证单调
		incrementBytes(&g.lastRand)
	} else {
		g.lastMS = ms
		if _, err := rand.Read(g.lastRand[:]); err != nil {
			// crypto/rand 失败属于系统级故障，不可静默降级为可预测 ID
			panic(fmt.Sprintf("platform: crypto/rand 不可用: %v", err))
		}
	}

	return ID(encode(ms, g.lastRand))
}

// incrementBytes 对 80bit 随机部分做大端 +1。
func incrementBytes(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// encode 将 48bit 时间戳 + 80bit 随机数编码为 26 字符 Crockford Base32。
//
// 128 bit 无法被 5 整除，标准 ULID 在最高位补 2 个 0 凑成 130 bit，
// 因此首字符实际只承载 3 bit。这里用逐位实现而非位运算技巧——
// ID 每次生成只调用一次，可读性远比微优化重要。
func encode(ms uint64, entropy [10]byte) string {
	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
	copy(raw[6:], entropy[:])

	out := make([]byte, ulidLen)
	for i := 0; i < ulidLen; i++ {
		// 第 i 个字符在 raw 位空间中的起始位置（前 2 bit 为补零，故减 2）
		start := i*5 - 2
		var v uint32
		for j := 0; j < 5; j++ {
			bit := start + j
			var b uint32
			if bit >= 0 && bit < 128 {
				b = uint32(raw[bit/8]>>(7-uint(bit)%8)) & 1
			}
			v = v<<1 | b
		}
		out[i] = crockfordAlp[v]
	}
	return string(out)
}

// Timestamp 从 ID 中还原创建时刻，用于排查问题。
//
// 前 10 个字符承载 50 bit，其中最高 2 bit 是编码补零，
// 余下 48 bit 即毫秒时间戳——因此累加结果无需额外移位。
func (id ID) Timestamp() (time.Time, bool) {
	if len(id) != ulidLen {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		idx := indexOf(byte(id[i]))
		if idx < 0 {
			return time.Time{}, false
		}
		ms = ms<<5 | uint64(idx)
	}
	if ms>>48 != 0 { // 补零位非零说明不是合法 ULID
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

func indexOf(c byte) int {
	for i := 0; i < len(crockfordAlp); i++ {
		if crockfordAlp[i] == c {
			return i
		}
	}
	return -1
}
