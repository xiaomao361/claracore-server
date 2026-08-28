# 贡献指南

感谢你改进 ClaraCore Server。这个项目优先保证权限隔离、可追溯写入和失败时的保守行为；新功能不能以隐藏越权、静默降级或破坏历史记录为代价。

除非你明确说明不是贡献，否则你有意提交并被项目接纳的贡献将依照 [Apache License 2.0](LICENSE) 授权，不附加额外条款。

## 开发环境

1. 安装 Go 1.26.3+ 与 MySQL 8.0+。
2. 复制 `.env.example` 为 `.env`，只填写本地测试凭据；不要提交 `.env`。
3. 执行 `make migrate`，然后运行 `make check`。
4. 改动 SQL、迁移、权限或 MCP 装配时，再运行 `make itest`。

## 修改纪律

- 先确认领域边界：MCP 与 Web 是并列 Host，共用 `internal/domain`，不要在 Host 中复制业务规则；
- 所有租户与空间过滤必须下推到 Store 查询，禁止先读全量再在应用层过滤；
- 内容状态变化保留历史；不要用删除替代 `deprecated`、`superseded` 或 archive；
- embedding 是增强能力，不得成为基本读写的启动依赖；
- 新增 MCP 工具前先检查工具总量与现有工具是否可扩展；
- 新增配置只在 `internal/config` 读取，并同步 `.env.example` 与部署模板；
- 数据库迁移只能前向追加，服务启动不会隐式迁移。

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

## 安全问题

不要在公开 Issue 中披露可利用漏洞、Token、密码、内网地址或生产数据。请按 [SECURITY.md](SECURITY.md) 使用 GitHub 私密漏洞报告。
