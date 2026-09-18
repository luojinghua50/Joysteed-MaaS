# Bifrost 二开：企业级 MaaS 平台改造技术方案

> 面向实施工程师。所有结论均基于本仓库代码实测，关键判断附 `文件:行号`。
> 未经验证的推断已显式标注。
>
> **状态：D1–D8 已关闭，Phase 0 验证已完成（见第 7 节）。
> Phase 1 第 4 项（Redis 版 `KVStore`）已完成并注入 `maas-gateway` 运行时，见 §7.1。
> D11 ✅ 已决策（D11a→④ 控制面自有库 + outbox，D11b→③ RLS），**M1 解除阻塞**。
> D12 / D13 已给出建议（§9.3 / §9.4），待确认后 M2 / M3 解除阻塞。**
>
> 2026-09 一次外部审核发现三处架构级错误，均已在原位修正并标注
> （§1 核心策略与行级隔离自相矛盾、M2.0 遗漏 Streams 广播语义、
> M3 的"硬额度"实际不原子）。修正块以 `⚠️ 修正（外部审核发现，2026-09）` 开头，
> 可全文检索。对应的待决策项为 D11–D13，见 §9.1。
>
> **D11 已拆为 D11a / D11b，并基于 RLS 实测 spike 给出建议，见 §9.2。**
> 该 spike 发现一个此前未识别的严重运维陷阱（超级用户 / `BYPASSRLS` / 缺 `FORCE`
> 会让 RLS 静默失效且不报错，见 R31），以及租户上下文的注入风险（R32）。
> D11a / D11b 仍待你确认，确认后 M1 即可开工。
>
> Phase 0 新增 D9、D10 两项，均不阻塞。
> 逐表归属审计已定稿于 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md)（实际 56 张表，非原估 ~50）。
> 剩余一项待实测：`docs/enterprise/` 所述 clustering 的实际完成度。

## 1. 背景与范围

在 Bifrost（Apache 2.0）基础上二开，构建商用企业级 MaaS 平台。

**核心策略**：不侵入 Bifrost 转发内核。以 Go module 依赖方式引入 `core` / `framework` / `plugins`，
**行为扩展**全部以 wrapper / overlay / plugin 形态存在。

> ⚠️ **修正（外部审核发现，2026-09）**：本节原文写的是"自研代码全部以 wrapper / overlay /
> plugin 形态存在，**不修改上游文件**"。这个绝对表述是错的，且与 §3.4 的行级隔离方案
> **直接矛盾**——两者不可能同时成立。
>
> 原因：Go 语言层面**无法从外部 module 给上游 struct 增加字段**。行级隔离要求给
> `tables.TableVirtualKey` 等 56 张表加 `tenant_id` 列，这必然改动上游的 struct 定义与 DDL。
> 更进一步，wrapper 也拦不住上游内部的未作用域查询：`GetGovernanceConfig`
> （`framework/configstore/rdb.go:6313` 起）连续用 `s.DB().WithContext(ctx).Find(&teams)`、
> `Find(&customers)`、`Find(&budgets)` 查多张表，全部绕过 `ScopedDB`，wrapper 无从介入。
>
> **准确的表述是**：`GovernanceStore` 那 4 个方法的**行为覆写**确实能做到零侵入
> （Phase 0 已验证，见 §7）；但**数据模型改造是另一种性质的工作**，做不到。
> 隔离方案的落地形态是 **D11 待决策项**（见第 9 节），四条候选路线的取舍未定之前，
> M1 的迁移动作不启动。

**租户隔离模型（方向已决策，落地形态待定）**：共享库 + 行级隔离（shared DB, row-level isolation）
作为基础底座。**但"行级隔离如何落地"是 D11**——fork 最小补丁 / 旁路租户映射表 /
Postgres 原生 RLS / 控制面投影为数据面只读快照，四者在运维复杂度、性能、
与上游同步成本上差别很大。

**部署前提：`config_store` 与 `logs_store` 都必须是 Postgres。**
这条曾被当成一件"SQLite → PostgreSQL 改造"来排期，**它不是**——上游两个 store
各自都已原生支持 Postgres（[`configstore/config.go:13-14`](framework/configstore/config.go#L13-L14)、
[`logstore/config.go:12-22`](framework/logstore/config.go#L12-L22)），连接层
[`framework/postgresconn`](framework/postgresconn/postgresconn.go#L102-L118)
还带了 `env.XXX` 密码引用、`PasswordCommand` 与连接池参数。要做的只是配置，
没有需要开发的转换层。

但它是**硬前提**，不是性能选项：

- 没有 Postgres 就没有 RLS——D11 选定的隔离路线在其他后端上**根本不存在**（R34）
- `dbForUpdate` 在非 postgres 方言上静默不加 `FOR UPDATE` 且不报错（R36）
- SQLite 只是**默认值**（[`lib/config.go:1062`](transports/bifrost-http/lib/config.go#L1062)），
  不显式配就是它；且**两个 store 独立选型**，配了一个另一个仍在 SQLite
- 反过来，配上 Postgres 也不等于隔离到手：它同时**打开了物化视图这条不受 RLS
  约束的读取路径**（R37），需要一并处置

启动即校验，见 §7.2.1 `rls.Enforce` 的 `dialect.not_postgres`。

**第一原则**：不重复实现已有能力。下面第 2 节是复用清单，实施时先查这张表。

---

## 2. 复用清单（禁止重写）

实测确认可直接复用，不要另起一套：

| 能力 | 位置 | 说明 |
|---|---|---|
| **行级隔离机制** | `framework/queryscope/` | 官方为 wrapper 设计，见 3.1 |
| **资源授权引擎** | `framework/grant/` | Access / Permit / Identity，provider+model+MCP tool 的 allow-deny、key 白名单、通配符 |
| **成本计算** | `framework/modelcatalog/`、`tables/modelpricing.go`、`tables/pricingoverride.go` | 逐请求算钱的数据源，计费系统直接接 |
| **日志存储** | `framework/logstore/` | postgres + ClickHouse hybrid，已支持高写入 |
| **分布式锁** | `framework/configstore/dlock.go` | DB 表实现，带 TTL / retry / Extend / IsHeld |
| **迁移框架** | `framework/migrator/` | `Migration{ID,Migrate,Rollback}`、`AddColumnIfNotExists`、迁移分布式锁 |
| **治理内核** | `plugins/governance/` | VK/Team/Customer 层级、预算、限流、扣费 |
| **MCP 网关** | `core/mcp/`、`handlers/mcp*.go` | 含完整 OAuth2 授权链路，自研需数月 |
| **OIDC 登录 + 目录同步** | `framework/oauth2/` | 含 `sync.go` 后台同步 |
| **可观测性** | `plugins/telemetry/`(Prometheus)、`plugins/otel/`、`framework/tracing/` | |
| **任务队列** | `framework/sidekiq/`、`framework/jobaccounting/` | 带 heartbeat 与计账 |
| **批量/异步推理** | `handlers/asyncinference.go`、`tables/batchjob.go` | |
| **Prompt 管理** | `tables/prompts.go`、`promptVersions.go`、`promptSessions.go` | 带版本与会话 |
| **智能路由** | `plugins/routing/` | 含基于复杂度的模型选择 |
| **静态加密 / 密钥托管** | `configstore/encryption.go`、`vault_callbacks.go` | |
| **前端工程与组件库** | `ui/` | Vite + React + Radix，见第 5 节 overlay 接缝 |
| **部署** | `helm-charts/`、`terraform/`、`transports/Dockerfile.redhat` | UBI 镜像可走 RedHat 认证 |

---

## 3. 租户隔离底座：行级隔离

### 3.1 关键前提：过滤机制已存在，不要自造

`framework/queryscope/queryscope.go` 的包注释原文：

> provides the primitive that **wrappers** use to push a per-call SQL constraint
> onto the request context for inner stores to consume.

已提供两个原语：

- **`QueryScope func(*gorm.DB) *gorm.DB`** — 决定调用方**能看到哪些行**。
  通过 `WithQueryScope(ctx, scope)` 挂到 context，内层 store 用 `ScopedDB(ctx)` 盲式应用。
- **`DimensionScope func(idCol string) (allowed []string, bounded bool)`** — 决定**聚合分组维度上允许出现哪些 ID 值**。

`DimensionScope` 解决的是一个多租户实现里极易漏掉的泄漏：一行数据在 team 维度上可见，
但它携带的 `customer_id` 调用方无权看到；按 customer 分组就会把这个 ID 暴露在排行、
直方图和筛选下拉里。**这个坑上游已经填了**，直接用。

`ScopedDB(ctx)` 已是 `ConfigStore` 接口方法（`framework/configstore/store.go:1009`），
logstore 侧同样具备（`framework/logstore/rdb.go:107`、`hybrid.go:586`）。
`framework/logstore/rdb.go:3983` 的注释明确写着这套机制服务于 "Enterprise DAC"——
即企业版的数据访问控制正是建在它上面。

**结论：行级隔离的工作量不在造机制，在补全覆盖面 + 加租户列 + 写注入 wrapper。**

### 3.2 覆盖率现状（实测）

| 指标 | 数值 |
|---|---|
| `ScopedDB(ctx)` 调用点 | **39** 处，仅分布在 4 个文件（`skills.go`、`prompts.go`、`store.go`、`rdb.go`） |
| 未加作用域的 `s.DB().` 调用点 | **318** 处 |
| 其中 `configstore/rdb.go` 独占 | **242** 处 |

覆盖率约一成。`rdb.go` 是主战场。

### 3.3 最高风险项：默认行为是 fail-open

`queryscope.FromContext` 的语义（见其注释）：scope 为 nil 时**等价于"无限制"，不加任何 WHERE**。

这对 OSS 单租户是合理默认，对商用多租户是**跨租户数据泄漏的直接通路**——
任何一处忘记注入 scope 的查询都会返回全量数据，且不会报错。

**必须改成 fail-closed。** 落地方式：

1. 自研 `TenantScopedStore` wrapper 包装 `ConfigStore`，在 wrapper 层校验：
   凡是标记为"租户级"的方法，ctx 上取不到 tenant scope 直接返回错误，不进 DB。
2. 平台级/后台任务路径显式传入 `WithPlatformScope(ctx)` 这类哨兵，
   让"无限制"成为**必须主动声明**的选项，而非默认。
3. 加一个测试：反射遍历 `ConfigStore` 接口所有方法，断言租户级方法在无 scope 时报错。
   这个测试是防止后续迭代退化的唯一可靠手段，优先级等同功能代码。

### 3.4 数据模型改造

新增 `tenants` 表（租户主体 + 生命周期状态机），并给租户级表加 `tenant_id`。

**分类以 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md) 为准**（Phase 0 已完成）。
实际表数是 **56**，不是原估的 ~50。审计结论摘要：

| 类别 | 张数 | 处理方式 |
|---|---|---|
| A. 平台级 | 16 | 不加 `tenant_id` |
| B. 租户级 | 27 | 加 `tenant_id` + 联合索引 |
| C. 混合级 | 1 | `config_keys`，可空 `tenant_id`（D1 双模，详见 3.5） |
| D. 已有 Scope 机制 | 3 | 需列**且**需注册 scope 值，二者不可互替 |
| E. 需产品决策 | 9 | 见审计文档第 6 节 |

审计推翻了本节原有分类的五处，其中两处影响实施顺序：

- **`sessions` 当前没有任何归属列**（只有 token/hash/过期时间）。D4 共享部署下
  租户会话与平台管理会话落在同一张无法区分二者的表里——这是 R16 的实证。
  必须加 `principal_type` + `tenant_id` + `user_id`，**且不能延后到 Phase 3**。
- **`governance_pricing_overrides` 原分类为平台级是错的**，应为租户级。
  按租户差异化定价（大客户折扣、阶梯价）是 MaaS 核心商业需求，
  建议 Phase 1 就加列，避免 Phase 2 做计费时返工。

另外发现一个官方扩展点：`governance_model_configs` 有 `RegisterModelConfigScope()`
运行时注册表（`tables/modelconfig.go:24-41`，注释明示企业版用它注册 `access_profile`）。
可注册 `tenant` scope 值——但 **scope 表达"作用于谁"，不等于"属于哪个租户"**，
隔离仍需 `tenant_id` 列。不要用其中一个替代另一个。

**三个实施要点**：

- **子表也要冗余 `tenant_id`**，不要依赖 FK join 推导租户。理由：`ScopedDB` 的 scope 是
  在单表查询上加 WHERE，靠 join 推导会让每个查询都被迫 join，性能和正确性双输。
- **请求日志表必须加 `tenant_id`**（`framework/logstore/tables.go`）。这是 ClickHouse 侧的
  高基数大表，租户维度要进排序键，否则按租户查日志会全表扫。
- ⚠️ **子表需要复合外键约束**（外部审核发现，本文档原先完全遗漏）。
  上一条只说了"子表冗余 `tenant_id`"，但**冗余列本身不阻止跨租户引用**：
  租户 A 的子记录完全可以指向租户 B 的父记录，两行各自的 `tenant_id` 都"正确"，
  却构成了一条跨租户的数据通路。
  必须建 `(tenant_id, parent_id)` 复合外键指向父表的 `(tenant_id, id)`，
  由数据库拒绝这种组合。B 类里 9 张子表（`prompt_versions`、`skill_files`、
  `governance_virtual_key_provider_config_keys` 等）都要覆盖。

**迁移写法**：沿用 `framework/migrator`，每个变更一个 `Migration{ID, Migrate, Rollback}`，
用 `migrator.AddColumnIfNotExists` 加列。参考 `configstore/migrations.go:659`
（`migrationAddBatchJobsAttributionColumns`）的现成范式。

> ⚠️ **修正（外部审核发现，2026-09）**：本节原文以"历史数据回填到默认租户"一句收尾，
> **这个做法对部分表是危险的**，不能一概而论。
>
> `config_keys` 里的 BYOK 密钥、`mcp_per_user_header_credentials` 里的用户凭据、
> 请求日志、历史账单——这些数据一旦被错误归属到默认租户，
> 不只是数据脏，而是**直接构成跨租户泄漏**（默认租户的管理员就能看到别人的密钥）。
>
> **迁移方案需要补齐的部分（属 D11 落地时一并设计）**：
>
> - 迁移前全量备份 + 迁移后校验（逐表比对行数与归属分布）
> - 每张表**各自**的归属映射规则，不用统一默认值
> - **无法判定归属的数据进 quarantine**（隔离表 / `tenant_id` 置 NULL 并禁止读取），
>   人工裁定后再归位，绝不塞进默认租户
> - 大表分批回填 + 在线建索引（`CREATE INDEX CONCURRENTLY`），避免长锁
> - 双写或只读窗口的选择，以及回滚方案
> - 复合外键的建立顺序（先回填、再校验、最后加约束，否则约束会因脏数据建不上）

### 3.5 provider key 归属：平台自持 + BYOK 双模（已决策 D1）

**本期两者都支持。** `key.tenant_id` 可空，一张表覆盖两种商业形态：

| `key.tenant_id` | 含义 | 计费口径 |
|---|---|---|
| `NULL` | 平台共享池（平台买断算力转售） | token 成本 + 平台加价 |
| 非 NULL | 租户 BYOK（租户自带 key） | 仅服务费/网关费，无 token 成本 |

`provider` 表保持平台级（provider 定义本身不分租户，只有 key 分）。

#### 3.5.1 隔离规则的例外：这里不能简单 fail-closed

3.3 节要求租户级表在无 scope 时报错。**`key` 表是唯一例外**——它的 scope 谓词是：

```sql
tenant_id = ? OR tenant_id IS NULL
```

即租户能看到自己的 key **加上**平台池的 key。这是本方案里唯一一处"允许看到非本租户行"的
合法查询，也因此是最容易写错的一处：

- 写成 `tenant_id = ?` → 租户用不了平台池，功能坏掉（会被测试发现）
- 写成不加谓词 → **租户能看到其它租户的 BYOK，是泄漏**（不会被测试发现）

**要求**：`key` 表的 scope 必须走一个专用构造函数（如 `KeyScopeFor(tenantID)`），
禁止在业务代码里手写这个谓词。并针对它单独写一个三方测试：
租户 A 的查询结果必须恰好等于 {A 的 key} ∪ {平台池 key}，且不含 B 的任何 key。

#### 3.5.2 解析优先级：BYOK 独占，兜底需租户显式开启（D7 已决策）

**决策：BYOK 优先且独占。** 租户配了自己的 key，该 provider 下就只用自己的，
用尽即失败，**不静默回落平台池**。

理由：静默回落会产生租户预期外的账单——他以为在用自己的额度，月底收到平台池的账单。
这类计费争议的处理成本远高于一次请求失败。

**平台池兜底做成租户级开关**，默认关闭：

- **开关位置**：`tenants` 表或其配置子表，**不能是全局开关**。
  不同租户对"可用性优先"还是"成本可控优先"的选择不同。
- **开启后必须可观测**：请求日志的 `key_ownership` 如实记为平台池（见 M6），
  且在用量报表或响应头上标记本次发生了回落。
  租户要能自己对账，而不是月底才发现。
- **关闭时错误要具体**：BYOK 用尽应返回明确的业务错误（如"租户密钥配额已用尽"），
  不要退化成通用 5xx——否则租户会误判为平台故障并提工单。

#### 3.5.3 共享池的公平性问题

平台池是跨租户共享资源，provider 侧的限额随之成为共享资源——
单一大租户打满平台 key 的 provider 配额会影响所有租户。这与 M10 是同一问题的两个面。

**已确认的接缝**：`schemas.KeySelector` 是可插拔函数类型
（`core/schemas/bifrost.go:16`），挂在 `BifrostConfig.KeySelector`
（`core/schemas/bifrost.go:36`），由 `SelectKeyForProviderRequestType`
（`core/bifrost.go:4690`）调用。上游只内置了 `WeightedRandom`
（`core/keyselectors/weightedrandom.go`）。

**这就是实现租户公平性的位置**：自研一个租户感知的 KeySelector，
按租户套餐档位分配平台池权重。属于 M10 范围，但 D1 定了双模后这块的必要性上升了——
平台池现在是明确的共享资源，不是可选优化。

#### 3.5.4 BYOK 的密钥安全

租户 BYOK 的 key 是**租户的商业机密**，平台运维不应能读取明文。
现有 `configstore/encryption.go` + `vault_callbacks.go` 已提供静态加密，
但要确认：平台管理员在后台**不能查看**租户 BYOK 明文（只能看到掩码 + 校验状态）。
`ClientConfig.Redacted()` / `ProviderConfig.Redacted()`
（`framework/configstore/clientconfig.go:482`、`:509`）提供了掩码范式，沿用它。

#### 3.5.5 日志侧的 key 身份泄漏

`framework/logstore/tables.go:207` 有 `SelectedKeyID` 列（带索引），
`tables.go:59` 有 `SelectedKeyIDs`。租户查自己的日志时：

- 若该请求走平台池 → `selected_key_id` **必须脱敏**，租户无权知道平台池的 key 构成
- 若走自己的 BYOK → 可以展示

同时 `SelectedKeyID` 作为聚合分组维度时要纳入 `DimensionScope` 约束
（参见 `tables.go:2326` 的 `RankingDimension` 机制），否则租户能在排行/筛选里
枚举出平台池或其它租户的 key ID。

### 3.6 租户身份的两条解析链路

两条链路的租户来源完全不同，必须分开设计：

| | 数据面（推理） | 控制面（管理 API） |
|---|---|---|
| 入口 | `HTTPTransportPreAuthHook`（`core/schemas/plugin.go:223`） | 管理会话 / JWT |
| 租户来源 | VK 凭证反查 | 登录态 |
| 落地方式 | 解析后写入 `BifrostContext` | 注入 `QueryScope` |
| 强制点 | GovernanceStore wrapper（见 4.1） | `TenantScopedStore` wrapper（见 3.3） |

---

## 4. 需新开发的功能模块

按依赖顺序编号。M1–M3 是地基，顺序不可颠倒。

### M1. 租户底座（TenantScopedStore + tenants 表）

见第 3 节。交付物：

- `tenants` 表 + 生命周期状态机（注册 / 试用 / 正式 / 欠费停服 / 注销）
- 全表 `tenant_id` 迁移（沿用 `framework/migrator`）
- `TenantScopedStore` wrapper：注入 scope + fail-closed 校验
- `PreAuthHook` 插件：VK → 租户解析
- 隔离验证测试：两租户零互见 + 接口方法全覆盖反射测试

**工作量集中在 `configstore/rdb.go` 的 242 处未加作用域查询。** 这是纯体力活，可并行拆分。

### M2. 配置下发总线

**为什么必须自研**：`pubsub` 在 `transports/bifrost-http/server/server.go:63` 的
`enterprisePlugins` 列表里（与 kafka / datadog / bigquery 并列），**不在本仓库**。
且 configstore 无配置轮询 ticker（已 grep 全部 `NewTicker` 确认），
`ServerCallbacks` 的 `ReloadPlugin` / `UpdateAuthConfig` 全是**进程内回调**。

即：**OSS 版本中，节点 A 改配置不会到达节点 B**。

最小契约四件事：

1. **版本号** — `config_hashes` 只有单个全局 hash（`tables/confighash.go` 仅 4 字段），
   做不了增量下发。需按租户/实体的 generation。
2. **总线** — 推"某租户某版本变了"，不推配置本体。选型见 M2.0（D3 已决策）。
3. **冷启动** — 节点启动拉全量快照，直接用 `GetGovernanceData`。
4. **兜底** — 总线故障退化为低频轮询 + `dlock` 选主，不让 Redis 单点打挂数据面。

#### M2.0 总线选型：Redis Streams（D3 已决策）

**结论：用 Redis Streams，不引入 NATS。**

排除 NATS 的理由不是它不好——JetStream 在这类场景表现优秀——而是
**M3 硬额度计数器已经强制需要 Redis**（原子 `INCRBY` + Lua，见 M3.2）。
既然 Redis 是必需品，再加 NATS 就是第二套中间件：部署、监控、加固、高可用各一份。
边际收益不足以抵消这份运维成本。

> 唯一会翻转这个结论的情况：你的基础设施里**已经在跑 NATS**。那就用 NATS，
> 别为 M2 单独引入 Redis 之外的东西。

**为什么是 Streams 而不是 pub/sub。** 这一点值得说清楚，因为 pub/sub 看起来更简单：

Redis pub/sub 是 **fire-and-forget / at-most-once**。订阅者断连期间的消息**直接丢弃**，
重连后没有任何补偿机制。对配置失效通知，这意味着一次网络抖动就可能让某个节点
**永久停留在旧配置上，且不报错**——它自己不知道错过了消息。
错过的可能是预算变更、permit 收紧、或租户隔离规则更新。

Redis Streams 是 **at-least-once**：消息持久化，consumer group 维护游标，
断连重连后从上次位置补齐。go-redis v9 原生支持（`XAdd` / `XReadGroup` / `XGroup`），
不需要额外库。

**配置失效通知天然幂等**（"租户 X 的配置到 v42 了"重复收到无害，重新拉一次快照即可），
所以 at-least-once 的重复投递对我们零成本。

> ⚠️ **修正（外部审核发现，2026-09）**：上面这段描述**漏掉了广播语义**，照它实施会出错。
>
> consumer group 是**组内竞争消费**——一条消息只投给组内**一个**消费者，不是广播给所有节点。
> 若所有数据面节点加入同一个 group，每条配置失效消息只有一个节点收到，
> 其余节点**永久停留旧配置**。这恰好是本节开头指责 pub/sub 的那个失败模式，
> 而且同样不报错。
>
> 上面"consumer group 维护游标，断连重连补齐"对**任务队列**成立，对**广播**不成立。
> 两者是不同用法。
>
> **正确做法（二选一，属 D12）**：
>
> 1. **每节点独立 consumer group**（组名含节点 ID）。同一条消息进入每个 group 各投一次，
>    达成广播 + 每节点各自维护游标。节点下线需清理僵尸 group，否则 PEL 无限增长。
> 2. **中继服务 fan-out**：单一 group 消费后再分发给各节点。多一跳，但 group 数量恒定。
>
> **无论选哪条，以下必须在设计里写明**（当前文档全部缺失）：
> consumer group 的创建与幂等重建；ACK 时机；Pending Entries List 的恢复与
> `XAUTOCLAIM` 策略；死信处理；`XTRIM` 保留窗口与裁剪策略；配置 generation 的
> fencing（防止慢节点用旧版本覆盖新版本）；Redis 不可用时的降级与告警。

也就是说 Streams 相比 pub/sub 多出的是**可补齐的投递**，但**广播语义要自己构造**——
不是开箱即得。这一点原文没说清，是本节最需要修正的地方。

> 全仓库当前**无 Redis Streams 使用**（已 grep 确认，命中项均为 Go stream / SSE 无关词），
> 这块是纯新增代码，没有可抄的范式。`framework/vectorstore/redis.go` 的连接配置仍可复用。

**兜底仍然必要。** Streams 降低了漏消息概率，但不消除 Redis 整体不可用的情况。
第 4 条兜底（低频轮询 + `dlock` 选主）保留，它同时也是 Streams 消费者
长时间落后时的追赶路径。

#### M2.0.1 备选：DB 变更日志表 + 轮询（若想暂缓引入 Redis）

如果希望 M2 阶段先不碰 Redis（把 Redis 收窄到只服务 M3 计数器），有一个
**与本仓库现有范式完全一致**的选项：DB 变更日志表 + 轮询。

依据：`framework/sidekiq/` 就是这么做的——**DB 后端 + 轮询 dispatcher**，
包内零 Redis 引用，靠 `time.NewTicker`（`sidekiq.go:420`）+ 认领式派发 + `staleAfter` 回收。
`framework/webhooks/dispatcher.go:224`、`framework/oauth2/sync.go:130` 同样是轮询范式。
`configstore/dlock.go` 提供选主。

**取舍**：延迟从亚秒级变成轮询间隔级（秒级），DB 多一份轮询负载；
换来的是零新增中间件、与上游范式一致、运维面更小。

**建议**：若团队 Redis 运维经验有限，或希望 Phase 1 收敛风险，走这条；
否则直接上 Streams。两条路的上层接口应当一致（发布"租户 X 到 v42"），
后期切换不影响业务代码——**设计时把总线抽成接口**，别让实现细节渗进调用方。

#### M2.1 可复用的接缝：kvstore 的 SyncDelegate

`framework/kvstore/kvstore.go:64` 定义了 `SyncDelegate`：

```go
type SyncDelegate interface {
    OnSet(key string, valueJSON []byte, writtenAt int64, expiresAt int64)
    OnDelete(key string, deletedAt int64)
}
```

注释原文：*"is notified of all mutations, enabling **cross-node replication**"*，
冲突解决为 last-write-wins（按 unix nano 时间戳）。
通过 `Store.SetDelegate(d)` 挂载，配套的 `RegisterDecoder(keyPrefix, decoder)`
用于接收侧按 key 前缀重建具体类型（注释里直接用了 "gossip payloads" 一词）。

OSS 中**无任何实现**（已 grep 确认），是留给企业版的挂载点。

**对 M2 而言 LWW 语义是正确的**——配置以"最后一次写入生效"为准符合预期。
所以配置/开关类状态的跨节点同步可以直接建在这个接缝上，
接收侧用 `RegisterDecoder` 还原类型，`transports/bifrost-http/integrations/utils.go:536`
的 `RegisterKVDecoders` 是现成范式。

⚠️ **但不要把这个接缝用于额度计数器**，LWW 会静默丢增量，详见 M3.2。

#### M2.2 连带发现：多节点下会话粘性静默失效

`core/schemas/bifrost.go:38` 的注释：*"shared KV store for clustering/session stickiness; nil = disabled"*。
`core/bifrost.go:9282` 使用 `DefaultSessionStickyTTL`（1 小时），
`core/bifrost.go:9330` 的 `getCachedKeyFromStore` 依赖注入的 `KVStore` 复用已选定的 key。
`plugins/routing/complexitysession.go` 同样依赖它做会话级复杂度分层缓存。

**问题**：OSS 唯一的实现是进程内内存。多节点部署时每个节点各有一份粘性状态，
同一会话被负载均衡打到不同节点就会拿到不同的 key/分层——
**功能不报错，只是静默失效**。

这不在原定 M1–M10 范围内，但与 M3.1 是同一个交付物：
实现 Redis 版 `schemas.KVStore` 并注入，会话粘性和 M2 的配置同步一起解决。

**D8 已决策：纳入 Phase 1**（Phase 1 第 4 项）。成本已被 M3.1 覆盖，
一个交付物同时解决三处：M2 配置同步、M3.1 共享 KV、本节的会话粘性。

验收标准：多节点部署下，同一会话连续请求打到不同节点时，
命中的 provider key 与复杂度分层保持一致。

### M3. 强额度共享计数器（原称"硬额度"，见本节修正块）

`plugins/governance/store.go:27-90` 显示治理用量全在进程内 `sync.Map`，
配合 `LastDBUsagesBudgets` 做增量对账、周期性 `DumpBudgets` 写回 DB。
governance 插件内 **grep 不到任何 Redis / kvstore 引用**。

多节点下超支窗口 = 节点数 × 单节点两次 dump 间消费量。有界，但对预付费场景不够小。

**分档处理，不要一刀切**：

- **软额度**（告警、成本归集）：沿用内存计数，零改动，性能最优。
- **硬额度**（预付费余额、租户封顶、超额熔断）：wrapper 覆写 `CheckBudgets` / `ChargeBudgets`，
  走 Redis 原子计数（INCRBY + Lua）。仅此路径付网络往返代价。

> ⚠️ **修正（外部审核发现，2026-09）**：上面原文写"INCRBY + Lua **保证检查与扣费原子**"，
> 这是错的。两个各自原子的 Lua 脚本**不构成一次原子的准入 + 扣费**。
>
> 实测执行链路（三处分离）：
>
> 1. `plugins/governance/resolver.go:180` 附近 —— `CheckBudgets` 前置检查
> 2. provider 实际调用（耗时、可能失败、可能 fallback 到别的 provider）
> 3. `plugins/governance/tracker.go:159` —— **异步 worker** 里 `ChargeBudgets`，
>    且**扣费失败只记日志**（`t.logger.Error("failed to bill request %s ...")`），
>    不回滚、不重试、不拒绝请求
>
> 检查与扣费分处一次网络调用的两端，中间隔着不确定的时长。并发请求仍可能同时通过检查，
> 请求也可能已发出而扣费失败。**当前设计下它是"有界超支的软额度"，不是硬额度。**
>
> **要做真正的硬额度，需要 reservation 模型（属 D13）**：
> 请求前预扣（reserve）→ 完成后按实际用量结算（settle）→ 未用额度释放（release）。
> 还需明确：fallback / retry / 流式中断各自的计费规则；Redis 不可用时 fail-open 还是
> fail-closed（预付费场景通常必须 fail-closed，与可用性直接冲突，是产品决策）。
>
> **但 Codex 审核中有一条要纠正**：它要求"设计 request_id + attempt_id 幂等"，
> 这个**上游已经实现了**。`tracker.go:132` 的 `tryClaimBilling` 按 RequestID + AttemptNumber
> 去重，注释说明它只对 TERMINAL settlement 去重（流式请求一次 attempt 会多次
> `UpdateUsage`，那些必须全部生效），专门防 success-terminal 与 cancellation-terminal 竞态。
> 这个子项**不需要新做，复用即可**。
>
> **在 D13 定案前，文档中所有"硬额度"字样应读作"待设计的强额度能力"**，
> 不要按现有描述实施。M6 计费依赖此项（见 M6 的"关键约束"）。

#### M3.1 D2 已实测：kvstore 是纯内存，但不需要自建抽象层

`framework/kvstore/kvstore.go` 实测结论：**纯进程内实现**，
`data map[string]entry` + `sync.RWMutex` + cleanup goroutine。
其 `Config` 只有 `CleanupInterval` 和 `DefaultTTL` 两个字段，**没有任何连接配置**——
不是"可配后端"，就是内存。

但**不需要自建抽象层**，因为三样东西都已就位：

1. **接口已定义** — `core/schemas/kvstore.go` 的 `KVStore` 接口只有 4 个方法
   （`Get` / `SetWithTTL` / `SetNXWithTTL` / `Delete`），
   通过 `BifrostConfig.KVStore` 注入（`core/schemas/bifrost.go:38`）。
   注释原文：*"shared KV store for **clustering**/session stickiness; nil = disabled"*——
   上游明确预期有一个共享实现，只是 OSS 没提供。
2. **依赖已存在** — `framework/go.mod:19` 已有 `github.com/redis/go-redis/v9 v9.17.2`，
   引入 Redis 不新增依赖。
3. **连接配置有现成范式** — `framework/vectorstore/redis.go:31` 的 `RedisConfig`
   已是生产级：TLS + CA 证书、cluster mode、pool 与各类 timeout，
   且用 `redis.UniversalClient` 同时支持 `NewClient` 与 `NewClusterClient`
   （`redis.go:1900`、`:1917`）。直接照抄这个结构体。

**落地方式**：实现 `schemas.KVStore` 的 Redis 版，通过 `BifrostConfig.KVStore` 注入。
零侵入，不改内核。

#### M3.2 但硬额度计数器不能走 KVStore 接口

**这是本节最关键的一条。** 上面那个接口不能用来做额度计数，两个独立原因：

**原因一：接口无原子自增。** `KVStore` 只有 `Get` / `SetWithTTL` / `SetNXWithTTL` / `Delete`，
没有 `Incr`。在它上面做计数只能 read-modify-write，多节点并发下必然丢增量——
这恰好是 M3 要解决的问题本身。

**原因二：`SyncDelegate` 的冲突语义与计数器根本不兼容。**
`kvstore.go:64` 的 `SyncDelegate` 接口（`OnSet` / `OnDelete`）注释写明用于
"cross-node replication"，冲突解决是 **last-write-wins**（按 unix nano 时间戳）。

LWW 对配置和开关是正确的，对计数器是**静默错误**：节点 A 记 +100、节点 B 记 +200，
LWW 只留下一个，另一个消失且无任何报错。看起来像现成方案，实际会直接损坏计费。

> OSS 中 `SyncDelegate` **无任何实现**（已 grep 确认；`framework/featureflags/featureflags.go:84`
> 是另一个同名但独立的接口）。它是留给企业版的挂载点。

**因此 M3 走独立路径**：直接用 go-redis 的 `INCRBY` + Lua 脚本，不经 `KVStore` 接口。
这与 4.1 覆写 `CheckBudgets` / `ChargeBudgets` 是同一件事。

> ⚠️ 本段原写"（保证检查与扣费原子）"，已删除该断言。Lua 只保证**单次调用内**原子，
> 而检查与扣费是两次调用、分处一次 provider 往返的两端。详见本节 M3 修正块与 **D13**。

**一句话总结 D2**：kvstore 不是 Redis，但省下的不是抽象层而是依赖和配置代码；
`KVStore` 接口用于 M2 与会话粘性，**不用于 M3 计数器**。

### 4.1 GovernanceStore 接管方式：嵌入，不要重写

`GovernanceStore` 接口约 40 个方法（`plugins/governance/store.go:130`）。
从零实现是陷阱——上游加一个方法你就编译不过。

**正确姿势：Go 结构体嵌入 `*LocalGovernanceStore`，只覆写必要方法。**
这是代码明确邀请的，`store.go:186` `HolderLimits` 注释原文：

> This is the seam a deployment reimplements to fund requests from something
> other than a key. Nothing downstream asks what kind of holder answered.

且 `LocalGovernanceStore` 提供 `onBudgetsReset` / `onRateLimitsReset` 钩子，
注释写明 *"Reset hooks allow **wrappers** to observe request-time local resets"*。

实际只需覆写 4 处，其余全部继承：

| 方法 | 覆写目的 |
|---|---|
| `ResolvePermits` | 把租户身份解析进来 |
| `HolderLimits` | 改为由租户/组织付费，而非 VK |
| `CheckBudgets` | 硬额度走 Redis 原子检查 |
| `ChargeBudgets` | 硬额度走 Redis 原子扣费 |

### M4. 管理面 RBAC

OSS 版**没有 roles / permissions 表**。直接证据——`tables/notification.go:5` 注释：

> RoleIDs is JSON rather than a foreign-key relation because **enterprise roles are
> supplied by an optional overlay and do not exist in every OSS database**

注意区分两层，不要混做：

- **数据面资源授权**：`framework/grant/` 已完整实现（provider / model / MCP tool 的
  allow-deny 组合、key 白名单、通配符）。**直接复用，不要重写。**
- **管理面 RBAC**：谁能建 key、谁能看日志原文、谁能改预算、谁能拉租户。**这是要自研的部分。**

顺着上游预留的挂载点长，比另起一套更贴合：
`GovernanceData.Users`（注释标 `enterprise-only`）、`BusinessUnitGovernance`（已定义未实现）、
`notification.RoleIDs`。

### M5. 审计日志（D6 已决策）

已 grep 全库 `audit`，**无审计表**，命中项均为无关词。等保 / ISO27001 / SOC2 的硬门槛。

**D6 决策：与请求日志分离存储**——不同保留期、不同访问权限、不同数据模型。

#### M5.1 存储后端：关系库为准，不用 ClickHouse

原建议写的是"可复用 ClickHouse 但独立表"，**实施时应改为与 configstore 同一关系库
（Postgres）**。原因是原子性，这一点比存储成本重要。

`ConfigStore` 接口已暴露 `ExecuteTransaction(ctx, fn func(tx *gorm.DB) error) error`
（`framework/configstore/store.go:554`），`rdb.go` 内已有 **57 处** `Transaction(func...)` 使用。
接口里还有方法直接接受 `tx *gorm.DB` 参数（`ReplaceVirtualKeyProviderConfigs`、
`DeleteModelConfigsForScope`、`GetModelConfigsForScope`），
说明"调用方传入事务以组合多个写操作"是**上游既有范式**，不是我们发明的用法。

把审计写入放进**与被审计操作同一个事务**，得到两个保证：

- 配置改了但审计没记 → 不可能（同事务回滚）
- 审计记了但配置实际没改 → 不可能（同上）

若审计落在 ClickHouse（独立系统），这两条都无法保证：配置提交成功、审计写入失败时
会**静默丢审计记录**，而这恰好是合规审计最不能出的错。

**长保留期的处理**：Postgres 存热数据（建议 12–24 个月），冷数据定期归档导出。
归档目标可用 ClickHouse 或对象存储——`framework/objectstore/` 已存在，
且 `framework/logstore/hybrid.go` 把 payload 卸载到 objectstore 是现成范式可参考。

> ⚠️ **修正（外部审核发现，2026-09）**：上面这套"审计与被审计操作同事务"的论证
> **与本方案的控制面独立自研前提冲突**，两者不能同时成立。
>
> 本文档一方面把控制面定义为独立建设（§6 的独立仓库、自研 API），
> 另一方面要求审计写入与 Bifrost 数据面的 configstore 共用同一个 Postgres 事务。
> 若控制面有自己的数据库和 API，这个跨系统事务**不存在**。
> 上面那两条"不可能"的保证随之失效。
>
> 这暴露的是更根本的问题：**谁是 source of truth 没有定义**。
> 现状是让数据面的 configstore 同时充当控制面数据库，这与"控制面独立"互相矛盾。
>
> **标准解法（属 D11 的一部分）**：控制面数据库 + **transactional outbox**。
> 审计与业务写入在**控制面自己的**事务里同时提交（原子性保证仍然成立，只是换了库），
> 配置变更事件写入同事务的 outbox 表，再由投递器推给数据面（衔接 M2 的总线）。
>
> 需要在 D11 中一并明确数据归属，建议划分：
>
> | 数据 | 归属 |
> |---|---|
> | 租户、用户、角色、账单、审计 | 控制面（自有库） |
> | provider 运行配置、路由策略、额度快照 | 数据面（控制面的只读投影） |
> | 请求用量、成本事件 | 计量/账务侧 |
> | 配置变更通知 | 控制面事务内 outbox |
>
> **本节的技术判断仍然有效**（审计必须与被审计操作同事务、不能落 ClickHouse、
> 归档走 objectstore），只是那个事务的宿主库应是控制面的，不是数据面 configstore 的。

#### M5.2 不可篡改性要落到 DB 权限

"仅追加不可改"不能只靠应用层自觉，三层一起做：

- **应用层**：不提供 UPDATE / DELETE 接口
- **DB 层**：审计表对应用角色只授 `INSERT` + `SELECT`，**不授** `UPDATE` / `DELETE`
- **归档侧**：对象存储开启版本控制或 WORM（若合规要求）

#### M5.3 记录内容与脱敏

记录：操作者（含 `principal_type`，见 5.3）、租户、操作类型、目标对象、前后值 diff、
来源 IP、时间戳、请求 ID（可关联请求日志）。

⚠️ **前后值 diff 必须脱敏**。审计要回答的是"谁改了什么"，
不是把 provider key 明文或租户 BYOK 明文再抄一份进审计表——
那会让审计表本身成为最高价值的泄漏目标。沿用 `Redacted()` 范式（见 3.5.4）。

前端页面骨架 `ui/app/workspace/audit-logs/` 已存在，走 5.1 的 overlay 接缝填充。

### M6. 计费与结算

**最大的空白。** Bifrost 只有"预算"（花超了拦住），没有"计费"（账期、出账、对账）。

缺：账期与账单出具、对账、预付费余额与充值、后付费信用额度、阶梯定价与折扣、
发票与税务、支付网关对接、欠费催缴与争议处理。

**关键约束：计费口径不能建在会漂移的内存计数上**，必须依赖 M3 的共享计数器。
数据源用现成的 `modelcatalog` + `modelpricing` + `pricingoverride`。

**D1 双模带来的额外要求**：逐请求必须记录**由哪类 key 服务**（平台池 / 租户 BYOK），
因为两者计费口径不同（见 3.5 表格）——平台池计 token 成本 + 加价，BYOK 只计服务费。

这个标记要落在请求日志上，且不能靠 `selected_key_id` 反查（key 可能已被删除或改归属，
历史账单必须可重算）。建议在日志行上加一个独立的 `key_ownership` 枚举列，出账时直接读它。

由此派生两张账：**租户账**（向租户收多少）与**平台成本账**（平台向 provider 付多少）。
毛利 = 平台池部分的差额；BYOK 部分平台无 token 成本。对账系统要能分别核这两张。

### M7. 套餐与配额体系

套餐定义月费、每月包含的 USD 额度、允许使用的模型白名单、租户最大并发和超额策略。
模型只决定访问权限，不再为每个模型分配独立 Token 配额；不同模型按实际 Provider 成本
（加平台 markup）从同一个共享额度池扣减，额度耗尽后按策略拒绝或允许继续使用。

### M8. 内容安全与合规（D5：本期降级，不进 Phase 1）

无 guardrails / moderation 插件（README 中 guardrails 明确列为 enterprise 能力）。

**D5 已决策：本期不需要国内合规备案**，因此《生成式人工智能服务管理暂行办法》
所要求的双向内容审核、违规拦截、使用者实名、模型备案**不进 Phase 1**。
本模块从"与 Phase 1 并行"降级到 **Phase 3**。

但**不要从方案中删除**，两个理由：

1. **企业客户仍会要求 guardrails 与 PII 脱敏**，这与备案无关，属于产品能力。
   金融、医疗、政务类客户几乎必然提出。
2. **触发重新提优的条件明确**：若后续要面向国内公众提供服务、
   或客户合同要求等保/行业合规，M8 需立刻回到 Phase 1 并行——
   其周期长于工程实现（备案与测评是外部流程，不可压缩）。
   **决策时点建议放在 Phase 2 结束前复核一次。**

实现方式（届时）：以 plugin 形态落地（`PreHook` 审入 / `PostHook` 审出），
前端骨架 `ui/app/workspace/guardrails/` 已存在，走 5.1 的 overlay 接缝填充。

### M9. 租户自服务门户

`ui/` 是**平台管理员视角的单一控制台**，不是租户视角。见第 5 节。

### M10. 多租户公平性（高并发下的隔舱）

Bifrost 数据面性能本身是强项（fasthttp、`sync.Map` 无锁读、logstore hybrid、
`framework/streaming/` 流式聚合、多级缓存），吞吐不是瓶颈。

真正缺的是**租户间隔舱**：无准入控制、无分级队列。单一大客户打满会拖累所有租户
（noisy neighbor），且套餐分档想给高价客户更高优先级没有承载机制。

**D1 定为双模后，本模块的优先级上升。** 平台池是明确的跨租户共享资源，
provider 侧限额成为共享资源，公平性不再是"可选优化"而是"平台池的正确性前提"。

两个落点：

1. **平台池 key 选择的租户公平性** — 自研租户感知的 `schemas.KeySelector`，
   按套餐档位分配权重。接缝见 3.5.3（`core/schemas/bifrost.go:16`、`core/bifrost.go:4690`）。
   上游只内置 `WeightedRandom`，这是官方留的可插拔点，不需要改内核。
2. **准入控制与分级队列** — 租户级并发上限 + 按套餐档位的排队优先级。
   这部分无现成接缝，需在自研 transport 层实现。

> ⚠️ **修正（外部审核发现，2026-09）**：上面第 2 条写"在自研 transport 层实现"，
> **漏掉了它必须是集群级的**。这是我在 M3 已经抓到、却在本节重犯的同一类错误。
>
> transport 层的队列和并发计数是**进程内、按节点**存在的。多副本部署后，
> "租户级并发上限 10"变成"每节点 10"，N 个节点就是 10N——
> 单一租户通过打到多个节点即可绕过限制。与 M3 的内存计数器问题是同一个形状：
> **本地状态不等于集群状态**。
>
> 需要补充设计：
>
> - Redis 分布式 semaphore 或 token bucket（可复用 M3.1 的 Redis 版 `KVStore`
>   的连接层，但计数同样不能走 `KVStore` 接口——无原子自增，见 M3.2）
> - 节点故障时的租约释放（持有 slot 的节点崩溃，slot 必须能超时回收，
>   否则租户配额被永久占用）
> - 租户级与 provider 级**双重**限流：租户没超，但平台池的 provider 配额可能已满
> - 高优先级租户的调度公平性（避免低优先级租户被完全饿死）
> - 队列满时的行为：拒绝 / 超时 / 降级到小模型，三者的选择是产品决策

注意 BYOK 租户不参与平台池竞争，隔舱策略只作用于平台池流量——
实现时要按 `key_ownership` 分流，别把 BYOK 请求也塞进平台池队列。

---

## 5. 管理后台界面设计

### 5.1 关键前提：UI 也有官方 overlay 接缝

`ui/vite.config.mts:10-43` 实测：

```js
const isEnterpriseBuild = fs.existsSync(path.join(__dirname, "app", "enterprise"));
// ...
alias: {
  "@enterprise": isEnterpriseBuild
    ? path.resolve(__dirname, "app", "enterprise")            // 你的实现
    : path.resolve(__dirname, "app", "_fallbacks", "enterprise"), // 招揽页
  "@schemas": isEnterpriseBuild ? ... : ...,
}
```

`ui/tsconfig.json:19-20` 有对应的 `paths` 映射。

**更重要的是路由骨架已经在 OSS 里了**，只有实现被替换成招揽页。已确认存在的路由：

`workspace/rbac`、`workspace/audit-logs`、`workspace/cluster`、`workspace/guardrails`、
`workspace/scim`、`workspace/alerting`、`workspace/circuit-breaker`、`workspace/edge-control`、
`workspace/governance`、`workspace/logs`、`workspace/dashboard`、`workspace/model-limits`、
`workspace/custom-pricing`、`workspace/observability` 等 30+ 个。

fallback 实现形如 `ui/app/_fallbacks/enterprise/components/rbac/rbacView.tsx`——
渲染一个 `ContactUsView`（"Unlock roles and permissions… part of the Bifrost enterprise license"）。

**结论：不要新建前端工程。** 把实现放进 `ui/app/enterprise/`，
按 fallback 的同名路径与导出签名对齐，这些页面自动点亮。
`ui/app/enterprise` 上游是 symlink 挂载（vite config 注释提到 preserve symlinks），
你可以让它指向自研前端仓库的一个目录。

已有的 Vite + React + Radix + TanStack Router + Monaco 全套组件库直接复用。

### 5.2 两套控制台，不要合并

现有 `ui/` 是**单租户单控制台**假设。商用 MaaS 需要拆成两个视角：

**A. 平台运营控制台**（平台方使用，在现有 `workspace/` 上扩展）

| 页面 | 说明 | 复用情况 |
|---|---|---|
| 租户管理 | 列表、开通、生命周期状态机、停服/恢复 | **新建** |
| 套餐/SKU 管理 | 档位定义、配额、超额策略 | **新建** |
| 计费与账单 | 出账、对账、充值记录、欠费清单 | **新建** |
| 全局用量看板 | 按租户/模型/时段的成本与调用 | 扩展 `workspace/dashboard` |
| 平台密钥池 | provider key 管理（平台自持部分） | 复用 `workspace/providers` |
| RBAC 管理 | 角色、权限、成员 | **填充** `workspace/rbac` 骨架 |
| 审计日志 | 查询、导出 | **填充** `workspace/audit-logs` 骨架 |
| 内容安全策略 | 审核规则、拦截记录 | **填充** `workspace/guardrails` 骨架 |
| 集群状态 | 节点、配置下发版本、总线健康 | **填充** `workspace/cluster` 骨架 |
| 模型纳管 | 自建推理引擎接入 | 扩展 `workspace/model-catalog` |

**B. 租户自服务门户**（租户使用，新建独立入口）

需要的页面：API Key 自管、用量与成本查询、账单与充值、成员邀请与角色分配、
预算与限流自配置、日志查询（仅本租户，受日志可见性策略约束）、模型列表与配额、文档与示例。

**设计要点**：租户门户与平台控制台**共享组件库但独立路由与鉴权**。
不要用同一套页面靠权限开关切换视角——权限判断散落在组件里是跨租户泄漏的常见来源，
且两者的信息架构本就不同（平台看横向对比，租户看纵向明细）。

### 5.3 共享部署下的安全约束（D4 已决策）

**本期决策：租户门户与平台控制台共享部署**（同进程、同源），不做独立部署。

这个选择降低了交付与运维复杂度，代价是**失去了网络层的隔离边界**——
原本可以靠"平台控制台不对公网开放"这一条兜住的风险，现在必须全部在应用层解决。
以下四条从"建议"升级为**硬性要求**：

**1. 路由级鉴权必须在服务端强制，不能只靠前端隐藏。**
共享部署下同一个构建同时服务两类用户。前端把平台管理菜单隐藏起来是**不够的**——
若对应的 API 路由未做服务端校验，租户拿到路径就能直接调用。
要求：平台管理类 API 路由全部显式校验"调用者是平台管理员"，
且默认拒绝（未标注的路由视为平台级，不对租户开放）。

**2. 两类会话必须可区分且不可提权。**
同源下租户会话与平台管理会话的 cookie 共存，必须在会话内携带
`principal_type`（platform_admin / tenant_user）与 `tenant_id`，
且服务端每次校验都以会话中的这两个字段为准，**不接受任何来自请求参数的租户身份**。
这一条与 3.6 的控制面链路是同一个约束：`QueryScope` 的 tenant 只能来自登录态。

**3. CSRF 与同源风险上升。**
同源意味着租户页面与管理接口共享 origin，浏览器不再提供跨站保护。
要求：状态变更接口全部要求 CSRF token；管理类操作额外要求二次确认或重认证。

**4. 路由前缀分离，便于审计与后续拆分。**
建议 `/portal/*`（租户）与 `/workspace/*`（平台）前缀分离，
API 侧同样 `/api/portal/*` 与 `/api/admin/*` 分离。
这样做有两个好处：网关层可以按前缀加一层粗粒度防护（纵深防御），
且**将来若要改为独立部署，拆分成本接近于零**。

> **建议保留独立部署的可能性**。共享部署适合早期交付，
> 但当出现"平台控制台需限内网访问"或"租户门户需独立扩容"的需求时会回到这个问题上。
> 按第 4 条做好前缀分离，届时是配置问题而非重构问题。

**日志可见性**：`handlers/logvisibility.go` 与 `framework/logstore/visibility.go` 已实现
"谁能看到 prompt 原文"的控制，租户门户的日志页直接接这套，不要另写。

---

## 6. 工程组织：优先依赖，是否 fork 取决于 D11

当前目录**不是 git 仓库**（`git rev-parse` 报 `not a git repository`）。这是好事——
现在就能定下正确的组织形态，避免以后从 fork 里往外拆。

本仓库是**每模块独立 go.mod 的 monorepo**：`core`、`framework`、`transports`、`cli`、
`plugins/*` 各自一个 module。这决定了最优形态：

```
your-maas/                      # 你的仓库
├── go.mod                      # require github.com/maximhq/bifrost/core vX.Y.Z
│                               #         github.com/maximhq/bifrost/framework vX.Y.Z
├── cmd/gateway/                # 自研 transport 二进制（替代 transports/bifrost-http）
├── internal/tenant/            # M1 TenantScopedStore + tenants
├── internal/governance/        # 4.1 嵌入 LocalGovernanceStore 的 wrapper
├── internal/configbus/         # M2 配置下发总线
├── internal/quota/             # M3 Redis 硬额度计数器
├── internal/rbac/              # M4
├── internal/audit/             # M5
├── internal/billing/           # M6 + M7
├── plugins/tenantauth/         # PreAuthHook：VK → 租户
├── plugins/guardrails/         # M8
└── ui-enterprise/              # symlink 到 bifrost 的 ui/app/enterprise
```

**升级路径**：`go get -u` 升版本号，跑测试。不需要 rebase，不需要解冲突。
唯一需要跟进的是上游接口变更（主要是 `GovernanceStore` 加方法），
而嵌入式 wrapper 让这类变更大多无感。

> ⚠️ **修正（外部审核发现，2026-09）**：上面这段**过于乐观**，有两处需要收回。
>
> **第一，原生 `.so` plugin 的版本匹配。** `framework/plugins/soloader.go` 确实
> `import "plugin"`——存在原生 Go plugin 加载路径。Go 的 `.so` plugin 要求主程序与
> plugin **使用完全一致的工具链版本和依赖版本**，任何一方升级都会导致加载失败
> （报 `plugin was built with a different version of package ...`）。
> 若你的部署用到这条路径，升级必须配套兼容性构建与回归测试，不是"升版本号跑测试"那么轻。
> （这条路径在整体架构中的权重我未进一步确认——若你的部署只用编译期集成的 plugin，
> 则不受影响。实施前请确认自己走的是哪条。）
>
> **第二，D11 若选择 fork 路线，本节整体前提改变。** §1 已修正：行级隔离做不到
> "不修改上游文件"。若 D11 定为"维护带最小补丁的 fork"，那么升级就**确实需要
> rebase 和解冲突**，本节描述的无痛升级不再成立。届时需要的是：
> 补丁集尽量小且集中（理想情况只有 struct 加字段 + DDL）、
> 用 `replace` 固定到自己的 fork、上游升级走"rebase 补丁集 + 全量回归"流程。

**唯一例外**：UI 需要在 bifrost 的 `ui/` 目录内构建（因为 `@enterprise` 别名指向
`ui/app/enterprise`）。用 symlink 把自研前端挂进去，源码仍在你的仓库。

---

## 7. 实施阶段

### 7.0 功能模块完成情况速览

第 4 节列出的新增功能模块（M1–M10）+ Phase 1 第 4 项，当前完成度一览。
细节见各模块自己的进度小节。M1–M10 已按本期确认范围完成；表中列出的支付、
税务、生产合规、自定义角色和高级调度等内容属于后续增强，不影响本期完成状态。

| 模块 | 状态 | 说明 |
|---|---|---|
| 完整 MaaS + Bifrost Compose 部署 | ✅ 已完成 | `docker-compose.yml` 已启动 Postgres、Redis、`maas-api`、`maas-ui` 和 `maas-gateway`；`cmd/maas-gateway` 复用完整 Bifrost HTTP Server/Dashboard，并把 `maas-tenantauth` 注册到真实数据面请求链。Bifrost config/logs 分库且运行角色无 `SUPERUSER`/`BYPASSRLS`，网关启动执行 MaaS migrations、indexes 与 RLS preflight。 |
| Phase 0 三项验证 | ✅ 已完成 | governance wrapper（6测试）、`KeyScopeFor` 三方隔离（10测试）、56 表归属审计，见 §7 开头 |
| Redis 版 `schemas.KVStore` | ✅ 已完成 | 18 个顶层测试全绿（含 `-race`），见 §7.1；Bifrost HTTP Server 已提供 `KVStoreFactory`，`maas-gateway` 在 Core、插件和 handler 初始化前注入 `internal/rediskv`，多节点会话粘性、协调租约和一次性 transport 状态均使用 Redis。 |
| **M1 租户底座** | ✅ 本期已完成 | 已交付（§7.2–7.4）：RLS 启动自检、租户绑定三函数（`RunInTenantTx`/`RunAsPlatform`/`RunAcrossTenants`）、`tenants` 表 + 生命周期状态机、迁移原语/runner、复合外键 23 条 + `SET NOT NULL` 收紧、R10 租户前导复合索引（`internal/migrate/indexes.go`）、VK → 租户的 `HTTPTransportPreAuthHook`（`internal/tenantauth` + `plugins/tenantauth`）、`TenantScopedStore` 的 fail-closed 接缝（`internal/tenant/store.go`）。`cmd/maas-gateway` 已在真实 Bifrost 请求链注册该 hook，并在启动时执行迁移、索引和 RLS preflight。真实存量数据的逐表回填规则及新增上游接口 wrapper 覆盖作为部署与持续维护项推进。 |
| M2 配置下发总线 | ✅ 本期已完成 | `internal/configbus` 已交付 generation + transactional outbox、Redis pub/sub 加速通知、节点侧 generation fencing/reconciliation；`internal/virtualkey` 与 `internal/bifrostprojection` 已把 Key 创建/撤销幂等投影到 Bifrost ConfigStore 和治理内存缓存，并在新网关节点首次对账时从 MaaS 真相源恢复已激活 Key。其他配置类型和部署告警属于后续扩展。 |
| M3 强额度共享计数器 | ✅ 本期已完成 | `internal/quota` 已交付 Redis 原子 `INCRBY`、固定窗口 TTL、幂等扣费；本期采用 D13 中间档，语义为有界超支的软额度。完整 reservation 和 `ChargeBudgets` outbox 重试属于后续硬额度增强。 |
| M4 管理面 RBAC | ✅ 本期已完成 | `internal/member`、`internal/authn` 已交付 bcrypt 成员凭据、Postgres 持久化 session、`owner/admin/developer/viewer` 系统角色、最后一个 owner 保护和成员禁用即时失效；Admin/Portal 路由同时校验 principal 类型与 CSRF。请求正文使用独立的 `request_log.read` 权限，仅默认授予 Owner/Admin/Developer，Viewer 不可见。自定义角色、SSO/MFA 和设备会话管理属于后续增强。 |
| M5 审计日志 | ✅ 本期已完成 | `internal/audit` 已交付控制面追加模型、`AppendTx` 原子写入、递归凭据脱敏、租户/平台查询；DB 角色权限加固、归档/WORM 属于生产部署与合规增强。 |
| M6 计费与结算 | ✅ 本期已完成 | `internal/billing` 已交付幂等 usage、平台池/BYOK ownership 快照、整数微单位、账期、发票/账户和 reservation；`plugins/tenantusage` 已按 request ID + attempt 把实际 token/provider cost/markup 写入账本，并持久化稳定的 `gateway_log_id` 关联 Bifrost 请求日志（旧行兼容 ID 回推）。失败事件自动重放、支付网关、税务发票和催缴/争议流程属于后续商用增强。 |
| M7 套餐与配额体系 | ✅ 本期已完成 | `Plan`/`PlanModel`/`SKU`/`TenantPlan`、默认 Developer 套餐、模型白名单、USD 成本定价和管理 API 已落地；数据面在每次 Provider attempt 前检查共享月额度，并以 Redis money counter 覆盖数据库结算延迟。旧 `PlanQuota` 仅用于迁移兼容。本期交付有界超额软额度，严格 reservation、阶梯价格和降级策略属于后续增强。 |
| M8 内容安全与合规 | ✅ 本期已完成 | `plugins/guardrails` 已交付输入/输出/流式 chunk 的 fail-closed hook 与可注入 checker，完成 D5 所定义的本期降级范围；生产 moderation、PII 规则、备案/测评等外部流程属于后续合规阶段。 |
| M9 租户自服务门户 | ✅ 本期已完成 | `maas-ui` 同源提供平台后台和租户门户；管理端已拆分概览、租户、套餐与定价工作区，租户成员可查看自己的共享额度、可用模型、用量金额、密钥、成员与审计，tenant ID 只从 session 派生。门户另有租户范围的请求日志工作区，用量流水可跳转到请求/响应/路由/错误详情；正文复用 Bifrost LogStore，不复制进账本，递归脱敏并限制响应体大小，日志过期后账单仍保留。平台可创建首个 owner，租户管理员不能创建、授予或修改 owner。支付/充值和自定义角色页面属于后续增强。 |
| M10 多租户公平性（隔舱） | ✅ 本期已完成 | `internal/fairness` 的 Redis 租约 semaphore 已由 `plugins/tenantusage` 接入 Bifrost `PreLLMHook/PostLLMHook`，对每个 Provider attempt 原子执行租户/provider 双上限并在结算时释放，崩溃由 TTL 回收。优先级队列、BYOK 分流和排队策略属于后续调度增强。 |

#### M4-M10 本轮代码交付边界

M4/M9 已形成可运行成员与门户闭环：租户成员凭据、固定系统角色、持久化 session、Admin/Portal API 以及双入口 UI 已接通。M6/M7/M10 已由 `plugins/tenantusage` 进入真实 Bifrost Provider attempt 生命周期，执行月额度检查、Redis 并发租约、实际 token/cost 幂等结算和共享 counter 扣减。没有把软额度虚标成硬额度：请求前只知道历史用量，一个大响应仍可能有界超额；严格零超支需要后续 reservation。Redis KV 已通过 Bifrost 运行时工厂注入；仍待接入的是计费失败自动重放、支付/税务、优先级排队、BYOK 分流和生产合规能力。

**决策与交付状态**：D11 已决策，M1 已完成本期范围。M2 / M3 已按 §9.3 / §9.4 的
D12 / D13 建议方案完成本期实现；两项建议仍需正式确认为架构决策，但不再阻塞当前交付。

### Phase 0：验证 ✅ 已完成

三项并行执行，全部通过。代码在 `../maas/`（sibling 目录，
以 module 依赖引入 Bifrost，验证了 §6 的"依赖而非 fork"形态）。

| # | 交付物 | 结果 |
|---|---|---|
| 1 | 嵌入 `*LocalGovernanceStore` 的 wrapper，覆写 `HolderLimits` | `internal/governance/tenantstore.go`，6 测试全绿 |
| 2 | `KeyScopeFor(tenantID)` + 三方隔离测试 | `internal/tenant/keyscope.go`，10 测试全绿 |
| 3 | 56 张表逐表租户归属审计 | [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md) |

**最有价值的一行**是 `tenantstore.go` 里的编译期断言：

```go
var _ upstream.GovernanceStore = (*TenantGovernanceStore)(nil)
```

它编译通过即证明"嵌入 + 覆写 4 个方法"满足了全部 ~40 个接口方法。
上游若新增方法，这里**编译失败**而不是静默退化成半实现——这就是 4.1 主张的可验证形式。

**已验证的行为**（每条都有对应测试钉住）：

- ctx 无租户时，wrapper 与嵌入层输出完全一致 → 可在租户解析未接入前先行部署
- 租户限额是**追加**而非替换：key 自身限额仍生效，任一方耗尽即拒绝
- `TenantLimits` 为 nil 时不 panic，退化为嵌入层行为 → 灰度期安全
- `KeyScopeFor` 空租户返回 error 而非 `(nil, nil)` → fail-closed，不会退化成"无限制"
- 无 scope 的 ctx 确实返回全部 6 行（跨租户）→ R1 fail-open 的实证

**Phase 0 新发现**：D9（无效 permit 仍计租户账）、R23（`sessions` 无归属列）、
R24（scope ≠ 隔离）。三项均已记入本文档。

### Phase 1：多租户底座（地基，顺序不可颠倒）

> 🚧 **前三项当前被 D11 / D12 / D13 阻塞（外部审核发现，2026-09）。**
> 三个架构级问题未定之前不要开始这三项的编码——它们决定的不是实现细节，
> 而是工程形态、投递语义和计费模型，选错的返工成本远高于先花一轮把它定下来。
>
> | 项 | 阻塞于 | 原因 |
> |---|---|---|
> | M1 | **D11** | 行级隔离如何落地（fork / 旁路表 / RLS / 只读投影）+ 数据归属 + 迁移安全方案 |
> | M2 | **D12** | Streams 的广播语义（每节点独立 group vs 中继 fan-out）|
> | M3 | **D13** | 是否采用 reservation 模型；不采用则只能称"有界超支的软额度" |
>
> **第 4 项（Redis 版 `KVStore`）已完成并注入网关运行时**，见 §7.1。
> 它是 Phase 1 里唯一无前置依赖的实现，同时服务 M2 总线连接层、会话粘性（D8）与
> M10 的分布式 semaphore。
>
> **Phase 0 的成果不受影响**：`KeyScopeFor` 的谓词逻辑与 fail-closed 语义、
> wrapper 的编译期断言、56 张表的审计结论均仍然有效，16 个测试仍全绿。
> 被阻塞的是"如何把 `tenant_id` 落到 56 张表上"，不是"谓词该长什么样"。

1. **M1 租户底座** 🚧→🔨 **D11 已决策，已开工**（见 §7.2、§7.3、§7.4）— 已交付：
   RLS 启动自检、租户绑定、`tenants` 表 + 生命周期状态机、
   迁移原语（加列 / 三种 RLS policy / `sessions` 三列改造）、
   平台模式（D15，`RunAcrossTenants`）；
   剩余：逐表回填（含 quarantine 与逐表归属规则）、`TenantScopedStore` 的全接口接线；
   R10 复合索引和 `PreAuthHook` 已交付。
2. **M2 配置下发总线** 🚧D12 — 没有它多节点配置不一致
3. **M3 强额度共享计数器** 🚧D13 — 没有它计费不可信
4. **Redis 版 `schemas.KVStore`** ✅ **实现、测试与运行时注入均已完成** — 见下 §7.1

#### 7.1 Phase 1 第 4 项：Redis 版 `KVStore` 已完成并注入运行时

代码在 `maas/internal/rediskv/`，**18 个顶层测试全绿（含 `-race`）**。
Bifrost transport 新增 `lib.RuntimeKVStore`（扩展 `schemas.KVStore`，补齐 transport 使用的
`GetAndDelete`、`RegisterDecoder` 与 `Close`）以及 `BifrostHTTPServer.KVStoreFactory`。
`Bootstrap` 在 Core、插件和 handler 持有 KV 之前关闭默认内存 Store、安装外部 Store，并注册
transport decoder。`cmd/maas-gateway` 通过该工厂创建 `internal/rediskv.Store`，且有
`var _ lib.RuntimeKVStore = (*rediskv.Store)(nil)` 编译期断言；因此当前运行时实际使用 Redis，
不再只是接口实现和契约验证。

| 文件 | 内容 |
|---|---|
| `config.go` | 配置与客户端构建，镜像 [`framework/vectorstore/redis.go`](framework/vectorstore/redis.go) 的 `RedisConfig`（同样的 `SecretVar` 字段、TLS/CA 固定、cluster 双模、RESP3、连接池），额外加 `OpTimeout` 与 `KeyPrefix` |
| `store.go` | 4 个 `schemas.KVStore` 方法 + 原子 `GetAndDelete`、最长前缀 `RegisterDecoder`、`Ping` / `Close`；含 `schemas.KVStore` 编译期断言 |
| `conformance_test.go` | 9 项，**同一套断言同时跑内存版与 Redis 版**，并覆盖一次性原子读取与注册 decoder 后的类型恢复 |
| `crossnode_test.go` | 6 项，跨实例（= 跨进程）行为 |
| `wireformat_test.go` | 3 项，与上游 gossip 编码的字节级一致性 |
| `cmd/maas-gateway/main.go` | 通过 `KVStoreFactory` 注入 Redis Store；默认复用 `MAAS_REDIS_ADDR/PASSWORD`，可由 `MAAS_KV_REDIS_*` 独立覆盖 |

**实现前先读了全部 4 个消费方，三个约束是它们逼出来的，不是设计偏好：**

1. **未命中必须返回 `kvstore.ErrNotFound`（上游 sentinel），不能自定义。**
   [`complexitysession.go:52`](plugins/routing/complexitysession.go#L52) 用
   `errors.Is(err, kvstore.ErrNotFound)` 区分"新会话"与"存储故障"。返回自己的
   sentinel 会把每次缓存未命中变成请求失败，**而且只检查 `err != nil` 的测试发现不了**。
2. **`Get` 返回 `[]byte`（JSON），不重建 Go 类型。** `any` 无法跨进程携带类型信息；
   而全部消费方都已写好 `[]byte` 分支并 unmarshal 兜底
   （[`core/bifrost.go:9336`](core/bifrost.go#L9336)、
   [`complexitysession.go:131`](plugins/routing/complexitysession.go#L131)、
   [`complexitykvwarmcoordinator.go:125`](plugins/routing/complexitykvwarmcoordinator.go#L125)）。
   上游注释说明了原因：gossip 到达对端时"arrives as the raw JSON bytes ... unless a
   decoder is registered"——**这个形态是上游自己的既有约定**，不是我引入的新约定。
3. **`ttl=0` = 永不过期**，与 `framework/kvstore` 一致。朴素的 Redis 映射会把 0
   当成"立即过期"，方向正好相反。

**最有价值的两个测试：**

- `conformance_test.go` 把内存版与 Redis 版放进同一张表跑**完全相同的断言**。
  只测 Redis 版只能证明"它能用"，证明不了"它可互换"。
  唯一刻意**不**断言的是 `Get` 返回相同 Go 类型——内存版返回原值、Redis 只能返回字节，
  这是任何跨进程实现都不可能有的性质，断言它等于断言一个假命题。
  所以每条断言都走消费方的解码逻辑，那才是真实契约。
- `TestCrossNode_InMemoryGivesTwoWinners` **故意钉住坏行为**：两个独立内存版 store
  对同一个 claim key 都返回 `true`，彼此互不可见。没有这条，Redis 那侧只证明了
  "Redis 能用"，证明不了"修好了什么"。上游若哪天让内存版支持集群，这条会失败并提示
  该 workaround 可能不再需要。

**跨节点已验证**（这是本项存在的理由，`SetNX` 映射到 Redis 原子 `SET NX`，
而内存版的"原子"只是进程内互斥锁）：

- 4 节点 × 8 goroutine 同时抢同一 key，**恰好一个赢家**
- 会话粘性：3 个节点读到同一个 pinned key（D8 / R15 的验收）
- claim 生命周期：Release 后可被他节点重新获取；租约过期后同样
- `GetAndDelete` 使用 Redis `GETDEL`，并发读取一次性 token/state 时恰好一个调用方成功
- 字节级与上游 `sonic.Marshal` 一致，且能被上游 `SetRemote` 与 `RegisterDecoder`
  正确消费——即**用上游自己的代码验证，而非我复制的解码逻辑**

**当前运行时覆盖**：Core 会话粘性、routing warm coordination、batch/job 租约、Gemini
上传会话、realtime/transport 一次性状态等所有 `RuntimeKVStore` 消费方。M3 额度计数和 M10
并发 semaphore 仍按设计直接使用各自的 Redis 原子/Lua 路径，不经 `schemas.KVStore`。

> 待办（不阻塞，需真实部署验证）：`OpTimeout` 默认 2s 是保守取值。
> `schemas.KVStore` 的方法**不带 context**，没有调用方 deadline 可继承、
> 也无法传播取消，而每次调用都在请求路径上（key 选择、warm claim），
> 所以超时只能由 store 自己兜。实际取值应按线上 Redis 延迟分布回调。

#### 7.2 M1 进度：RLS 启动自检与租户绑定已交付

D11 定案后 M1 开工。按 §9.2.5 的硬性要求顺序，**先交付两件地基**，
它们必须在任何迁移之前就位——否则后续所有隔离测试都可能在"看起来配好了、
实际零隔离"的环境里通过。

| 交付物 | 代码 | 测试 |
|---|---|---|
| RLS 启动自检 | `internal/rls/preflight.go` | 12 项 |
| 租户绑定（`set_config` + 事务生命周期） | `internal/tenant/binding.go` | 9 项（另有 Phase 0 的 10 项仍全绿） |

**全部 117 个顶层测试通过（8 个包，含 `-race`），vet 干净。**
（计数口径：`go test -v` 的顶层 `--- PASS` 条数，不含子测试；各包为
`tests/controlplane` 25 / `tests/migrate` 25 / `tests/tenant` 19 / `tests/rediskv` 16 /
`tests/rls` 16 / `tests/rlsspike` 9 / `tests/governance` 6，另加
`internal/controlplane` 1——见下 §7.5 的目录约定。
早前记录的"82 项 / 5 个包"口径不明，此处以实测重新校准。）

##### 7.2.1 启动自检：`rls.Enforce`

`Enforce(ctx, db, tenantTables)` 在启动时校验 §9.2.3 的三个条件，不满足**拒绝启动**。
之所以必须是显式自检而不是靠测试兜：**这类失效在查询期不报任何错**，
断言 error 的测试天然抓不到。

覆盖的检查项（每项都有对应测试证明它**能抓到**，而非只证明配置正确时通过）：

| 检查 | 抓的是什么 |
|---|---|
| `dialect.not_postgres` | **后端不是 Postgres**——不只是没有 RLS，`dbForUpdate` 还会静默不加行锁（R36） |
| `role.superuser` | 超级用户无视 `FORCE` |
| `role.bypassrls` | 持 `BYPASSRLS` 同样无视 |
| `role.bypass_reachable` | 角色自身干净，但可 `SET ROLE` 到豁免角色——隔离只维持到有人执行 `SET ROLE` |
| `table.not_forced` | RLS 开了但没 `FORCE`，而**运行时角色恰是表 owner**（Bifrost 单 config 双池的默认形态） |
| `table.rls_disabled` | 压根没开 RLS |
| `table.no_policy` | 开了 RLS 但零 policy：fail-closed 不泄漏，但功能坏了且原因不可见 |
| `table.missing` | 表不存在 |
| `tables.empty` | **没传租户表清单**——校验了零张表却报告成功，是最危险的假阳性 |

三个设计取舍值得记下来：

- **一次报出全部问题，不是遇到第一个就返回。** 修这些问题要改 DB 授权和迁移，
  每次重启只学到一个问题会把五分钟的活拖成一下午。
- **租户表清单由调用方传入，不自动发现。** "哪些表属于租户"是产品决策
  （记在 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md)），不是能从 schema 推断的事实。
  传不全本身就是风险，所以"传空"被单独列为一条 finding。
- **方言检查排第一且短路，是上面那条"全部报出"的唯一例外。** 其余每项检查都要读
  `pg_*` 系统表，在别的后端上会以 SQL 错误的形式失败——报出来的是
  `no such table: pg_roles`，一个关于自检自身的错误，而不是一个关于部署的结论。
  同时它被实现为 **finding 而非 `Check` 返回的 error**：连接是健康的、答案是已知的，
  这正是 finding 的语义。`Result.Role` 因此在这条路径上为空，
  错误信息改用方言指代主体，否则会报成"角色 `""` 有问题"。

> **方言那 4 项测试是本仓库唯一故意跑 SQLite 的测试**，与 R34（隔离测试强制 Postgres）
> 不冲突：R34 管的是**验证隔离**的测试，而这 4 项验证的是**拒绝一个无法隔离的后端**，
> 被测对象本身就是"处在错误的后端上"。它们也因此不需要 Postgres 容器。

##### 7.2.2 租户绑定：`tenant.RunInTenantTx`

这是读租户表的**唯一**受支持方式。三个设计决定各自关掉一个静默失效：

**① 自己开事务，不接受外部传入的事务。** 绑定是事务作用域的（`set_config` 的
`local=true`），而在显式事务之外每条语句自成隐式事务——绑定会在设置的那一刻就被丢弃，
后续所有读都看不到租户。事后检测得到，但**让它无法表达更好**，所以事务由本函数开。

**② 不提供会话级变体。** 会话级绑定（`local=false`）在请求结束后**残留在池化连接上**，
下一个请求继承前一个租户的身份。这是跨租户读，不是一个值得放在开关后面的模式。

**③ 无租户时 fail-closed。** 未解析出租户是解析链的 bug，
继续执行会以"无绑定"状态查询——在 RLS 下读作空集而不报错，把 bug 藏起来。

另外两点：

- **`SettingName` 导出为常量**，让 CREATE POLICY 的迁移和运行时绑定共用一个名字。
  若 policy 读 `app.tenant_id` 而运行时设 `app.tenant`，**不会有任何报错**——
  policy 只是匹配不到，每次租户读都返回空集，原因不可见。
- **绑定后回读校验**（`bind` 内）。多一次往返，换来把"绑定静默缺失"整类故障
  （名字写错、pooler 重置了状态、调用方不在真事务里）从**错误答案**变成**错误**。
  考虑到那个错误答案是跨租户读或空集，这一次往返值得。
- **`RunAsPlatform` 是独立命名的函数**而非 `RunInTenantTx` 的 flag，
  让"跨越隔离边界"在调用点可见、可 grep。它不是万能钥匙：
  在这套 policy 下未绑定事务只能看到 `tenant_id IS NULL` 的平台池行。

##### 7.2.3 已被测试钉住的行为

| 性质 | 测试 |
|---|---|
| 三方隔离：A 看到 {A 的行} ∪ {平台池}，绝不含 B | `TestRunInTenantTx_ThreePartyIsolation` |
| **绑定不残留在池化连接上**（`maxConns=1` 强制复用同一物理连接） | `TestRunInTenantTx_BindingDoesNotLeakOntoPooledConnection` |
| 单连接上连续服务不同租户，各自只见自己的行 | `TestRunInTenantTx_SequentialTenantsOnOneConnection` |
| 24 goroutine / 4 连接池并发，无交叉污染 | `TestRunInTenantTx_ConcurrentTenantsStayIsolated` |
| 回滚不留下绑定 | `TestRunInTenantTx_RollbackDiscardsBinding` |
| **租户 ID 是参数不是拼串**（R32）：含 `'; DROP TABLE ...; --` 的 ID 被原样绑定，表仍在 | `TestRunInTenantTx_TenantIsParameterNotInterpolated` |
| 未解析租户时 fn 不执行 | `TestRunInTenantTx_FailsClosedWithoutTenant` |

> **这些测试强制跑 Postgres，不跑 SQLite**（R34）。SQLite 无 RLS，
> 同样的断言在那里会全部通过却什么都没证明。

> ~~M8 内容安全需在此阶段并行启动~~ — **D5 已决策本期无需国内备案，M8 移至 Phase 3。**
> 复核时点：Phase 2 结束前重新评估一次（触发条件见 M8）。

#### 7.3 M1 进度：`tenants` 表与生命周期状态机已交付

代码在 `internal/controlplane/`，**26 项测试全绿（含 `-race`）**。
放在独立包而非 `internal/tenant`：后者是数据面的隔离原语（context / 绑定 / scope），
这里是控制面自有库的数据模型（D11a → ④）。

| 文件 | 内容 |
|---|---|
| `tenant.go` | `Tenant` 模型、`Status` 五态、`CanServeTraffic` / `TrialExpired` |
| `lifecycle.go` | 状态机（邻接表）、`CanTransition`、三个语义各异的 error |
| `store.go` | `Migrate` / `Create` / `Get` / `Transition` / `StartTrial` / `ListExpiredTrials` |

##### 7.3.1 五个设计决定

**① `StatusDeregistered` 是终态，没有出边。** 复活一个已注销账户不是状态翻转：
它的 `tenant_id` 可能已从租户表清除，翻回 active 会得到一个"存在且能服务、
但数据静默消失"的租户。回来意味着新建租户记录。

**② `StatusRegistered` 可直接到 `StatusActive`，跳过试用。**
先签合同后用产品的客户从来没有试用期，硬走一遍会给付费账户挂上 `TrialEndsAt`。

**③ `CanServeTraffic(now)` 读时钟，而不是只看状态。**
过期试用的存储状态仍是 `trial`——过期由定时任务翻转。只看状态意味着任务一卡死，
过期试用就无限期免费服务，且不报任何错。在判定点读时钟让这个判断不依赖任务的健康度。
同理，`trial` 但 `TrialEndsAt` 为 nil 判为**不可服务**：那是开通流程的 bug，
不是无限期试用——两种读法里这是较不慷慨的那个。

**④ 进入 `trial` 只能走 `StartTrial`，不能走 `Transition`。**
`Transition` 的签名没有地方放结束日期，允许它进 trial 就等于开了唯一一条
"造出无结束日期的试用"的路径。而离开 trial 的 `Transition` **清空** `TrialEndsAt`：
留在 active 行上的旧日期是个活的绊索，它读作"试用已结束"，
任何后来查这个字段而非状态的代码都会停掉一个付费客户。

**⑤ 状态集合在数据库侧也有 `CHECK` 约束，且由 `AllStatuses()` 生成。**
Go 状态机只管经过本包的写入；迁移脚本或一次 psql 会话不经过。
那样写进去的坏状态不是被拒绝的写入，而是一个所有谓词都静默为 false 的租户——
`CanServeTraffic` 拒绝，且无处报错。约束由 `AllStatuses()` 拼出而非另写一份字面量，
两处不会各自漂移。

##### 7.3.2 一个测试自证不成立的记录

`TestTransition_ConcurrentSameTargetHasExactlyOneWinner`（8 goroutine 抢同一次
active → suspended）**原本声称证明了行锁的必要性，实测不成立**：把
`FOR UPDATE` 删掉后它 10/10 仍然通过。原因是短事务下 8 个 goroutine 实际被串行化，
每个败者读到的都已是提交后的状态——它测到的是 API 在并发下的行为，不是锁。

改为由 `lock_test.go` 用**手工交错的两个事务**确定性地证明，并已验证它能被证伪
（删掉 `FOR UPDATE` 后该测试失败，报"第二次加锁读没有阻塞，返回了 `active`"）：

| 性质 | 断言 |
|---|---|
| 第二次加锁读在 tx1 持锁期间**阻塞** | 300 ms 内不返回 |
| 解除阻塞后读到的是**已提交**状态 | `suspended`，而非阻塞前的值 |
| **反事实**：同一时刻的**非**加锁读返回**陈旧**状态 | `active`——这正是无锁 `Transition` 会拿去校验的值 |

第三行是这个测试的重点：无锁时 `Transition` 会第二次批准 active → suspended，
而在 M5 下那是一条重复审计记录，等催缴逻辑挂上这些转换后，还是一次重复副作用。

##### 7.3.3 尚未做的

`tenants` 表本身**没有加 RLS**。它是平台级表，租户门户不应直接读它——
读取要经过 `TenantScopedStore`（尚未交付）。等门户接入时需重新评估：
如果有任何租户可达的代码路径直接查这张表，它就需要自己的 policy，
否则租户可以枚举全部租户。

#### 7.4 M1 进度：迁移原语与 RLS 策略安装已交付

代码在 `internal/migrate/`，**25 项测试全绿（含 `-race`）**。零上游改动——
经 `RunMigration` 钩子，上游注释点名这是给下游用的
（[`postgres.go:81`](framework/configstore/postgres.go#L81)：
*"downstream consumers (e.g. bifrost-enterprise) run their migrations via this hook"*）。

| 文件 | 内容 |
|---|---|
| `tenantcolumn.go` | 加列 / 建索引 / 删列 / `SET NOT NULL` / 未归属行计数 |
| `rlspolicy.go` | 三种 policy 形状的安装与卸载 |
| `sessions.go` | `sessions` 三列改造 + 两条 CHECK 约束 |
| `tables.go` | 27 张 B 类 + `config_keys` + `sessions` 的分类，与 policy 形状的映射 |
| `tenant/binding.go`（扩充） | 平台模式：`RunAcrossTenants` + `PlatformModeSettingName`（D15，见 §7.4.5） |

表清单已与上游实际声明的 **56 个 `TableName()` 值逐一核对**（不是照抄审计文档）。
`prompt_version_messages` 属 A 类、`routing_targets` 属 D 类，二者不在 B 类 27 张内——
这两处是核对时确认的，凭印象容易算错。

##### 7.4.1 文档纠错：`AddColumnIfNotExists` 在我们这里不适用

§3.4 与审计 §8 都要求用 `migrator.AddColumnIfNotExists` 加列。**这个做法在本项目不成立。**

该函数内部调 `stmt.Schema.LookUpField(field)`，字段不在 Go struct 上就返回
`failed to look up field`。它服务的场景是"struct 已声明该字段、只有数据库还没建列"
（`batch_jobs` 那次迁移就是这样）——而我们要加的列**在任何上游 struct 上都不存在**，
那个不存在恰恰是迁移存在的理由。两者正好相反。

所以改用原生 DDL（`ALTER TABLE ... ADD COLUMN IF NOT EXISTS`），仍用 `migrator.New`
登记版本号。`IF NOT EXISTS` 而非 `if !HasColumn` 守卫的理由沿用上游自己的注释：
守卫是 check-then-act，滚动发布下两个 runner 会同时判断"列不存在"并各发一条 ADD，
败者的整个事务被 SQLSTATE 42701 中断。

##### 7.4.2 三种 policy 形状，不是一种

| 形状 | 用于 | 语义 |
|---|---|---|
| `PolicyStrict` | 27 张 B 类 | 只有本租户的行。`tenant_id` 为 NULL 的行**对任何人不可见** |
| `PolicyPool` | `config_keys`（C 类，D1 双模） | 本租户的行 **∪** 平台池（`tenant_id IS NULL`） |
| `PolicyExclusive` | `sessions`（审计 §6.1） | 已绑定→只见本租户；**未绑定→只见平台行**。两层互不可见 |

第三种是被 `sessions` 逼出来的，前两种在那里都不成立：

- 用 `PolicyStrict`：平台管理员的会话行 `tenant_id IS NULL`，而严格谓词对未绑定事务
  求值为假 → **管理员无法登录**
- 用 `PolicyPool`：`tenant_id IS NULL` 是显式析取项 → **每个租户都能读到管理员会话**，
  正是 R16 要求的鉴权边界的反面

##### 7.4.3 spike 的 policy 有一个写入侧的洞（已修）

spike 验证过的谓词是
`FOR ALL USING (tenant_id IS NULL OR tenant_id = current_setting(...))`，**没有 `WITH CHECK`**。
Postgres 在省略时把 `WITH CHECK` 默认为 `USING` 表达式，于是这条 policy
**允许租户 INSERT 一行 `tenant_id IS NULL`**——把自己的 key 写进平台共享池，
而共享池是所有租户都读得到的。

读平台池是特性（D1），写进去不是。修法是让两个子句**不对称**：

```
USING      (tenant_id IS NULL OR <bound>)   -- 读：本租户 + 平台池
WITH CHECK (<bound>)                        -- 写：只能是本租户
```

spike 的 9 项测试全是读，所以看不到这一点。
`TestPolicyPool_TenantCannotWriteIntoPlatformPool` **把反事实一起钉住了**：
测试后半段重新装上 spike 的原谓词，同一条 INSERT 就成功，且另一个租户随即能读到那行。
没有这半段，只能证明"现在拦住了"，证明不了"拦住的是一个真实存在的洞"。

##### 7.4.4 本包自己交付过一个静默 bug（已修，附回归测试）

`PolicyExclusive` 的第一版用 `current_setting(...) IS NULL` 判断"未绑定"。**这是错的。**

事务级 `set_config`（`local=true`）在事务提交时**不会把变量恢复成 NULL，而是留下空串**，
然后这条物理连接被还回连接池。PG16 实测：

```sql
BEGIN; SELECT set_config('app.probe','t-a',true); COMMIT;
SELECT current_setting('app.probe', true) IS NULL;   -- f，值是 ''
```

后果：`IS NULL` 只在**从未服务过租户的连接**上读作"未绑定"。在复用连接上它读作假，
于是走进已绑定分支、按 `tenant_id = ''` 过滤、匹配不到任何行——
**平台管理员在冷节点上能登录，等这条连接服务过一次租户请求后就静默返回零会话。**
故障与请求拿到哪条池化连接有关，不可复现。

修法是把"未设置"归一到一个表示：全部谓词改为与 `''` 比较
（`coalesce(current_setting(...), '')`），并让已绑定分支额外要求非空，
这样陈旧空串连 `tenant_id = ''` 的行也匹配不到。

> **这个 bug 最初是被测试顺序碰巧撞出来的**（租户读恰好排在平台读之前，脏了那条连接）。
> 靠运气发现不是性质，所以补了两条 `maxConns=1` 的确定性回归测试，
> 并验证它们能被证伪——把 `coalesce` 改回 `IS NULL` 后两条都失败。
> 其中一条还先断言"残留确实存在"，这样哪天驱动开始重置会话状态，
> 失败的会是那条前置断言并说明原因，而不是测试悄悄不再覆盖任何东西。

##### 7.4.5 D15：平台级读在严格 policy 下是空集 ✅ 已决策并交付

**决策：第 3 条（显式平台模式会话变量）。已实现，7 项测试。**
下面先记录问题本身，再记录实现与它的边界。

`PolicyStrict` 让未绑定事务在 B 类表上**什么都读不到**（不是"只读到平台行"——
严格谓词没有 NULL 那一支）。而上游有大量平台级读依赖裸 `s.DB()`，
已核实的一处是
[`GetGovernanceConfig`](framework/configstore/rdb.go#L6296)：

```go
s.DB().WithContext(ctx).Select(teamSelectWithVKCount).Find(&teams)
s.DB().WithContext(ctx).Find(&customers)
```

它是数据面启动与 reload 时加载治理配置的入口，不带任何租户上下文。
装上严格 policy 后它返回**零行**，即数据面以"没有任何 VK、没有任何预算"的状态启动。
方向上是 fail-closed（请求被拒而非串租户），但功能上是全量中断。

同类还有对账循环、账单汇总、迁移自身。运行时角色按 §9.2.5 硬性要求
**不能持 `BYPASSRLS`**，所以没有现成的绕过路径。

三条候选，各有代价，**建议第 3 条**：

| # | 做法 | 代价 |
|---|---|---|
| 1 | 严格 policy 增加"未绑定即全部可见"的析取项 | 等于把 fail-open 写回 policy，一次漏绑定就是跨租户读。**否决** |
| 2 | 迁移/对账用第二个持 `BYPASSRLS` 的角色，与运行时角色分离 | 隔离最干净，但 Bifrost 每个 store 只暴露一份凭据，需要两份配置文件 + 先迁移再重启的部署步骤（`01_bootstrap.sql` 已把这条记为已知缺口） |
| 3 | 增设第二个会话变量（如 `app.platform_read`），policy 加一支显式平台模式；由 `RunAsPlatform` 设置 | 改动最小、调用点可 grep。但**它不是对抗凭据泄露的边界**——app 角色本来就能自己设这个变量。它把"跨租户读"从隐式变成显式，防的是漏绑定，不是防攻击者 |

**已选第 3 条**，并已认可其性质：它与 `01_bootstrap.sql` 已记录的立场一致
（表 owner 能 `NO FORCE`、能 `DROP POLICY`，所以隔离本就不对抗被盗凭据）。
若日后要求它对抗被盗凭据，必须改走第 2 条——这不是加固第 3 条能达到的。

###### 实现：`app.platform_mode` + `RunAcrossTenants`

变量名与"开"值都导出为常量（`tenant.PlatformModeSettingName` / `PlatformModeOn`），
理由同 `SettingName`：写 policy 的迁移与设值的运行时必须用同一个名字，
不一致**不会报错**——policy 的平台支永不匹配，每次平台级读都返回空集。

**做成第三个具名函数，而不是给 `RunAsPlatform` 加语义。** 这一点是刻意的：
`RunAsPlatform` 之所以是独立函数而非 `RunInTenantTx` 的 flag，
就是为了让跨越隔离边界在调用点可见、可 grep。同样的理由适用于这里——
若让 `RunAsPlatform` 直接设标志，现有每一处"不绑定租户"的调用
（对账循环、迁移、平台读）都会**静默地**从"只见平台行"变成"见全部租户"。
于是分三层：

| 函数 | 绑定 | 在严格表上看到 |
|---|---|---|
| `RunInTenantTx` | 该租户 | 只有该租户的行 |
| `RunAsPlatform` | 无 | **空集**（语义未变，仍是"没有租户过滤"） |
| `RunAcrossTenants` | 无 + 平台模式 | 全部租户的行 |

谓词里两处细节是决定，不是写法：

- **平台支要求 `unbound`**，所以已绑定租户的事务无论标志如何都受该租户谓词约束。
  矛盾组合解析到**更窄**的一侧，不是更宽的一侧。
- **`WITH CHECK` 也带平台支**，因为回填是 `UPDATE tenant_id`：
  一个只读的逃生口能找到未归属的行却改不了它们，而改它们正是回填的全部目的。

`RunAcrossTenants` 在 ctx 已有租户时返回 `ErrPlatformModeWithTenant` 而不执行 fn。
持一个租户又要求全部租户是两个矛盾意图，二者都不该被猜。

###### 这个逃生口能否接受，取决于一条测试

`TestPlatformMode_DoesNotLeakOntoAPooledConnection`（`maxConns=1` 强制复用同一物理连接）。
标志是事务级的，所以提交后那条连接必须回到"跨租户可见性关闭"。
若它泄漏，下一个由该连接服务的普通租户请求就带着全部租户的访问权跑
——而与泄漏的租户绑定不同，**这在日志里连"看起来不对"都做不到**。

已验证它能被证伪：把 `set_config` 的 `local` 从 `true` 改成 `false`，该测试失败
（"platform mode must not survive its transaction"）。断言不止查标志本身，
还查可观察后果：那条连接上的未绑定读回到空集，租户读回到只见自己的行。

另外 `TestPlatformMode_PoolTableStillRefusesTenantWritesIntoThePool` 钉住
D15 的析取项**没有重新打开 §7.4.3 那个洞**：平台模式可以写平台池，绑定的租户依然不能。

##### 7.4.6 尚未做的

- **逐表回填**：需要真实数据与产品规则，不能凭空写。`CountUnattributed` 已备好，
  供回填后、`SET NOT NULL` 前校验。未知归属行必须 quarantine，不能猜默认租户
- **约束的线上收紧**：23 条复合外键、`SET NOT NULL` 以及 `RunTightening` runner
  已实现并有 Postgres 测试，但迁移集会在首个未完成回填表处停止；生产库尚未执行
  这些强约束，直到产品侧完成逐表归属确认
- D15 已解除阻塞，回填与对账现在有可用的平台级读写路径（`RunAcrossTenants`）
- **R10 的复合索引**：`internal/migrate/indexes.go` 已提供独立 `RunIndexes` 迁移，
  覆盖 27 张 B 类表、`config_keys` 与 `sessions`。实际部署仍需按线上表规模评估
  `CREATE INDEX CONCURRENTLY` 和窗口安排；逐表回填与复合外键仍需真实数据规则

#### 7.5 测试目录约定：`tests/<包名>/`

测试文件从 `internal/<包>/` 移到 `tests/<包>/`，与实现代码分开。
模块路径同时从 `github.com/luojinghua50/Joysteed-MaaS-phase0` 改为 `github.com/luojinghua50/Joysteed-MaaS`。

```
internal/tenant/binding.go          tests/tenant/binding_test.go
internal/migrate/rlspolicy.go       tests/migrate/policy_test.go
```

**16 个文件移出，1 个文件留下。** 留下的那个是
`internal/controlplane/lock_test.go`——这不是偏好，是 Go 的硬约束：
它调用未导出的 `lockTenant`（§7.3.2 那条锁的证明），而未导出标识符
只对同目录同包可见。移出去只有两条路：把 `lockTenant` 导出，或者
删掉这条证明。两者都是用真实的封装性/覆盖率去换目录整齐，不值得。

其余 16 个之所以能移，是因为它们只用导出面。其中 5 个原本声明为
`package X`（白盒），移动时改成 `package X_test` 并给标识符加了包前缀;
另外 11 个本来就是 `package X_test`，纯位移、内容零改动。

**这个布局有一个必须知道的代价：`-cover` 默认失效。**

```
go test ./tests/tenant/ -cover                        # coverage: [no statements]
go test ./tests/tenant/ -cover -coverpkg=./internal/... # coverage: 52.3% of statements
```

默认的 `-cover` 统计"被测包自身"的语句，而 `tests/tenant/` 里没有语句——
被测代码在 `internal/tenant/`。所以此后**测覆盖率必须带 `-coverpkg=./internal/...`**，
否则会读到一个看起来正常、实则恒为空的数字。这比覆盖率偏低更危险，
因为它不报错。

`go test ./...` 不受影响，仍然跑全部 117 项。

#### 7.6 M2/M3：配置收敛与共享用量计数原语已交付

本轮交付把 7.0 中两个仍为设计状态、但契约已经足够明确的底层模块落成。它们
没有把尚未接好的业务调用点伪装成完成：M2 的 reload 回调由部署层注入，M3 的
计数器只负责完成后的共享扣费，不能被当作预付费 reservation。

| 文件 | 内容 |
|---|---|
| `internal/configbus/outbox.go` | `config_generations` + `config_change_outbox`，同事务分配租户 generation 并写 durable change；`PublishAndNotify` 先提交再发加速通知 |
| `internal/configbus/reconcile.go` | 仅按 generation 对账；stale/duplicate 通知丢弃，reload 失败不推进 fence；周期 runner 带随机初始抖动 |
| `internal/configbus/redis.go` | Redis pub/sub 广播租户与 generation，不携带配置本体；消息丢失由数据库对账兜底 |
| `internal/virtualkey/` | MaaS 自有 `tenant_virtual_keys` 真相表、AES-GCM 可恢复密文、一次性明文返回、`PENDING/ACTIVE/FAILED/REVOKING/REVOKED` 状态机和幂等投影器 |
| `internal/bifrostprojection/` | 在租户绑定事务内写 Bifrost `governance_virtual_keys`，首次 INSERT 原子携带 `tenant_id`，复用 Bifrost secret hook 二次加密并更新治理内存缓存 |
| `cmd/maas-gateway/` | 消费 Redis 通知并每 15 秒按 generation 对账；新节点首次观察租户时从 MaaS 真相源回放已激活 Key，修复冷启动空缓存和 Bifrost 侧漂移 |
| `internal/quota/counter.go` | Redis Lua 原子 `INCRBY`，首次写入设置固定窗口 TTL；`ChargeOnce` 将 request/attempt 幂等 claim 与扣费放在同一脚本 |

已覆盖的性质包括：outbox 事务回滚不留下 generation 或事件、加速器故障不影响
durable source of truth、generation fencing 不允许旧快照覆盖新快照，以及 Redis
计数脚本的参数/身份校验。Virtual Key 已完成真实 Compose E2E：创建由 `pending` 收敛到
`active`，重启后旧 Key 仍可调用数据面，撤销由 `revoking` 收敛到 `revoked`，Bifrost 行
被删除且旧凭证返回 401。其他配置类型和治理插件的 `ChargeBudgets` outbox 重试仍未接入；
quota/fairness/真实用量回写已在下一节通过 MaaS 自有插件完成。

#### 7.7 M4/M6/M7/M9/M10：成员门户与网关计量闭环

本轮新增 `internal/member` 和 `internal/authn`。成员密码只保存 bcrypt hash，会话 Bearer/CSRF
只保存 SHA-256 摘要；会话位于控制面 Postgres，因此 API 重启和多副本不会使登录失效。
每个租户初始化 `owner/admin/developer/viewer` 四个固定角色。平台管理员创建首个 owner；租户侧
具备 `member.manage` 的用户仍不能创建、授予或修改 owner，并且存储层拒绝移除最后一个 active owner。
成员一旦 disabled，所有尚未过期的现有会话也会在下一次请求时立即拒绝。

`maas-ui` 保持同源部署，但登录入口和渲染表面按 principal 分开。Portal API 不接收 tenant ID，
只从服务端 session 提取作用域，已覆盖成员、Virtual Key、当前套餐/配额、本月用量金额和租户审计。
Admin 路由额外要求 `platform_admin`，关闭了“租户用户持有同名 permission 后调用 `/api/admin/*`
并自行指定 tenant ID”的越权路径。

平台端新增套餐目录与 SKU 定价页面及 `/api/admin/skus` 管理接口。租户状态、成员、套餐创建、
SKU 更新和套餐分配均改为业务变更与 `AppendTx` 在控制面同一事务提交；不再存在业务已提交、
审计追加失败后只能写日志的窗口。租户创建时固定角色、默认套餐和审计也在同一事务完成。
审计 scope 与 actor 分离：平台代租户执行的操作保留 `platform_admin` 操作者，同时携带资源
`tenant_id`，所以平台能查全量记录，租户门户只能查本租户记录。

`plugins/tenantusage` 作为类型化 LLM plugin 注册到完整 Bifrost Server：

```text
HTTP tenantauth -> PreLLM: DB 套餐/模型白名单 + Redis money counter + semaphore
                -> Provider attempt
                -> PostLLM: 释放 lease -> Redis 幂等扣 USD micros -> Postgres 幂等 usage
```

结算幂等键为 `request_id + attempt`，fallback 的物理 Provider 调用分别记账；流式请求只在 final
chunk 结算，取消/超时使用 Bifrost `BilledUsage` 记录已被上游消耗的 token。Provider 未报告成本时，
回退到 MaaS `SKU` 的输入/输出每百万 token 微单位价格，再按 basis points 计算 markup；
Provider 成本和 markup 的总和从租户本月共享 USD 额度扣减。
并发租约在长请求执行期间按 TTL 的三分之一自动续期；正常结束主动释放，进程崩溃后仍由 TTL 回收，
避免流式响应超过初始租期后被提前腾出并发槽。

当前额度语义仍是 D13 中间档：准入时用数据库账本和 Redis 当月 counter 的较大值，一个已获准请求
可能在响应后跨过剩余额度。它保证后续请求被拒绝，不保证单请求零超支。严格预付费必须在请求前按
`max_tokens` reservation，响应后 capture/release；在那之前不得把该能力销售为硬额度。

### Phase 2：能收钱

**M6 计费结算** + **M7 套餐配额** + **M9 租户自服务门户**。

### Phase 3：能过审 / 能规模化

**M4 管理面 RBAC** + **M5 审计日志** + **M8 内容安全**（D5 降级至此）+ **M10 租户隔舱**。

> M4/M5/M8 的前端骨架已存在（`workspace/rbac`、`workspace/audit-logs`、`workspace/guardrails`），
> 走 5.1 的 overlay 接缝填充，实际工作量小于新建页面。

⚠️ **M4 在共享部署下优先级上升**。D4 定为共享部署后，平台管理与租户门户同源，
失去了网络层隔离边界（见 5.3）。管理面 RBAC 从"合规需要"变成"隔离需要"——
5.3 的第 1、2 条（路由级服务端强制鉴权、会话不可提权）**必须在租户门户上线前就位**，
不能等到 Phase 3。建议把这两条的最小实现提前到 Phase 2 与 M9 同步交付，
Phase 3 再补完整的角色/权限管理界面。

### Phase 4：商用完善

租户生命周期状态机细化、SLA 与工单、私有化交付（离线包 / license 签发校验）、
配置版本化与回滚（`config_hashes` 只是单 hash，不是版本化）、多区域与容灾、
自建模型纳管（vLLM / SGLang 等）。

---

## 8. 风险登记

| # | 风险 | 影响 | 应对 |
|---|---|---|---|
| R1 | **`queryscope` 默认 fail-open** | 跨租户数据泄漏，静默无报错 | 3.3 的 wrapper 强制校验 + 反射覆盖测试。**最高优先级** |
| R2 | 租户可见读路径漏加作用域 | 同 R1 | 反射测试 + CI 门禁。⚠️ **原表述"禁止新增裸 `s.DB()` 调用"过于粗暴**：`rdb.go:358-361` 的注释明确规定 `DB().WithContext(ctx)` 就是给写入和内部 lookup（如推理路径 VK 鉴权）用的、**必须**绕过 scope。318 处不都是缺陷。需先按"租户可见读 / 平台读 / 写入 / 迁移 / 后台任务"五类分拣，只对第一类强制作用域 |
| R3 | 聚合查询维度泄漏 | 租户 A 在下拉框看到租户 B 的 ID | 用 `DimensionScope`，不要只用 `QueryScope`。上游已提供 |
| R4 | 内存计数器导致计费漂移 | 收入不准，超支 | M3 硬额度走 Redis；软额度可容忍 |
| R5 | 配置下发缺失导致节点状态分裂 | 改配置部分生效，行为不一致 | M2；总线故障要能退化轮询 |
| R6 | 重复实现已有能力 | 工期浪费，与上游偏离加大维护成本 | 动手前查第 2 节复用清单 |
| R7 | 上游 `GovernanceStore` 接口变更 | 编译失败 | 嵌入而非重写（4.1）；锁定版本，主动升级 |
| R8 | 合规要求中途出现（D5 翻转） | M8 周期长于工程实现，无法压缩，直接卡上线 | Phase 2 结束前复核一次；触发条件见 M8。**降级不等于移除** |
| R16 | **共享部署下租户可达平台管理 API** | 越权访问平台级数据，等同全域泄漏 | 5.3 第 1、2 条：服务端路由级强制鉴权 + 会话 `principal_type`；前端隐藏菜单**不算防护** |
| R17 | 同源 CSRF | 借租户会话触发管理操作 | 5.3 第 3 条：状态变更接口强制 CSRF token，管理操作二次认证 |
| R18 | 把 Redis pub/sub 当成配置真相 | 断连期间消息丢弃，若没有持久化兜底会永久停留旧配置 | M2：outbox/generation 是唯一真相；pub/sub 只做加速，定期对账保证有界收敛 |
| R19 | **审计记录与被审计操作不在同一事务** | 配置改成功、审计写失败 → 静默丢审计，合规审计最不能出的错 | M5.1：走 `ExecuteTransaction`，审计与操作同事务；**不要把审计落在 ClickHouse** |
| R20 | 审计表成为泄漏目标 | 前后值 diff 抄了 key / BYOK 明文 | M5.3：diff 必须脱敏，沿用 `Redacted()` |
| R21 | 审计记录被篡改 | 审计失去证明力 | M5.2：DB 层只授 `INSERT`+`SELECT`，应用层不提供 UPDATE/DELETE |
| R22 | BYOK 回落默认开启或全局开关 | 租户预期外账单，计费争议 | 3.5.2：租户级开关且默认关闭；回落必须可观测 |
| R23 | **`sessions` 表无归属列**（Phase 0 实测） | D4 共享部署下租户会话与平台管理会话不可区分 → R16 的实证通路 | 加 `principal_type`+`tenant_id`+`user_id`，**Phase 1 必做**，见 `MAAS_TABLE_AUDIT.md` §6.1 |
| R24 | 误以为注册 `tenant` scope 就等于隔离 | `governance_model_configs` 等表漏加 `tenant_id`，跨租户可见 | scope 表达"作用于谁"，`tenant_id` 表达"属于谁"，两个维度都要，见审计 §5.1 |
| R25 | **按"不改上游"实施行级隔离** | 语言层面做不到；wrapper 拦不住 `GetGovernanceConfig` 那类内部裸查询，隔离形同虚设 | §1 已修正表述；落地形态见 **D11**，定案前不动迁移 |
| R26 | **所有节点共用一个 consumer group** | Streams 组内竞争消费，每条配置变更只有一个节点收到，其余永久停留旧配置且不报错 | M2.0 修正块：每节点独立 group 或中继 fan-out，见 **D12** |
| R27 | **把当前设计当硬额度用于预付费** | 检查与扣费分处网络调用两端、扣费失败只记日志，实际是有界超支的软额度 | M3 修正块：reservation 模型，见 **D13**；未定案前不承诺硬额度 |
| R28 | **子表跨租户引用父表** | A 的子记录指向 B 的父记录，两行 `tenant_id` 各自"正确"，构成隐蔽的跨租户通路 | §3.4 第三条实施要点：`(tenant_id, parent_id)` 复合外键，覆盖 B 类 9 张子表 |
| R29 | **迁移回填统一塞默认租户** | BYOK 密钥 / 用户凭据 / 历史日志错误归属 = 直接的跨租户泄漏 | §3.4 迁移修正块：逐表映射规则 + 无法判定者进 quarantine |
| R30 | 租户并发上限只在节点内生效 | N 个节点 = N 倍配额，单租户多打几个节点即绕过 | M10 修正块：Redis 分布式 semaphore + 租约超时回收 |
| **R31** | **RLS 装了但角色是超级用户 / 持 `BYPASSRLS` / 表 owner 无 `FORCE`** | **全部 policy 静默失效，零隔离，且任何地方都不报错**；容器 Postgres 默认给超级用户，运行时角色又恰是建表 owner（同一份 config），两个坑都极易踩中 | §9.2.3 ③ 实测确认；启动自检三条件，不满足拒绝启动 |
| **R32** | 租户上下文用 `SET` 拼串而非 `set_config()` | `SET` 不接受绑定参数，唯一写法是拼串 → 租户上下文本身成为 SQL 注入点 | §9.2.3 ②：强制 `set_config('app.tenant_id', ?, true)` |
| **R33** | 为省往返把 `set_config` 折进 CTE | Postgres 不保证 CTE 副作用与 RLS 谓词求值顺序；计划一变静默返回错误行数且不报错 | §9.2.4 修正块：只用分开写法，实测已钉住 |
| **R34** | 隔离测试只在 SQLite 上跑 | SQLite 无 RLS，测试全过但生产语义完全不同，隔离从未被真正验证 | §9.2.5：隔离测试强制 Postgres，沿用 `migrations_test.go` 的独立 schema 范式 |
| **R35** | **`ChargeBudgets` 失败只记日志** | [`tracker.go:160-162`](plugins/governance/tracker.go#L160-L162)：不重试、不回滚、不告警 → Redis/DB 一抖动该笔消费**永久不计费且无人知道**，是静默漏收入 | §9.4 修正块：扣费意图写入控制面 outbox 由投递器重试；幂等复用现成的 `tryClaimBilling`（RequestID + AttemptNumber）。**纳入 M3 范围** |
| **R36** | **非 Postgres 后端上 `dbForUpdate` 静默不加锁** | [`governance.go:34-40`](transports/bifrost-http/handlers/governance.go#L34-L40)：非 postgres 方言时**原样返回 db，不加 `FOR UPDATE`，不报错**。调用方拿到一个看起来加了行锁、实际没加的查询。全仓库唯一调用点是 [`governance.go:2064`](transports/bifrost-http/handlers/governance.go#L2064) 的 VK 更新事务——连带 `Budgets` / `RateLimit` / `ProviderConfigs` 一起 preload 后做协调（含决定删哪些行），锁缺失的后果落在**管理面并发改同一 VK**上，不是运行时扣费（那是 R35）。缺锁在别的后端上表现为丢更新还是 busy 错误，取决于该后端自身的锁机制，**此处未实测**。同源的 [`store.go:2764`](plugins/governance/store.go#L2764) `supportsBatchedDump` 只是性能回退，有 fallback，不同级 | §7.2.1 `rls.Enforce` 首查方言，非 postgres 拒绝启动。**注意 `config_store` 与 `logs_store` 独立选型，只配一个另一个仍在 SQLite** |
| **R37** | **物化视图不受 RLS 约束，且无法为其补加 RLS** | 实测（PG16，spike 容器）：底表 `ENABLE` + `FORCE ROW LEVEL SECURITY` + `app.tenant_id` 策略后，**同一 session、同一租户绑定**下——底表只返回本租户行，建在其上的物化视图返回**全部租户的聚合行**。补救路径也被堵死：`ALTER MATERIALIZED VIEW ... ENABLE ROW LEVEL SECURITY` 报 `This operation is not supported for materialized views`；`security_invoker` 对物化视图是 `unrecognized parameter`（只有普通 view 支持，实测普通 view 带 `security_invoker = true` 能正确过滤）。这不是配置疏漏而是结构性的：物化视图是**快照**，读它不重新求值底表策略。上游 [`matviews.go:32`](framework/logstore/matviews.go#L32) 的 `mv_logs_hourly` 及三张 filter 视图，聚合维度里**没有 `tenant_id`**；而 [`visibility.go:27-37`](framework/logstore/visibility.go#L27-L37) 在 postgres 上**优先走**物化视图——即"改用 Postgres"本身打开了这条绕过隔离的读取路径。（实测中视图 owner 恰为 superuser + `BYPASSRLS`，这对 refresh 阶段是个混淆项；但读取侧无法加 RLS 是独立于 owner 的结论，由上述两个 `ERROR` 直接钉住。反过来若 owner 非豁免，`FORCE` 下 refresh 只能捞到 owner 可见行，那是**数据不全**，同样不构成隔离） | 二选一，M1 内定：① `matview_refresh_interval: "off"`（[`postgres.go:26`](framework/logstore/postgres.go#L26)）关掉物化视图，回退到受 RLS 约束的裸查询，代价是聚合查询变慢；② 给四张视图加 `tenant_id` 维度并在读取侧强制附加租户谓词——但视图本身仍不隔离，隔离退化为"每处查询都别忘了加谓词"，正是 R1 的形态。**推荐 ①**。见 D14 |
| R9 | **`key` 表 scope 谓词写错** | 写成不加谓词 = 租户互见 BYOK，**测试不会发现** | 3.5.1：谓词必须走专用构造函数 `KeyScopeFor(tenantID)`，禁止手写；配三方隔离测试。**D1 双模后此项风险等级等同 R1** |
| R10 | 行级隔离的性能退化 | 大租户查询变慢 | `tenant_id` 进联合索引首位；ClickHouse 侧进排序键 |
| R11 | 平台池被单一租户打满 | 影响全部租户可用性 | 3.5.3 + M10：租户感知的 `KeySelector` + 准入控制 |
| R12 | 平台运维可读租户 BYOK 明文 | 租户商业机密泄漏，信任崩塌 | 3.5.4：后台仅展示掩码，沿用 `Redacted()` 范式 |
| R13 | 租户从日志反推平台池 key 构成 | 平台密钥资产暴露 | 3.5.5：`selected_key_id` 按 `key_ownership` 脱敏 + 纳入 `DimensionScope` |
| R14 | **误用 `SyncDelegate` 做额度计数** | LWW 静默丢增量，**计费损坏且无报错** | M3.2：计数器走 go-redis `INCRBY` + Lua，禁止经 `KVStore` 接口或 `SyncDelegate` |
| R15 | 多节点会话粘性静默失效 | 同一会话拿到不同 key/分层，行为不一致 | ✅ 已关闭：Redis 版 `KVStore` 已通过 `KVStoreFactory` 注入 `maas-gateway`（§7.1），`TestCrossNode_RedisSessionStickinessIsShared` 验证 3 节点读到同一 pinned key，工厂生命周期测试验证注入发生在运行时消费方初始化前且默认内存 Store 被关闭 |

---

## 9. 待决策清单

| # | 决策 | 影响面 | 建议 |
|---|---|---|---|
| ~~D1~~ | ~~provider key 归属~~ | ~~数据模型、商业形态~~ | ✅ **已决策：本期两者都支持**，`key.tenant_id` 可空。详见 3.5 |
| ~~D7~~ | ~~BYOK 用尽时是否回落平台池~~ | ~~计费争议、可用性~~ | ✅ **已决策：BYOK 独占**，不静默回落。兜底为租户级开关且默认关闭，详见 3.5.2 |
| ~~D2~~ | ~~`framework/kvstore/` 是否已 Redis 后端~~ | ~~M3 是否需自建抽象层~~ | ✅ **已实测：纯内存，但无需自建抽象层**。接口 / 依赖 / 配置范式均已就位，详见 M3.1。**关键约束：计数器不能走此接口**，见 M3.2 |
| ~~D8~~ | ~~会话粘性是否纳入 Phase 1~~ | ~~多节点行为正确性~~ | ✅ **已决策并已交付**：纳入 Phase 1 第 4 项，实现见 §7.1。M2.2 的验收标准由 `TestCrossNode_RedisSessionStickinessIsShared` 覆盖 |
| ~~D3~~ | ~~总线选型：Redis pub/sub vs NATS~~ | ~~M2 运维复杂度~~ | ✅ **已决策：Redis Streams**（非 pub/sub，非 NATS）。理由见 M2.0；若想暂缓 Redis 见 M2.0.1 的 DB 轮询备选 |
| ~~D4~~ | ~~租户门户是否独立部署~~ | ~~安全边界、发布节奏~~ | ✅ **已决策：本期共享部署**。失去网络层隔离，5.3 的四条约束升级为硬性要求 |
| ~~D5~~ | ~~是否需要国内合规备案~~ | ~~M8 优先级与周期~~ | ✅ **已决策：本期不需要**。M8 降级至 Phase 3，Phase 2 结束前复核（触发条件见 M8） |
| ~~D6~~ | ~~审计日志存储后端~~ | ~~M5~~ | ✅ **已决策：与请求日志分离**。实施细化为 **Postgres（与 configstore 同库同事务）**而非 ClickHouse——原子性要求所致，详见 M5.1；冷数据归档到 objectstore |

| **D9** | 无法解析的 permit 是否仍向租户计费 | 计费正确性 | **Phase 0 实测发现**：租户计费按 ctx 挂载，permit 解析失败时嵌入层返回 0 限额，但租户限额仍被追加——即持无效 key 的请求仍计租户账。当前行为已用测试钉住（`TestHolderLimits_UnknownPermit_TenantStillFunds`），需 M1 明确策略 |
| **D10** | E 类 9 张表的归属 | 迁移范围 | 见 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md) §6，每张已给建议与理由；`sessions`（§6.1）不是决策项而是必做项 |
| ~~**D15**~~ | ~~平台级读在严格 policy 下如何进行~~ | ~~M1 回填与 runner 接线~~ | ✅ **已决策并已交付（2026-09）：第 3 条——显式平台模式会话变量 `app.platform_mode`**。实现见 §7.4.5，7 项测试。已认可其性质：**防漏绑定，不防被盗凭据**（app 角色本就能自行设置该变量），与 `01_bootstrap.sql` 已记录的边界一致。**M1 回填与 runner 解除阻塞** |
| **D14** | **物化视图的处置：关掉，还是加租户维度** | **M1 / 日志面查询性能** | R37 已实测定性：物化视图不受 RLS 约束，且补加 RLS 与 `security_invoker` 都被 Postgres 拒绝。建议 **`matview_refresh_interval: "off"`**——用聚合查询变慢，换隔离仍然 fail-closed；只有当日志量下确实不可接受时才走"加 `tenant_id` 维度 + 读取侧强制谓词"，且那条路必须为每个读取点配隔离测试。**另注**：Postgres logstore 的表**无分区**（`PARTITION BY` 只出现在 ClickHouse 路径 [`clickhousemigrate.go:97`](framework/logstore/clickhousemigrate.go#L97)），保留期清理在规模上是独立问题，不因选 Postgres 而消失 |

### 9.1 P0 阻塞项：D11 / D12 / D13（外部审核发现，2026-09）

这三项与 D1–D10 不同性质：**它们不是"选哪个更好"，而是"当前文档写的做不到"**。
每一项都对应文档里一处已修正的错误，且各自阻塞 Phase 1 的一个交付物。

| # | 决策 | 阻塞 | 候选方案 |
|---|---|---|---|
| ~~**D11**~~ | ~~行级隔离如何落地 + 数据归属 + 迁移安全方案~~ | ~~M1~~ | ✅ **已决策（2026-09）**：**D11a → ④ 控制面自有库 + transactional outbox**；**D11b → ③ Postgres RLS**（用 `RunMigration` 加列，不 fork）。② 旁路映射表已排除。落地硬性要求见 §9.2.5，**M1 解除阻塞** |
| **D12** | **Streams 的广播语义** | **M2** | ⚠️ **D11a 已改变前提，原两个候选均不再需要，见 §9.3**。建议：outbox 为准 + 定期对账兜底 + pub/sub 加速（三层），不用 consumer group |
| **D13** | **是否采用 reservation 模型** | **M3** | ⚠️ **两候选之间还有一档，见 §9.4**。建议：M3 只做中间档（Redis 原子 `INCRBY`），完整 reservation 留到 M6 随预付费交付 |

**D11 是三者中最重的**，因为它同时决定四件事，且彼此牵连：

1. **工程形态** — 选 ① 则 §6 的"无痛升级"不再成立，需要 rebase 补丁集流程；
   选 ③ 则数据面连接需按租户切换 session 变量，与连接池复用冲突；
   选 ④ 则数据面不再直连 configstore，改动面最大但隔离最彻底
2. **source of truth 归属** — 当前让数据面 configstore 兼任控制面数据库，
   与"控制面独立自研"矛盾（见 M5.1 修正块）。建议控制面自有库 + transactional outbox
3. **审计事务宿主** — 跟着第 2 点走，M5.1 的原子性论证仍成立，只是换库
4. **迁移安全方案** — 逐表归属映射 + quarantine + 复合外键建立顺序（见 §3.4 修正块）

**D12 / D13 相对独立**，可以与 D11 并行决策。

> **三项都不影响 Phase 0 已交付的内容**，也没有阻挡已经完成的 Phase 1 第 4 项
> （Redis 版 `KVStore`）实现、契约测试与网关运行时注入，见 §7.1。

### 9.2 D11 路线对比与 RLS 实测结论（2026-09）

#### 9.2.1 前提修正：加列不需要 fork，让上游查询遵守它才需要

§9.1 把 D11 列成四条互斥路线，这个划分本身有问题。核查后发现一个此前判断错的点：

[`framework/configstore/postgres.go:81-82`](framework/configstore/postgres.go#L81-L82) 是上游自己的注释：

> `migrateOnFreshFn`: downstream consumers (e.g. **bifrost-enterprise**) run their
> migrations via this hook on a throwaway pool that closes after fn.

即**下游通过 `RunMigration` + `AddColumnIfNotExists` 给上游表加 `tenant_id`，是上游官方
支持的扩展路径，不构成 fork**。此前把"改表结构"与"改上游代码"混为一谈，是错的。

真正的难点是第二步：列加上了，**上游自己的 Go 代码不会去过滤它**。
[`rdb.go:6296`](framework/configstore/rdb.go#L6296) `GetGovernanceConfig` 就是典型——
裸 `s.DB()` 多表读，wrapper 拦不到。而 [`ScopedDB` 的注释](framework/configstore/rdb.go#L358-L361)
明确写了写入路径与内部查询**按设计**绕过 scope。

**所以 D11 应拆成两个子决策**，四条路线其实在回答两个不同问题：

| | 问题 | 结论 |
|---|---|---|
| **D11a** | 控制面数据的 source of truth 归谁 | 选 ④（控制面自有库 + outbox），见 §9.1 第 2/3 点，基本无替代方案 |
| **D11b** | 数据面共享库的行级隔离靠什么强制 | ③ RLS 与 ① fork 之间取舍，已实测，见下 |

#### 9.2.2 四条路线对比

| | ① fork 最小补丁 | ② 旁路映射表 | ③ Postgres RLS | ④ 控制面为准 + 只读投影 |
|---|---|---|---|---|
| 强制点 | 应用层（改后的 Go 代码） | 应用层（仅 wrapper 覆盖的读路径） | **DB 层（低于应用）** | 架构层（数据面拿不到全量） |
| 覆盖裸 `s.DB()` | 需逐个改，可能漏 | ❌ **覆盖不了** | ✅ 自动覆盖 | ✅ 库里就没有别家数据 |
| 是否仍需加列 | 需要 | 不需要 | **仍需要**（policy 得有列可 filter） | 投影侧需要 |
| 运维复杂度 | 低（运行时无新组件） | 低 | **高**（DB role、policy 迁移、连接模型） | **高**（投影管道、outbox 投递器） |
| 上游同步成本 | **高且永久** | 低 | 低（Go 代码不改） | 低 |
| M5.1 审计事务 | 单库，原论证成立 | 单库，成立 | 单库，成立 | 换控制面库，论证成立 |
| SQLite 开发环境 | 一致 | 一致 | ❌ **无 RLS，语义分叉** | 一致 |

**②不能作为安全边界，排除。** 它避开了上游修改，但没有解决问题：wrapper 只覆盖
`ScopedDB` 读路径，写入与内部查询按设计绕过；上游 insert 与映射表 insert 不在同一事务，
中间态是"行已存在但无归属"，而 [`FromContext` 的 nil = 无限制](framework/queryscope/queryscope.go#L28-L31)
语义下**无归属就是所有人可见**；且无法建外键。它适合做归属标注（计费、报表维度），
不适合做隔离边界。

**①的补丁面比"387 处裸调用"小得多**：按 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md)
分类，需要过滤的是 B 类 27 张 + C 类 1 张 + E 类待定 9 张 ≈ 37 张表的读路径；
A 类 16 张平台表与 D 类 3 张已有 scope 的不动。代价是每次上游发版都要 rebase，
且**漏改一处是静默泄漏**——不报错，只多返回几行，要靠 37 张表各自的三方测试兜住。

#### 9.2.3 RLS 实测结论（Phase 0 spike，9 项行为测试 + 6 项基准全通过）

代码在 `maas/internal/rlsspike/`，走 `framework/postgresconn`（生产连接路径，
含 `ApplyPoolTuning`）连 Postgres 16，非手写 `gorm.Open`。**结论：RLS 可行，但有三个
此前未识别的硬约束，其中第三个是严重运维陷阱。**

**① 确认 RLS 能约束裸查询。** `TestRLS_BlocksRawUnscopedQuery`：一条**不带任何租户
谓词**的 `SELECT`（即上游 387 处调用的形态），在 RLS 下正确只返回
{tenant-a 的 key} ∪ {平台池}，不含 tenant-b。这是选 ③ 的核心依据——上游代码不需要
知道租户存在。

**② 租户上下文必须走 `set_config()`，不能用 `SET`。** `SET LOCAL x = ?` 直接报
`syntax error at or near "$1"`（SQLSTATE 42601）——`SET` 是 utility 语法，不接受绑定参数。
这不是写法偏好问题：**唯一能用 `SET` 的写法是把租户 ID 拼进语句字符串，那会让租户上下文
本身变成 SQL 注入点**。`set_config('app.tenant_id', ?, true)` 是普通函数调用，参数是真参数。

**③ 必须同时满足三个条件，否则 RLS 静默失效。** 这是最重要的发现：

| 角色 | RLS 是否生效 | 测试 |
|---|---|---|
| 超级用户 / 持 `BYPASSRLS` | ❌ **完全不生效，`FORCE` 也不管用** | `TestRLS_SuperuserBypassesEvenWithForce` |
| 表 owner（非超级用户），无 `FORCE` | ❌ 不生效 | `TestRLS_OwnerBypassesWithoutForce` |
| 表 owner + `FORCE ROW LEVEL SECURITY` | ✅ 生效 | `TestRLS_ForceConstrainsOwner` |
| 非 owner 普通角色 | ✅ 生效 | `TestRLS_BlocksRawUnscopedQuery` |

**为什么这条对 Bifrost 特别危险**：[`postgres.go`](framework/configstore/postgres.go#L34-L62)
里迁移池与运行时池由**同一份 config** 构建，所以**运行时角色就是建表的 owner**。
若不加 `FORCE`，policy 装了也等于没装。更糟的是容器 Postgres 默认给的就是超级用户
（本 spike 的 bootstrap 角色即是），此时**连 `FORCE` 都挡不住，全部 policy 形同虚设，
且任何地方都不报错**。这是"看起来配好了、实际零隔离"的典型形态，必须在部署自检里硬性拦住。

**④ 失败方向是安全的（与 queryscope 相反）。** `TestRLS_FailsClosedWhenTenantUnset`：
忘记设租户时 `current_setting(..., true)` 返回 NULL，谓词不匹配任何租户行，只剩平台池
（`tenant_id IS NULL` 恒真）。对比 queryscope 的 nil scope = 无限制（fail-open），
RLS 是 fail-closed，这正是选 ③ 的第二个理由：**漏一处的后果是少返回数据，不是泄漏**。

**⑤ 租户绑定必须在显式事务内。** `TestSetLocal_RequiresTransaction` 与
`TestSetSession_LeaksAcrossPooledConnections` 分别钉住两个方向：`local=true` 在事务外
会被立即丢弃（每条 Exec 自成隐式事务）；`local=false`（会话级）则**残留在池化连接上，
下一个请求继承前一个租户的身份**——实测确认泄漏。所以每个请求必须自带事务，
这是 ③ 的主要改造成本。

#### 9.2.4 性能实测：固定开销，不是倍数

Postgres 16 容器 + loopback，`-benchtime 1000x -count=3` 取中位数：

| 形态 | 耗时 | 相对 | 往返 |
|---|---|---|---|
| 裸 SELECT（今天的形态） | ~107 µs | 1.00× | 1 |
| 事务 + SELECT（隔离出事务成本） | ~319 µs | 2.98× | 3 |
| 事务 + `set_config` + SELECT（**RLS 形态**） | ~409 µs | 3.82× | 4 |
| 重查询：裸 SELECT（2 万行聚合） | ~1000 µs | 1.00× | 1 |
| 重查询：**RLS 形态** | ~1235 µs | **1.24×** | 4 |

**关键读法：开销是固定的 ~250–300 µs（3 个额外往返 × ~100 µs），不是 3.8 倍。**
轻查询上表现为 3.8×，是因为分母只有 107 µs；查询本身一重，比例立刻掉到 1.24×。
**所以 ③ 的实际成本取决于 Bifrost 的读混合构成**——密集的小查询代价明显，
本身较重的查询几乎无感。这也说明主要成本来自往返次数而非 policy 求值。

> ⚠️ 一个省往返的写法**实测证明不能用**。把 `set_config` 折进 CTE 与读同批下发
> （~350 µs，省一个往返），`TestPipelinedSetConfig_IsNotSafe` 在 PG16 上确实返回了正确的
> 2 行——但 **Postgres 不保证 CTE 副作用与外层 RLS 谓词的求值顺序**，policy 是随扫描求值的。
> 也就是说它现在对是碰巧，**执行计划一变就静默翻成 fail-closed（返回 1 行）且不报错**。
> 上表引用的 409 µs 是有保证的分开写法（`TestSeparateSetConfig_IsCorrect`）的成本，
> 不采纳 CTE 写法。

#### 9.2.5 建议

**D11a → ④**：租户/用户/角色/账单/审计进控制面自有库，配置变更走同事务 outbox 推数据面
（衔接 M2）。这是唯一让"控制面独立自研"不自相矛盾的做法。

**D11b → ③ RLS 作为强制点**，用 `RunMigration` 加列（不需要 fork）。理由：

1. 列本来就要加，且加列已被上游官方支持（§9.2.1）
2. RLS 是唯一覆盖裸 `s.DB()` 的机制，而那正是漏洞所在（§9.2.3 ①）
3. 失败方向安全：漏一处是少返回，不是泄漏（§9.2.3 ④）；而 ① 漏一处是静默泄漏
4. 性能成本是固定 ~250 µs，非倍数（§9.2.4）

**落地必须硬性满足（否则等于零隔离）**：

- 运行时角色**非超级用户、不持 `BYPASSRLS`**，且所有租户表加 `FORCE ROW LEVEL SECURITY`
- **启动时自检上述三条，不满足直接拒绝启动**——这个失效模式不会自己报错
- 租户上下文只走 `set_config()`，禁止拼串（§9.2.3 ②）
- 每请求一事务；会话级绑定在池化连接下会泄漏身份，禁用（§9.2.3 ⑤）
- 隔离测试**强制跑 Postgres**，不允许只在 SQLite 上验证
  （SQLite 无 RLS；[`migrations_test.go:39`](framework/configstore/migrations_test.go#L39)
  的 `postgresDSN` + 独立 schema 是现成范式）

**退路是 ①，不是 ②。** 若"每请求一事务"在实际读混合下代价不可接受，退到 fork 最小补丁；
② 在安全边界上不成立。

### 9.3 D12 建议：outbox 已改变前提，总线降级为延迟优化

**D12 原本的两个候选（每节点独立 group / 中继 fan-out）都是在"总线必须保证投递"
这个前提下的解法。D11a 定为 ④ 之后，这个前提不再成立。**

理由：控制面自有库 + transactional outbox 意味着 **"租户 X 到 v42" 这件事已经
持久化在 Postgres 的 outbox 表里**。总线不再是真相的载体，只是让节点更快知道。
漏一条消息的后果从"永久停留旧配置"降级为"晚一个对账周期才更新"。

这一步把 R18（pub/sub 丢消息）和 R26（同组竞争消费）**同时解掉**，
因为正确性不再依赖投递语义。

**建议：outbox 为准 + 定期对账为兜底 + pub/sub 为延迟优化（三层）。**

| 层 | 机制 | 作用 |
|---|---|---|
| 真相 | 控制面 outbox 表的 generation | 唯一 source of truth |
| 兜底 | 每节点定期对账（建议 10s）：查各租户 generation，有变化才拉快照 | 保证**有界陈旧**，Redis 挂了也照常收敛 |
| 加速 | Redis pub/sub 广播"某租户变了" | 把常态延迟压到亚秒级 |

**为什么 pub/sub 现在够用了。** pub/sub 是原生广播——一条消息投给所有订阅者，
**不需要 consumer group，因此 D12 的两个候选都不再需要**。
它唯一的缺陷（at-most-once、断连丢消息）已被对账层覆盖。

**为什么不再选 Streams。** 对比要付的成本：

| | Streams（每节点独立 group） | pub/sub + 对账 |
|---|---|---|
| 需实现 | group 幂等创建/重建、ACK 时机、PEL 恢复与 `XAUTOCLAIM`、死信、`XTRIM` 保留窗口、僵尸 group 清理 | 一个 generation 查询 + 一个 ticker |
| 最坏陈旧 | 通常秒级恢复；但 group 异常时**无界** | **有界**（≤ 对账周期） |
| Redis 不可用 | 停止收敛 | **照常收敛**（退化为纯对账） |

而 Streams 的 at-least-once 在这个架构里**是冗余的**——对账层已经提供了同等保证，
且 M2 第 4 条"兜底"本来就要求实现对账。也就是说 Streams 那套 group 生命周期管理
是为一个已经被别处保证的性质付第二次成本。

> **这不是推翻 D3。** D3（选 Streams 而非 pub/sub / NATS）是在 D11a 之前定的，
> 当时总线确实要独立保证不丢配置更新，那个前提下 Streams 优于 pub/sub 的判断是对的。
> 变的是前提：outbox 出现后，"持久化 + 游标补齐"这件事 Postgres 已经做了。
>
> 顺带一提，D11a 也让 **M2.0.1（DB 变更日志表 + 轮询）的"变更日志表"部分免费到手**
> ——outbox 表就是它。本建议实际上是 M2.0.1 加一层 pub/sub 加速，
> 而 M2.0.1 的依据是 `framework/sidekiq/` 等既有轮询范式，与上游一致。

**Redis 在此项中从必需降为可选。** 没有它系统仍然正确，只是配置生效慢到对账周期。
这对"Redis 单点不打挂数据面"（M2 第 4 条）是实质改善。

**必须在设计里写明的**（比原 D12 清单短很多）：

- 对账周期与抖动（避免所有节点同时对账打 DB）
- generation 的 fencing：只接受比本地更新的版本，防慢节点用旧快照覆盖
- pub/sub 消息只当提示，**不携带配置本体**，收到即触发一次对账
- 对账查询必须廉价（只查 generation，不拉快照）

### 9.4 D13 建议：分三档，M3 只做中间档

**D13 的两个候选之间还有一档，而它恰好是 M3 真正需要的那一档。**

| 档 | 机制 | 超支上界 | 成本 |
|---|---|---|---|
| 现状 | 进程内 `sync.Map` + 周期 dump | 节点数 × dump 间隔消费量（**松**） | 0 |
| **中间档** | **Redis 原子 `INCRBY` 扣费**（完成后扣，仍非预扣） | **在途并发数 × 单请求最大成本**（紧但非零） | 一次 Redis 往返 |
| 完整 | reserve / settle / release 三段式 | ≈ 预估误差（**接近零**） | 两次往返 + 孤儿 reservation 回收 |

**建议：M3 交付中间档，完整 reservation 留到 M6（Phase 2）随预付费一起做。**

理由：

1. **M3 的职责是"多节点下计费可信"，中间档已完全解决。** 当前真正的缺陷是
   进程内计数导致 **N 个节点 = N 倍额度**（R30 同源问题）。原子计数消除了它。
   剩下的"在途并发窗口"是量级小得多的问题。
2. **reservation 的难点不在计数器，在结算策略。** fallback 到别的 provider 算谁的？
   retry 重复预扣如何合并？流式中断按已产出 token 还是全额？
   这些是**产品规则**，在 M6 有真实计费需求前设计，等于对着假设写代码。
3. **中间档是完整 reservation 的前置，不是岔路。** 同一套 Redis 计数器基础设施，
   M6 时在其上加 reserve/release 两个 Lua 脚本即可，不重写。

**必须明确说出口的代价**：在完整 reservation 落地前，**不能卖"硬封顶预付费"**。
可以卖后付费信用额度（有界超支可接受，月底出账对齐），
可以卖软额度告警。M6 若要上预付费，reservation 必须同期交付——这条要写进 M6 验收。

> **另有一项与 D13 无关、但比 D13 更紧急的缺陷**，无论 D13 怎么选都要修：
>
> [`tracker.go:160-162`](plugins/governance/tracker.go#L160-L162) 的 `ChargeBudgets`
> **失败只记日志**，不重试、不回滚、不告警。也就是说 Redis/DB 抖一下，
> 这笔消费就**永久不计费且无人知道**——这是静默漏收入，不是超支。
>
> D11a 的 outbox 正好免费提供解法：把扣费意图写进控制面事务的 outbox，由投递器重试。
> 且**幂等已经现成**——`tracker.go:132` 的 `tryClaimBilling` 按 RequestID + AttemptNumber
> 去重，重试安全。修复成本很低，建议纳入 M3 范围（已记为 R35）。



---

## 附录：本方案的实测依据

| 结论 | 依据 |
|---|---|
| Apache 2.0，可商用二开 | `LICENSE`，无附加限制条款 |
| 行级隔离机制已存在且为 wrapper 设计 | `framework/queryscope/queryscope.go` 包注释 |
| 企业版 DAC 建在此机制上 | `framework/logstore/rdb.go:3983` 注释 |
| ScopedDB 覆盖率约一成 | configstore 内 44 处 `ScopedDB(ctx)` vs 387 处 `s.DB().`（按出现次数计；早前记录的 39 / 318 是按行计，结论不变） |
| 下游给上游表加列不需要 fork | `framework/configstore/postgres.go:81-82` 注释点名 bifrost-enterprise 经 `migrateOnFreshFn` 跑自己的迁移；`RunMigration` 为公开方法 |
| 写入与内部查询按设计绕过 scope | `framework/configstore/rdb.go:358-361` `ScopedDB` 注释原文 |
| 上游有裸 `s.DB()` 多表读，wrapper 拦不到 | `framework/configstore/rdb.go:6296` `GetGovernanceConfig` |
| 上游无任何 RLS 基础 | 全仓库 grep `ROW LEVEL SECURITY` / `CREATE POLICY` / `row_security` 零命中 |
| 运行时角色即建表 owner | `framework/configstore/postgres.go:34-62`：迁移池与运行时池同一份 config，仅 DSN 追加 `default_query_exec_mode` |
| **RLS 能约束不带租户谓词的裸查询** | spike `TestRLS_BlocksRawUnscopedQuery`（PG16，经 `postgresconn` 生产连接路径） |
| **超级用户 / `BYPASSRLS` 无视 `FORCE`，policy 静默失效** | spike `TestRLS_SuperuserBypassesEvenWithForce`；`pg_roles` 实测 `rolsuper=t, rolbypassrls=t` |
| **表 owner 无 `FORCE` 时不受自己表的 RLS 约束** | spike `TestRLS_OwnerBypassesWithoutForce` / `TestRLS_ForceConstrainsOwner` 对照 |
| **`SET LOCAL x = ?` 不接受绑定参数** | spike 实测 `syntax error at or near "$1"`（SQLSTATE 42601）；须用 `set_config()` |
| **RLS 未设租户时 fail-closed** | spike `TestRLS_FailsClosedWhenTenantUnset`（仅剩 `tenant_id IS NULL` 的平台池） |
| **会话级租户绑定会残留在池化连接上** | spike `TestSetSession_LeaksAcrossPooledConnections`；`local=true` 在事务外被丢弃见 `TestSetLocal_RequiresTransaction` |
| **CTE 内 `set_config` 与 RLS 谓词求值顺序无保证** | spike `TestPipelinedSetConfig_IsNotSafe`：PG16 上碰巧正确，但计划一变即静默返回错误行数 |
| **RLS 开销是固定 ~250 µs，非倍数** | spike 基准：轻查询 107→409 µs（3.82×），2 万行聚合 1000→1235 µs（1.24×） |
| **物化视图不受底表 RLS 约束** | spike 实测（PG16）：同一 session、绑定 `app.tenant_id='t-a'`，底表（`FORCE` + policy）返回本租户行，其上物化视图返回全部租户聚合行 |
| **无法给物化视图补加 RLS 或 `security_invoker`** | spike 实测两个 `ERROR`：`ENABLE ROW SECURITY ... not supported for materialized views`、`unrecognized parameter "security_invoker"`；同一语句用于普通 view 则成功且过滤正确 |
| **Postgres logstore 表无分区** | `PARTITION BY` 仅出现于 `framework/logstore/clickhousemigrate.go:97`/`:134`，Postgres 建表路径无对应语句 |
| 默认 fail-open | `queryscope.FromContext` 注释：nil scope = 无限制 |
| 跨节点配置传播不在 OSS | `server.go:63` `enterprisePlugins` 含 `pubsub`；configstore 无配置 ticker |
| 治理计数器为进程内内存 | `plugins/governance/store.go:27-90`，全 `sync.Map`，无 Redis 引用 |
| kvstore 是纯内存，非可配后端 | `framework/kvstore/kvstore.go:42`（`data map[string]entry` + `sync.RWMutex`）、`:26`（`Config` 仅 `CleanupInterval` / `DefaultTTL`，无连接字段） |
| KVStore 接口已定义且为注入式 | `core/schemas/kvstore.go`（4 方法）、`core/schemas/bifrost.go:38`（注释：shared KV store for **clustering**/session stickiness） |
| Redis 依赖已存在 + 生产级配置范式 | `framework/go.mod:19`（go-redis v9.17.2）、`framework/vectorstore/redis.go:31`（TLS/CA、cluster mode、pool、timeouts）、`:1900`/`:1917`（UniversalClient 双模） |
| kvstore 有集群同步接缝但无实现 | `framework/kvstore/kvstore.go:64`（`SyncDelegate`，LWW 语义）、`:74`（`SetDelegate`）、`:80`（`RegisterDecoder`）；OSS 无实现 |
| 会话粘性依赖注入的 KVStore | `core/bifrost.go:9282`（`DefaultSessionStickyTTL`）、`:9330`（`getCachedKeyFromStore`）、`plugins/routing/complexitysession.go:42` |
| 全仓库无 Redis Streams 使用 | grep `XAdd`/`XReadGroup`/`XGroup` 无命中（命中项均为 Go stream / SSE 无关词）；M2.0 属纯新增代码 |
| 轮询范式是本仓库既有做法（M2.0.1 依据） | `framework/sidekiq/` 为 DB 后端 + 轮询，包内零 Redis 引用（`sidekiq.go:420` ticker + 认领式派发 + `staleAfter` 回收）；`framework/webhooks/dispatcher.go:224`、`framework/oauth2/sync.go:130` 同为轮询 |
| GovernanceStore 邀请 wrapper 接管 | `store.go:186` `HolderLimits` 注释 |
| OSS 无 roles/permissions 表 | `tables/notification.go:5` 注释 |
| 无审计表 | 全库 grep `audit` 无命中相关表 |
| UI 有 overlay 接缝 + 路由骨架已存在 | `ui/vite.config.mts:10-43`、`ui/tsconfig.json:19-20`、`ui/app/_fallbacks/enterprise/` |
| key 选择逻辑可插拔（租户公平性接缝） | `core/schemas/bifrost.go:16`（`KeySelector` 类型）、`:36`（`BifrostConfig.KeySelector`）、`core/bifrost.go:4690`（调用点）；上游仅内置 `core/keyselectors/weightedrandom.go` |
| 日志含 key 身份列（BYOK 脱敏依据） | `framework/logstore/tables.go:207`（`SelectedKeyID`，带索引）、`:59`（`SelectedKeyIDs`）、`:2326`（`RankingDimension` 机制） |
| 密钥掩码范式可复用 | `framework/configstore/clientconfig.go:482`（`ClientConfig.Redacted()`）、`:509`（`ProviderConfig.Redacted()`） |
| 上游预留企业挂载点 | `GovernanceData.Users`（`enterprise-only`）、`BusinessUnitGovernance`、`notification.RoleIDs`、`tables/mcpoauth2.go:409`（SCIM） |
| 事务接缝已存在（M5.1 依据） | `framework/configstore/store.go:554`（`ExecuteTransaction`）；`rdb.go` 内 **57 处** `Transaction(func...)`；接口方法已接受 `tx *gorm.DB`（`store.go:380`、`:486`、`:490`）——调用方传入事务是上游既有范式 |
| objectstore 归档范式可参考 | `framework/objectstore/`；`framework/logstore/hybrid.go`（payload 卸载到对象存储的现成实现） |

**未经验证、需实施前确认**：`docs/enterprise/` 所述 clustering 的实际完成度。

> 已消除的待验证项：
> - `framework/kvstore/` 的后端实现 → D2 实测确认（纯内存）
> - ~50 张表的逐表租户归属 → Phase 0 完成，实际 **56** 张，定稿于 [`MAAS_TABLE_AUDIT.md`](MAAS_TABLE_AUDIT.md)
