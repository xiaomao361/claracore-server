# 单实例部署与回滚

当前支持形态是 Linux 单实例服务（systemd 或 Supervisor），MySQL 外置，Redis 与 Embedding 可选。自动上下文不运行第二个大模型，只对 embedding 结果做确定性预筛，再由终端 Agent 的模型最终判断。部署包不含任何真实凭据；配置独立放在环境文件中。

推荐固定目录为 `/opt/claracore-server`，配置模板见 `deploy/`。凭据只放 owner-readable 的环境文件（0600），不要写进 systemd、Supervisor 或 nginx 配置。

生产通过 HTTPS nginx vhost 提供 Web 与 MCP，应用自身只监听 `127.0.0.1:51890`。模板见 `deploy/claracore-server.nginx.conf`；替换示例域名、接入证书后，必须先执行 `nginx -t`，通过后才能 reload。

## 1. 构建发布包

在开发机先跑完整门槛，再按服务器架构打包：

```bash
make check
make itest
./scripts/package-release.sh 0.2.0 amd64
# arm64 服务器把最后一个参数改为 arm64
```

产物在 `dist/`，包括 `.tar.gz` 与 `.sha256`。上传后先在服务器验证校验和，再解压。

自动上下文用 `CCS_AUTO_CONTEXT_MIN_SCORE` 与 `CCS_AUTO_CONTEXT_MIN_MARGIN` 控制确定性 Stage B。默认 `0.55 / 0.03` 按 bge-m3 项目评估集校准；替换 embedding 模型后必须重跑检索与自动上下文评估再调整，不能照搬阈值。无语义证据或请求超过 15 秒总上限时放弃，不影响显式 `card_recall`。

单实例默认启用进程内 embedding LRU：`CCS_EMBEDDING_CACHE_ENTRIES=1024`、`CCS_EMBEDDING_CACHE_TTL_SEC=86400`。key 包含 model 与原文，替换模型不会命中旧向量；设 entries 为 0 可关闭。`server_status.embeddingCache` 暴露 enabled/hits/misses。多实例需要共享命中时再实现 Redis decorator，Redis 仍不是启动依赖。

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

## 3. 升级顺序

1. 校验发布包 SHA-256，并把新版本解压到新的 `releases/<version>` 目录；不要覆盖当前目录。
2. 在数据库主机做可恢复备份。推荐使用权限为 `0600` 的 MySQL client 配置文件配合 `mysqldump --defaults-extra-file=... --single-transaction --routines --triggers claracore_server > backup.sql`，避免密码出现在命令行。
3. 用新二进制和生产配置显式执行一次 `claracore-server -migrate`。迁移不会在服务启动时自动发生。
4. 原子切换 `/opt/claracore-server/current` 符号链接，执行 `systemctl restart claracore-server`。
5. 运行 `./verify-deployment.sh http://127.0.0.1:51890`，再对公开 HTTPS 地址重复一次。
6. 用一个测试 token 调用 `whoami`，确认 MCP 协议、身份与空间权限，不把 token 写进脚本或 shell 历史。

Supervisor 实例在步骤 4 使用（命令路径按发行版调整）：

```bash
supervisorctl reread
supervisorctl update
supervisorctl restart claracore-server
```

`healthz` 证明进程和 MySQL 可达；验证脚本再证明 Web 登录页与 MCP 无凭证拒绝。它们不证明业务数据正确，也不替代登录后的人工页面验收。

## 4. 回滚

保留至少一个上一版 release 目录。服务异常时，把 `current` 原子切回上一版本，重启后重跑验证脚本。

`0007_ingest_source_tracking.sql` 只增加可空列和索引，旧二进制可以继续运行，因此本次代码回滚无需回滚数据库。不要为回滚代码而删除 `source_name`：DDL 删除会扩大故障面，也会丢掉新版本已经写入的来源信息。

只有确认数据库内容本身被错误改写时才考虑恢复备份；先恢复到隔离库并核对表、行数和代表性记录，不能把“备份文件存在”当成可恢复证明。

## 5. 本轮人工验收清单

- 管理员登录后能看到“空间权限”和“导入”。
- 新建项目空间，给另一个成员设置只读与读写，再撤销；目标成员的可见/可写范围随之变化。
- 从允许根目录导入一小批 Markdown：首次显示新建，原样重跑显示未变化。
- 删除测试源中的一份文件再全量扫描：页面只报告“源文件缺失”，对应卡片仍保留且版本不变。
- 制造一个不可读或超限文件：其余文件继续导入，页面提示本轮未执行缺失检测。
- `/healthz` 为 `ok=true`，`initialize` 版本正确，`tools/list` 为 24 个工具。
- 测试 token 的 `whoami`、Memory supersede/current-historical 查询、Card evolution、Shared Line archive/restore 正常。
- `gateway_auto_context` 的相关正样本交付一张卡，无关负样本 abstain；反馈可记录 `used` 或 `wrong`。
- 连续调用相同情境后，`server_status.embeddingCache.hits` 增加。

完整使用与验收步骤见 `08-usage-guide.md`。
