# ClaraCore Server

ClaraCore Server is a self-hosted, multi-tenant memory and knowledge service for AI agents. It exposes an MCP endpoint for agents and a server-rendered Web governance console for people.

ClaraCore Server 是面向 AI Agent 的自托管记忆与知识服务。它把三类长期上下文作为并列的一等公民：

- **记忆（Memory）**：可演进的事实，保留 current / historical 时间视图；
- **知识卡（Knowledge Card）**：带“何时该想起它”的结构化经验；
- **共同线（Shared Line）**：跨 Agent、跨会话继续同一目标的可恢复工作线。

服务端只做权限过滤、候选检索和保守的自动上下文预筛。最终相关性判断由终端 Agent 配置的大模型负责；服务端不会再运行一个 7B 判断模型。

本项目采用宽松、带明确专利授权的 [Apache License 2.0](LICENSE)。
正式发布包还包含按目标 Linux 架构生成的 CycloneDX 1.6 SBOM、第三方许可证报告、许可证/NOTICE 文件，以及 MPL-2.0 依赖要求随分发提供的对应源码；这些材料进入同一发布清单。SBOM 绑定最终二进制 SHA-256、链接模块、Go 标准库和目标平台，不替代漏洞扫描、许可证复核或发布者签名。

## 能力

- Streamable HTTP MCP，Bearer Token 鉴权，24 个显式工具；
- tenant / user / agent / team-project-personal space 权限隔离；
- 中文全文检索，选配 bge-m3 或 OpenAI-compatible embedding 的两阶段语义检索；
- 进程内 LRU + 可选 Redis 的两级 embedding 缓存与索引租约，Redis 故障时自动降级；
- Web 登录在密码哈希前按 IP 与 IP+账号双维度限流，多实例共享且 Redis 故障时保留本地保护；
- 成功登录会把逐项弱于当前配置的 Argon2id 哈希渐进升级；条件更新同时锁定旧哈希与认证版本，避免覆盖并发改密，也不会因较低或不可比较的新配置自动降级存量哈希；
- Web 会话持久化并绑定用户认证版本；改密、管理员重置或停用账号后，即使有旧密码登录在“删除旧会话”之后并发落库，也会在下一次请求因版本不匹配被拒绝；
- MCP 认证用单次 JOIN 返回只含身份与状态的专用投影；提交的 Token 哈希只参与索引查找，查询结果结构不含 Token 或密码哈希；
- MCP 认证后同时执行每 agent 与每租户聚合固定窗口预算；同一 agent 轮换 token 或同一团队新增 agent 都不能绕过对应硬门，配置 Redis 时跨实例共享，故障时退回每实例有界保护；
- 应用 listener 自带慢请求、请求头和请求体硬边界；nginx 模板同步限制 1 MiB Web 表单与 4 MiB MCP 请求，反代误配或 loopback 直连不能绕过；
- 每个 HTTP 请求返回服务端生成的 `X-Request-ID` 并写一条结构化完成日志；日志只含受控路由、状态、耗时和响应字节，不记录 query、凭据、Cookie、正文或 panic 值；
- `make check` 使用固定版本的 Go 官方 `govulncheck` 和实时官方漏洞库阻断可达漏洞；正式打包再次扫描实际 Linux 二进制，并把工具链、模块版本与扫描回执绑定进发布清单；
- Linux amd64/arm64 运行时依赖经过固定版本许可证检查；未知或禁止许可证阻断发布，第三方报告、归属材料和 MPL-2.0 MySQL 驱动源码随制品分发；
- Linux amd64/arm64 发布二进制生成固定 CycloneDX 1.6 SBOM；独立门禁核对最终二进制 SHA-256、目标 PURL、链接模块、Go 标准库与依赖图，宿主平台误标、组件缺失或绝对路径泄漏都会阻断发布；
- 生产依赖传输 fail-closed：非 loopback MySQL/Redis 必须使用验证服务端身份的 TLS 1.2+，支持私有 CA 与可选 mTLS；非 loopback Embedding/Atlas 必须使用 HTTPS，系统页展示当前进程配置并装配的传输形态；
- 可选的独立 loopback Prometheus 文本端点，提供实例接流量、滚动 HTTP 健康、MySQL 连接池、备份、技术恢复与数据完整性回执新鲜度、外部活源健康快照指标；抓取路径不执行 SQL、不读取恢复数据，公网 nginx 明确拒绝 `/metrics`；
- 管理员系统页按需生成租户级 repeatable-read 容量快照，展示成员、Agent、空间、Memory、Card、共同线、索引、外部镜像与审计的精确行数和逻辑载荷；不读取正文、不进入 Prometheus 抓取路径，也不冒充物理磁盘或账单用量；
- 随发布包提供低噪声 Prometheus 告警规则，覆盖 target、异常摘流、生产开发构建、长时间混合制品、panic、持续 5xx、连接池压力、备份/技术恢复/数据完整性回执缺失或过期、数据关系违规，以及外部活源监控中断、同步失败/过期与手动请求停滞，并抑制已知同根因重复通知；`make check` 与正式打包均用固定 Prometheus 3.14.0 `promtool` 真实解析 22 条规则，并执行 target/draining、备份、外部 stale 和请求 backlog 四组低噪声行为测试，规则与测试 SHA、检查回执进入发布清单；
- SSR 页面与 MCP 成功/失败响应均不可缓存；Web 使用无内联脚本的严格 CSP、防嵌套/防索引/权限策略，同源静态资源因 URL 未指纹化只缓存 5 分钟；
- 保守单证据自动召回：Memory / Card 各自通过最低分与 top-1 margin 门槛，跨域双命中也放弃；已知 Shared Line 可精确带入；
- 兼容的 `memoria_search` 统一视图：Memory / Card / Atlas 镜像的同模型语义命中跨域排序，全文降级结果明确分组；Atlas 只返回空间、来源链接与同步新鲜度，正文不落本地；
- Web 治理：可行动概览看板、按权限汇总的治理待办、7/30 天自动上下文质量、按 Agent 汇总的接入状态，以及成员、空间、Token、导入、索引、外部来源与有界手动同步、逐张发布审核、有界批量淘汰、内容浏览、重复候选、活动与审计；成员、Token、共享空间及单空间授权使用有界 keyset 分页，页面读模型不读取密码或 Token 哈希；管理员系统页实时显示 schema 一致性、数据关系巡检、实例运行时间、最近 5 分钟 HTTP 请求/错误/panic/延迟、MySQL 连接池压力、备份新鲜度、恢复证据边界与依赖降级；
- 管理员可生成租户隔离的可移植团队导出：一致性快照包含 Memory、Card、召回条件、标签、关联、空间权限与 Shared Line 全部历史，明确排除凭据、会话、embedding、审计与运行态；发布包内同一二进制可离线校验与语义预检，并把通过预检的数据原子导入一个已迁移且完全空的部署；接管管理员必须设置新密码，其余成员保持禁用，凭据不会跨环境迁移；
- Markdown 幂等摄取、来源变更与缺失检测；Atlas body-free 定时镜像在上游故障或连接配置暂时关闭时继续提供上一成功版本；
- 显式数据库迁移、停服前只读升级计划、启动期 schema fail-fast、独立 liveness/readiness、SIGTERM 摘流与优雅关闭、定时原子 MySQL 逻辑备份、新鲜度校验、隔离恢复演练和原子技术回执，以及区分二进制回滚/数据库恢复的 systemd、Supervisor、nginx 部署模板。技术回执不替代代表性 Memory、Card、Shared Line 人工抽查或 RPO/RTO 验收。
- 可选的离线 Ed25519 发布签名：发布机只读 0600 私钥，对外层 SHA-256 companion 做 detached signature；部署机用预先固定的公钥先认证发布者和校验和，再验证归档字节。私钥、公钥信任根和轮换策略不进入发布包。
- 发布归档由仓库内写入器确定性装配：固定路径顺序、tar/gzip 时间、owner、权限与压缩参数，相同目录内容即生成相同 `.tar.gz` 字节；符号链接或特殊文件会阻断归档。正式二进制还会在同一发布主机上用两个独立空 Go 构建缓存各编译一次，字节不一致就阻断，并生成 `BUILD_REPRODUCIBILITY.json`。这只证明同主机、同工具链、同输入的有界重复构建，不承诺 hermetic 或跨主机可复现；实时漏洞库、工具链或源码变化仍会改变包内内容。
- 每个发布包包含 `BUILD_PROVENANCE.intoto.json`：使用 in-toto Statement v1 与 SLSA provenance v1 predicate，把最终 Linux 二进制绑定到版本、目标、Go 工具链、源码/构建输入清单及 8 份供应链回执（含有界可复现性回执）；同一二进制可在不加载配置或依赖的情况下离线验证。该本地 builder 不是托管或加固构建平台，项目不据此宣称 SLSA Build Level。

## 快速开始

要求：Go 1.26.6+、MySQL 8.0+。Embedding 和 Redis 都是可选依赖；禁用 embedding 时系统退化为全文检索，不影响基本读写。

```bash
cp .env.example .env
# 编辑 .env，至少填写 CCS_MYSQL_HOST / USER / PASSWORD / DATABASE
make migrate
```

配置采用严格解析：类型错误、危险范围和损坏的 `.env` 会阻止启动；生产环境另外强制 Secure Cookie、HTTPS `CCS_PUBLIC_MCP_URL` 与远程依赖的身份验证传输，避免“配置看似生效、实际退回默认值”。同机 loopback MySQL/Redis/Embedding/Atlas 可保持明文；远程 MySQL/Redis 使用 `verify-full`，远程 HTTP 依赖使用 HTTPS。

首次安装要在空数据库创建租户和管理员。密码必须放在权限为 `0600` 的绝对路径文件中：

```bash
umask 077
openssl rand -base64 24 > /tmp/claracore-initial-password
go run ./cmd/server -bootstrap \
  -bootstrap-tenant 'ClaraCore 团队' \
  -bootstrap-team-space '团队知识' \
  -bootstrap-admin admin \
  -bootstrap-password-file /tmp/claracore-initial-password
go run ./cmd/server
```

默认 Web 地址为 `http://127.0.0.1:51890/`，MCP 地址为 `http://127.0.0.1:51890/mcp`。设置 `CCS_METRICS_ADDR=127.0.0.1:51892` 后，本机监控可抓取 `http://127.0.0.1:51892/metrics`；该监听只接受数字 loopback 地址。管理员首次登录必须修改密码。完成交接后删除临时密码文件。

管理员可在“团队导出”生成机器可读 ZIP。它是数据可移植与空部署迁移格式，不是 MySQL 恢复镜像；默认单次上限由 `CCS_EXPORT_MAX_BYTES=1073741824` 控制，完整生成期间使用 0600 临时文件，同一实例只允许一个导出任务。迁移前先运行 `-verify-export` 与 `-plan-import`；实际 `-import-export` 只接受空部署，并要求 0600 密码文件和显式确认。配置了 embedding 时，导入事务会为当前模型原子排入 Memory、Card 与召回条件索引，不在迁移命令中调用外部模型；禁用时回执会明确要求日后回填。如果提交后终端回执丢失，在启动服务前用 `-check-import -import-admin <账号>` 只读重建纯净导入收据，不要盲目重跑导入。完整命令与接管边界见 `docs/07-deployment.md`。

MCP 客户端配置示例：

```json
{
  "mcpServers": {
    "claracore": {
      "type": "http",
      "url": "https://claracore.example.com/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

不要把 Token 提交进仓库、截图或问题报告。生产必须使用 HTTPS，并保持 `CCS_WEB_SECURE_COOKIE=true`。

## 验证

```bash
make check   # 格式、vet、架构约束、覆盖率与全部普通测试
make itest   # 需要已经完成迁移的真实 MySQL
go run ./cmd/server -version # 不读取配置或依赖的版本/源码与二进制摘要/平台 JSON
go run ./cmd/server -verify-provenance /absolute/release/BUILD_PROVENANCE.intoto.json # 发布目录离线来源证据
go run ./cmd/server -check-upgrade # 只读判断是否需要迁移维护窗口及数据库恢复型回退
go run ./cmd/server -check  # 只读验证配置、MySQL 与 schema，不启动服务或后台 worker
go run ./cmd/server -check-integrity # 一致性只读快照检查关键跨表引用；违规时仍输出 JSON 并返回非零
# 多实例目标环境：为每个进程设置唯一 CCS_INSTANCE_ID，再运行发布包内 verify-cluster.sh
```

领域层覆盖率门槛是 85%。迁移、SQL 或权限路径发生变化时，普通测试通过还不够，必须运行 `make itest`。

## 文档

- [架构](docs/01-architecture.md)
- [数据模型](docs/02-data-model.md)
- [MCP 合约](docs/03-mcp-contract.md)
- [Web 治理范围](docs/04-web-scope.md)
- [代码结构与测试纪律](docs/05-code-structure.md)
- [部署与回滚](docs/07-deployment.md)
- [使用与验收](docs/08-usage-guide.md)
- [发布 Provenance](docs/09-release-provenance.md)
- [贡献指南](CONTRIBUTING.md)
- [安全策略](SECURITY.md)

## 已知边界

- 当前已验收 Linux 单实例；正式打包会把应用源码树 SHA-256 嵌入二进制，进程启动时再计算实际可执行文件 SHA-256，两者通过 `-version`、探针、本机指标与管理员系统页暴露。单实例部署脚本会核对 live/ready 身份一致；多实例用公开安全的 `CCS_INSTANCE_ID` 区分响应，`verify-cluster.sh` 会逐实例核对版本、源码/二进制摘要、环境与 schema，并采样负载均衡入口覆盖；生产开发构建或混合制品会失败。它不触发故障或摘流；两台 Linux、真实网络分区、负载均衡摘流与滚动发布仍需部署侧验收；
- 发布脚本与验证器已经支持 Ed25519 detached signature，但仓库不携带生产私钥或替维护者决定公钥信任根；首次生产发布前仍需离线生成并妥善保管私钥，通过独立可信渠道固定公钥指纹，并演练轮换。
- 自动召回阈值按当前 bge-m3 评估集校准，换模型后必须重跑评估；
- Web 重复治理只给人工候选，不自动合并或删除；
- `/livez` 只证明进程存活；`/readyz` 与兼容入口 `/healthz` 证明 MySQL 可达且迁移与当前二进制一致。构建、ready 与部署成功仍不等于业务验收，发布后必须用真实身份和空间做读写验证。

## License

[Apache License 2.0](LICENSE)
