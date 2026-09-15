# 逐表租户归属审计（Phase 0 交付物）

> 本文件定稿 `MAAS_TECH_DESIGN.md` §3.4 中标注"基于表名推断、未逐表核对"的分类表。
> 该节的分类表**以本文件为准**。
>
> **方法**：从 `framework/configstore/tables/*.go` 机械提取全部 `TableName()` 声明
> （含多行声明形式），再提取每个 struct 的归属列（`VirtualKeyID` / `TeamID` /
> `CustomerID` / `UserID` / `CreatedBy` / `Scope` / `ScopeID`），
> 结合列语义与上游注释判定。
>
> **表总数：56**。（过程中两个错误计数已排除：38 漏掉了多行 `TableName()` 声明；
> 另一次 56 来自宽松正则，数字偶然正确但匹配依据错误。）
>
> **置信度标注**：✅ 已依据列结构与注释确认；⚠️ 需产品决策（不是技术不确定）。

---

## 1. 汇总

| 类别 | 张数 | 处理方式 |
|---|---|---|
| A. 平台级 | 16 | 不加 `tenant_id` |
| B. 租户级 | 27 | 加 `tenant_id` + 联合索引 |
| C. 混合级 | 1 | `config_keys`：可空 `tenant_id`（D1 双模） |
| D. 已有 Scope 机制 | 3 | 复用，可能只需注册新 scope 值 |
| E. 需产品决策 | 9 | 见第 6 节 |

> 16 + 27 + 1 + 3 + 9 = **56** ✅（已用脚本核对：56 张表全部覆盖，无一遗漏、无一重复分类）
>
> 计数口径：`oauth_tokens` 与 `governance_pricing_overrides` 在第 2 / 第 5 节中作为
> 上下文出现，但归属**在第 6 节定稿**，因此只计入 E 类一次。

---

## 2. A 类：平台级（不加 `tenant_id`）

这些表描述**部署自身**，不描述任何租户的资产。

| 表 | 依据 |
|---|---|
| `config_client` | 部署级客户端配置 ✅ |
| `config_env_keys` | 环境变量引用 ✅ |
| `config_hashes` | 单行全局配置哈希（仅 4 字段）✅ |
| `config_log_store` | 日志后端连接配置 ✅ |
| `config_models` | 模型目录 ✅ |
| `config_plugins` | 插件注册表 ✅ |
| `config_providers` | provider 定义（key 才分租户，见 C 类）✅ |
| `config_vector_store` | 向量库连接配置 ✅ |
| `distributed_locks` | 基础设施（`dlock.go`）✅ |
| `feature_flags` | 部署级开关 ✅ |
| `framework_configs` | 框架配置 ✅ |
| `governance_config` | 治理全局配置 ✅ |
| `governance_model_pricing` | 官方价目表 ✅ |
| `mcp_library` | 平台 MCP 目录（`Slug`/`Category`，无归属列）✅ |
| `sidekiq` | 任务队列 ✅ |
| ~~`oauth_tokens`~~ | 倾向平台级（MCP OAuth 服务端令牌，非用户态），但**归属在 6.7 定稿**，计入 E 类 |
| `prompt_version_messages` | `prompt_versions` 子表，随父表隔离 ✅ |

> `prompt_version_messages` 列入 A 类是因为它**没有独立查询入口**，
> 始终经父表访问。若后续出现直查该表的接口，须改为 B 类（冗余 `tenant_id`）。

---

## 3. B 类：租户级（加 `tenant_id` + 联合索引）

`tenant_id` 进联合索引首位（R10）。子表一律冗余 `tenant_id`，不靠 FK join 推导
（§3.4 实施要点）。

### 3.1 治理层级

| 表 | 现有归属列 | 说明 |
|---|---|---|
| `governance_customers` | — | OSS 层级顶端，租户下的一级实体 ✅ |
| `governance_teams` | `CustomerID` | ✅ |
| `governance_virtual_keys` | `TeamID`,`CustomerID`,`RateLimitID` | ✅ |
| `governance_virtual_key_provider_configs` | `VirtualKeyID` | 子表，冗余 ✅ |
| `governance_virtual_key_provider_config_keys` | — | 二级子表，冗余 ✅ |
| `governance_virtual_key_mcp_configs` | `VirtualKeyID` | 子表，冗余 ✅ |
| `governance_budgets` | `VirtualKeyID`,`TeamID`,`CustomerID` | ✅ |
| `governance_rate_limits` | — | 经 `RateLimitID` 被引用 ✅ |

> ⚠️ **注意 `governance_customers` 的语义陷阱**：它在 OSS 中是"成本归集层级的顶端"，
> 不是租户。租户是它**之上**的新一层。不要把 customer 直接当 tenant 复用——
> `MAAS_TECH_DESIGN.md` §1 已述，`store.go:1884` 的 `organizationLimitsForVirtualKey`
> 是层级语义的直接证据。

### 3.2 Prompt / Skill / 文件

| 表 | 现有归属列 | 说明 |
|---|---|---|
| `prompts` | — | 已走 `ScopedDB`（`prompts.go`）✅ |
| `prompt_versions` | — | 子表，冗余 ✅ |
| `prompt_sessions` | — | ✅ |
| `prompt_session_messages` | — | 子表，冗余 ✅ |
| `folders` | — | prompts 的组织结构 ✅ |
| `skills` | `CreatedBy` | 已走 `ScopedDB`（`skills.go`）✅ |
| `skill_versions` | — | 子表，冗余 ✅ |
| `skill_files` | — | 子表，冗余 ✅ |
| `skill_file_blobs` | — | 二级子表，冗余 ✅ |

> `prompts` 与 `skills` 是**已经在用 `ScopedDB` 的两个模块**（39 处调用集中在此）。
> 它们是补全其余 318 处未加作用域查询时最好的参照实现。

### 3.3 作业 / 令牌 / MCP 用户态

| 表 | 现有归属列 | 说明 |
|---|---|---|
| `batch_jobs` | `CustomerID`,`TeamID`,`UserID`,`VirtualKeyID` | 归属列最完整 ✅ |
| `temp_tokens` | `Scope` | ⚠️ `Scope` 是**用途命名空间**（如 `mcp_auth`），**不是归属**。仍需 `tenant_id` ✅ |
| `mcp_oauth_flows` | — | 用户态 OAuth 流程 ✅ |
| `mcp_oauth_tokens` | — | 用户态令牌 ✅ |
| `mcp_per_user_header_flows` | — | 按用户 ✅ |
| `mcp_per_user_header_credentials` | — | 按用户，含凭证 ✅ |
| `oauth_user_sessions` | — | 用户会话 ✅ |
| `oauth_user_tokens` | — | 用户令牌 ✅ |
| `enterprise_mcp_tool_groups` | — | ✅ |
| `enterprise_mcp_tool_group_virtual_keys` | `VirtualKeyID` | join 表，冗余 ✅ |

---

## 4. C 类：混合级 —— `config_keys`

**唯一一张可空 `tenant_id` 的表**（D1：平台自持 + BYOK 双模）。

- `tenant_id IS NULL` → 平台共享池
- `tenant_id = ?` → 租户 BYOK

谓词必须走 `KeyScopeFor(tenantID)`，禁止手写（§3.5.1 / R9）。
Phase 0 已实现并验证：`internal/tenant/keyscope.go` + 10 条隔离测试全绿。

---

## 5. D 类：已有 Scope 机制的 3 张表

这几张表**已经带 scope 维度**，处理方式与其余不同——可能只需注册新 scope 值。
（`governance_pricing_overrides` 也有 scope 机制，但归属在 6.5 定稿，计入 E 类。）

| 表 | 机制 | 现有值 | 是否有注册表 |
|---|---|---|---|
| `governance_model_configs` | `Scope` + `ScopeID` | `global` / `virtual_key` / `project` | ✅ **有**：`RegisterModelConfigScope()` |
| `routing_rules` | `Scope` + `ScopeID` | `global`/`team`/`customer`/`virtual_key`/`user`（仅注释声明） | ❌ 无，纯字符串列 |
| `routing_targets` | — | `routing_rules` 子表 | — |
| ~~`governance_pricing_overrides`~~ | `ScopeKind` + `UserID`/`VirtualKeyID` | — | ❌ 无。**归属在 6.5 定稿**，计入 E 类 |

### 5.1 `RegisterModelConfigScope` 是官方扩展点

`framework/configstore/tables/modelconfig.go:24-41` 的注释原文：

> validModelConfigScopes is the runtime registry of accepted scope values.
> OSS seeds it with global + virtual_key; **downstream consumers (e.g. the
> enterprise build registering "access_profile") extend it at startup via
> RegisterModelConfigScope**.

也就是说：**注册一个 `tenant` scope 值即可让模型配置支持租户级**，
不需要给这张表加列。启动时调一次 `RegisterModelConfigScope("tenant")`，
`IsValidModelConfigScope` 与 `TableModelConfig.BeforeSave`（`modelconfig.go:147`）自动接受。

> ⚠️ 但 `Scope` 表达的是"这条配置作用于谁"，
> **不等于**"这条配置属于哪个租户"。一条 `scope=virtual_key` 的记录仍需知道
> 该 VK 属于哪个租户才能做行级过滤。
> **结论：仍需 `tenant_id` 列做隔离，`tenant` scope 值用于表达租户级默认配置。**
> 两者是不同维度，不要用其中一个替代另一个。

### 5.2 `routing_rules` 没有注册表

`routing_rules.Scope` 是纯 `varchar(50)`，有效值只写在注释里
（`routingrules.go:31`），无 `RegisterRoutingRuleScope` 函数（已 grep 确认）。
新增 `tenant` scope 值不受校验保护——需要在自研代码里自行约束。

---

## 6. E 类：需产品决策的 9 张表

这些**不是技术不确定，是产品语义待定**。每条给出建议与理由。

### 6.1 `sessions` —— 优先级最高

**现状（关键发现）**：整张表只有 `ID` / `Token` / `TokenHash` / `ExpiresAt` /
`CreatedAt` / `UpdatedAt` / `EncryptionStatus`。
**没有 `user_id`，没有 `principal_type`，没有任何归属列。**

D4 定为共享部署后，租户门户会话与平台管理会话会落在**同一张无法区分二者的表**里。
这正是 R16 / §5.3 第 2 条要求的"会话必须携带 `principal_type` 与 `tenant_id`"——
本审计确认了它当前**完全不具备**该能力。

**建议**：加 `principal_type`（`platform_admin` / `tenant_user`）+ `tenant_id`（可空，
平台管理员为 NULL）+ `user_id`。**这是 Phase 1 必须做的改造，不能延后**——
共享部署下没有它就没有鉴权边界。

### 6.2 `config_webhook_endpoints` / `webhook_jobs`

现状：`ID`/`Name`/`URL`/`Secret`/`EventsJSON`/`HeadersJSON`，无归属列。

**建议：租户级**。租户应能配置自己的 webhook 接收自己的事件。
平台级 webhook（运维告警）用可空 `tenant_id` 区分，与 `config_keys` 同构。

⚠️ 附带安全约束：租户可控 URL 意味着 SSRF 面。表里已有 `AllowPrivateNetwork` 字段，
**租户级 webhook 必须强制该字段为 false**，不可由租户自行开启。

### 6.3 `config_mcp_clients`

现状：`ClientID`/`Name`/`ConnectionType` 等，无归属列。

**建议：混合级**（可空 `tenant_id`）。平台提供公共 MCP 客户端，
租户可注册自己的私有客户端。与 `config_keys` 同一模式。

### 6.4 `governance_model_parameters`

现状：仅 `ID` / `Model` / `Data`,无归属列、无 scope。

**建议：混合级**。平台设默认参数（`tenant_id` NULL），租户可覆盖。

### 6.5 `governance_pricing_overrides`

现状：有 `ScopeKind` + `UserID` + `VirtualKeyID`，但无租户维度。

**建议：租户级**。MaaS 的核心商业需求之一就是**按租户差异化定价**
（大客户折扣、阶梯价）。这张表是 M6/M7 的关键依赖，建议在 Phase 1 就加上 `tenant_id`，
避免 Phase 2 做计费时返工。

### 6.6 `notifications`

现状：`Audience` + `RoleIDs`（JSON），无 `tenant_id`。

**建议：混合级**。平台公告（`tenant_id` NULL，全体可见）与租户内通知（限本租户）。
注意 `RoleIDs` 的注释说明企业版角色由 overlay 提供——这张表与 M4 管理面 RBAC 耦合。

### 6.7 `oauth_configs` / `oauth_tokens`

现状：`oauth_configs` 含 `ClientSecret`（加密）、`Status`；表结构无租户维度。

**需区分两种用途**：

- **MCP 服务端 OAuth**（Bifrost 作为 OAuth 客户端连外部 MCP）→ 平台级
- **租户 SSO**（租户接自己的 IdP）→ 租户级

**建议**：本期若不做租户自带 IdP，保持平台级（列入 A 类）；
若企业客户要求各自 SSO，需加 `tenant_id`。**这是与 M4 一并决策的事项。**

---

## 7. 对 `MAAS_TECH_DESIGN.md` 的修正

本审计推翻或细化了 §3.4 原分类表中的以下几处：

| 项 | §3.4 原分类 | 审计结论 |
|---|---|---|
| `modelconfig` | 租户级（加列） | D 类：已有 Scope 机制 + 官方注册表，需列**且**需注册 scope 值 |
| `routingrules` | 未列入 | D 类：有 Scope 但**无**注册表校验 |
| `sessions` | 租户级（加列） | E 类最高优先级：**当前无任何归属列**，共享部署下是鉴权空洞 |
| `webhooks` | 租户级（加列） | E 类：需决策，且附带 SSRF 约束（`AllowPrivateNetwork` 必须锁死） |
| `pricingoverride` | 平台级（不加列） | **错误**。应为租户级——差异化定价是 MaaS 核心商业需求 |
| `temptokens` | 租户级（`Scope`） | 确认需加列：`Scope` 是用途命名空间，非归属 |
| `logstore` | 平台级 | 澄清：`config_log_store`（连接配置）平台级 ✅；但**请求日志表**（`framework/logstore/tables.go`）需 `tenant_id`，二者不同 |

---

## 8. 迁移实施顺序建议

1. **`sessions` 改造**（6.1）—— 共享部署的鉴权前提，最高优先
2. **治理层级 + `config_keys`**（3.1 + 4）—— 核心隔离链路，`KeyScopeFor` 已就绪
3. **`governance_pricing_overrides`**（6.5）—— 避免 M6 计费返工
4. **Prompt / Skill 族**（3.2）—— 有 `ScopedDB` 参照实现，风险最低
5. **其余 B 类 + E 类决策落地**

每步一个 `migrator.Migration{ID, Migrate, Rollback}`，
用 `migrator.AddColumnIfNotExists`，参考 `configstore/migrations.go:659`。
历史数据回填到默认租户。
