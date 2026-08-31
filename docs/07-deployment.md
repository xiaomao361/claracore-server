# 单实例部署与回滚

当前支持形态是 Linux 单实例服务（systemd 或 Supervisor），MySQL 外置，Redis 与 Embedding 可选。自动上下文不运行第二个大模型，只对 embedding 结果做确定性预筛，再由终端 Agent 的模型最终判断。部署包不含任何真实凭据；配置独立放在环境文件中。

推荐固定目录为 `/opt/claracore-server`，配置模板见 `deploy/`。凭据只放 owner-readable 的环境文件（0600），不要写进 systemd、Supervisor 或 nginx 配置。

配置采用 fail-fast：非空整数、布尔值和小数必须能严格解析并落在安全范围内，损坏或重复的 `.env` 行会带行号报错，服务不会因拼写错误静默使用默认值。`CCS_ENV=production` 时必须启用 `CCS_WEB_SECURE_COOKIE=true`，且 `CCS_PUBLIC_MCP_URL` 必须是完整 HTTPS 地址。非 loopback MySQL/Redis 还必须把对应 `CCS_*_TLS_MODE` 设为 `verify-full`；非 loopback Embedding/Atlas URL 必须使用 HTTPS。普通服务启动还会只读核对内置迁移与 `schema_migrations`：缺少迁移、已发布迁移 checksum 漂移，或数据库存在当前旧二进制不认识的新版本时都会拒绝启动。迁移仍只能显式执行 `claracore-server -migrate`，不会由普通实例偷偷改表。

MySQL 与 Redis TLS 只支持 `disabled` 和 `verify-full`，不提供 skip-verify、仅加密不验身份或明文 fallback。`verify-full` 最低为 TLS 1.2，默认使用系统信任根与目标 host 做身份验证；私有 PKI 可配置绝对路径 `*_TLS_CA_FILE`，证书名称与连接地址不同时可显式设置 `*_TLS_SERVER_NAME`。可选 mTLS 的 `*_TLS_CERT_FILE` / `*_TLS_KEY_FILE` 必须成对出现；私钥必须是 owner-only 普通非符号链接文件，CA/证书不得被 group/other 写入，每个 PEM 最大 1 MiB。应用系统页只展示传输形态，不展示主机、证书路径或服务端名称。

同机依赖可继续使用 loopback 明文；这不是对任意内网地址的豁免。生产配置把 MySQL/Redis 改到远程地址却未同时启用验证 TLS，或者把 Embedding/Atlas 改成非 loopback HTTP，`-check-upgrade`、`-check`、迁移和普通启动都会在连接前拒绝。备份/恢复 CLI 使用独立的 0600 client file；连接远程 MySQL 时也要按 `mysql-client.cnf.example` 启用 `ssl-mode=VERIFY_IDENTITY` 和受信任 CA。

`claracore-server -check` 是无副作用发布预检：它只加载并严格校验生产配置、连接 MySQL、核对全部迁移版本/checksum，然后向 stdout 输出不含连接信息的 JSON 并退出；不会监听端口、连接 Redis/Embedding 或启动清理与索引 worker。把它放在迁移完成、切换 release 之前，能在旧实例仍服务时发现新二进制不兼容。`-check` 不能与 `-migrate`、`-bootstrap` 或 `-ingest` 混用。

`claracore-server -check-integrity` 是独立的数据语义门禁。迁移有意不建外键，正常写入由领域层保证引用关系；这条命令用于发现绕过应用写入、异常恢复或历史缺陷留下的悬空引用与跨租户关系。它先严格校验配置和当前 schema，再在一个 repeatable-read 只读快照中执行二进制内固定的 29 条规则，覆盖治理身份/空间权限、Memory/Card/召回/标签/关联/索引、Shared Line 及外部镜像的关键引用。stdout 只含检查时间、schema、快照模式、规则数和违规规则计数，不含实体 ID、账号、标题、正文或连接信息；健康时 `status=verified` 且退出 0，发现违规时 `status=violations_found`，仍先输出 JSON 再退出非 0。它不连接 Redis/Embedding，不启动 listener/worker，也不修复或删除数据。

生产至少在备份之后、切换 release 之前运行一次；恢复演练和周期巡检也应运行。设置 `CCS_INTEGRITY_STATUS_FILE` 后，健康或发现违规都会用同目录 0600 临时文件、`fsync` 和原子替换发布 `claracore-data-integrity/v1` 回执；基础设施、schema、SQL 或发布失败不会伪造新结果，而是保留旧回执并让它过期。`CCS_INTEGRITY_MAX_AGE_SEC` 默认 90000 秒，覆盖每日巡检与随机延迟。普通服务只严格读取最大 16 KiB、当前 schema 的普通非符号链接回执，系统页与指标请求不会重跑 29 条 SQL。出现违规时保留 JSON、对应备份与服务日志，在另行授权的调查中按规则定位，不能靠删除行让门禁变绿。`-check-integrity` 证明的是固定规则在同一数据库快照内没有发现违规，不替代 MySQL `CHECK TABLE`、备份可恢复性、内容语义抽查或业务验收。

既有安装在停止服务前先运行 `claracore-server -check-upgrade`。它同样只读，但允许数据库是当前二进制迁移集合的连续可信前缀：逐个核对已应用 checksum，拒绝未知版本和“后面的版本已应用、前面的版本缺失”的账本空洞，再输出当前/目标 schema、pending 数、下一迁移、`requiresSchemaMaintenance` 与 `rollbackMode`。`pending=0` 表示本次没有数据库变化，可在多实例拓扑中做逐实例二进制滚动；`pending>0` 会明确返回 `requiresSchemaMaintenance=true`、`rollbackMode=restore_required`。这不是迁移试跑，也不证明 SQL 能成功执行；它解决的是在停服务前发现错误发布包、checksum 漂移和回滚边界。

`0012_governance_list_indexes.sql` 为成员 `(tenant_id,id)`、Token `(tenant_id,user_id,id)` 与共享空间 `(tenant_id,id)` 补齐 keyset 分页访问路径。升级到含该迁移的版本前仍按上述规则停写、备份、显式迁移；迁移后的集成门禁会核对三组索引的完整列顺序，并对真实 MySQL 验证租户隔离、单空间授权和 Token 所有权。

`0013_session_auth_version.sql` 为用户与 Web 会话增加认证版本。密码变更、管理员重置或账号转为停用会推进用户版本；恢复会话时必须与用户当前版本相等。这补上了“重置已删除旧会话，但一个先前已验过旧密码的并发登录随后才插入”的吊销窗口。该迁移仍是共享 schema 变更，生产必须停止所有 writer、完成可恢复备份后显式执行，不能用旧二进制继续接流量。

收到 SIGTERM 后，实例先把 `/readyz` 切成 `503 draining` 并拒绝新 Web/MCP 请求，`/livez` 继续返回 200；等待 `CCS_SHUTDOWN_DRAIN_SEC`（默认 5 秒、允许 0–60 秒）给负载均衡摘流后，才关闭 listener 并等待已进入 handler 的请求完成。systemd 与 Supervisor 模板的 30 秒停止预算覆盖默认 5 秒摘流加 15 秒请求关闭；如果提高摘流窗口，也要同步提高管理器超时。

多实例部署必须为每个进程配置唯一的 `CCS_INSTANCE_ID`，例如 `ccs-a`、`ccs-b`。该值会公开出现在 `/livez`、`/readyz` 与兼容 `/healthz` 的 `instanceId`，只允许 1–64 字节的字母、数字、点、下划线和短横线，且首字符必须是字母或数字。它只应是无业务含义的运维标签，不要放主机名、内网拓扑、租户、凭据或其他敏感信息。单实例可留空，此时响应省略该字段。

生产通过 HTTPS nginx vhost 提供 Web 与 MCP，应用自身只监听 `127.0.0.1:51890`。模板见 `deploy/claracore-server.nginx.conf`；替换示例域名、接入证书后，必须先执行 `nginx -t`，通过后才能 reload。

应用 listener 本身限制 10 秒请求头、30 秒完整读取、300 秒响应写入、120 秒空闲连接和 64 KiB 请求头；Web 表单默认最多 1 MiB，MCP SDK 请求体默认最多 4 MiB。对应变量是 `CCS_HTTP_*`、`CCS_WEB_MAX_FORM_BYTES` 与 `CCS_MCP_MAX_BODY_BYTES`，越界配置会拒绝启动。Web 写入只接受 URL-encoded form，超限返回 413，错误媒体类型返回 415；MCP 的限制同时覆盖已知长度与流式请求。nginx 模板镜像相同 body 和慢请求边界以便尽早拒绝，但应用限制才是 loopback 直连和反代误配时仍有效的最终防线。提高 embedding timeout 时，write timeout 必须不小于它；提高应用边界时也要同步检查 nginx 与服务管理器预算，不能只改一层。

MCP 有三层固定窗口保护：认证前按可信来源计数，预算是 agent 值的 6 倍，用于限制随机无效 token 对 MySQL 的查询；认证成功、身份进入服务端 context 后先按 agent 计数，默认每个 agent 每分钟 300 次，再按 tenant 计数，默认每个团队合计每分钟 3000 次。同一 agent 的多个 token 共用 agent 预算，同一租户新增 agent 仍共用聚合预算，达到任一上限返回 429 与向上取整的 `Retry-After`。先检查 agent，避免一个已经越界的 agent 继续消耗团队预算。限流 key 仅持有来源、agent ID 或 tenant ID 的 SHA-256，不持有 token、用户、租户或 agent 文本；随机 token 只落入同一个来源 bucket，不会制造无限 key。来源识别与登录相同：只在直连 peer 为 loopback 时信任 nginx 覆盖的 `X-Real-IP`，否则使用真实 peer 地址。配置 Redis 时多个实例共享全部预算，Redis 不可用时退回每进程有界保护。对应变量为 `CCS_MCP_RATE_LIMIT`、`CCS_MCP_TENANT_RATE_LIMIT` 与 `CCS_MCP_RATE_WINDOW_SEC`；租户值不得小于 agent 值，配置校验不允许关闭。这个边界限制泄漏凭据、失控 agent 或大量 agent 的总调用成本，不是内容存储、成员数或计费配额。

每个进入应用的请求都会覆盖任何同名入站值，生成新的 `X-Request-ID` 响应头，并向 stderr 写一条 `slog` 完成记录：`request_id`、HTTP method、ServeMux 的受控 route pattern、status、response bytes 与 duration。成功健康探针使用 debug 级别，429 使用 warn，5xx 或 panic 使用 error；其他请求使用 info。记录不包含 URL query、原始未匹配路径、Authorization、Cookie、表单/MCP 正文、用户身份或 panic 值。未提交响应时的 panic 被转换为不含细节的 500；已经开始流式响应时则写脱敏 stack 后以 `http.ErrAbortHandler` 让 `net/http` 关闭连接，避免发送伪完整响应或由默认 panic 日志泄露任意值。journal 访问仍应只授予运维人员，排障时让调用方提供 `X-Request-ID`，不要索要 token 或请求正文。

应用负责内容级响应策略：SSR、重定向和 Web 错误均为 `Cache-Control: no-store, private`，MCP 的 200/4xx/协议错误在真正提交响应头时强制为 `no-store, no-transform`，避免 SDK 后写入的 `no-cache` 允许代理保存团队知识。健康探针同样 no-store。静态 CSS/JS 因 URL 未做内容指纹只缓存 300 秒；升级后最多等待该窗口，不要在 nginx 另加长期 immutable cache。Web 同时返回严格 CSP、DENY frame fallback、no-referrer、权限/跨源策略、nosniff 与 noindex。nginx 模板关闭版本号并只承担 TLS/HSTS；应用头仍需通过公开 HTTPS 地址实际复核，不能只看模板。

Web 登录在 Argon2 密码校验前执行固定窗口限流：默认每个“客户端 IP + 账号”15 分钟 10 次，同一 IP 总计 60 次；达到上限返回 429 与 `Retry-After`。计数 key 只保存 SHA-256，不把账号或地址写入 Redis。配置 Redis 时多个实例共享计数，Redis 不可用时退回每进程最多 20,000 个 key 的有界本地保护。`CCS_LOGIN_RATE_LIMIT` 与 `CCS_LOGIN_RATE_WINDOW_SEC` 可调整，但配置校验不允许关闭保护。登录与 MCP 使用独立的本地容量池，MCP agent 洪泛不会挤掉新的登录 key。

调整 `CCS_ARGON2_*` 后，新建、重置和主动改密立即使用新参数；已有 PHC 哈希在下次成功登录时渐进升级，但只接受 memory、iterations、parallelism、盐长度和输出长度逐项不下降的变化。升级 SQL 同时匹配旧哈希与 `auth_version`，不会覆盖并发重置；另一个实例已经完成等价升级时会重新验证后继续。更强或参数互有高低的旧哈希保持不变，系统不会自动降级。这个机制不保存明文，也不做离线批量重写；调低参数或改变参数取舍时必须由安全评审明确处理。

应用只在直接连接来自 loopback 时信任 `X-Real-IP`。nginx 必须显式设置 `proxy_set_header X-Real-IP $remote_addr`；非 loopback 客户端自行伪造的转发头会被忽略。若省略这行，所有经本机 nginx 的用户会共享 loopback 的 IP 总上限。

## 1. 构建发布包

在开发机先跑完整门槛，再按服务器架构打包：

```bash
make check
make itest
./scripts/package-release.sh 0.2.0 amd64 dist /secure/offline/claracore-release-ed25519.pem
# arm64 服务器把第二个位置参数改为 arm64
```

第 4 个参数是可选的 Ed25519 私钥绝对路径；开发包可省略，但生产发布必须提供。私钥必须由当前用户拥有、不是符号链接，且不能授予 group/other 任何权限。脚本只用它签署外层 `.sha256` companion，不把私钥或公钥复制进发布包。首次生产发布前在离线或受控发布机生成密钥，并通过与归档分离的组织可信渠道固定公钥 DER 指纹：

```bash
umask 077
openssl genpkey -algorithm ED25519 -out /secure/offline/claracore-release-ed25519.pem
openssl pkey -in /secure/offline/claracore-release-ed25519.pem -pubout \
  -out /secure/offline/claracore-release-ed25519.pub.pem
openssl pkey -pubin -in /secure/offline/claracore-release-ed25519.pub.pem -outform DER | sha256sum
```

不要把私钥提交到 Git、放进 release 目录、同步到部署机或写入 CI 日志。公钥可以公开，但“与未知归档放在同一个下载目录”不构成信任；部署机必须预先固定公钥或由独立管理渠道核对其指纹。轮换时先分发并固定新公钥，再发布第一份新密钥签名的制品；仍支持旧 release 时保留旧公钥。

签名发布会在 `dist/` 生成 `.tar.gz`、`.tar.gz.sha256` 与 64 字节 `.tar.gz.sha256.sig`。先使用可信来源取得的验证脚本和预先固定的公钥认证 detached signature，再解压；包内还有逐文件 `MANIFEST.sha256` 与校验器，用于确认解包目录没有缺失、篡改、符号链接替换或额外文件：

```bash
./scripts/verify-release-signature.sh \
  dist/claracore-server_0.2.0_linux_amd64.tar.gz \
  dist/claracore-server_0.2.0_linux_amd64.tar.gz.sha256 \
  dist/claracore-server_0.2.0_linux_amd64.tar.gz.sha256.sig \
  /etc/claracore-server/trusted-release-ed25519.pub.pem
tar -C dist -xzf dist/claracore-server_0.2.0_linux_amd64.tar.gz
cd dist/claracore-server_0.2.0_linux_amd64
./verify-release.sh
./claracore-server -version
sha256sum claracore-server
./claracore-server -verify-provenance "$(pwd)/BUILD_PROVENANCE.intoto.json"
```

最终 `.tar.gz` 不再交给宿主机 `tar` 的默认元数据决定。仓库内 `scripts/release-archive` 按字典序写入目录，统一目录为 `0755`、可执行文件为 `0755`、其他文件为 `0644`，并固定 tar/gzip 时间、uid/gid、用户名、组名、PAX 格式和 gzip 参数；源目录是相对路径、符号链接或含特殊文件时会失败。自动门禁会把真实发布包解开后重新装配并逐字节比较，再改变整棵目录 mtime 重复比较。因此，只要**进入归档的目录名、文件内容和可执行位相同**，归档装配字节就相同，外层 SHA 与签名不会因 staging 时间漂移。正式二进制本身还会固定 Go 1.26.6、清空 GOENV/GOFLAGS/workspace、禁用 VCS stamp，并在两个新建且分离的 Go build cache 下各编译一次；两份字节不一致就阻断，匹配结果写入 `BUILD_REPRODUCIBILITY.json`。这个结论只证明同一发布主机、同一工具链与同一输入的有界重复构建：模块缓存、工具链分发、宿主 OS/文件系统仍在信任边界内，源码、实时漏洞库、许可证/SBOM/Prometheus 工具输出变化也会合理改变包内内容；它不是 hermetic、独立重建者、跨主机 reproducible-build 或 SLSA 声明。

发布包还保存 `SOURCE_MANIFEST.sha256`、`BUILD_INPUTS.sha256`、`BUILD_REPRODUCIBILITY.json` 与 `BUILD_PROVENANCE.intoto.json`。前两份清单分别展开已链接进二进制的应用源码集合和本地发布器/部署模板/文档等装配输入；两者在编译前固定，长耗时门禁结束后源码逐文件复读、provenance 生成时构建输入重新计算，任一漂移都会阻断混合输入发布。可复现性回执固定版本、linux/架构、Go、sourceDigest、CGO/trimpath/buildVCS 和主/重建二进制 SHA，并由离线校验器把两份 SHA 重新绑定到当前发布二进制。provenance 使用 `https://in-toto.io/Statement/v1` 与 `https://slsa.dev/provenance/v1`，subject 是最终 `claracore-server` 二进制，`buildDefinition` 固定版本、linux/架构、Go 1.26.6、CGO/trimpath、sourceDigest 及两份输入清单，`runDetails` 固定本地 builder 身份并绑定 BUILDINFO、有界可复现性、漏洞、许可证、SBOM 与 Prometheus 共 8 份回执。生成后立即由独立路径重算全部 SHA；真实发布测试还会篡改可复现性/subject SHA 证明拒绝。目标机用同一发布二进制的 `-verify-provenance` 校验，动作发生在 `.env` 与外部依赖加载之前，provenance 必须使用发布目录内的绝对普通文件路径。

provenance 自身进入 `MANIFEST.sha256`；manifest 进入确定性归档，外层 SHA 和预先固定公钥验证过的 Ed25519 signature 间接认证整条链。它不是 DSSE envelope。builder 是维护者控制主机上的本地 shell 流程，信任边界仍包含主机、OS、网络、实时漏洞库、工具链和操作者；没有托管、隔离或加固构建平台证据，因此项目不声明任何 SLSA Build Level。字段解释与 build type 见 `PROVENANCE.md` / `docs/09-release-provenance.md`。

`package-release.sh` 会按稳定路径顺序散列 `go.mod`、`go.sum` 以及 `cmd/`、`internal/`、`migrations/` 下参与应用发布的 Go、SQL 与内嵌 Web 资源，把 64 位十六进制 `sourceDigest` 链入二进制。它包含当前工作树中的相关未提交文件，不会拿 Git HEAD 冒充实际构建内容。每次进程启动还会只读计算当前可执行文件字节的 SHA-256 `binaryDigest`；启动时无法读取自身制品会 fail-fast。`-version` 输出应用版本、源码摘要、二进制摘要、Go 工具链与目标 `GOOS/GOARCH` 的 JSON，不加载 `.env`、不连接 MySQL/Redis/Embedding，也不启动 listener 或 worker。输出的 `binaryDigest` 必须与目标主机 `sha256sum claracore-server` 一致。`-check-upgrade`、`-check`、三个探针、本机 `claracore_build_info` 与管理员系统页也暴露同一组身份，既识别同版本异源码，也识别同源码但编译器、参数或制品字节不同的实例。

源码进入完整门禁时会由固定版本 `govulncheck` 按可达符号读取 Go 官方实时漏洞库；数据库不可达或存在可达漏洞均失败。正式打包在 Linux 交叉编译后再次扫描**最终二进制**，任一发现都会在归档发布前中止。制品不使用 `-s -w` 剥离：除了保留事后诊断信息，也避免二进制扫描失去调用关系后把仅存在于依赖模块、实际不可达的符号误报为制品路径。归档中的 `BUILDINFO.txt` 记录实际 Go 工具链、模块版本与校验和，`VULNERABILITY_SCAN.txt` 记录扫描器、漏洞库更新时间和结果；二者都进入内层 manifest 并由外层签名间接绑定。扫描回执只证明该漏洞库快照下没有已知可达项，部署或长期支持期间仍需重新扫描当前制品。

许可证门禁使用固定 `go-licenses v2.0.1`，分别按 `CGO_ENABLED=0` 的 Linux amd64 与 arm64 运行时依赖图检查；未知或工具判定禁止的许可证会失败。正式包针对实际目标架构生成 `THIRD_PARTY_LICENSES.csv` 和 `THIRD_PARTY_REVIEW.txt`，并在 `third_party_licenses/` 保存许可证、NOTICE 及 reciprocal 许可证要求的材料。当前 MySQL 驱动为 MPL-2.0，因此目录中必须同时存在它的 LICENSE 和对应 v1.10.0 源码；缺任一项不发布。纯汇编 `.s` 文件会形成不含本机绝对路径的显式复核清单，出现 C、预编译对象或其他 native 文件则 fail closed 等待人工判断。自动分类与材料收集不是法律意见；新增 reciprocal、restricted、unknown 或 native 依赖仍需维护者复核义务。

SBOM 门禁使用固定 `cyclonedx-gomod v1.10.0` 的 binary 模式读取**同一个最终 Linux 二进制**，生成 `SBOM.cdx.json` 与 `SBOM_REVIEW.txt`。生成进程显式设置目标 `GOOS=linux`、`GOARCH` 与 `CGO_ENABLED=0`，避免把发布机的 macOS/架构错误写入 Package URL；输出固定为 CycloneDX 1.6，并省略随机 serial number 与生成时间。独立 Go 校验器不执行目标二进制，而是读取其 build info，逐项核对发布版本、二进制 SHA-256、目标 PURL、链接模块及版本、Go 标准库、主组件依赖图与 composition；缺组件、多组件、错误平台、路径泄漏或哈希不一致都会在归档前失败。SBOM 是构建时库存快照，不说明组件当前无漏洞、不裁定许可证义务、不证明构建可复现，也不认证发布者；这些边界分别由漏洞回执、许可证材料、源码/制品身份和 Ed25519 信任链承担。

源码摘要证明发布脚本看到的应用源码集合一致；二进制摘要证明当前进程实际执行的文件字节，并可与发布清单和主机 `sha256sum` 交叉核对。二进制摘要不同只证明制品不同，不自动解释差异原因。provenance 让“该签名发布者声明如何得到这个二进制”可机器核对，但不能独立证明 builder 的声明真实；确定性归档只消除相同 staging 内容的元数据/压缩漂移。Ed25519 signature 在部署机使用预先固定公钥验证成功后，才把发布者身份绑定到外层校验和；随后 SHA-256 绑定归档字节，内层 `verify-release.sh` 再绑定解包文件集合。任一层失败都不得切换 release。签名、provenance 与确定性装配仍不证明完整构建过程可复现，也不替代源码审计、密钥保管或部署后业务验收。

自动上下文用 `CCS_AUTO_CONTEXT_MIN_SCORE` 与 `CCS_AUTO_CONTEXT_MIN_MARGIN` 同时控制 Memory / Card 的确定性 Stage B。默认 `0.55 / 0.03` 按 bge-m3 项目评估集校准；替换 embedding 模型后必须重跑两域检索与自动上下文评估再调整，不能照搬阈值。无语义证据、域内/跨域歧义或请求超过 15 秒总上限时放弃，不影响显式 `memoria_search` / `card_recall`。显式 `lineId` 的 Shared Line 精确路径不使用这组语义阈值。

`make itest` 对这组默认值执行真实 MySQL + embedding 质量门禁：四类知识卡在同一共享语料中竞争时都必须交付标注卡，无关问题必须低相关拒答；重复知识卡和重复 Memory 必须以域内 top-2 歧义拒答；真 token → MCP → 权限解析的完整链路还必须在 Memory 与 Card 同时高置信时跨域拒答，且不生成证据块或 `contextId`。这组门禁证明固定样本上的确定性预筛，不等于线上准确率；真实 `wrong` / `ignored` 反馈仍需进入评估集后再讨论阈值。

单实例默认启用进程内 embedding LRU：`CCS_EMBEDDING_CACHE_ENTRIES=1024`、`CCS_EMBEDDING_CACHE_TTL_SEC=86400`。key 包含 model 与原文，替换模型不会命中旧向量；设 entries 为 0 可关闭。配置 `CCS_REDIS_ADDR` 后，同一个 decorator 会在本地 LRU 未命中时读写 Redis，供多实例和进程重启后共享；索引 worker 还会按模型取得 token+TTL 短租约，避免多个实例同时领取并计算同一批。租约覆盖两倍 embedding 超时再加写回余量，释放时校验 token，不会由旧 worker 误删新租约。`CCS_REDIS_TIMEOUT_MS` 默认 500ms；缓存故障按 miss 回退，租约故障按单实例语义 fail-open，不中断检索或索引，最坏只会临时重复一次幂等计算。`server_status.embeddingCache` 暴露合并后的 hits/misses，`/admin/system` 显示 Redis 实时连通状态。Redis 仍不是启动依赖。

Atlas 外部活源默认关闭。启用时填写 `CCS_ATLAS_MCP_URL`、`CCS_ATLAS_SYSTEM`、
`CCS_ATLAS_SOURCE_NAME` 与目标 `CCS_ATLAS_SPACE_ID`，并保持 embedding 启用；可选服务 token
只放在 0600 环境文件中，绝不写入 MySQL。production 中一旦配置 token，Atlas URL 必须为
HTTPS。worker 启动即同步，之后按 `CCS_ATLAS_SYNC_INTERVAL_SEC` 重跑；
`CCS_ATLAS_SYNC_TIMEOUT_SEC` 是整轮硬上限，`CCS_ATLAS_TIMEOUT_SEC` 是单次 MCP 请求上限。
数据库只保存来源元数据、同步回执和派生向量，不保存正文。完整快照成功才把本轮未见文档
标为 missing；超时、上游失败、分页总数漂移或正文版本竞态均保留上次可用镜像。同步所有权
由 MySQL 有期限租约串行化，不依赖可选 Redis，避免多实例快照互相误删。

管理员可在 `/admin/sources` 查看最近 20 次 body-free 运行回执并请求“开始同步”。HTTP 请求经过权限、CSRF 与租户校验后，把请求持久写入 `external_sources`，不等待上游枚举；重复提交只保留一轮 pending，当前 claim 处理期间仍可保留一轮 follow-up。任一实例每 5 秒检查持久请求，同实例另有有界 channel 只做即时唤醒。领取本身带有期限和 owner token：实例退出、未取得完整快照租约或状态写回失败时，请求可由其他实例重新领取。定时与手动请求仍共用同一个 worker，完整快照继续以独立 MySQL run 租约为最终所有权门禁。页面只承诺“同步请求已记录”，成功与否必须以随后出现的运行回执为准。

Atlas 连接配置暂时关闭不会删除既有镜像。只要 embedding 模型标识仍与镜像向量一致，
`memoria_search.matches` 会以 `externalStatus=mirror_only` 继续返回上一成功版本；重新启用连接并
完成同步后恢复为 `available`。模型切换但尚未重建外部向量时不会跨模型比较，结果会明确
`externalSemanticAvailable=false`，不能把空结果解释成来源没有内容。

`make itest` 包含双 worker 装配门禁：两个独立 Redis client 对同一真实 MySQL pending 队列竞争时只允许一次 embedding；注入 Redis 命令故障后，两边都会 fail-open，同一行并发写回仍收敛为一条 `ready`。这证明代码级协调与降级语义，不等于两台 Linux 实例、真实网络分区或滚动发布已验收；生产扩容前仍要完成下述实机检查。

## 2. 首次安装

建议目录：

```text
/opt/claracore-server/releases/<version>/claracore-server
/opt/claracore-server/current -> releases/<version>
/etc/claracore-server/claracore-server.env
/var/lib/claracore-server/
/srv/claracore-ingest/                 # 可选，只读导入根目录
```

创建专用系统用户，把示例配置复制到 `/etc/claracore-server/claracore-server.env`，填入真实连接信息，并将文件权限设为 `0600`。安装 `deploy/claracore-server.service` 后再启用服务。服务进程不需要 root 权限。

迁移完成后、首次启动服务前，用一个仅所有者可读的临时文件初始化首个租户与管理员：

```bash
umask 077
openssl rand -base64 24 > /tmp/claracore-initial-password
./claracore-server -bootstrap \
  -bootstrap-tenant 'ClaraCore 团队' \
  -bootstrap-team-space '团队记忆' \
  -bootstrap-admin admin \
  -bootstrap-admin-display '管理员' \
  -bootstrap-password-file /tmp/claracore-initial-password
```

该命令只允许在没有任何租户的数据库执行；重复执行会拒绝，不会创建第二套数据。密码文件必须是绝对路径、普通文件且不能授予组或其他用户权限。把临时密码交给管理员后应删除文件；首次登录会强制修改密码。

Web 导入只接受 `CCS_INGEST_ROOT` 下的相对路径，并同时校验符号链接真实路径；留空即关闭入口。systemd 用户需要该目录的读取权限，不需要写权限。

### 2.1 团队数据可移植导出

管理员“团队导出”不是数据库管理入口。提交前必须经过登录、已改初始密码、admin、CSRF 与显式确认；应用会先持久写入 `admin.export.download` 的 meta-audit，审计不可用时在读取第一条内容前拒绝导出。随后用 MySQL repeatable-read 只读事务生成同一快照内的有序 JSONL：成员公开身份、Agent、空间与权限、Memory、Card、召回条件、标签、关联、Shared Line 当前位置、位置历史与交接历史。密码/Token 哈希、会话、embedding、审计日志、外部镜像与运行队列固定排除，并在 `manifest.json` 中明示。

导出不会直接向浏览器流式拼半个文件。当前实例先在系统临时目录创建 0600 文件，逐文件计算 SHA-256，全部查询与最终 manifest、ZIP central directory 成功并落盘后才发送带精确 `Content-Length` 的下载；失败会删除临时文件并返回页面错误。同一实例一次只生成一份，默认上限 `CCS_EXPORT_MAX_BYTES=1073741824`（允许 1 MiB–16 GiB）。多实例各有一个本机槽位，数据库事务仍保证每份归档内部一致；如果组织需要全局只允许一个导出，应在入口或运维流程额外串行化。导出生成和下载都受请求取消与 `CCS_HTTP_WRITE_TIMEOUT_SEC` 约束，大团队提高大小上限时要同步核对临时磁盘、MySQL 只读负载、应用/nginx 写超时与下载带宽。

下载完成后，用与服务同一发布包内的二进制执行离线校验；该动作在配置加载之前退出，不读取 `.env`，也不连接 MySQL、Redis 或 Embedding：

```bash
./claracore-server -verify-export /absolute/path/claracore-team-export.zip
```

成功时 stdout 只输出一行 JSON，包含格式版本、schema、租户 ID、生成时间、文件数、记录数、归档与解压字节数；失败时退出非 0。校验器严格要求 v1 的完整且唯一文件集合、最终 `manifest.json`、固定排除项、逐文件 SHA-256、JSONL 对象与记录数一致，并拒绝未知字段、尾随 JSON、路径穿越、符号链接、非普通文件、异常压缩方法及超过归档/单文件/总解压/单行/记录数预算的输入。它会完整读取每个 ZIP entry，因此同时触发 ZIP CRC 校验；无需再把手工 `unzip -t` 和逐文件散列作为标准流程。

这些内嵌哈希用于发现截断、意外损坏与格式漂移，没有独立签名。攻击者若能同时替换归档和 manifest 仍可重算，校验成功不能当成发布者认证或异机备份。下载后放到受控、加密且有访问审计的存储，不要附到公开 issue 或普通聊天。

#### 2.1.1 空部署团队迁移

先在离线环境执行语义预检。它复用已校验且保持打开的同一个归档文件，严格检查 13 个 JSONL 数据集的字段、角色、状态、唯一性以及成员、Agent、空间、Memory、Card、标签、关联和 Shared Line 历史之间的引用关系。两条命令都在配置加载前退出，不读取 `.env`，不连接外部依赖：

```bash
./claracore-server -plan-import /absolute/path/claracore-team-export.zip
./claracore-server -plan-import /absolute/path/claracore-team-export.zip -import-admin alice
```

不指定 `-import-admin` 时，JSON 只列出可接管的归档管理员候选；指定后会验证该账号确实是候选，并给出总记录数、将保持禁用的其他成员数和不迁移凭据等边界。计划成功不是写库，也不证明归档来源可信。

实际导入只支持一个已经执行当前版本迁移、但 `tenants` 表仍为空的新部署。先为接管管理员准备新的初始密码文件，再显式确认空部署接管：

```bash
umask 077
openssl rand -base64 24 > /secure/offline/claracore-import-admin-password
./claracore-server -import-export /absolute/path/claracore-team-export.zip \
  -import-admin alice \
  -import-password-file /secure/offline/claracore-import-admin-password \
  -confirm-import-empty
```

应用会先重做归档和语义预检，再加载配置，只连接 MySQL 并验证 schema 精确一致；它不连接 Redis 或 Embedding 服务，不启动 worker 或 HTTP listener。整个导入、当前配置模型的 pending 索引行以及一条 `admin.import.apply` CLI 治理审计位于同一个 serializable 事务：任何一条数据、索引队列或审计写入失败都会回滚，检测到任意既有租户会拒绝，不支持合并或覆盖已有部署。归档租户、成员、Agent、内容和 Shared Line 的 ID、作者及历史保持不变；归档租户在新部署中启用。模型名会进入持久队列身份，配置必须无首尾空白且不超过 64 个字符。

归档不含密码、Token、会话、embedding、旧审计、外部镜像或运行队列。指定的管理员会成为 active admin，使用新密码且首次登录必须修改；所有其他成员保留身份和角色但处于 disabled，并写入不可从归档恢复的随机凭据哈希。管理员必须在确认人员身份后逐个重置密码并重新启用，不能把源环境凭据当成已迁移。

导入成功 JSON 的索引字段给出精确后续动作：`indexingStatus=queued` 表示当前配置模型的可检索 Memory、active Card 和每条召回条件已经与正文同事务入队，`indexingQueued` 是待 worker 处理的行数，启动服务后等待索引页的 pending/failed 归零即可；迁移命令本身不会调用模型。`indexingStatus=backfill_required` 表示导入时 embedding 被禁用，日后启用模型后必须在索引页反复执行回填，直到没有新增和待处理项。`not_required` 表示归档没有需要语义索引的 active 内容。任一路径完成后都要分别抽查代表性 Memory、Card 召回和 Shared Line 历史。完成接管后删除临时密码文件。

导入事务可能已经提交，但 SSH、终端或 stdout 在 JSON 回执显示前断开。此时不能凭“没有看到成功”重跑：第二次必然遇到非空部署，而且人工清库再试会破坏可追溯性。保持 Server 尚未启动，使用相同配置和接管账号执行只读回执重建：

```bash
./claracore-server -check-import -import-admin alice
```

该命令只连接 MySQL 并精确核对当前 schema；它在 repeatable-read 只读快照中要求：恰好一个 active 租户、恰好一条匹配的 `admin.import.apply` 成功审计、接管管理员仍是 active/admin/必须改密、其他成员全部 disabled/必须改密、13 个数据集总数与审计一致、Token/Session/外部运行态及其他审计为零，并且索引模型、数量和 pending 形态与导入审计一致。成功 JSON 的 `status=verified` 才证明“导入已提交且尚未接受正常流量”的纯净窗口。

这不是常驻健康检查。管理员登录、创建 Token、worker 把 pending 变成 ready、外部同步或任何正常治理审计发生后，它都应拒绝，防止把变化后的数据库冒充原始导入收据。因此标准顺序是 `-import-export` → 立即保存 JSON → `-check-import` → 保存重建 JSON → 启动服务。若检查失败，保留数据库与日志调查，不要自动删除、覆盖或重复导入。

这条能力解决团队数据在新部署间的可移植迁移，不替代灾难恢复。需要保留完整运行态、旧审计或原环境凭据边界时，仍使用下一节的 MySQL 备份、隔离恢复演练与代表性语义抽查。

## 3. 备份与恢复演练

发布包包含 `backup-mysql.sh`、`verify-mysql-backup.sh`、`rehearse-mysql-restore.sh` 与它们的公共安全检查。前者只需要能执行逻辑导出的只读账号；后者应使用**隔离 MySQL 实例**上的独立账号，因为演练会创建并删除一个本次随机生成、名称以 `ccs_restore_` 开头的临时数据库。不要为了方便给生产备份账号增加 CREATE / DROP 权限。

先把 `deploy/mysql-client.cnf.example` 复制到 release 目录之外，分别填写备份端和隔离恢复端连接信息并设为 0600。密码只出现在这个文件中，不进命令行、日志、备份文件或校验和：

```bash
install -o claracore -g claracore -m 0600 deploy/mysql-client.cnf.example /etc/claracore-server/mysql-backup.cnf
install -d -o claracore -g claracore -m 0700 /var/backups/claracore-server

sudo -u claracore ./backup-mysql.sh \
  --defaults-file /etc/claracore-server/mysql-backup.cnf \
  --database claracore_server \
  --output-dir /var/backups/claracore-server
```

脚本使用 `--single-transaction`、routines、triggers、events 与 hex-blob 生成压缩逻辑备份。同一输出目录用原子目录锁拒绝重叠任务；半成品只使用隐藏临时文件，gzip 与 SHA-256 均成功后，`.sha256`、`.metadata` 先就绪，`.sql.gz` 最后以不覆盖既有文件的方式原子发布。任何 dump 或压缩失败都不能留下看似成功的归档。进程被强制杀死后若残留 `.claracore-backup.lock`，先确认没有备份进程和 partial 文件，再只移除该锁目录。备份不包含 MySQL 用户、授权或服务端配置，这些仍须单独管理。

systemd 模板每天 UTC 02:15 触发，最多随机延迟 30 分钟，并用 `Persistent=true` 在机器错过计划时间后补跑。默认数据库名是 `claracore_server`；如实际名称不同，先用 unit drop-in 覆盖 `CCS_BACKUP_DATABASE`。安装后先手工启动一次 oneshot 并检查结果，再启用 timer：

```bash
install -o root -g root -m 0644 deploy/claracore-server-backup.service /etc/systemd/system/
install -o root -g root -m 0644 deploy/claracore-server-backup.timer /etc/systemd/system/
systemctl daemon-reload
systemctl start claracore-server-backup.service
systemctl enable --now claracore-server-backup.timer
systemctl status claracore-server-backup.service claracore-server-backup.timer
```

数据完整性巡检默认在备份之后的 UTC 03:15 运行，最多随机延迟 30 分钟。它直接使用生产环境文件，并把最新回执写到 `/var/lib/claracore-server/integrity-status.json`；先手工运行一次并确认退出码与回执，再启用 timer：

```bash
install -o root -g root -m 0644 deploy/claracore-server-integrity.service /etc/systemd/system/
install -o root -g root -m 0644 deploy/claracore-server-integrity.timer /etc/systemd/system/
systemctl daemon-reload
systemctl start claracore-server-integrity.service
systemctl enable --now claracore-server-integrity.timer
systemctl status claracore-server-integrity.service claracore-server-integrity.timer
```

发现违规时 oneshot 会按设计失败，但已经发布带违规计数的新回执，系统页、指标和告警据此进入注意态。Supervisor 只管理长驻应用进程；使用 Supervisor 的部署仍应由 systemd timer、受控 cron 或组织调度器运行同一条只读命令，不能把周期巡检塞进长驻进程。

oneshot 在生成后立即执行新鲜度检查；也可以由监控独立调用。下面的 93600 秒是“每天一次 + 30 分钟随机延迟”之外留出 90 分钟故障预算，超过即非零退出：

```bash
sudo -u claracore ./verify-mysql-backup.sh \
  --backup-dir /var/backups/claracore-server \
  --database claracore_server \
  --max-age-seconds 93600 \
  --status-file /var/lib/claracore-server/backup-status.json
```

检查器只选择指定数据库最新的原子发布归档，并交叉验证文件名、metadata、字节数、SHA-256、gzip 与 UTC 创建时间；成功 JSON 仍明确返回 `"recoverability":"not_proven"`。传入 `--status-file` 时，同一 JSON 会以 0600 临时文件在目标目录内原子替换为最新成功回执；失败保留旧回执，使它随时间自然转成过期状态，而不是把失败伪装成新的成功。

生产环境模板把 `CCS_BACKUP_STATUS_FILE` 指向这个回执。管理员系统页只读取最大 8 KiB 的普通非符号链接文件，严格拒绝未知字段、路径型归档名、时间不一致或“已证明可恢复”等越界口径，并按当前时间重新计算年龄；页面请求不会读取数据库正文、扫描备份目录或重新哈希大归档。生产告警仍必须同时覆盖 `claracore-server-backup.service` 失败和新鲜度检查失败，不能只依赖有人打开页面。模板**不会删除历史备份**；保留期、异机复制和删除必须在恢复演练及第二份独立副本策略确定后另行配置。

校验和与 gzip 完整不代表可恢复。把归档复制到隔离恢复环境，使用该环境的 0600 client file 执行：

```bash
./rehearse-mysql-restore.sh \
  --defaults-file /etc/claracore-server/mysql-restore-isolated.cnf \
  --backup /var/backups/claracore-server/claracore-server_claracore_server_<UTC>.sql.gz \
  --confirm-isolated-server \
  --max-age-seconds 2592000 \
  --status-file /secure/output/restore-status.json
```

演练先验证 companion SHA-256 和 gzip，并在任何 MySQL 调用之前拒绝含 `USE` / `CREATE DATABASE` / `DROP DATABASE` 的源库定向 SQL；随后才把 dump 导入脚本自建的随机数据库。它逐表执行 `CHECK TABLE QUICK`、读取精确行数，严格校验 migration 版本/校验和，并把结果写入不可覆盖的 `.restore-report.tsv`。默认路径只有在报告发布且本次隔离库成功删除后，才把 0600 技术恢复回执原子替换到 `--status-file`；任一步失败都保留旧回执，让它随时间自然过期。`--status-file` 不能与 `--keep-target` 同时使用，避免尚未清理的人工检查库被误报成完整技术演练。

把 0600 回执和不可变恢复报告通过受信任的管理通道传回应用主机，并把 `CCS_RESTORE_STATUS_FILE` 指向回执；回执中的报告 SHA-256 可用于交叉核对所传报告。管理员系统页和指标端点只安全解析最大 8 KiB 的回执，不打开恢复报告或归档，也不会在请求路径重新哈希大文件。因此它证明的是指定归档在隔离 MySQL 中完成导入、逐表 QUICK 检查、行数与迁移账本枚举，并在最后清理目标库；不证明传输通道、报告文件或归档此刻仍在应用主机可读。

如果需要人工抽查代表性的 tenant、Memory、Card 与 Shared Line，增加 `--keep-target` 并省略 `--status-file`，按脚本打印的准确库名检查后再人工删除。技术恢复回执固定标记 `semantic_inspection=not_proven`，不能替代这一步，也不能证明 RPO/RTO 达标。

恢复报告与技术回执都不证明业务语义全部正确，也不证明 RPO/RTO 达标。生产策略至少还要：定期自动备份、把 archive + checksum + metadata + 最近恢复报告复制到独立异机介质，并定期人工抽查代表性内容。删除旧备份前先确认另一份独立副本仍可恢复。

## 4. 升级顺序

1. 用预先固定的 Ed25519 公钥运行 `verify-release-signature.sh`，认证发布者、外层 SHA-256 与归档字节；解压到新的 `releases/<version>` 目录后运行 `verify-release.sh`，检查 `BUILDINFO.txt`、`BUILD_REPRODUCIBILITY.json`、`BUILD_PROVENANCE.intoto.json`、`SOURCE_MANIFEST.sha256`、`BUILD_INPUTS.sha256`、`VULNERABILITY_SCAN.txt`、`SBOM.cdx.json`、`SBOM_REVIEW.txt`、`PROMETHEUS_RULES_CHECK.txt`、`THIRD_PARTY_LICENSES.csv`、`THIRD_PARTY_REVIEW.txt` 与 `third_party_licenses/`，再用 `claracore-server -version` 核对版本和平台，并用绝对路径执行 `claracore-server -verify-provenance .../BUILD_PROVENANCE.intoto.json`。不要覆盖当前目录，也不要在 release 目录混放环境文件或临时文件，否则清单验证应失败；同主机重复构建、provenance、SBOM、规则和构建时扫描回执不是托管 builder、跨主机独立重建、当前漏洞库复扫、目标监控加载或新依赖人工许可复核的替代品。
2. 在旧实例仍服务时，用**新二进制和生产配置**执行 `./claracore-server -check-upgrade`。失败时不要停服务、迁移或切换 release。
3. 若结果为 `pending=0` / `rollbackMode=binary_only`，数据库已经与新旧二进制共同匹配：执行新二进制 `-check` 与 `-check-integrity`，两者都通过后再按实例逐个摘流、切换、启动和验证。单实例仍有正常重启窗口，但不需要 schema 维护窗口，失败时可切回旧二进制。
4. 若结果为 `pending>0` / `requiresSchemaMaintenance=true`，先安排维护窗口并摘流、停止**全部** ClaraCore Server 实例和摄取命令，确认没有应用 writer；不要让旧二进制在数据库迁移后继续接流量。
5. 停写完成后再用上节脚本生成本次维护窗口的原子备份，校验 archive/checksum/metadata，并确认已有近期隔离恢复报告和独立异机副本。备份后继续保持停写，使这份归档成为无写入缺口的数据库回退点。
6. 用新二进制显式执行 `./claracore-server -migrate`。迁移不会由普通服务启动隐式执行；账本不连续、旧 checksum 漂移或未知新版本都会在继续 DDL 前失败。
7. 仍在新 release 目录执行 `./claracore-server -check` 与 `./claracore-server -check-integrity`；前者必须得到 `ok=true`、`mysql=reachable`、`schema=current`，后者必须得到 `ok=true`、`status=verified`、`violations=0`。随后原子切换 `current`，启动新实例；多实例逐个启动并只把 ready=200 的实例加入流量池。
8. 在目标主机运行 `./verify-deployment.sh http://127.0.0.1:51890 http://127.0.0.1:51892`，同时验证业务 listener 与本机指标；提供 metrics URL 时，脚本还要求备份回执、技术恢复回执和数据完整性回执都存在且未过期，并要求完整性状态为已验证且零违规。再对公开 HTTPS 地址执行 `./verify-deployment.sh https://claracore.example.com`，不把 loopback metrics 地址改成公网地址。
9. 多实例全部就绪后，从能直连每个实例且能访问公开入口的运维主机执行：

   ```bash
   ./verify-cluster.sh --public-url https://claracore.example.com \
     --instance ccs-a=http://10.0.0.11:51890 \
     --instance ccs-b=http://10.0.0.12:51890 \
     --attempts 20
   ```

   `verify-deployment.sh` 已先要求 live/ready 的版本、源码摘要、二进制摘要与环境完全一致；`verify-cluster.sh` 再只读验证每个直连实例的 MySQL/schema 和完整制品身份一致性，并用禁用缓存、关闭连接的请求采样公开入口，要求每个预期 `instanceId` 至少返回一次。`env=production` 时，默认 `go build` 产生的 `sourceDigest=development` 会被拒绝，必须使用正式发布脚本产物。成功只证明当前时刻的源码/制品/实例一致性与入口分发，不证明发布者身份、统计意义上的流量均衡，也不等于摘流、故障切换或滚动发布验收。
10. 用一个测试 token 调用 `whoami`，确认 MCP 协议、身份与空间权限，不把 token 写进脚本或 shell 历史；完成业务抽查前仍保持维护窗口可快速处置。

Supervisor 实例在步骤 4 使用（命令路径按发行版调整）：

```bash
supervisorctl reread
supervisorctl update
supervisorctl restart claracore-server
```

`/livez` 只回答 HTTP 进程是否存活，不访问外部依赖，依赖短暂故障不应触发编排器重启风暴。`/readyz` 只有在实例未开始退出、MySQL 可达且全部迁移版本/checksum 与当前二进制精确匹配时才返回 200；因此旧实例在新迁移应用后必然退出 ready，这正是有迁移升级不能冒充无停机滚动的原因。Redis 与 embedding 有明确降级语义，不是 readiness 硬依赖。`/healthz` 为兼容旧监控保留，语义与 `/readyz` 相同。验证脚本同时检查 `/livez`、`/readyz`、Web 登录页与 MCP 无凭证 401；这些仍不证明业务数据正确，也不替代登录后的人工页面验收。

管理员“系统状态”页使用同一只读 schema 检查，并展示当前进程的 HTTP 健康窗口、MySQL 连接池占用、MySQL/Redis/Embedding/Atlas 已配置并装配的传输形态、最新成功备份回执、技术恢复回执与数据完整性回执。数据关系卡按当前 schema、检查时间和最大年龄重新判定，展示规则数、违规总数及最多三个固定规则名；它不打开业务行，也不在页面请求中执行关系 SQL。HTTP 窗口只统计本实例最近 5 分钟已完成的非探针请求，显示 4xx、5xx、panic、当前在途、平均耗时与固定直方图算出的 P95 上界；任一 5xx/panic 给注意态。它是常量内存的现场摘要，重启即清空，多实例时不做跨节点合并，也不替代外部时序监控和告警。`使用 / 上限` 达到 80% 时页面提示注意；是否扩容仍要结合持续时间、数据库端连接数与慢查询判断。`累计等待`、`累计等待时长` 和连接回收数自本次进程启动起累积，不是当前请求延迟。配置模板显式给出池上限，`CCS_MYSQL_MAX_OPEN_CONNS` 被限制在 1–10000，不能用无限连接掩盖容量问题。备份卡片绿色只证明完整性与年龄门禁；独立恢复卡片绿色只证明上述技术恢复范围；数据关系卡绿色只证明固定规则在一次一致性快照内未发现违规。代表性 Memory、Card、Shared Line 语义抽查和 RPO/RTO 仍需另行验收。

系统页的“租户容量快照”是另一条低频、显式的 SQL 路径：管理员打开页面时，Server 用 3 秒 deadline 在 repeatable-read read-only 事务中聚合当前租户行数和应用逻辑载荷。迁移 `0011` 为原先缺少 tenant-leading 路径的 Memory、Card、召回条件、关联和 embedding 表补索引。统计结果不包含正文、ID 或 actor，只显示类别计数与 `OCTET_LENGTH` 合计；它不含 InnoDB 行/索引开销、空闲页、binlog、备份和副本，因此不可当成磁盘或计费用量。超时只使该卡片不可用，不影响核心读写、readiness 或公开指标。

### 4.1 本机 Prometheus 指标

设置 `CCS_METRICS_ADDR=127.0.0.1:51892` 会启动第二个、只允许数字 loopback 地址的 HTTP listener；端口必须与 `CCS_ADDR` 不同。它只暴露 `GET /metrics`，使用 Prometheus text format 0.0.4，抓取本身不查询 MySQL/Redis，也不读取身份、URL、route、request ID、审计或业务正文。租户容量快照明确不接入这条抓取路径。外部活源启用时，独立后台采样器每 30 秒用 3 秒超时读取一次 body-free 聚合健康状态并存入常量大小的内存快照；抓取只读该快照，因此 Prometheus 抓取频率不会放大 SQL。公网 nginx 模板显式对 `/metrics` 返回 404；不要把该端口绑定到 `0.0.0.0`、容器公共网卡或共享反代。系统页只显示是否装配及本机地址。

指标保持常量基数、没有用户/租户/agent/route 标签，主要包括：

- `claracore_instance_accepting_requests`：正常接流量为 1，SIGTERM 摘流窗口为 0；
- `claracore_http_*_window`：本实例最近 5 分钟请求、4xx、5xx、panic 与延迟摘要，是滚动 gauge，不是自启动累计 counter；
- `claracore_mysql_*`：`database/sql` 连接池即时占用及累计等待/回收；读取本地 pool stats，不执行 SQL；
- `claracore_mcp_agent_rate_limit` / `claracore_mcp_tenant_rate_limit` / `claracore_mcp_rate_window_seconds`：当前进程装配的认证后双层固定窗口预算；只暴露配置，不含 tenant/agent 标签或实时身份计数；
- `claracore_backup_receipt_available` / `claracore_backup_fresh` / age：从最大 8 KiB 的安全回执重新按当前时钟计算，不扫描归档、不哈希大文件，也不声称可恢复性已证明。
- `claracore_restore_receipt_available` / `claracore_restore_fresh` / age / table / migration：从最大 8 KiB 的技术恢复回执计算；不读取恢复数据、报告或归档，且固定不把代表性内容抽查标成已证明。
- `claracore_integrity_receipt_available` / `claracore_integrity_fresh` / `claracore_integrity_verified` / age / rule / violation：从最大 16 KiB 的严格回执计算；无规则名标签，不执行关系 SQL，schema 不匹配时回执不可用。
- `claracore_external_*`：后台快照是否可用及年龄、活跃镜像总数、同步成功新鲜度、失败/运行来源，以及持久手动请求和租约总数；没有 tenant、source、URL、error、run 或 user 标签。来源新鲜度预算取 `max(15 分钟, 2 × 同步周期 + 整轮超时)`，未来超过 5 分钟的成功时间也按过期处理。

Prometheus 最小抓取配置示例：

```yaml
scrape_configs:
  - job_name: claracore-server
    static_configs:
      - targets: ["127.0.0.1:51892"]
```

发布包的 `deploy/prometheus.yml.example` 给出同机抓取与 `rule_files` 接线，但只能合并进现有 Prometheus 配置，不能覆盖组织已有的 retention、外部标签、Alertmanager 或通知路由。`deploy/claracore-server.rules.yml` 提供默认告警，`deploy/claracore-server.rules.test.yml` 固定四组行为场景：target down 不与 draining 重复、备份缺失不与 stale 重复、明确外部失败抑制 stale、活动 claim 抑制未领取 backlog。源码 `make monitoring` 与每次正式打包都会从 Go 校验和数据库验证过的 `github.com/prometheus/prometheus@v0.314.0` 源码构建 Prometheus 3.14.0 `promtool`，固定 revision 与 Go 1.26.6，真实执行 `check rules` 验证全部 22 条规则，再以 `test rules` 验证 7 个 firing/抑制断言；包内 `PROMETHEUS_RULES_CHECK.txt` 绑定规则与测试文件 SHA、工具来源和结果，进入 manifest 与外层签名链。这消除了开发机缺少系统 `promtool` 时的跳过，但它仍不证明目标 Prometheus 版本兼容、配置已 reload、告警已触发或通知路由可达。

安装或升级前仍必须使用**监控主机实际安装版本配套的 promtool**再次执行：

```bash
promtool check rules /etc/prometheus/claracore-server.rules.yml
```

通过后再 reload Prometheus。默认规则刻意压低噪声：

| 信号 | 默认等待 | 处理口径 |
|---|---:|---|
| target `up == 0` | 2 分钟 | critical；检查进程、loopback listener 与主机网络 |
| 仍可抓取但 `accepting=0` | 10 分钟 | warning；正常滚动发布不应持续这么久，并与 target down 互斥 |
| production 使用 `source_digest=development` | 5 分钟 | critical；替换为正式发布脚本生成且已验证的制品 |
| 同一 job 长时间存在多组版本/源码/二进制/环境身份 | 15 分钟 | warning；容忍正常短滚动，超时说明发布没有收敛 |
| 备份回执缺失 / 回执存在但过期 | 各 15 分钟 | critical；两条互斥，避免同一根因双报 |
| 技术恢复回执缺失 / 回执存在但过期 | 24 小时 / 1 小时 | warning / critical；缺失只在备份回执健康时触发，避免备份监控故障时重复通知；过期只在回执可用时触发 |
| 数据完整性回执缺失 / 过期 / 发现违规 | 1 小时 / 1 小时 / 15 分钟 | warning / warning / critical；过期只在已验证回执上判断，违规只在回执可用时触发，避免同一结果重复通知 |
| panic | 1 分钟 | critical；按服务端 request ID 查脱敏日志，不索取请求正文或凭据 |
| 5xx 持续留在滚动窗口 | 10 分钟 | warning；一次瞬时 5xx 不会直接通知 |
| P95 超出 30 秒桶 | 5 分钟 | warning；结合依赖延迟与连接池压力 |
| pool stats 缺失 | 5 分钟 | critical；此时不能信任容量告警 |
| 连接占用 ≥80% / 持续新增等待 | 各 10 分钟 | warning；先查慢操作与数据库容量，不直接抬高上限 |
| 外部健康快照失败或超过 120 秒未更新 | 10 分钟 | warning；其余外部症状规则只在快照可用时判断，避免监控盲区制造假恢复 |
| 外部来源最近同步失败 | 15 分钟 | warning；只在快照可用时触发，详情从有界回执和脱敏日志查 |
| 外部镜像超过动态新鲜度预算 | 30 分钟 | warning；存在明确失败时抑制该规则，避免同根因双报 |
| 手动同步 claim 过期 | 10 分钟 | warning；确认 worker 是否存活及数据库时间，正常应被其他实例重领 |
| 手动请求 pending 且无活动/过期 claim | 15 分钟 | warning；活动执行和已知过期租约会抑制 backlog 告警 |

多实例必须逐实例抓取；不要把各实例的滚动 HTTP gauge 当 counter 直接做 `rate()`。混合制品规则按 job 聚合并等待 15 分钟，避免正常短滚动产生通知；其余实例症状通常保留 `instance`。每个实例会独立采样同一租户的持久外部状态，Alertmanager 可按 `alertname`、`job` 分组，但是否合并不同实例仍由部署团队决定。默认规则用固定 `job="claracore-server"`，修改 scrape job 时必须同步修改规则并重跑 `promtool`。规则只生成 Prometheus alert，不自带通知接收器；Alertmanager 路由、值班人和升级策略仍由部署团队配置。

## 5. 回滚

保留至少一个上一版 release 目录，但先按升级计划区分两种回滚：

- `rollbackMode=binary_only`：本次没有 pending migration。摘流当前实例，用上一版二进制执行 `-check`；通过后才切回 `current`、启动并重跑部署探针。
- `rollbackMode=restore_required`：迁移已经改变共享数据库，上一版二进制会因为数据库领先而拒绝启动/readiness。**不能只切符号链接，也不能临时放宽 schema 门禁。**优先保持停写并前向修复新版本；确需回到旧版时，属于数据库恢复操作，必须使用维护窗口停写后生成的精确备份，先在隔离库验证并核对代表性 Memory、Card、Shared Line，再经明确恢复决策处理生产库。

不要用手工 `DROP COLUMN/TABLE` 猜测“逆迁移”；MySQL DDL 非事务化，且迁移后若曾恢复写入，数据库恢复可能丢失新写入。若维护窗口纪律被破坏，先盘点迁移后写入和审计事件，再决定前向修复或数据合并，不能把旧归档直接覆盖成无损回滚。

## 6. 本轮人工验收清单

- 管理员登录后能看到“空间权限”和“导入”。
- 最新备份有匹配的 SHA-256、metadata 与隔离恢复报告；报告列出全部基础表和 schema migrations，并已人工抽查一条代表性 Memory、Card 与 Shared Line。
- 从隔离测试地址连续提交错误密码直到收到 429：响应带 `Retry-After`，随后正确密码也必须等待窗口恢复，页面不能泄露账号是否存在。
- 新建项目空间，给另一个成员设置只读与读写，再撤销；目标成员的可见/可写范围随之变化。
- 从允许根目录导入一小批 Markdown：首次显示新建，原样重跑显示未变化。
- 删除测试源中的一份文件再全量扫描：页面只报告“源文件缺失”，对应卡片仍保留且版本不变。
- 制造一个不可读或超限文件：其余文件继续导入，页面提示本轮未执行缺失检测。
- `/livez` 为 `status=alive`，`/readyz` 为 `status=ready`、`mysql=reachable`、`schema=current`，且 `schemaVersion` 与最新迁移一致；`initialize` 版本正确，`tools/list` 为 24 个工具。
- 测试 token 的 `whoami`、Memory supersede/current-historical 查询、Card evolution、Shared Line archive/restore 正常。
- `gateway_auto_context` 的共享 Card 语料正样本能交付标注证据，无关负样本、重复 Card、重复 Memory 与跨域双命中样本均 abstain；显式 active `lineId` 返回 exact Shared Line；三类反馈都可记录 `used` 或 `wrong`。
- 连续调用相同情境后，`server_status.embeddingCache.hits` 增加。
- 配置 Redis 时重启服务后再次调用同一情境，hits 仍能增加；临时停止 Redis 后调用仍可完成，系统页应显示已降级。
- 用隔离测试 agent 连续调用 MCP 直至超过配置预算，确认最后一次返回 429、`Retry-After` 合理且系统页显示当前预算；换同一 agent 的另一个 token 不应绕过计数。只在隔离 token 上做，避免干扰真实 agent。
- 多实例部署时同时启动两个进程，写入一批新内容后确认两边日志合计只有一个实例处理该模型批次，队列最终清空。
- 为各实例设置唯一 `CCS_INSTANCE_ID`，先运行 `verify-cluster.sh` 证明直连实例版本/源码摘要/二进制摘要/环境/schema 一致且公开入口实际覆盖全部实例；保留输出作为当前分发回执，但不要把它当作下列故障注入的替代品。
- 在双实例都存活时短暂停止 Redis：新内容仍应进入 `ready`，允许日志出现重复计算，但不得出现永久 pending；恢复 Redis 后租约与共享缓存自动恢复。
- 向其中一个实例发送 SIGTERM：其 `/readyz` 立即变成 `503 draining` 并带覆盖摘流窗口的 `Retry-After`，`/livez` 在窗口内保持 200，新 Web/MCP 请求返回可重试 503；负载均衡应在 listener 真正关闭前停止向它发流量。
- 滚动重启其中一个实例，另一实例的 Web、MCP 与索引继续可用；负载均衡只能把 `/readyz` 为 200 的实例加入流量池。完成后分别验证两个实例的 `/livez`、`/readyz`，再验证负载均衡入口。

完整使用与验收步骤见 `08-usage-guide.md`。
