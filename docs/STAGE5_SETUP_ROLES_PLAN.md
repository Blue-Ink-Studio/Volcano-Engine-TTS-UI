# STAGE5 实施契约 — Setup 改版 + 用户体系埋点

| 项 | 值 |
|---|---|
| 文档状态 | **契约冻结（可开工）** |
| 上游策划 | Downloads 的《STAGE5 定稿 v5》+ 第五轮 review 的 10 条定案（本文档 §3） |
| 代码基线 | STAGE4 已落地并提交（`46162cd`，在 `develop`） |
| schema | `schemaVersion` 5 → **6** |
| 读者/分工 | **后端 = 本人实现**；前端（`setup.html` + `admin-user.html` role 适配）= 用户 |
| 边界 | STAGE4 的权威契约仍是 [STAGE4_PORTAL_BILLING_PLAN.md](STAGE4_PORTAL_BILLING_PLAN.md)；**新增错误码已在 [§17.1](STAGE4_PORTAL_BILLING_PLAN.md) 合并**，本文档不重复定义 |
| 发布纪律 | 只进 `develop`，**绝不动 `main`** |

---

## §1 前提（已由用户确认，影响一条重要决策）

> **当前没有生产环境。** ⇒ 业务 key **干净剥离、不做任何过渡兼容**：不留 `OPENAI_TTS_API_KEY` 的 seed 兼容路径，不留"过渡 WARN 后仍 seed"的旁路。

这条前提推翻了本文档早期草案里的"过渡 seed"方案（原意是避免存量调用方升级即 401）。既然无生产环境，**一刀切是正确且更干净的做法**。

---

## §2 范围

1. **Setup 改版**：安装流程从「填 auth_key / 火山凭证 / 音色」收敛为「注册超管账号」。
2. **用户体系埋点**：`role` / `user_group` / `channel_group` 一次落 schema，后续只加逻辑。

**为什么这是消除架构矛盾而非简化**：`setup` 要填火山 `api_key` + `speaker`，而本系统自称"多渠道 hub"且**早已有渠道管理页**（`admin-channels`，支持 per-channel voices）。把渠道配置焊死在建站流程里是单渠道时代的遗产。

---

## §3 决策表

### 3.1 继承 v5

| 编号 | 定案 |
|---|---|
| D1 | 超管用户名可自定义（防默认账号撞库） |
| D2 | 安装 token 保留、随机 32 字符 hex、启动打印（**已实现**，仅删 env 分支） |
| D3 | 去掉 `TTS_ADMIN_KEY` env；token 只随机生成 |
| D3b | `auth_key` **退役**（含业务回退） |
| D3c | `OPENAI_TTS_API_KEY` **退役**，不再 seed 默认 token（**无过渡**，见 §1） |
| D3d | `admin_key`(DB) **保留**，等价 owner 的运维后门，非启动必需 |
| D4 | 首次设密保留（老库空密码账号兜底） |
| D5 | **DB 写入**单事务（`CreateLock` / `SetMode` 在事务外） |
| D6 | 超管 `balance=0` |
| D7 | 装完重定向 `/login`；登录后无渠道给引导提示 |
| B1 | setup 只收 `{token, admin:{name,password}}` |
| B2 | 无可用渠道 → **503 `no_channel`** + 「请到渠道管理添加」 |
| B5 | 普通用户不可自助升降 `user_group`；变更仅 owner/admin |

### 3.2 第五轮 review 定案（10 条，均为本人拍板）

| # | 定案 |
|---|---|
| **A1** | 无渠道判定**不用** `GetRouter().Empty()`（它在"渠道全禁用"时判错，见 §7），改用 `controller.CountEnabledChannels(GetRouter().Snapshot()) == 0`；同时**修正 `Empty()` 那条说谎的注释**，但不改它的实现（它被 `tts.go` 用于"走 setting 兜底"判定） |
| **A2** | `PATCH /api/admin/users/{id}` **只挂 `RequireAdminOrJWT`**；handler 内**按 body 是否出现 `role='owner'` 条件判定**是否需要 owner。**不得给这个多用途端点挂 `RequireOwner`**（那会让 admin 连改 `user_group` 都被 403，违反权限矩阵） |
| **A3** | `TransferOwner` 三条校验：① `old == new` → **短路 no-op 返回 200**（否则单条 CASE UPDATE 会把 owner 自己降成 admin ⇒ 零 owner 自锁）；② 目标不存在 → 404；③ 目标 `status <> 1`（含软删）→ 400 `invalid_target` |
| **A4** | 清空 `admin_key` **允许**；但把 STAGE4 原有的"`auth_key` 为空则拒绝"判断换成更准的一条：**若不存在任何「`role='owner'` 且 `status=1` 且已设密码」的账号，才拒绝** |
| **A5** | 两处"提 owner"**只允许有一份规则**：抽成 `store.EnsureOwnerExists()`，`migrate`（版本门控）与 `EnforceInitialSecurityPolicy` 都调它；内部带统一的 `NOT EXISTS(SELECT 1 FROM users WHERE role='owner' AND status<>3)` 护卫 |
| **A6** | 新增错误码并入 **STAGE4 §17.1**（已做）；措辞收紧为「`admin_key` 在**管理接口**上等价 owner」；前端**按 `code` 分支，不按 status 分支**（`no_channel` 与 `db_not_ready` 同为 503） |
| **A7** | `admin-user.html` 按 `role` 渲染按钮：owner → 只显示"转让"或只读徽标（**不显示"降级"**）；admin/user → 保留升降级 |
| **A8** | partial unique index 必须用 **`CREATE UNIQUE INDEX IF NOT EXISTS`**，否则"建失败降 WARN"会变成**每次启动刷一条 WARN** |
| **A9** | 删掉"管理凭证为空即 Fatal"后，**保留一条 WARN**（"未配置 admin_key，紧急恢复仅依赖账号密码"） |
| **A10** | 验收清单补两条断言：CSV 归一化（`" default , default , vip "` → `default,vip`）、**迁移幂等**（连续重启 3 次不报错、不重复刷 WARN） |

### 3.3 范围收缩（本人决定）

`group` 的**语义**推迟，**只落数据层**：

| 本期做 | 本期不做（留到实现路由匹配那一期） |
|---|---|
| 列类型（CSV）、`NOT NULL DEFAULT 'default'`、**禁止清空/非法值**（数据完整性） | 成员判断表达式、集合交集语义、403「该用户组无可用渠道」的具体文案与判定 |

理由：`channel_group` 路由匹配本期不做（v5 §八），在没有匹配逻辑的情况下把"多组 vs 单组""交集 vs 优先级""空集怎么办"猜死，改起来比现在定更贵。**但列类型必须现在定**（改类型要迁移）。

> ⚠️ 若将来实现匹配，**严禁 `LIKE '%x%'`**（`svip` 会被误判为 `vip`）。正确写法见 §6.3。

---

## §4 schema 变更（5 → 6）

### 4.1 新增列（全部走 `ensureColumn`，只增不改删）

```sql
ALTER TABLE users    ADD COLUMN role          TEXT NOT NULL DEFAULT 'user';
ALTER TABLE users    ADD COLUMN user_group    TEXT NOT NULL DEFAULT 'default';
ALTER TABLE channels ADD COLUMN channel_group TEXT NOT NULL DEFAULT 'default';
```

`DEFAULT` 同时充当**存量回填**（`ALTER TABLE ... ADD COLUMN ... DEFAULT` 会把已有行填成该默认值）。

### 4.2 🔴 一次性列值回填**必须用 `schema_version` 门控**（本人发现，非文档原有）

`role` 需要从**冻结的** `is_admin` 做一次性映射。天真的写法是：

```go
// ❌ 绝对不要这样：每次启动都会重跑
UPDATE users SET role = 'admin' WHERE is_admin = 1;
```

**为什么是 bug**：`is_admin` 已冻结、不再维护（§6.1），所以一个曾被提升、后来被降级为 `role='user'` 的账号，其 `is_admin` 永远是 1 ⇒ **每次重启都会被重新提权为 admin** ⇒ 权限静默回弹。这是最容易被漏掉、后果最严重的一处。

**正确做法**：在 `migrate()` 里读版本号做门控。

```go
// migrate() 内的顺序（关键）
1. CREATE TABLE IF NOT EXISTS schema_version            // 必须先建表才能读
2. cur := SELECT COALESCE(MAX(version),0) FROM schema_version
3. …既有 DDL / ensureColumn…
4. if cur < 6 {                                        // ← 只在 5→6 那一次执行
       UPDATE users SET role = CASE WHEN is_admin = 1 THEN 'admin' ELSE 'user' END;
       EnsureOwnerExists()                             // 见 §5
   }
5. INSERT OR IGNORE INTO schema_version (version) VALUES (6)
```

- `schema_version` 的 PK 是 `version`，所以跨版本会**累积多行**：升级前 `MAX=5`、升级后 `MAX=6`，天然可作"是否已跑过 6"的标记；
- 全新库 `MAX=0` ⇒ `cur < 6` 成立，回填在空表上执行 = 无副作用；
- 顺手**删除** STAGE4 留下的死逻辑 `UPDATE users SET is_admin=1 WHERE name='default' AND is_admin=0`（migrate 与 `EnforceInitialSecurityPolicy` 里各一条）。

---

## §5 迁移与 owner 保障

### 5.1 一次性映射（`cur < 6` 时）

| 条件 | 结果 |
|---|---|
| `is_admin=1` 且 `name='default'` 且 `status<>3` | `role='owner'`（经 `EnsureOwnerExists` 的唯一性护卫） |
| 其余 `is_admin=1` | `role='admin'` |
| `is_admin=0` | `role='user'` |

### 5.2 `EnsureOwnerExists()`（**唯一的 owner 保障规则**）

```
1) 若 SELECT COUNT(*) FROM users WHERE role='owner' AND status<>3 > 0 → 直接返回(幂等)
2) 候选①：SELECT id FROM users WHERE status=1 AND is_admin=1 ORDER BY id ASC LIMIT 1
   候选②（候选①为空）：SELECT id FROM users WHERE status<>3 ORDER BY id ASC LIMIT 1  → 打 WARN
   两者皆空（空库）→ 返回，什么都不做（setup 会创建 owner）
3) UPDATE users SET role='owner' WHERE id=?
     AND NOT EXISTS(SELECT 1 FROM users WHERE role='owner' AND status<>3)
```

- **软删（`status=3`）不计入 owner**，也**不参与候选**；
- 调用点两处，规则同一份：`migrate()`（`cur<6`）与 `EnforceInitialSecurityPolicy()`（每次启动，用于"owner 被删/软删后自动补位"，并打 WARN 说明补了谁）。

### 5.3 `EnforceInitialSecurityPolicy` 的改动边界

- **(a) 只改这一半**：「提 `default` 为 admin」→「调 `EnsureOwnerExists()`」；
- **(b) 原样保留**：「`default` 无密码 → 强制 `allow_register=0` + WARN」——这是 STAGE4 的 D24 安全防线，**不得因为重写而丢失**。

---

## §6 用户体系契约

### 6.1 `is_admin` 的处理（冻结，不删列）

| 项 | 规则 |
|---|---|
| 列 | **保留、只读冻结、不再维护**（与 `users.quota` 同先例，回滚安全） |
| 新建用户 | `is_admin` **恒写 0**（不反映真实角色；勿作报表依据） |
| 运行期读取 | **一律不得以 `is_admin` 作判定或查询条件**（`userColumns` 里移除它、`UserCountActiveAdmins` 改造等） |
| 迁移读取 | **允许**（它是迁移的历史输入，见 §4.2） |
| API 输出 | 从 `role` **派生** `is_admin = (role=='owner' \|\| role=='admin')`，读路径前端零改 |
| 授权判定 | **每请求回查 DB 的 `role`**；JWT claims 里的 `is_admin` **不参与鉴权**（改角色立即生效） |

> 这两句必须同时成立：**迁移用 `is_admin`（历史），运行期用 `role`（真源）**。只写"清掉全部 `is_admin` 查询"会让迁移失去输入，只写"用 is_admin"则会让权限回弹（§4.2）。

### 6.2 role / user_group

- `role` ∈ `owner | admin | user`，**唯一真源**；`user_group` 为**非空**白名单子集（本期硬编码 `default/vip/svip`）。
- **owner 唯一**：`partial unique index ... WHERE role='owner' AND status<>3`（软删不占位）；逻辑挡三处（setup / 迁移 / `TransferOwner`+PATCH）。
- **写路径兼容**：`is_admin=true` → `role='admin'`（**永不是 owner**）；`is_admin=false` 打向 `role='owner'` → **409 `last_owner`**；`role` 与 `is_admin` 同时出现 → **`role` 优先**。

### 6.3 CSV 语义（本期只做数据层约束）

- `user_group` / `channel_group` 为 CSV；写入前 **trim + 去重 + 排序**；**禁止空集**，非法/空 → 400 `invalid_group`。
- 将来做匹配时，成员判断**必须**用带分隔符的形式：
  ```sql
  ',' || user_group || ',' LIKE '%,vip,%'      -- ✅
  instr(',' || user_group || ',', ',vip,') > 0 -- ✅
  user_group LIKE '%vip%'                      -- ❌ svip 会被误判为 vip
  ```

### 6.4 权限矩阵（与 v5 §五一致）

| 操作 | owner | admin | user |
|---|---|---|---|
| 转让 owner | ✓（原子） | ✗ | ✗ |
| 建/管理 admin | ✓ | ✓（不能动 owner、不能转让） | ✗ |
| 管理用户 / 改 user_group | ✓ | ✓ | ✗ |
| 充/扣款 / 重置密码 | ✓ | ✓（不能动 owner） | ✗ |
| 自助（自己的 token / 余额 / 流水） | ✓ | ✓ | ✓ |

- **`admin_key` 通道（`SessionCallerFromContext(r) == nil`）等价 owner**，不受上表与 owner 保护约束（运维自救）；
- **`RequireOwner` 本期只用于"转让 owner"这一处**（即 PATCH 内 `role='owner'` 的条件判定），**不要给一片接口加**。

### 6.5 `TransferOwner` 实现口径

```sql
-- 单条语句，无中间状态（A3：old==new 必须在 handler 里短路，不依赖 SQL）
UPDATE users SET role = CASE WHEN id = ?old THEN 'admin' WHEN id = ?new THEN 'owner' END
WHERE id IN (?old, ?new)
```
- 若拆成两条，**必须先降旧、再升新**（否则中间态撞唯一索引）；
- 前置校验：目标存在、`status=1`、`old != new`。

---

## §7 无渠道 503（B2）

### 7.1 判定（A1）

```go
// controller 层；必须在 preConsumeQuota 之前调用
if controller.CountEnabledChannels(controller.GetRouter().Snapshot()) == 0 {
    // 503 no_channel +「请到渠道管理添加」
}
```

**为什么不用 `GetRouter().Empty()`**（代码事实）：

```go
// adapter/route/router.go:42-47
// Empty 没有任何启用渠道时返 true(让 controller 走 setting 兜底路径)。
func (r *Router) Empty() bool { return len(r.channels) == 0 }   // ← 数的是全部渠道
```
而路由的构建来源是 `reloadChannels()` 里的 `s.ChannelList(true)`（注释明确是**全量含 disabled**）⇒ **只要库里存在渠道（哪怕全部禁用），`Empty()` 就是 false** ⇒ `no_channel` 不会触发。**这是 B2 唯一会真正失效的点。**

三个连带的确认（已核对代码）：
- **渠道热更新存在**：`reloadChannels()` 的注释是"写操作(POST/PATCH/DELETE/status)成功后调,触发 router 热更新" ⇒ **加完渠道不需要重启**；
- `CountEnabledChannels(chs []route.Channel)` 已在 `controller/channels.go:188` 存在，数的是 `c.Enabled()`，语义正好；
- `Empty()` 的实现**不动**（它被 `tts.go` 用于"走 setting 兜底"的判定），只**改它那条与实现不符的注释**。

### 7.2 顺序与优先级

- 检查位置**在预扣之前**（与现有 config 检查同位置）⇒ **不会先扣钱再 503**，不产生污染流水的退款行；
- 因此 **503 `no_channel` 优先于 402**（欠费用户遇到无渠道时先看到"去加渠道"）——这条要写进断言，防将来调换顺序导致行为漂移。

### 7.3 overview 的两个字段

| 字段 | 来源 | 语义 |
|---|---|---|
| `channel_count` | `store.ChannelCount()` | 渠道**总数**（含 disabled） |
| `channel_enabled_count` | `controller.CountEnabledChannels(GetRouter().Snapshot())` | enabled 渠道数（**前端判"要不要给引导"用这个**） |

与既有 `voice_count` / `voice_enabled_count` 同风格。

---

## §8 退役清单（无过渡，见 §1）

| 对象 | 处置 |
|---|---|
| `TTS_ADMIN_KEY` env | 删除读取分支；`SetupTokenSource` 恒为 `ephemeral`。`.env.example` / README 同步 |
| `auth_key`(DB + settings 页) | 退役：`GetAuthAPIKeys()` 移除、`PUT /api/settings/auth-key` 下线、`ValidateAPIKey` 的 setup 分支不再依赖它 |
| `OPENAI_TTS_API_KEY` env | 退役：**`SeedDefaultTokensIfEmpty` 从 `main.go` 启动流程移除**（不是靠空列表自然 no-op）；`controller/seed.go` 一并删除或标注废弃 |
| `admin_key`(DB) | **保留**（D3d）。启动时若为空 → **打 WARN**（A9） |

---

## §9 接口变更清单

| 接口 | 变更 |
|---|---|
| `POST /api/setup` | body 只收 `{token, admin:{name,password}}`；单事务建超管（`role='owner'`、`user_group='default'`、`balance=0`、bcrypt）→ 写 `settings(installed_at)` → 写 lock → 切 normal；响应 `{ok, redirect:"/login"}` |
| `GET /api/setup/prefill` | **删除** |
| `GET /api/setup/status` | **转匿名**（摘掉管理鉴权） |
| `GET /api/admin/overview` | 加 `channel_count` + `channel_enabled_count` |
| `GET /api/me` | 加 `role` + `user_group`（保留派生 `is_admin`） |
| `GET /api/admin/users` / `/{id}` | 视图加 `role` + `user_group` |
| `POST /api/admin/users` | 支持 `role` / `user_group`；`is_admin` 作为兼容别名映射 |
| `PATCH /api/admin/users/{id}` | 支持 `role` / `user_group`；`role='owner'` → **隐式 `TransferOwner`**（要求 owner 权限）；`is_admin` 别名映射；非法值 → `invalid_role` / `invalid_group` |
| `DELETE /api/admin/users/{id}`、`PATCH {status:2}` | 目标为唯一 owner → **409 `last_owner`** |
| `PUT /api/admin/settings/admin-key` | 清空判断从"auth_key 为空"改为"**是否存在可用 owner**"（A4） |
| `/api/admin/channels*` | 支持 `channel_group`（创建/更新/视图） |
| `GET /v1/audio/speech` | 无 enabled 渠道 → **503 `no_channel`**（预扣前） |

---

## §10 实施顺序

1. **store 层**：`schemaVersion=6`、版本门控回填、`role`/`user_group`/`channel_group`、`EnsureOwnerExists`、`TransferOwner`、`RoleIsAdmin` 派生、清掉运行期 `is_admin` 读取、partial unique index
2. **middleware**：`SessionCaller.Role`、`RequireOwner`、admin_key 恒等 owner 的判定口径
3. **controller**：setup 重写、prefill 删除、status 匿名、users（role/group/转让/owner 守护）、settings（admin_key 清空判定）、channels（channel_group）、overview（两个 channel 字段）、tts（`no_channel`）
4. **router / main**：路由调整、删 prefill、status 匿名、两道 fail-fast、移除 seed、WARN
5. **配置与文档同步**：`.env.example`、README（含"同机反代必须显式设 admin_key"）
6. **自测**：`go build` / `go vet` / store 单测（含 §11 的断言）/ 端到端冒烟

---

## §11 验收清单

**Schema / 迁移**
- [ ] `users.role` / `users.user_group` / `channels.channel_group` 落 schema，`DEFAULT` 正确回填存量
- [ ] 回填**由 `schema_version` 门控**：`cur<6` 才跑
- [ ] **迁移幂等**：连续重启 3 次不报错、不重复刷 WARN、**被降级的账号不会被重新提权**（§4.2 的核心断言）
- [ ] partial unique index 用 `IF NOT EXISTS`；`WHERE role='owner' AND status<>3`
- [ ] `EnsureOwnerExists` 幂等；零 owner 时按"最早 `is_admin=1` 非软删 → 否则 id 最小非软删 + WARN"补位
- [ ] `EnforceInitialSecurityPolicy` 只改 (a)、(b)「default 无密码 → 强制关注册 + WARN」保留

**Setup**
- [ ] `POST /api/setup` 只收 `{token, admin:{name,password}}`，建超管（owner / default / balance 0 / bcrypt / UNIQUE）
- [ ] `prefill` 已删、`status` 匿名可读
- [ ] 装完重定向 `/login`；DB 写入单事务
- [ ] 装完**不需要重启**即可通过渠道管理页加渠道并调用成功（热更新）

**角色与权限**
- [ ] API 派生 `is_admin`；`/api/me` 与 admin 用户详情返回 `role` + `user_group`
- [ ] `PATCH {role:'owner'}` = 原子转让；admin 发 → 403 `owner_required`
- [ ] `TransferOwner`：`old==new` → no-op 200；目标不存在 → 404；目标非启用/软删 → 400 `invalid_target`
- [ ] 降级/删除/禁用唯一 owner → 409 `last_owner`
- [ ] `admin_key` 可自救（等价 owner），且 `/api/me/*` 仍**拒绝** admin_key
- [ ] `PATCH` 端点对 admin 仍可改 `role`(→admin/user) 与 `user_group`（**不可被 `RequireOwner` 一刀切**）

**无渠道 / 计费回归**
- [ ] 0 渠道 → 503 `no_channel`；**渠道全禁用 → 同样 503 `no_channel`**（§7.1 的核心断言）
- [ ] 503 在预扣之前 ⇒ 不产生退款流水
- [ ] 503 `no_channel` **优先于** 402
- [ ] 402 四个分码（`insufficient_balance` / `balance_negative` / `token_limit_exceeded` / `duplicate_request`）行为与顺序不变
- [ ] 注册开关与 `init_balance` 不受影响

**退役 / 配置**
- [ ] `TTS_ADMIN_KEY` / `auth_key` / `OPENAI_TTS_API_KEY` 均不再生效；`SeedDefaultTokensIfEmpty` 已移出启动
- [ ] `admin_key` 为空时启动打 WARN
- [ ] `.env.example` / README 同步（含"同机反代必须显式设 admin_key"）

**静态**
- [ ] `go build ./...` / `go vet ./...` 干净
- [ ] 前端 `node --check` 通过

---

## §12 遗留（schema 已埋，只加逻辑）

- `channel_group` / `user_group` **路由匹配** + 403「该用户组无可用渠道」（§3.3 已把语义推迟到这期）
- 渠道/门户的 group/role **UI**（owner/admin 专用）
- 用户自助升级 `user_group` 申请流程
- 站点名称等站点级设置
- (D3-②) owner 恢复增强：本机 **AND** 持久化紧急恢复码的通道（本期先靠 ①引导 + A9 WARN + `admin_key`）

---

## §13 变更记录

| 版本 | 内容 |
|---|---|
| v1.0 | 由《STAGE5 定稿 v5》+ 第五轮 review 的 10 条定案（A1–A10）冻结而成；新增 §4.2 的 **`schema_version` 门控**（防止 `is_admin` 冻结后重启把被降级账号重新提权）；§3.3 收缩 `group` 到只落数据层；确认**无生产环境** ⇒ 业务 key 无过渡剥离 |

---

## §14 实现完成与验证证据

### 14.1 静态与单测

| 项 | 结果 |
|---|---|
| `go build ./...` | ✅ |
| `go vet ./...` | ✅ |
| `store` 单测 | ✅ **24 PASS / 0 FAIL**（STAGE4 的 13 条回归全绿 + STAGE5 新增 11 条） |
| 残留扫描 | `prefill` 0 处、`os.Getenv("TTS_ADMIN_KEY")` 0 处、`os.Getenv("OPENAI_TTS_API_KEY")` 0 处、运行期 `WHERE is_admin` 0 处 |

STAGE5 新增的 11 条单测：`TestStage5BackfillIsVersionGated`（§4.2 的回归锁）、`TestNormalizeGroupSetCSV`、`TestValidGroupSet`、`TestOwnerUniqueIndexBlocksSecondOwner`、`TestOwnerSoftDeleteDoesNotOccupyIndex`、`TestEnsureOwnerExistsIdempotent`、`TestEnsureOwnerExistsSkipsDisabledCandidates`、`TestTransferOwner`、`TestHasUsableOwner`、`TestIsAdminIsDerivedFromRole`、`TestChannelGroupNormalizedAndValidated`。

### 14.2 端到端 HTTP 实测（全新实例，**不设任何凭证类 env**）

**33 PASS / 0 FAIL**（另有 2 条是脚本把期望写错，实现行为经核对是正确的：见 14.3）。

| 验收点 | 实测 |
|---|---|
| 无任何凭证 env 即可启动 | ✅ 日志自动生成一次性安装 token（32 hex） |
| `GET /api/setup/status` **匿名** | ✅ `200 {"installed":false,"mode":"setup"}` |
| `GET /api/setup/prefill` | ✅ **404**（已删除） |
| 错误 token / 弱口令 | ✅ `401 invalid_token` / `400 weak_password` |
| `POST /api/setup {token,admin}` | ✅ **200** `{"ok":true,"message":"installed","owner":"boss","redirect":"/login"}` |
| 装完 | ✅ `{"installed":true,"mode":"normal"}`；重复 POST `/api/setup` → **404** |
| 超管登录 + `/api/me` | ✅ `role=owner`、`user_group=default`、`is_admin=true`（派生） |
| `GET /api/admin/overview` | ✅ `channel_count` / `channel_enabled_count`（0 → 建渠道后变 1） |
| 门户自建业务令牌 | ✅ `POST /api/me/tokens` → 201 带完整 key |
| **无渠道调用** | ✅ **503 `no_channel`** |
| **503 优先于 402** | ✅ 超管 `balance=0`，若渠道检查在预扣之后本应返 402，实际返 503 |
| 有渠道后 | ✅ 不再是 `no_channel`，改为 `400 unknown_voice`（证明 503 确实源自渠道判定） |
| 唯一 owner 降级 | ✅ **409 `last_owner`** |
| `is_admin=false` 兼容别名打向 owner | ✅ 同样 **409 `last_owner`** |
| 非法值 | ✅ `400 invalid_role` / `400 invalid_group` |
| `channel_group` 归一化 | ✅ 提交 `" vip , default "` → 落库 `"default,vip"` |
| `PATCH {role:"owner"}` 指向已是 owner 的自己 | ✅ **200 幂等 no-op**（A3 的短路生效，未自锁） |
| **重启进 normal 模式（H1 死锁）** | ✅ 进程正常存活；仅两条 WARN：`TTS 全局配置未就绪(mode=normal,不阻断启动…)` 与 `未配置 admin_key…`；**无 FATAL** |
| owner 角色与渠道跨重启存活 | ✅ 重启后原 JWT 仍有效，`role=owner`、`channel_enabled_count=1` |

### 14.3 实测到的对外契约细节（**前端需按此实现**）

1. **`POST /api/setup` 成功返回 `200`**（不是 201，沿用 STAGE4 行为）：
   `{"ok":true,"message":"installed","owner":"<name>","redirect":"/login"}`
2. **`POST /api/admin/channels` 成功只返回 `{"id":N}`**（不是完整视图，沿用阶段 2 行为）；要校验落库结果需再 `GET /api/admin/channels/{id}`。
3. **`GET /api/admin/channels` 的列表信封是 `{channels:[...],total}`**，与 `/api/admin/users` 的 `{items,total,limit,offset}` **不同**（阶段 2 遗留，本期未改）。
4. `POST /api/me/tokens` 返回 `201`，字段含 `key`（完整值仅此一次返回）、`key_prefix`、`unlimited`、`limit_quota`。

### 14.4 残余验证缺口（诚实记录）

- **真实上游成功路径未测**：本次只验证到"有渠道后进入上游调用"（因凭证是假的，止于 voice 校验）。真实火山凭证下的 **预扣 → 结算 → 真实补扣**、以及真实上游下的 `balance_negative` 负债停服，仍未端到端跑过（与 STAGE4 §21.5 记录的缺口相同）。
- **前端未做**：`setup.html` 分步表单与 `admin-user.html` 的 role 按钮（按 §十一 分工归用户）。因此 `admin-user.html` 当前仍会把 owner 显示成"可降级"，点下去会得到 `409 last_owner` —— 功能正确但文案误导，需随前端一并修（A7）。
- **`group` 路由匹配未实现**（§3.3 明确推迟）：本期把渠道设成 `vip` 不产生任何隔离效果。
