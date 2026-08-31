# 贡献指南

感谢你改进 ClaraCore Server。这个项目优先保证权限隔离、可追溯写入和失败时的保守行为；新功能不能以隐藏越权、静默降级或破坏历史记录为代价。

除非你明确说明不是贡献，否则你有意提交并被项目接纳的贡献将依照 [Apache License 2.0](LICENSE) 授权，不附加额外条款。

## 开发环境

1. 安装 Go 1.26.6+ 与 MySQL 8.0+。
2. 复制 `.env.example` 为 `.env`，只填写本地测试凭据；不要提交 `.env`。
3. 执行 `make migrate`，然后在能访问 Go 官方漏洞库的环境运行 `make check`；漏洞库不可达或发现可达漏洞都会让检查失败。
4. 改动 SQL、迁移、权限或 MCP 装配时，再运行 `make itest`。

## 修改纪律

- 先确认领域边界：MCP 与 Web 是并列 Host，共用 `internal/domain`，不要在 Host 中复制业务规则；
- 所有租户与空间过滤必须下推到 Store 查询，禁止先读全量再在应用层过滤；
- 内容状态变化保留历史；不要用删除替代 `deprecated`、`superseded` 或 archive；
- embedding 是增强能力，不得成为基本读写的启动依赖；
- 新增 MCP 工具前先检查工具总量与现有工具是否可扩展；
- 新增配置只在 `internal/config` 读取，并同步 `.env.example` 与部署模板；
- 数据库迁移只能前向追加，服务启动不会隐式迁移。
- 新增或升级依赖时运行 `make licenses` 与 `make sbom`，分别检查 Linux amd64/arm64 的许可证分类，以及最终二进制的 CycloneDX 组件集、标准库、目标平台和 SHA-256 绑定；未知、限制性或新的 reciprocal/native 依赖必须在合入前人工复核分发义务。
- 修改 Prometheus 指标、告警表达式、等待窗口或抑制关系时同步更新 `deploy/claracore-server.rules.test.yml` 并运行 `make monitoring`；它从 Go 校验和数据库验证过的固定 Prometheus 源码构建 `promtool`，同时执行 `check rules` 与 `test rules`，不能用 YAML 能解析或本机偶然存在的其它版本代替。

## Pull Request

PR 应说明：

- 问题与用户可见结果；
- 权限、租户隔离和数据保留影响；
- 执行过的验证命令；
- 是否修改迁移、配置、MCP 合约或部署方式；
- 尚未验证的真实环境或人工验收项。

请保持 PR 聚焦，不顺手格式化或重构无关代码。提交前至少运行：

```bash
make check
```

集成测试因缺少数据库而跳过不算通过；涉及持久化行为时，请提供真实 MySQL 的 `make itest` 结果。

`make security` 固定 `govulncheck` 实现版本但读取实时漏洞库，因此一份旧的绿色输出不会永久证明依赖安全。发布时还会扫描最终 Linux 二进制；不要通过剥离扫描回执或复用旧回执绕过新发现。

许可证工具同样是自动证据而不是法律意见。正式发布会保存目标架构依赖报告、许可证/NOTICE 和 reciprocal 依赖所需源码；不要删除这些材料来缩小归档。

SBOM 是目标制品的软件清单，不是漏洞扫描、许可证结论、可复现构建或发布者认证。正式发布必须保留 `SBOM.cdx.json` 与 `SBOM_REVIEW.txt`，并让它们继续受内层 manifest 和外层签名链保护。

正式发布还会保存 `PROMETHEUS_RULES_CHECK.txt`，绑定 promtool 版本、上游模块校验和/revision、Go 工具链、规则 SHA 和规则数。它证明构建时规则被固定工具接受，不证明目标 Prometheus 已加载、告警进入 pending/firing 或通知路由可达。

正式 `.tar.gz` 必须继续使用 `scripts/release-archive` 装配，不能退回宿主机默认 `tar -czf`。写入器固定路径顺序、tar/gzip 元数据、权限和压缩参数，并拒绝链接/特殊文件；发布测试会从解包目录逐字节重建归档且验证 mtime 不影响结果。这证明相同 staging 内容的归档装配确定性，不证明编译器、实时漏洞库或其他生成步骤已经构成完整可复现构建。

正式发布必须保留 `SOURCE_MANIFEST.sha256`、`BUILD_INPUTS.sha256`、`BUILD_REPRODUCIBILITY.json` 与 `BUILD_PROVENANCE.intoto.json`，并让同一二进制的 `-verify-provenance` 继续在配置加载前校验 subject、目标、源码/输入清单、有界双构建结果和其他构建回执。发布构建必须固定 Go 1.26.6、禁用 VCS stamp，并用两个新建且分离的 Go build cache 得到完全相同的二进制；不能把这一同主机门禁表述成 hermetic 或跨主机 reproducible build。新增或改名打包输入时同步更新 provenance 的输入集合和 `docs/09-release-provenance.md`。该 statement 使用 SLSA provenance v1 格式，但本地 builder 不是托管/加固平台；不要在发布说明或 PR 中宣称未经独立证明的 SLSA Build Level。

## 安全问题

不要在公开 Issue 中披露可利用漏洞、Token、密码、内网地址或生产数据。请按 [SECURITY.md](SECURITY.md) 使用 GitHub 私密漏洞报告。
