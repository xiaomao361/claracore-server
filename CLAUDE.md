# claracore-server —— 精简版（Go / SQLite，部署在 bosun 之后）

> 最后更新：2026-10-16（`slim` 分支）

团队 agent 的记忆与知识库 MCP 服务。只做一件事：按调用人保存和检索记忆（memory）与 agent 自有知识（knowledge）。身份、授权、限流、审计、连接时提示全部由 bosun 负责；结果格式遵循 shipwright 契约 v1。

- 代码：`cmd/server`（入口）、`internal/memo`（存储与检索）、`internal/mcpserver`（MCP 外壳与契约）、`internal/embedding`（向量服务客户端）。
- 身份只认 `X-Bosun-User`，只有设置了 `CLARACORE_TOKEN` 时才可信。bosun 登记时必须勾选 per_caller。
- 不删数据：过时的条目用 supersede 取代。schema 版本记在 `PRAGMA user_version`，库版本比程序新时拒绝启动。
- 2026-10 精简前的实现（MySQL、Web、治理、审核、Continuity）在 `main` 分支；2026-09-17 未验证的团队改造保存在本地分支 `wip/team-rollout-0917`。

接手前先读 `HANDOFF.md` 顶部的「精简版」一节。
