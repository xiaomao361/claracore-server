# ClaraCore Server 使用与验收指南

> 适用版本：`dev-20260827-agent-judged-v1` 及以后
>
> 示例地址：`https://claracore.example.com`
> 本文只放使用方法和可观察结果；设计理由分别见 `01`–`05`。

## 1. 两个入口

- **Web 治理面**：浏览 Memory / Card、审核摄取草稿、管理空间权限、成员、token、索引、活动与 Shared Line。内容业务写入仍由 MCP 统一承载；Web 只提供草稿审核与 Shared Line 归档/恢复这两类明确的人类治理动作。
- **MCP 工作面**：agent 创建、检索、更新、召回、提升内容，维护 Shared Line，并请求自动上下文。身份只来自 bearer token，不接受请求参数伪造 user / tenant / agent。

连接配置示例（token 用 Web 的“我的接入”页签发，本文不保存真实值）：

```json
{
  "mcpServers": {
    "claracore": {
      "type": "http",
      "url": "https://claracore.example.com/mcp",
      "headers": { "Authorization": "Bearer <TOKEN>" }
    }
  }
}
```

接入后先调用 `server_status` 和 `whoami`：前者确认服务版本与语义检索形态，后者确认默认个人空间及可读写空间。

`server_status.embeddingCache` 显示两级 embedding 缓存是否启用及本进程累计的 hits/misses。相同情境重复调用后 hits 应增加；计数在进程重启后归零，但配置 Redis 时缓存值仍可跨重启和实例复用。管理员可在“系统状态”查看 Redis 当前是否连通。

管理员“系统状态”还给出实例运行时间、当前 schema 是否仍与二进制一致、数据关系巡检状态、SIGTERM 摘流窗口、MySQL 连接池的使用/上限、打开、空闲和累计等待、每 agent 与每租户 MCP 调用预算、租户逻辑容量、MySQL/Redis/Embedding/Atlas 已配置并装配的传输形态，以及最近一次成功备份和技术恢复演练的 UTC 时间、年龄预算与证据边界。占用达到 80% 会提示注意，但累计等待是本次进程的历史计数，不等于当前请求变慢；排障时还要结合运行时间、数据库端连接与慢查询。容量快照是管理员打开页面时生成的当前租户精确行数与应用载荷字节：可以观察 Memory、Card、共同线、索引、外部镜像和审计增长，但不含数据库索引/行开销、备份、日志或副本，因此不等于物理磁盘和账单用量；当前也不会据此拒绝写入。MCP 调用预算只表示请求硬门，同样不是存储、成员或计费配额。备份显示“按期完成”只表示 verifier 已通过 SHA-256、gzip 和 metadata；恢复显示“技术演练按期完成”只表示隔离导入、逐表 QUICK 检查、行数/迁移枚举和目标清理完成；数据关系显示“已验证”只表示固定 29 条规则在一次一致性快照中零违规。代表性 Memory、Card、Shared Line 语义抽查仍未由这些回执证明。

### 概览看板怎么读

- “内容概览”是当前用户可读空间的内容量与待审工作，不代表租户全库；
- “进行中的共同线”是当前团队租户的精确生命周期统计，待复核与已陈旧是 active 子集；
- “有召回条件的卡片”只以 active 卡片为分母，历史、废弃与草稿不会粉饰覆盖率；
- “语义索引失败”中的“已耗尽重试”是 failed 子集，不与 failed 相加；
- “近 30 天已关联反馈的选择”用匿名 `contextId` 显示可验证的反馈覆盖；反馈 outcome 仍由调用方主动提交，不能把覆盖率或 outcome 比例解释为准确率。
- “近 7 天共享协作”只统计当前用户可读 team/project 空间里的成功写入，并按 UTC 自然日给出完整日桶；活跃成员按用户账号去重，活跃 Agent 按接入身份去重。个人空间、不可读项目、失败操作和读取次数不进入数字，这组信号只代表采用情况，不代表知识质量。

每张信号卡都链接到对应治理页面；局部统计失败时首页仍可使用，并会在顶部明确提示哪组暂不可用。

“治理待办”把这些信号重新排成人可以逐项处理的工作单：草稿与已发布卡片质量只统计当前成员有写权限的空间，管理员才会看到索引和外部来源运行项。它不复制写操作，仍跳转到审核、内容库、共同线、上下文质量或管理员页面完成处理。

“自动上下文质量”可切换近 7 天和近 30 天，分别查看选择、放弃、可关联反馈覆盖、固定放弃原因与主动反馈 outcome。没有反馈表示未知；页面上的进度和比例都不是准确率。需要核对具体反馈事件时，从页面进入已经按 `gateway_context_feedback` 和同一时间窗口过滤的活动流。

“我的接入”先按 Agent 汇总有效/全部 token、最近签发与最后使用，并可直接进入该 Agent 的活动流；下面的 Token 明细仍用于轮换和吊销。轮换时先签发并验证新 token，确认旧 token 的最后使用不再变化后再吊销。

## 2. Memory：事实与变化

### 记一条当前事实

```json
{
  "title": "当前发布窗口",
  "body": "常规发布窗口为每周四 20:30。",
  "labels": ["发布窗口"]
}
```

调用 `memoria_create`；不传 `space` 默认写个人空间。

### 事实发生变化

先搜索并确认旧事实 ID，再创建新事实并传 `supersedes`。服务在一个事务中：

1. 创建当前事实；
2. 把旧事实改为 `superseded`；
3. 建立 `current -> historical` 的 `supersedes` 链。

`memoria_search.timeView`：

- `current`：默认，只查 active；
- `historical`：只查 superseded；
- `all`：同时查当前与历史。

同一事实的文字修正用 `memoria_update`；事实真的变了才用 supersede。无法确认谁是当前事实时，用 `memoria_link(kind=contradicts)`，不要擅自判定。

## 3. Card：可复用经验与演进

Card 至少应包含：标题、提炼、理解，以及具体的 `recallTriggers`。触发条件写“未来正在做什么时该想起”，不要写关键词。

```json
{
  "title": "发布验证要分层",
  "cardType": "发布规范",
  "distilled": "源码、构建、部署、运行时和业务验收分别留证。",
  "understanding": "上游成功不能替代下游环境与业务结果。",
  "recallTriggers": ["准备宣布一次服务发布已经完成时"]
}
```

- `card_search`：按主题找卡；
- `card_recall`：显式按当前工作情境快速召回；
- `card_create.evolvesFrom`：创建新版本、把旧卡置为 `deprecated` 并建立 `evolved-from` 链，三步原子提交；
- `promote`：复制到项目或团队空间，原件保留。自动召回会折叠同一提升谱系，避免原件和副本重复占位。

摄取只产出 `draft` 原料，不让模型自动把外部文档当成已确认知识。人在 Web 审核页补齐理解与召回条件后再激活。

## 4. 自动上下文

每个需要自动召回的用户回合，把真实消息原样传给 `gateway_auto_context.prompt`。若调用方已明确处于某条 Shared Line，可同时传 `lineId`；精确 line scope 优先，服务不会从多条 active line 中猜测。返回只有两种：

- `decision=deliver_one`：读取且只使用 `block.body`；保存返回的匿名 `contextId`，后续反馈时原样回传。`selected` / `selectedMemory` 只是候选证据，不代表已经交付或使用；
- `decision=abstain`：没有足够可靠的证据，或 embedding 不可用、分数不足、域内 top-2 歧义、Memory 与 Card 跨域双命中、超时；不要用显式搜索绕过这次自动放弃。

未传 `lineId` 时，自动路径让 current Memory 与 active Card 分别做语义候选，再用同一组最低分和 top-2 差值做确定性 Stage B。只允许一域胜出；两域都有高置信首条时保守放弃，不把两种不同知识形态的分数硬通约。整体硬上限 15 秒，证据正文按字符数截断并显式标记，避免一条长 Memory 占满调用方上下文。证据块会明确要求终端 Agent 的大模型做最终相关性判断；候选字段不等于 `delivered` 或 `used`。显式 `memoria_search` / `card_recall` 不经过自动策略。

调用方发生真实结果后，用 `gateway_context_feedback` 记录：

- `delivered`：确实放入了上下文；
- `used`：实际影响了回答或动作；
- `ignored`：交付了但没有使用；
- `wrong`：召回错误；
- `corrected`：错误已被当前证据纠正。

新调用用 `domain + id` 标识 `memory`、`knowledge_card` 或 `shared_line`，并原样回传本次 `deliver_one.contextId`。`contextId` 是服务端生成的随机关联 ID，不含 prompt、实体正文或 actor；Server 只把能匹配当前租户、本统计窗口真实选择的去重 ID 计入反馈覆盖。旧的 `cardId` 与不带 `contextId` 的反馈仍兼容：outcome 会保留，但不计入选择覆盖。反馈只进入治理活动，不改写实体正文。

用 `context_quality_status` 查看当前租户近 30 天的 `selected`、`abstained`、`abstainReasons`、`feedbackCoveredSelections` 与已提交 outcome 计数。放弃原因固定归为无相关证据、歧义、语义不可用、检索超时、Shared Line 未就绪与其他/旧记录；六类之和必须与 `abstained` 一致。前两类主要指向语料覆盖或重复治理，语义不可用/超时主要指向运行链路，但它们仍只是确定性服务端决策分类，不是准确率。`feedbackCoveredSelections / selected` 是本窗口可验证的反馈覆盖；outcome 仍由调用方主动提交，不能把 `used / submitted` 或任何 outcome 比例直接解释为自动召回准确率。

## 5. Shared Line

Shared Line 保存“工作推进到哪里”，不是第二套事实库：`factsUsed` 只保存 Memory / Card ID。

典型顺序：

1. `shared_line_list` 选择 exact line；
2. `shared_line_get` 读取当前位置、最近交接和最近 10 条位置/交接历史；
3. 有意义的进展用 `shared_line_update`；
4. 阶段切换用 `shared_line_handoff`；
5. 完成后用 `shared_line_status(status=archived)`，继续时改回 `active`。

当前 agent 有多条 active line 而未传 `lineId` 时，服务返回有界候选并拒绝猜测。

## 6. Web 筛选和演示数据

- “内容浏览”可在类型、空间、状态范围内全文查找标题与正文；下拉框会选择即提交，切换类型时清除不兼容的旧状态，按钮是无 JavaScript 兜底。查找保留 draft / deprecated 等治理状态和稳定分页，不做语义重排。
- “我的接入”“成员管理”“空间与权限”都使用稳定的 keyset 分页；空间治理一次只打开一个共享空间的成员授权。新增记录不会像 OFFSET 分页那样挤动已经翻过的行。治理列表只读取展示字段，不把密码哈希或 Token 哈希带入 Web 进程；Token 明文仍只在签发成功时显示一次。
- 改密、管理员重置密码和停用成员会同时删除已知 Web 会话并推进用户认证版本。会话在每次请求恢复时比对签发版本，因此并发于重置的旧密码登录也不会在删除窗口后复活。
- 管理员提高 Argon2id 配置后，成员下一次成功登录会无感升级逐项较弱的存量哈希；升级不撤销会话，也不会覆盖同时发生的密码重置。调低或不可直接比较的参数不会自动改写旧哈希。
- “审核队列”的空间筛选同样生效。可勾选当前页中明确不需要的草稿后批量移出队列；每批最多 50 张，服务端原子校验版本与可写权限，任一项变化则整批不处理。批量发布不存在，发布仍需逐张补齐理解与召回条件。
- 知识卡审核与详情页会复用当前检索模型查找 active 重复候选：规范化正文相同会标为“内容相同”，语义分不低于 0.90 会标为近重复。候选只供人工比较，系统不自动合并或删除；确认重复后把非保留项标为 `deprecated`，历史仍可按 ID 与状态筛选查看。未启用 embedding 时仍能发现进入全文候选集的精确重复，但不会声称语义近似。
- “共同线”是左侧独立导航，不混入 Memory / Card 类型；可看进行中、已归档、全部，进入详情查看当前位置、最近 10 条位置/交接历史，并执行归档或恢复。状态写入有 CSRF 与治理审计。
- 管理员“团队导出”在明确确认后生成完整租户 ZIP。下载包含成员公开身份、空间权限、Memory、Card 与 Shared Line 历史，不包含密码/Token 哈希、会话、embedding、旧审计或运行队列；敏感读取会先写 meta-audit。下载后先运行发布包中的 `-verify-export`，再用 `-plan-import` 检查实体关系并选择接管管理员，这两个离线动作都不读取 `.env` 或连接外部依赖。`-import-export` 只能原子写入已迁移的空部署：接管管理员设置新密码，其他成员保持禁用，不能合并或覆盖现有团队。配置 embedding 时会把当前模型的可检索内容与导入数据同事务入队，但不会在 CLI 中调用模型；禁用时回执明确标为 `backfill_required`。导入 JSON 若因终端断开而丢失，在启动服务前用 `-check-import -import-admin <账号>` 只读重建纯净收据，不能盲目重跑。详细命令和索引收口见 `07-deployment.md`；迁移成功仍不替代来源认证、灾备恢复演练或代表性内容人工抽查。
- `[演示]` 前缀及 `demo-seed` 标签的数据是拟真测试材料，不是团队业务事实。

多用户展示数据由独立命令生成，不需要替换或重启正在运行的 Server。命令必须显式指定目标 tenant 和固定 seed key；首次应用把 5 组随机初始密码与 MCP token 写入一个新的 `0600` 绝对路径文件，标准输出只含不带秘密的 ID 回执。重复应用会核对数据库回执并返回 `alreadyApplied=true`，不会新增第二批数据：

```bash
go run ./cmd/demo-seed \
  -mode apply \
  -tenant <目标 tenant ULID> \
  -confirm demo-seed-20260831-multi-user-v1 \
  -credentials-file /secure/absolute/demo-credentials.json

go run ./cmd/demo-seed -mode inspect -tenant <目标 tenant ULID>
```

清理只接受同一个 tenant 与 seed key，并按数据库回执中的精确 ID 在单事务内删除。若演示账号后来产生了回执之外的 Agent、Token、空间、Memory、Card 或 Shared Line，命令会拒绝自动清理，避免把后续人工数据当作 seed 一并删除：

```bash
go run ./cmd/demo-seed \
  -mode cleanup \
  -tenant <目标 tenant ULID> \
  -confirm demo-seed-20260831-multi-user-v1
```

页面人工验收仍由用户完成：刷新后分别核对 1440px 与 390px、筛选结果、共同线内容、登录/权限/活动页；自动测试和 HTTP 探针不等于视觉验收。

## 7. 运维验证

本地发布门槛：

```bash
make check
make itest       # 修改 SQL、迁移或真实适配器后必须运行
go run ./cmd/server -check  # 只读预检，不启动 HTTP/Redis/Embedding/worker
go run ./cmd/server -check-upgrade # 既有安装停服务前判断迁移维护窗口与回滚模式
go run ./cmd/server -check-integrity # 只读检查关键跨表关系；违规 JSON 写出后返回非零
./scripts/package-release.sh <version> amd64 dist /secure/offline/claracore-release-ed25519.pem
# 解包后：./claracore-server -verify-provenance "$(pwd)/BUILD_PROVENANCE.intoto.json"
```

部署后分层验证：

1. 用独立可信渠道预先固定的 Ed25519 公钥运行 `verify-release-signature.sh`，认证发布者、外层 SHA-256 与归档字节；解包后运行 `verify-release.sh`，检查 `BUILDINFO.txt`、`BUILD_REPRODUCIBILITY.json`、`BUILD_PROVENANCE.intoto.json`、`SOURCE_MANIFEST.sha256`、`BUILD_INPUTS.sha256`、`VULNERABILITY_SCAN.txt`、`SBOM.cdx.json`、`SBOM_REVIEW.txt`、`PROMETHEUS_RULES_CHECK.txt`、`THIRD_PARTY_LICENSES.csv`、`THIRD_PARTY_REVIEW.txt` 与 `third_party_licenses/`，再用当前二进制和绝对路径执行 `-verify-provenance`，确认 subject SHA、版本、目标、Go/sourceDigest、两份输入清单和 8 份回执与当前目录完全匹配；其中有界可复现性回执的 primary/rebuild SHA 必须同时等于当前二进制。随后用 `-version` 核对应用版本、源码摘要、二进制摘要与目标平台，其中 `binaryDigest` 必须等于目标文件的 `sha256sum`，也必须等于 SBOM、可复现性回执与 provenance subject 的 SHA-256。发布归档固定 tar/gzip 元数据与压缩参数，相同解包内容和可执行位可逐字节重建同一归档。两次构建只隔离同一主机上的 Go build cache；provenance 是签名发布者对本地构建事实的机器可读声明，不是 DSSE，也没有托管/加固 builder 证据，不能解释成 SLSA Build Level、hermetic/跨主机二进制可复现或独立来源证明。公钥若只和未知归档一起下载，不构成信任根；SBOM 不是发布者认证或漏洞/许可结论，构建时 promtool 回执也不证明目标监控已加载/触发，旧扫描回执不替代上线前按当前漏洞库复扫，自动许可证分类也不替代新依赖人工复核；
2. 数据库备份 companion 校验、原子技术恢复回执、隔离恢复报告与代表性内容人工抽查；技术回执固定不声称语义抽查完成；
3. 新 release 先运行 `-check-upgrade`；它必须同时接受远程依赖的验证 TLS 配置。有 pending migration 时必须进入停写维护窗口并在停写后生成回退备份，迁移完成后 `-check` 才能返回 MySQL reachable 与 schema current，`-check-integrity` 必须返回 `verified` 且零违规；
4. Supervisor / systemd 进程；
5. `verify-deployment.sh` 证明 `/livez` 与 `/readyz` 的版本、源码摘要、二进制摘要和环境一致，再验证 MySQL + schema readiness、Web、MCP 401 边界；提供 metrics URL 时还要求备份与技术恢复回执都健康；
6. 多实例为每个进程配置唯一 `CCS_INSTANCE_ID`，用发布包内 `verify-cluster.sh` 逐实例核对 live/ready、版本、源码摘要、二进制摘要、环境与 schema，并确认公开入口采样覆盖全部预期实例；生产环境不能使用 `sourceDigest=development` 的手工构建。该回执不证明发布者身份，也不替代摘流、故障切换或滚动发布验收；
7. 管理员系统页的 HTTP 最近窗口随实际 Web/MCP 调用增加；隔离制造的测试 5xx 出现注意态并在 5 分钟后退出窗口。该卡只代表当前实例，不能当作集群告警；
8. 系统页明确显示 MCP 的每 agent 与每租户聚合调用预算；用隔离 agent/token 做限流验收时，同一 agent 的第二个 token 不应获得新预算，同一租户新增的第二个 agent 也不能绕过团队总预算，429 应带 `Retry-After`。多实例时让两个隔离 agent 分别命中不同实例，验证 Redis 共享计数；不要用真实日常 token 压测；
9. 系统页生成租户容量快照：确认成员/Agent/空间、Memory/Card、共同线、索引、外部镜像和审计行数与目标租户一致；连续抓取 `/metrics` 不应触发容量 SQL。将逻辑载荷只用于增长基线，不与 `du`、InnoDB 物理占用或账单额度直接比较；
10. 从目标主机直接抓取独立 metrics 地址，确认 build、accepting、HTTP、MySQL pool、backup、restore、integrity 与 external health snapshot 指标存在；连续两次抓取不应增加数据库查询，公开 Web 域名的 `/metrics` 必须为 404；
11. 用 `promtool check rules` 校验随包规则，加载后在隔离目标上分别验证 target down、备份/技术恢复/数据完整性回执缺失或过期、数据关系违规、持续连接池压力、外部健康快照中断与手动请求停滞能进入 pending/firing；确认 down 与摘流、备份缺失与过期、恢复缺失与备份故障、完整性过期与违规、外部失败与 stale 不会重复通知；
12. `initialize` 的版本与 `tools/list`；
13. 自动上下文的共享 Card 正样本、无关负样本、重复 Card/Memory 域内歧义与 Memory/Card 跨域歧义；
14. 代表性的 Memory supersede、Card evolution、Shared Line archive/restore 读写。
15. 管理员完成一次团队导出：确认响应不可缓存；用发布包的 `-verify-export` 与 `-plan-import` 得到成功 JSON；再人工确认当前租户代表性 Memory/Card/Shared Line 存在，且归档全文检索不到已知测试密码哈希、Token 哈希或其他租户标记。
16. 空部署迁移演练：使用隔离的、已迁移且没有租户的 MySQL 执行 `-import-export`；在启动服务前立即用 `-check-import` 保存第二份只读回执，确认接管管理员必须改密、其他成员禁用、13 个数据集计数、排除运行态和索引 pending 形态；然后启动 worker，确认 pending/failed 归零，再做代表性语义抽查。embedding 禁用时需在启用后人工回填。不要在现有团队数据库上试跑，也不要把 `-check-import` 当成服务启动后的健康检查。
17. 在目标数据库备份完成后运行发布包的 `-check-integrity`，保存 stdout JSON；必须是 29 条规则、`violations=0`，并生成 0600 `claracore-data-integrity/v1` 回执。启用每日 timer 后，在系统页确认“数据关系”已验证，在 metrics 确认 available/fresh/verified 为 1 且 violations 为 0，再用部署探针复核。若返回非零，新的违规回执会进入告警；先保留数据库和备份调查，不要自动删行或把规则计数误当成内容语义验收。

详细命令与回滚见 `07-deployment.md`。

## 8. 当前剩余边界

- 目标服务器登录后的 1440px / 390px 人工视觉与完整业务验收尚未由用户确认。
- 团队导出已具备离线格式校验、关系预检和空部署原子导入，配置模型的索引队列也与导入同事务生成；但尚未在目标服务器用真实团队归档完成接管、worker 清空队列（或禁用形态下的回填）与人工语义抽查。它不支持租户合并、增量同步或覆盖恢复。
- 自动上下文已覆盖 Memory / Card 的保守单证据竞争，并支持调用方明确 `lineId` 的 Shared Line 精确上下文；尚未做 Shared Line 的语义发现，因为系统不能从多条工作线中可靠猜测当前 scope。
- embedding 已有进程内有界 LRU、可选 Redis 共享层与按模型的短索引租约；多实例探针现在能用唯一 `instanceId`、正式发布源码摘要与当前可执行文件摘要验证直连实例一致性与负载均衡入口当前分发覆盖，监控也会提示生产开发构建或长时间混合制品。单进程 SIGTERM 已验证 readiness draining、业务请求拒绝、liveness 保持和延迟关 listener。但两台 Linux、真实负载均衡摘流、多实例故障注入与滚动发布仍未完成实机验收。Redis 故障时会 fail-open 并允许临时重复幂等计算。
- 真实演示数据已出现近重复卡，Stage B 会因 margin 不足安全放弃；现在详情页会给出重复候选并复用 `deprecated` 完成人工废弃。仍未提供自动合并，因为系统不能替人决定两个相似表述是否是同一事实。
- `gateway_context_feedback` 已留治理事件；`context_quality_status` 提供近 30 天租户级选择、放弃、已关联反馈选择和 outcome 汇总。覆盖分母已经可验证，但 outcome 仍是主动提交样本；仍需随着真实 wrong/ignored 反馈扩充固定评估集，不能把它当成独立准确率。
- Atlas 活知识源已支持 body-free 定时同步、统一检索和管理员 `/admin/sources` 治理页。管理员可查看来源状态、手动请求状态与最近 20 次运行回执，或持久记录一轮同步请求；重复请求被合并，实例重启或切换后可由其他实例领取。“同步请求已记录”不等于同步成功，需以随后出现的成功/失败回执为准。odoc 当前仍缺稳定枚举、版本与按 ID 取正文契约，暂不接入。
- 原子 MySQL 备份、隔离恢复报告和有界技术恢复回执已进入发布包，并贯通系统页、指标、部署门禁与告警；但当前开发主机没有 mysql/mysqldump CLI，尚需在真实隔离 MySQL 生成回执/报告并人工抽查代表性 Memory、Card 与 Shared Line。模拟 CLI 和自动脚本测试不能替代这层证据。
- Ed25519 发布签名与攻击路径门禁已经实现，但仓库不保存生产私钥；首次真实生产签名仍需维护者在受控发布机生成私钥、通过独立渠道固定公钥指纹并演练轮换。
- 远程生产依赖现在会在启动前强制验证 TLS/HTTPS；本地测试已覆盖私有 CA、mTLS 文件边界和真实 Redis TLS 读写，但尚未在目标网络用真实 MySQL TLS、企业私有 CA、远程 Embedding/Atlas 证书及轮换过程完成部署验收。
- 不提供 Desktop 的 InnerLife 和个人时序 Records；这是产品边界，不是遗漏。
