# ClaraCore Server

ClaraCore Server is a self-hosted, multi-tenant memory and knowledge service for AI agents. It exposes an MCP endpoint for agents and a server-rendered Web governance console for people.

ClaraCore Server 是面向 AI Agent 的自托管记忆与知识服务。它把三类长期上下文作为并列的一等公民：

- **记忆（Memory）**：可演进的事实，保留 current / historical 时间视图；
- **知识卡（Knowledge Card）**：带“何时该想起它”的结构化经验；
- **共同线（Shared Line）**：跨 Agent、跨会话继续同一目标的可恢复工作线。

服务端只做权限过滤、候选检索和保守的自动上下文预筛。最终相关性判断由终端 Agent 配置的大模型负责；服务端不会再运行一个 7B 判断模型。

本项目采用宽松、带明确专利授权的 [Apache License 2.0](LICENSE)。

## 能力

- Streamable HTTP MCP，Bearer Token 鉴权，24 个显式工具；
- tenant / user / agent / team-project-personal space 权限隔离；
- 中文全文检索，选配 bge-m3 或 OpenAI-compatible embedding 的两阶段语义检索；
- 保守自动召回：最低分与 top-1 margin 双门槛，歧义时主动放弃；
- Web 治理：成员、空间、Token、导入、索引、审核、内容浏览、重复候选、活动与审计；
- Markdown 幂等摄取、来源变更与缺失检测；
- 显式数据库迁移、可回滚单二进制部署、systemd / Supervisor / nginx 模板。

## 快速开始

要求：Go 1.26.3+、MySQL 8.0+。Embedding 和 Redis 都是可选依赖；禁用 embedding 时系统退化为全文检索，不影响基本读写。

```bash
cp .env.example .env
# 编辑 .env，至少填写 CCS_MYSQL_HOST / USER / PASSWORD / DATABASE
make migrate
```

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

默认 Web 地址为 `http://127.0.0.1:51890/`，MCP 地址为 `http://127.0.0.1:51890/mcp`。管理员首次登录必须修改密码。完成交接后删除临时密码文件。

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
- [贡献指南](CONTRIBUTING.md)
- [安全策略](SECURITY.md)

## 已知边界

- 当前正式支持 Linux 单实例；多实例下 embedding 队列可能重复计算，结果幂等但会浪费调用；
- 进程内 embedding 缓存不跨实例共享；
- 自动召回阈值按当前 bge-m3 评估集校准，换模型后必须重跑评估；
- Web 重复治理只给人工候选，不自动合并或删除；
- `healthz`、构建与部署成功都不等于业务验收，发布后仍要用真实身份和空间做读写验证。

## License

[Apache License 2.0](LICENSE)
