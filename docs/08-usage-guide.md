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

`server_status.embeddingCache` 显示进程内 embedding 缓存是否启用及 hits/misses。相同情境重复调用后 hits 应增加；进程重启后计数归零是预期行为。

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

每个需要自动召回的用户回合，把真实消息原样传给 `gateway_auto_context.prompt`。返回只有两种：

- `decision=deliver_one`：读取且只使用 `block.body`；`selected` 只是候选证据，不代表已经交付或使用；
- `decision=abstain`：没有足够可靠的卡，或 embedding 不可用、分数不足、top-2 歧义、超时；不要用显式搜索绕过这次自动放弃。

自动路径先做语义候选，再用最低分和 top-2 差值做确定性 Stage B；最多选择一张，整体硬上限 15 秒。证据块会明确要求终端 Agent 的大模型做最终相关性判断；`selected` 不等于 `delivered` 或 `used`。显式 `card_recall` 不经过自动策略。

调用方发生真实结果后，用 `gateway_context_feedback` 记录：

- `delivered`：确实放入了上下文；
- `used`：实际影响了回答或动作；
- `ignored`：交付了但没有使用；
- `wrong`：召回错误；
- `corrected`：错误已被当前证据纠正。

反馈只进入治理活动，不改写卡片正文。

用 `context_quality_status` 查看当前租户近 30 天的已提交 outcome 计数。它只说明反馈样本覆盖与分布；没有反馈的交付不可见，因此不能把 `used / submitted` 直接解释为自动召回准确率。

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
- “审核队列”的空间、状态筛选同样生效。
- 知识卡审核与详情页会复用当前检索模型查找 active 重复候选：规范化正文相同会标为“内容相同”，语义分不低于 0.90 会标为近重复。候选只供人工比较，系统不自动合并或删除；确认重复后把非保留项标为 `deprecated`，历史仍可按 ID 与状态筛选查看。未启用 embedding 时仍能发现进入全文候选集的精确重复，但不会声称语义近似。
- “共同线”是左侧独立导航，不混入 Memory / Card 类型；可看进行中、已归档、全部，进入详情查看当前位置、最近 10 条位置/交接历史，并执行归档或恢复。状态写入有 CSRF 与治理审计。
- `[演示]` 前缀及 `demo-seed` 标签的数据是拟真测试材料，不是团队业务事实。

页面人工验收仍由用户完成：刷新后分别核对 1440px 与 390px、筛选结果、共同线内容、登录/权限/活动页；自动测试和 HTTP 探针不等于视觉验收。

## 7. 运维验证

本地发布门槛：

```bash
make check
make itest       # 修改 SQL、迁移或真实适配器后必须运行
./scripts/package-release.sh <version> amd64
```

部署后分层验证：

1. SHA-256 与解包；
2. Supervisor / systemd 进程；
3. `/healthz`、Web、MCP 401 边界；
4. `initialize` 的版本与 `tools/list`；
5. 一个正样本和一个负样本自动上下文；
6. 代表性的 Memory supersede、Card evolution、Shared Line archive/restore 读写。

详细命令与回滚见 `07-deployment.md`。

## 8. 当前剩余边界

- 目标服务器登录后的 1440px / 390px 人工视觉与完整业务验收尚未由用户确认。
- 自动上下文当前召回 **Card**；它不是 Desktop 那套 Memory Controller 的完整移植，也不会自动组合 Memory + Shared Line 多域上下文。
- 单实例已有进程内有界 embedding LRU；多实例间尚不能共享命中，进程重启后会重新预热。Redis 共享缓存留到多实例部署时实现。
- 审核队列没有批量操作。
- 真实演示数据已出现近重复卡，Stage B 会因 margin 不足安全放弃；现在详情页会给出重复候选并复用 `deprecated` 完成人工废弃。仍未提供自动合并，因为系统不能替人决定两个相似表述是否是同一事实。
- `gateway_context_feedback` 已留治理事件，`context_quality_status` 提供近 30 天租户级 outcome 汇总；仍需随着真实 wrong/ignored 反馈扩充固定评估集，且不能把自选择反馈样本当成独立准确率。
- 外部活知识源连接（如 odoc 索引同步）尚未实现；现有摄取只支持受控 Markdown 目录。
- 不提供 Desktop 的 InnerLife 和个人时序 Records；这是产品边界，不是遗漏。
