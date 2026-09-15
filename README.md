# MaaS — 基于 Bifrost 的企业级多租户平台

在 [Bifrost](https://github.com/maximhq/bifrost)（Apache 2.0）之上构建的商用企业级
MaaS（Model-as-a-Service）平台：多租户隔离、配额计量、计费结算、管理后台与租户自服务门户。

**工程策略：依赖，不 fork。** `core` / `framework` / `plugins` 以 Go module 依赖引入，
自研能力以 wrapper、overlay、plugin 与迁移原语的形态存在，让上游升级保持廉价。
唯一例外是数据模型——行级隔离需要给上游表加 `tenant_id` 列，这部分通过上游的
`framework/migrator` 完成，不改上游源码。

> **当前状态：可通过 Compose 运行的控制面基础版。**
> 管理 UI、管理员登录、租户目录、概览与审计查询已经可用；支付网关、生产 moderation、
> 完整租户自服务门户和逐表数据回填等仍未实现。详见下方[模块完成度](#模块完成度)。

---

## 隔离模型

租户隔离建立在 **Postgres 行级安全（RLS）** 上，而不是靠应用层记得加 `WHERE tenant_id = ?`。
选择 RLS 的原因很直接：Bifrost 数据面有约 387 处裸 `s.DB()` 查询，wrapper 拦不住它们，
而 RLS 在连接层生效，未作用域的查询也会被自动过滤。

三条关键机制：

- **启动自检**（[internal/rls/preflight.go](internal/rls/preflight.go)）— 方言不是 Postgres、
  运行角色带 `SUPERUSER` 或 `BYPASSRLS`、表未开 `FORCE ROW LEVEL SECURITY` 时拒绝启动。
  这些条件下 policy 会静默停止过滤，fail-closed 比 fail-open 重要。
- **租户绑定**（[internal/tenant/binding.go](internal/tenant/binding.go)）— `RunInTenantTx`
  用 `set_config('app.tenant_id', ?, true)` 把租户绑到事务上；`set_config` 是函数调用，
  参数是真参数，而 `SET LOCAL x = ?` 只能靠字符串拼接，那会把租户上下文变成注入点。
- **平台模式**（`RunAsPlatform` / `RunAcrossTenants`）— 严格 policy 下未绑定租户的事务在租户表上
  读到**零行**，而上游 `GetGovernanceConfig` 恰好做无绑定多表读取。显式的
  `app.platform_mode` 会话变量是这条逃生通道。它防的是"忘记绑定"，不防"凭据被盗"——
  应用角色自己就能设这个变量，这是已知且已接受的边界。

**部署硬前提**：`config_store` 与 `logs_store` 都必须是 Postgres。上游两个 store 原生支持
Postgres，配置即可，但 SQLite 是默认值——不显式配就没有 RLS，隔离方案在其他后端上根本不存在。
另外 Postgres 也打开了物化视图这条不受 RLS 约束的读取路径，需要把 logs_store 的
`matview_refresh_interval` 设为 `off`。

---

## 代码结构

```
cmd/maas-api/          控制面 HTTP 服务入口
internal/
  rls/                 RLS 启动自检（方言、角色、FORCE、policy 覆盖）
  tenant/              租户 ID、上下文、事务绑定、KeyScopeFor、TenantScopedStore 接缝
  migrate/             迁移原语：加列、三种 RLS policy、复合外键、租户前导索引、sessions 改造
  controlplane/        租户注册表与生命周期状态机
  governance/          嵌入上游 GovernanceStore 的 wrapper（租户限额叠加而非替换）
  tenantauth/          虚拟 key → 租户的解析
  configbus/           generation + 事务性 outbox + Redis pub/sub 通知 + 节点侧 fencing
  quota/               Redis 原子 INCRBY 共享计数器（D13 中间档，非 reservation）
  billing/             幂等计量、账期、发票、余额、预付费 reservation（整数微单位）
  rbac/                控制面授权边界：平台/租户角色隔离、不可提权会话、CSRF
  audit/               追加式审计事件（无 update / delete 接口）
  portal/              租户自服务门户 service 与默认拒绝路由守卫
  fairness/            Redis 集群级租约 semaphore（租户 × provider 双上限）
  rediskv/             Redis 版 schemas.KVStore
  pglock/              共享 catalog 行 DDL 的 advisory lock
  httpapi/             控制面 API 与迁移装配
  rlsspike/            Phase 0 的 RLS 验证与基准（实测依据）
plugins/
  tenantauth/          HTTPTransportPreAuthHook：请求入口解析租户
  guardrails/          fail-closed 内容策略 hook（输入/输出/流式 chunk）
maas-ui/               控制面前端（静态资源 + nginx）
deploy/postgres/       首次启动前的角色与库 bootstrap
docs/                  技术方案与 56 张表的租户归属审计
```

---

## 快速开始

### 前置条件

- Go 1.27+
- Docker / Docker Compose
- 完整源码测试仍需要 Bifrost 检出与本仓库同级，因为 `go.mod` 的本地开发 `replace`
  指向 `../bifrost/{core,framework,plugins/governance}`：

  ```
  workspaces/ai/gateway/
  ├── bifrost/     # git clone https://github.com/maximhq/bifrost
  └── maas/        # 本仓库
  ```

  Compose 的 `maas-api` 已与数据面依赖解耦，构建和启动控制面不需要 sibling Bifrost 源码。

### Compose 启动

建议先创建环境配置并替换密码：

```bash
cp .env.example .env
```

在本仓库目录下启动：

```bash
docker compose up -d --build
```

起来之后：控制台 http://localhost:3000 ，API http://localhost:18080/healthz 。
默认账号 `admin` / `change-me`。

```bash
docker compose ps
docker compose logs -f maas-api maas-ui
```

### 本地开发

```bash
go build ./...
go test ./...
```

部分测试需要真实的 Postgres 和 Redis——RLS 的行为不可能用 SQLite 或内存 fake 验证，
那样测的就不是被依赖的那个性质了。先起这两个一次性容器：

```bash
docker run -d --name maas-rls-spike \
  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine

docker run -d --name maas-kv-redis -p 56379:6379 redis:7-alpine
```

| 依赖 | 涉及包 | 覆盖端点的环境变量 |
|---|---|---|
| Postgres :55432 | `internal/{rls,tenant,migrate,controlplane,rlsspike}` | `MAAS_SPIKE_PG_{HOST,PORT,USER,PASSWORD,DB}` |
| Redis :56379 | `internal/rediskv` | `MAAS_KV_REDIS_ADDR` |
| 无（SQLite / 纯逻辑） | `internal/{audit,billing,configbus,rbac,quota,fairness,portal,governance}`、`plugins/*` | — |

清理：`docker rm -f maas-rls-spike maas-kv-redis`

---

## 配置

`cmd/maas-api` 读取的环境变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `MAAS_HTTP_ADDR` | `:8080` | 监听地址 |
| `MAAS_DATABASE_URL` | `host=postgres ... dbname=maas` | 控制面 Postgres DSN |
| `MAAS_REDIS_ADDR` | `redis:6379` | 置空则不连 Redis |
| `MAAS_REDIS_PASSWORD` | — | |
| `MAAS_ADMIN_USERNAME` | `admin` | 平台管理员 |
| `MAAS_ADMIN_PASSWORD` | `change-me` | **生产环境必须覆盖** |
| `MAAS_SESSION_LIFETIME` | `12h` | Go duration 格式 |
| `MAAS_CORS_ORIGIN` | `http://localhost:3000` | 控制台来源 |

生产部署前先跑 [deploy/postgres/01_bootstrap.sql](deploy/postgres/01_bootstrap.sql)。
它创建 Bifrost 首次启动前必须存在的库与角色，且**故意不建表**——上游 43 张表的 schema
由 GORM struct tag 在运行时生成，手写 DDL 会变成第二份真相源，还会破坏待迁移检测。
脚本里的 `NOSUPERUSER` / `NOBYPASSRLS` 是承重的，不是卫生习惯。

---

## 控制面 API

当前 `cmd/maas-api` 暴露的端点：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz`、`/api/health` | 健康检查（容器 healthcheck 用同一入口） |
| POST | `/api/auth/login` | 平台管理员登录 |
| GET | `/api/auth/me` | 当前会话身份 |
| GET | `/api/admin/summary` | 租户 / 审计 / 用量 / Redis 概览 |
| GET · POST | `/api/admin/tenants` | 租户列表与创建 |
| GET | `/api/admin/audit` | 审计事件查询 |

当前 `/api/admin/*` 已接入服务端会话、RBAC 和写请求 CSRF 校验。`internal/portal`
提供的租户自服务 service 仍未全部映射为 `/api/portal/*` HTTP 接口。

---

## 模块完成度

| 模块 | 状态 | 剩余工作 |
|---|---|---|
| Phase 0 三项验证 | ✅ | — |
| Redis `schemas.KVStore` | ✅ | — |
| M1 租户底座 | 🚧 | 逐表回填（需真实数据与产品规则）、`TenantScopedStore` 全接口接线 |
| M2 配置下发总线 | 🚧 | 接入具体配置 reload、对账 runner、部署告警 |
| M3 强额度计数器 | 🚧 | 完整 reservation（留给 M6） |
| M4 管理面 RBAC | 🚧 | Compose 管理员会话已接入；仍需持久化 session、租户用户与生产角色管理 |
| M5 审计日志 | 🚧 | DB 角色权限、归档 / WORM |
| M6 计费与结算 | 🚧 | 支付网关、税务发票、催缴与争议流程 |
| M7 套餐与配额 | 🚧 | 套餐准入、阶梯价格、降级策略 |
| M8 内容安全 | 🚧 | 生产 moderation、PII 规则、备案与测评 |
| M9 自服务门户 | 🚧 | 平台管理 UI 已可用；仍需租户门户、成员 / key / model 管理 API |
| M10 多租户公平性 | 🚧 | 优先级队列、BYOK 分流、transport 接线 |

没有对应运行时或 UI 的部分不虚标为完成。

---

## 已知边界

上传前值得先看清楚的几条，都是设计层面的取舍，不是待修的 bug：

- **RLS 防不住被盗凭据。** 应用角色是它自己创建的表的 owner，因此 `FORCE ROW LEVEL SECURITY`
  是强制项；但即便如此，owner 仍能 `ALTER TABLE ... NO FORCE` 或 `DROP POLICY`。隔离在
  SQL 注入与漏加谓词的场景下成立，在应用凭据泄露的场景下不成立。彻底解决需要把迁移角色与
  运行角色拆开，而上游每个 store 只暴露一份凭据，代价是两份配置文件加一次
  migrate-then-restart。已记为缺口。
- **`internal/quota` 不是 reservation。** 它是请求完成后的原子 `INCRBY`，超支有界但存在。
  在 reservation 落地前，**不能对外销售零超支的预付费硬额度**。
- **物化视图不受 RLS 约束**，且无法使之受约束。把 logs_store 的刷新关掉。
- **默认凭据是 `admin` / `change-me`**，UI 登录框里还预填了它。方便本地起步，生产必须覆盖。
- 控制面 session 当前保存在单个 `maas-api` 进程内，容器重启后需要重新登录；多副本部署前需改为 Redis session store。

---

## 文档

- [docs/MAAS_TECH_DESIGN.md](docs/MAAS_TECH_DESIGN.md) — 完整技术方案。第 4 节是模块
  M1–M10 的依赖顺序，第 7 节是阶段进度与各交付物的实际落地说明，第 8 节风险登记（R 编号），
  第 9 节决策日志（D 编号）与 RLS 实测结论。
- [docs/MAAS_TABLE_AUDIT.md](docs/MAAS_TABLE_AUDIT.md) — 上游 56 张表逐表租户归属，A–E 分类，
  附推荐迁移顺序。哪些表加 `tenant_id` 是产品决策，记在这里，不从 schema 反推。

Go 源码注释按名称与章节号引用这两份文档（如 "MAAS_TECH_DESIGN.md §9.2.3"、"R36"），
章节号是承重的——重新编号会打断这些引用。

---

## 许可

[Apache License 2.0](LICENSE)。本项目基于 Bifrost（同为 Apache License 2.0）构建，
上游代码以 Go module 依赖引入，未 fork——仓库本身不再分发上游源码。

分发编译产物（二进制、Docker 镜像）时，需要一并带上 bifrost 的
[`LICENSE`](https://github.com/maximhq/bifrost/blob/main/LICENSE) 与
[`THIRD_PARTY_NOTICES.md`](https://github.com/maximhq/bifrost/blob/main/THIRD_PARTY_NOTICES.md)。
当前 [Dockerfile](Dockerfile) 已将 MaaS 与 Bifrost 的许可证和第三方通知放入镜像。




