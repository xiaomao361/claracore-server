# ClaraCore Server MCP 工具契约

> 创建日期：2026-08-05
> 状态：设计中，无代码
> 前置文档：`01-architecture.md`、`02-data-model.md`
>
> **本文档是两条产品线的共同基准**：Desktop（JS）与 Server（Go）各自实现，工具名称、参数、语义保持一致。

---

## 1. 首要原则：控制工具数量

Desktop 当前对外暴露 **80+ 个 MCP 工具**（`memoria_*` 约 35 个、`shared_line_*` 约 17 个、`innerlife_*` 约 25 个、gateway 若干）。这是随功能演进逐步累加的结果，代价是实际存在的：

- 每个工具的 schema 都占用 agent 的上下文预算
- 工具越多，agent 选错工具的概率越高
- 大量工具是低频维护操作，却与高频工作工具混在同一个命名空间里

**Server 版目标：对 agent 暴露的工具控制在 20 个以内。**

### 1.1 精简方法：工作面 / 治理面切分

这条切分线与 `01-architecture.md` §10 中 Web 页面的定位完全一致：

| | 归属 | 举例 |
|---|---|---|
| **工作面** | **MCP 工具**（agent 用） | 创建、检索、更新、召回、关联、提升 |
| **治理面** | **Web 界面**（人用） | 导入导出、合并建议、休眠归档、标签别名管理、维护审计、配额、成员与 token |

Desktop 中 `memoria_maintenance_*`、`memoria_merge_suggestions`、`memoria_archive_dormant`、`memoria_import/export`、`memoria_label_alias_*` 这类工具**一律不进 MCP**——它们是人的运维动作，agent 不该也不需要调用。

**推论**：这不是功能删减，是把操作放到正确的入口。

---

## 2. 命名与共享契约

**保持 Desktop 的 `memoria_*` / `shared_line_*` 前缀。**

命名美观不是优先项——同一个人会同时使用两条产品线，工具语义与名称不一致会造成实际的使用混乱；且个人版数据将来需要能导入团队版。

新增的 Card 类工具使用 `card_*`，Desktop 将来若支持 Card，直接沿用同名工具。

---

## 3. 通用约定

### 3.1 空间参数

| 场景 | 行为 |
|---|---|
| **写入类**（create / update） | `space` **可选**，默认写入调用者的**个人空间** |
| **检索类**（search / recall / list） | `spaces` **可选**，默认覆盖调用者**全部可读空间** |

> **与「作用域字段不给默认值」的关系**：`02-data-model.md` §7 的约束针对**数据库层**——存储时 `space_id` 必须是显式确定的值。API 层有明确的默认行为不违反该约束，因为默认值在领域层就已解析为具体的 `space_id`，不存在"缺失"状态传入存储层。
>
> 写入默认个人空间是有意的低摩擦设计：随手记录不该有心智负担，要共享再走 `promote`。这与「个人空间是敢于记录的前提」一致。

### 3.2 检索结果必须标注来源空间

所有检索类工具的返回项**必须**包含：

```json
{
  "space": { "id": "...", "type": "team|project|personal", "name": "..." }
}
```

agent 据此判断权威性：`team` = 已达成共识的约定，`personal` = 个人经验。缺少此字段，agent 无法正确加权。

### 3.3 身份不由参数传递

**任何工具都不接受 `agentId` / `userId` / `tenantId` 参数。**

身份全部从 bearer token 反查（`01-architecture.md` §5.1）。Desktop 通过 header 声明 agent 身份的做法在 Server 版被彻底移除——参数化的身份等同于可伪造的身份。

### 3.4 并发冲突

更新类工具接受可选的 `version`。传入且与当前版本不符时，返回明确错误：

```json
{ "error": { "code": "VERSION_CONFLICT", "currentVersion": 7 } }
```

**不静默覆盖。** 由 agent 决定重试或合并。

---

## 4. 工具清单

### 4.1 Memory（6 个）

| 工具 | 用途 | 关键参数 |
|---|---|---|
| `memoria_create` | 创建记忆 | `title`, `body`, `labels[]`, `space?` |
| `memoria_search` | 语义 + 标签检索 | `query`, `labels[]?`, `spaces[]?`, `limit?` |
| `memoria_get` | 按 id 取单条（含关联） | `id` |
| `memoria_update` | 更新内容 / 标签 / 状态 | `id`, `version?`, 各字段 |
| `memoria_delete` | 软删除 | `id` |
| `memoria_link` | 建立/删除关联 | `fromId`, `toId`, `kind`, `note?`, `remove?` |

**合并说明**：Desktop 的 `archive` / `restore` / `restrict` / `unrestrict` / `supersede` / `tag` 全部并入 `memoria_update`（通过 `status`、`labels`、`supersedes` 字段表达）。语义不丢失，工具数从 8 降到 1。

### 4.2 Card（5 个）

| 工具 | 用途 | 关键参数 |
|---|---|---|
| `card_create` | 创建知识卡片 | `title`, `cardType`, 各 section, `recallTriggers[]`, `space?` |
| `card_recall` | **按当前情境召回** | `situation`, `spaces[]?`, `limit?` |
| `card_search` | 按内容检索卡片 | `query`, `cardType?`, `spaces[]?` |
| `card_get` | 取完整卡片 | `id` |
| `card_update` | 更新卡片 / 触发条件 / 状态 | `id`, `version?`, 各字段 |

#### `card_recall` 与 `card_search` 为何不合并

两者是**不同的意图**，合并会同时损害两边的精度：

| | 意图 | 匹配目标 |
|---|---|---|
| `card_recall` | "我正在做 X，有什么该注意的？" | **`card_recall_triggers` 表**——作者显式写下的触发条件 |
| `card_search` | "关于 JVM 的卡片有哪些？" | 卡片正文（提炼 / 理解 / 标题） |

`card_recall` 是本产品的核心能力（`02-data-model.md` §3.3）。它匹配的是**每条触发条件的独立 embedding**，而非卡片全文——因为一张 50 行复盘的整体向量会把"即将执行删除操作"这类关键信号稀释掉。

**典型调用**：

```json
{
  "situation": "用户让我清理 ~/.qclaw 目录释放磁盘空间，我准备执行 rm -rf"
}
```

命中《备份成功不等于数据可恢复》的触发条件"用户说'没用了''删掉吧'，但当前目录中可能含有不可再生历史时"，返回整张卡片。

**这类精确场景匹配，纯靠对全文做向量相似度是命不中的。**

### 检索类工具的返回：三个必须有的字段（2026-08-10 实现检索时定）

领域层已经把这三样算好了，Host 层只需要编码出去：

| 字段 | 为什么必须有 |
|---|---|
| `space` | §3.2 已定。注意 `knowledge` 只返回 `space_id`，映射成 `{id, type, name}` 是 Host 用治理域的信息做的 |
| `semantic`（每条命中） | 语义命中与全文命中**不通约成同一个分数**，分两组返回。缺了这个标记，调用方分不出「真的相关」与「只是字面撞上了」 |
| `semanticAvailable`（整次检索） | `false` 表示这次没用上向量（没配 Embedder，或算查询向量时外部服务失败）。缺了它，「没搜到」和「语义那一半根本没跑」长得一模一样，而下一步完全不同——与「检索为空 ≠ 无权检索」是同一条哲学 |

另外：**检索结果不带 `labels`**。粗筛刻意不填（N+1 查询或让候选集膨胀的 JOIN），按 ID 取单条时才有。

### 4.3 空间（3 个）

| 工具 | 用途 | 关键参数 |
|---|---|---|
| `space_list` | 列出我可访问的空间及读写权限 | — |
| `promote` | 提升（复制）到其他空间 | `entityType`, `entityId`, `toSpace` |
| `space_stats` | 空间内容统计 | `space?` |

`promote` 对 Memory 和 Card 通用，行为见 `02-data-model.md` §4：复制而非移动，记录 `copied_from`，不做自动同步。

### 4.4 Continuity（4 个）

| 工具 | 用途 |
|---|---|
| `shared_line_list` | 列出线 |
| `shared_line_get` | 取线的 resume packet |
| `shared_line_update` | 更新位置 / 摘要 / 下一步 |
| `shared_line_handoff` | 里程碑交接 |

Desktop 的 17 个 `shared_line_*` 中，`compact` / `archive` / `restore` / `rename` / `model_adjustment_*` 归入治理面（Web）。

### 4.5 系统（2 个）

| 工具 | 用途 |
|---|---|
| `whoami` | 返回当前 token 解析出的身份与权限概览 |
| `server_status` | 服务健康与版本 |

`whoami` 在 Server 版是必需品：agent 需要知道自己是谁、能写哪些空间，才能正确选择 `space` 参数。

---

**工具总数：20 个**（Memory 6 + Card 5 + 空间 3 + Continuity 4 + 系统 2），对比 Desktop 的 80+。

> **2026-08-10 实现状态**：Memory 6 + Card 5 + 空间 3 + 系统 2 = **16 个已落地**并在真库真模型上跑通（`test/mcp_tools_integration_test.go`）。Continuity 那 4 个还没做——它们需要 `internal/domain/continuity`，那个包目前是空的。
>
> 落地时确认的两处形态：
>
> **① `VERSION_CONFLICT` 不是 JSON 对象。** §3.4 写的是 `{"error": {"code": ..., "currentVersion": 7}}`，但 MCP SDK 的错误通道只能传字符串（tool 返回 error → `isError` + 文本）。实际形态是一句格式稳定的话：`VERSION_CONFLICT: 内容已被他人修改，当前版本 7。请重新 get 后基于最新内容再改`。版本号仍然必须带上——只说「撞车了」的话，agent 得再发一次 get 才能重试，而那次 get 之后可能再撞一次，重试就不收敛。
>
> **② `memoria_update` 暂时没有 `supersedes` 字段。** §4.1 说 Desktop 的 `supersede` 并入 update，但那是两次写（旧的置 `superseded` + 建一条 `evolved-from` 关联）且必须原子完成——那是一个领域用例，不是 Host 层能编排的。`archive` / `restore` / `tag` 都已经并进来了（通过 `status` 与 `labels`）。

---

## 5. 与 Desktop 的差异对照

| 维度 | Desktop | Server | 原因 |
|---|---|---|---|
| 工具数量 | 80+ | 20 | §1，工作面/治理面切分 |
| 身份传递 | header 声明 `X-ClaraCore-Agent-ID` | token 反查，不接受身份参数 | §3.3，可伪造 |
| 空间概念 | 无 | 三层空间 + 结果标注来源 | 团队场景必需 |
| 内容类型 | 仅 Memory（Card 靠"文件+指针"变通） | Memory + Card 双一等公民 | Card 原生落库 |
| 情境召回 | 无 | `card_recall` | 核心能力 |
| 并发 | 无（单进程） | `version` 乐观锁 + `VERSION_CONFLICT` | 多实例必需 |
| InnerLife | 25 个工具 | **无** | 已砍，且 stateless 下无法主动推送 |
| Records（时序） | `memoria_record_*` | **无** | 生理时序是个人版特性。团队场景若真需要「事件时间线」（部署记录、故障时间线），那是另一个需求，不拿这套表结构硬套 |
| 维护类工具 | 混在 MCP 中 | 移入 Web 治理面 | 人的动作不该占 agent 上下文 |

---

## 6. 待确认

- [x] **`memoria_search` 是否同时返回 Card 命中**：**分离，但返回信号**（2026-08-06 定）。内容不混排（memory 的「相关性」与 Card 的「该不该现在想起它」不是一个量纲，通约出来的排序是假的），但 `memoria_search` 的返回里带一个「另有 N 条卡片命中，用 `card_search` 查看」的计数。解决的是真实风险：agent 搜一次 memory 没找到就以为没有，而 Card 才是产品的核心载体。与「检索为空 ≠ 无权检索」同一哲学。实现上可只在 memory 命中少于阈值时才做这次 count（ngram 索引上的 count 不算白给）。
- [x] **`card_recall` 是否应由服务端自动注入触发**：**二期再说，第一版显式调用**（2026-08-06 定）。stateless 下服务端拿不到对话上下文，实现受限；且在召回质量被数据验证之前就自动往上下文里塞东西，错一次就会让人永久关掉它。先把显式调用的命中率跑出数据。
- [x] **`promote` 的权限校验点**：**两端都校**（2026-08-06 定）——源空间可读 + 目标空间可写。见 `02-data-model.md` §9。
- [x] **Records（时序数据）是否进 Server**：**不进**（2026-08-06 定）。Desktop 的 record 是步数/体重/用药这类生理时序，团队知识库里放它没有意义——与「InnerLife 不进团队版」是同一类判断。**但必须在本文档显式标注这个缺口**（见 §5 对照表），而不是默默没有：两边共享契约，agent 得知道缺口在哪，否则它会在 Server 上试着调 `memoria_record_*` 然后拿到一个莫名其妙的错。
