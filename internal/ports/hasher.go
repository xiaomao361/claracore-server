package ports

// PasswordHasher 负责密码的哈希与校验。
//
// # 为什么这件事不在领域层
//
// argon2id 的 memory / time / parallelism 是**配置**（不同机器该配不同的值），
// 而领域层不读配置（docs/05-code-structure.md §6.3，由 test/arch_test.go 强制）。
// 所以领域服务只依赖这个端口，不直接依赖具体算法或配置来源。
//
// # 为什么 Verify 也在这里
//
// 让调用方自己比对是个陷阱：
//
//   - 用 == 比字符串会泄露时序信息。常数时间比较必须只有一处实现。
//   - 哈希串里编码着当时用的参数。将来调高参数后，老用户的 hash 仍要能验过——
//     那个「用 hash 里记的参数去验」的逻辑，跟「用当前配置去算」的逻辑
//     必须写在一起，否则某次调参就会把所有存量用户锁在门外。
//
// # 实现约定
//
//   - Hash 每次返回不同的结果（盐随机），所以**不能**拿两次 Hash 的结果比大小。
//   - Verify 对畸形 hash 返回 (false, error)，不 panic：库里可能存着
//     测试塞进去的假值，一次登录尝试不该让进程挂掉。
//   - NeedsRehash 只在当前参数可以逐项提升存量哈希时返回 true；
//     不得因运维误配较低参数而自动降级更强的旧 hash。
type PasswordHasher interface {
	Hash(plaintext string) (string, error)
	Verify(plaintext, hash string) (bool, error)
	NeedsRehash(hash string) (bool, error)
}
