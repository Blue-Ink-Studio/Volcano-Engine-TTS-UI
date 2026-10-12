# STAGE4 策划案 — 普通用户自助门户 + 完整计费（new-api 式）

| 项 | 值 |
|---|---|
| 文档状态 | **契约已冻结**（v1.3：开放问题 **0 项**，可直接进入实施） |
| 代码基线 | `8e56a2f`（3b 管理前端）+ develop 工作区 |
| schema | `schemaVersion` 4 → **5** |
| 读者 | 软件部（后端实现）、前端（本人）、运维/发布 |
| 分工 | **后端 = 软件部**；**前端 = 本人**（§9） |
| 前置文档 | Downloads 下 STAGE4 系列策划（D1–D33 已拍板） |

---

## §0 一页速览（给评审会）

**要做什么**：在已完成的 3b 多用户（管理视角）之上，给**普通用户**一套 WebUI 自助能力（注册/登录/管自己令牌/看自己用量），并把计费从 **token 层**整体迁移到 **user 钱包**（模型 C，new-api 式）。

**三个必须记住的结论**：

1. **扣费源从 `tokens.remain_quota` 改为 `users.balance`**，三段式（预扣→结算→退款）逻辑不变，但**落点由 1 处变 3 处**：
   `billing.PreConsume` →（原 `store.TokenDecreaseQuota`，改为 user 预扣）、`store.UsageSettle`、`store.UsageRefund`。
2. **token 不再扣费**，只做两件事：认证凭证 + 可选准入限额（`tokens.limit_quota`，NULL=不限）。
   `tokens.remain_quota` **冻结**为历史只读值，不再更新。
3. **`tokens.used_quota` 语义变更**：从"累计消费镜像"变为"**限额预占计数器**"（预扣 +Pre、结算 +Actual-Pre、退款 -Pre）。历史值直接参与限额判定。

**安全三件套（3b 没有、本期必须做对）**：

| 编号 | 内容 |
|---|---|
| **D29** | `password_hash` 为空 → 登录**永不签发任何凭证**，只返 `{need_setup_password:true}`。**这是本期最关键的一条**，否则空密码账号可被远程直接换成管理员 JWT。 |
| **D14/D32** | 首次设密只允许 `r.RemoteAddr` socket peer 是 `127.0.0.1/::1`，或携带有效 admin_key。**只读 socket peer，忽略一切 XFF / X-Real-IP。** |
| **D24/D31** | 存量 `default` 无密码时，仅在 `allow_register` **从未被设置过**的情况下置 0，并打 WARN。 |

**开放问题：0 项（全部定案）** —— 原 X-Q1/Q2/Q3 与 X25 均已在 v1.2/v1.3 定案：

| 原编号 | 定案 |
|---|---|
| X-Q1 结算补扣 | **允许扣成负 + `balance < 0` 即欠费停服** → 见 **X24** |
| X-Q2 `usage.status=0` | **引入预扣中态** → 见 **X26** |
| X-Q3 依赖获取 | **已实测可联网；钉 `jwt v5.3.1` + `x/crypto v0.58.0`（bcrypt）** → 见 **X27** |
| X25 `limit_quota` 穿透 | **(a) 接受约 5% 穿透**，语义明确为「准入阈值」 |

**已定案的关键计费策略（X24）**：结算补扣**允许**把余额扣成负；`balance < 0` 即**欠费停服**，直至充值使 `balance >= pre` 才恢复；`unlimited` token **同样受限**。

---

## §1 代码基线核实（全部已对照源码，带行号）

### 1.1 计费现状

| 事实 | 位置 |
|---|---|
| 预扣在 token 层，原子 SQL | [store/tokens.go:220-236](../store/tokens.go#L220-L236) `TokenDecreaseQuota`（`WHERE remain_quota >= ?`） |
| 预扣调用点（唯一） | [billing/billing.go:147](../billing/billing.go#L147) |
| 计费入口 | [controller/tts.go:459](../controller/tts.go#L459) `billing.PreConsume(db, caller.UserID, caller.TokenID, caller.Unlimited, chars, ratios)` |
| 结算 | [store/usage.go:168-212](../store/usage.go#L168-L212) `UsageSettle`（tx：token.remain_quota 多退少补 + token.used_quota + users.used_quota + INSERT usage） |
| 退款 | [store/usage.go:224-269](../store/usage.go#L224-L269) `UsageRefund`（tx：按 request_id+status=2 幂等判重 + token.remain_quota 退回 + INSERT usage） |
| 倍率 | [billing/billing.go:27-32](../billing/billing.go#L27-L32) Char/Sec/Group=1.0，Multiplier=1.2 |
| `usage` 状态常量 | [store/usage.go:10-13](../store/usage.go#L10-L13) 1=settled 2=refunded |
| `UsageInsert` 把 Status=0 兜底为 1 | [store/usage.go:45-47](../store/usage.go#L45-L47) ⚠️ 见 §7.4 |

### 1.2 数据库与迁移

| 事实 | 位置 |
|---|---|
| `schemaVersion = 4` | [store/db.go:29](../store/db.go#L29) |
| `SetMaxOpenConns(1)` + WAL | [store/db.go:47-61](../store/db.go#L47-L61) |
| `migrate()` 在 `Open()` 内，**每次启动都跑** | [store/db.go:64](../store/db.go#L64)、[store/db.go:115-290](../store/db.go#L115-L290) |
| `ensureColumn` 幂等补列 | [store/db.go:298-333](../store/db.go#L298-L333) |
| ⚠️ 单连接自锁约束 | [store/db.go:294](../store/db.go#L294)：持有 rows 时再 Exec 会自锁 → **tx 内只能用 Tx 版方法** |
| `users` 表 DDL | [store/db.go:193-204](../store/db.go#L193-L204)（quota/used_quota 已存在） |
| `tokens` 表 DDL | [store/db.go:209-224](../store/db.go#L209-L224)（remain_quota/used_quota 已存在） |
| `usage` 表 DDL + 3 列补列 | [store/db.go:234-287](../store/db.go#L234-L287) |
| `settings` 表**已存在**（无需新建） | [store/db.go:126-133](../store/db.go#L126-L133) + [store/settings.go](../store/settings.go) |
| ⚠️ `CurrentVersion()` 返回 1，与 db.go 的 4 不一致 | [store/migrate.go:24-27](../store/migrate.go#L27) |

### 1.3 鉴权现状

| 事实 | 位置 |
|---|---|
| 业务线鉴权 `ValidateAPIKey`：查 tokens 表 | [middleware/auth.go](../middleware/auth.go) |
| 已校验 `user.status != 1 → user_disabled` | [middleware/auth.go:123-126](../middleware/auth.go#L123-L126) |
| 管理线鉴权 `RequireAdmin`：Bearer 命中 `setting.GetAdminKeys()`，常量时间比较 | [middleware/admin_auth.go:25-71](../middleware/admin_auth.go#L25-L71) |
| 失败返 401 `admin_required` | [middleware/admin_auth.go:77-83](../middleware/admin_auth.go#L77-L83) |
| 错误体统一形状 | [middleware/auth.go:129-139](../middleware/auth.go#L129-L139) → `{"error":{"message","type","code"}}` |

### 1.4 前端现状

| 事实 | 位置 |
|---|---|
| 多页面原生 HTML + Vue 3.4.21 + axios 1.6.7（CDN） | [router/admin-login.html:7-8](../router/admin-login.html#L7-L8) |
| 凭证存 **sessionStorage 单槽** `KEY_STORAGE` | [router/admin-shell.js:12-15](../router/admin-shell.js#L12-L15) |
| `http = axios.create({baseURL:'/api'})`，拦截器只加一枚 `Authorization: Bearer <槽值>` | [router/admin-shell.js:18-21](../router/admin-shell.js#L18-L21) |
| 登录页**只有 API Key 一个输入** | [router/admin-login.html:19](../router/admin-login.html#L19) |
| 错误读取路径 `e.response.data.error.message` | [router/admin-login.html:48](../router/admin-login.html#L48) |
| 列表统一信封 `{items,total,limit,offset}` | [controller/users.go:112-117](../controller/users.go#L112-L117) |
| 分页统一口径 limit 默认 20 / 上限 100 | [controller/users.go:24-43](../controller/users.go#L24-L43) |
| `/dashboard/login` 公开无鉴权 | [router/router.go:108](../router/router.go#L108) |
| 页面壳走 `htmlAwareAdmin`（HTML 请求返壳，非 HTML 强制鉴权） | [router/router.go:91-106](../router/router.go#L91-L106)、[router/router.go:250](../router/router.go#L250) |
| ⚠️ **`/api/admin/settings` 已存在**（GET/PUT） | [router/router.go:172-173](../router/router.go#L172-L173) |
| `/api/settings` 为过渡期别名（同一 handler） | [router/router.go:177-182](../router/router.go#L177-L182) |
| 全局趋势/按天聚合接口**不存在** | 全库 grep 无 `trend`/`GROUP BY date` |

### 1.5 依赖现状

| 事实 | 位置 |
|---|---|
| go 1.26.0（windows/amd64），GOPROXY=`goproxy.cn,goproxy.io,direct` | `go env` |
| `go.mod` **仅有** `gorilla/mux` + modernc/sqlite 系列 | [go.mod](../go.mod) |
| 模块缓存仅 `golang.org/x/sys@v0.47.0`，**无 golang-jwt / x-crypto** | `$GOMODCACHE` |

---

## §2 决策总表（D1–D33，已拍板）

### 2.1 基础决策

| 编号 | 决策 | 定案 |
|---|---|---|
| D1 | 计费模型 | **C（new-api 双扣）**：`users.balance` 主扣，token 可选限额 |
| D2 | 充值方式 | 手动 + 充值码；**第三方支付本期不做**，留接口 |
| D3 | 初始余额 | 默认 0，settings `init_balance` 可配 |
| D4 | 注册开关存储 | DB `settings` 表 |
| D5 | admin 角色来源 | seed 默认 user `default` → `is_admin=1` |
| D6 | 登录态存储 | `sessionStorage`（关页即登出） |
| D7 | 门户前缀 | `/portal` |

### 2.2 认证与安全

| 编号 | 决策 | 定案 |
|---|---|---|
| D8 | admin 初始密码 | `default` 空密码 + 首次登录强制设密 |
| D9 | JWT 有效期 + 改密踢旧 | 24h；`users.token_version`，claims 带 `ver`，不一致 401 |
| D10 | 登录防爆破 | 账号级 5 次失败锁 15 分钟（内存，重启失效） |
| D14 | 设密安全通道 | 本机来源 **或** 有效 admin_key |
| D19 | 密码规范 | 长度 ≥8、禁空/纯空白，注册与改密共用 `ValidatePassword` |
| D21+D30 | JWT secret 来源 | 与 DB 同目录派生 `filepath.Join(filepath.Dir(dbPath),"jwt_secret")`；env `TTS_JWT_SECRET_PATH` 可覆盖 |
| D22 | 注册开关公开可读 | `GET /api/auth/register-status → {"allow":bool}`，无鉴权 |
| D23 | 首次设密接口 | `POST /api/auth/setup-password`；本机 或 Bearer admin_key |
| D24+D31 | 存量 default 空密码窗口 | 仅当 `allow_register` **从未设置过**才置 0 + WARN 日志 |
| D26 | admin 建号扩字段 | 扩 `balance?/is_admin?/password?`；**最后一个 admin 保护**（不可降级/删除/禁用） |
| D28 | `/api/me/*` 接纳 admin JWT | 接纳，按 `user_id` 限自己 |
| **D29** | **空密码签发 JWT 漏洞** | **`password_hash` 为空 → login 永不签发凭证**，仅返 `{need_setup_password:true}` |
| D32 | 设密本机判定 IP 源 | **只读 `r.RemoteAddr` socket peer**，忽略一切 XFF / X-Real-IP |

### 2.3 计费与数据

| 编号 | 决策 | 定案 |
|---|---|---|
| D11 | token 限额准入时机 | 预扣前预检：① balance 够 ② limit 未超 ③ 才预扣 |
| D12 | 充值码 | **本期做** |
| D13 | 注册赠送 | 默认 0，`init_balance` 可配 |
| D16 | `init_balance` 作用域 | **仅自助注册**；admin 建号默认 0，可显式填 |
| D17 | limit 并发原子性 | 原子预占；结算/退款同步回退 `used_quota` |
| D18 | `limit_quota` 可空表示 | Go `*int64`（nil=不限），DB `limit_quota INTEGER` NULL |
| D20 | 充值码并发兑换 | 原子 `UPDATE ... WHERE used_by IS NULL` + `RowsAffected==1`，否则 409 |
| D25 | `charge_logs.type` 枚举 | `charge / redeem / manual_adjust / grant`；消费在 usage、退款在 usage.status |
| D27 | 预占与 balance 事务边界 | **同一事务**完成，失败整体回滚，不做补偿 |
| D33 | 余额注入记账 | admin 建号填 balance / 注册赠 `init_balance` 都写 `charge_logs(type=grant)`；`charge_logs` **去掉 `token_id`** |

### 2.4 其它

| 编号 | 决策 | 定案 |
|---|---|---|
| D15 | 统一登录页 | 升级 `admin-login.html` 双 tab；保留 `/dashboard/login` + 加 `/login` 别名 |
| — | 邀请分销 / 实名手机号 / i18n / 额度工单 | **均不做** |
| — | 移动端 | portal 基础响应式（复用 admin.css 响应式类） |
| — | 分页 | 沿用 `parsePagination`（limit 默认 20 / 上限 100，offset） |

---

## §3 补充决定（X1–X27，全部定案；开放问题 0 项）

> 原策划留白的部分，由本节补齐。**X1–X27 全部为定案**（X24 由用户直接拍板，X26/X27 为原 X-Q2/X-Q3 的定案），软件部按此实现即可。

### X1 依赖冻结（见 §4）
### X2 env 命名统一
只认 **`TTS_JWT_SECRET_PATH`**（secret 文件路径覆盖）。**取消**早期文档中的 `JWT_SECRET`（与 `TTS_DB_PATH` / `TTS_ADMIN_KEY` 前缀约定保持一致）。

### X3 settings **扩展而非新建**
[`/api/admin/settings`](../router/router.go#L172-L173) 已存在，本期**只扩字段**：
- `SettingsResponse` 增 `allow_register bool`、`init_balance int`
- `SettingsUpdateRequest` 增 `AllowRegister *bool`、`InitBalance *int`
- `/api/settings` 别名（同一 handler）自动继承，**不新增路由**
- 键白名单：[store/settings.go:16](../store/settings.go#L16) `allowedSettingsKeys` 当前为空 = 全放行。本期**不收紧**（保持宽松，避免引入回归），但在注释中登记这两个键名。

### X4 前端**单一凭证槽**
不拆双槽。sessionStorage 沿用 [admin-shell.js](../router/admin-shell.js#L12) 的 `KEY_STORAGE` 单槽，槽内存 **admin_key 或 JWT 二者之一**。
- `RequireAdminOrJWT`：**先试 JWT 解析**，失败再走 admin_key 常量时间比较
- `ValidateSession`：只接受 JWT
- 好处：双 tab 不再互相清凭证；`/portal` 与 `/dashboard` 共用一套登录态逻辑

### X5 `/api/me/stats` **增加 `trend`**
3b 无按天聚合接口，前端又不能用分页（上限 100 条）拉全量自己聚合 → **必须后端出趋势**。按 `date(created_at)` 分组，默认 14 天、上限 90 天，只统计 `status=1`。

### X6 `PATCH` 的「省略 vs null」语义
用自定义类型区分三态：

```go
// OptInt64：Set=false 表示字段未出现（不改）；Set=true 且 Value=nil 表示 null（清为不限）
type OptInt64 struct {
    Set   bool
    Value *int64
}
func (o *OptInt64) UnmarshalJSON(b []byte) error { o.Set = true; ... }
```

### X7 `setup-password` **只允许给未设密账号**
`password_hash` 非空 → 409 `password_already_set`。防止 admin_key 通道被当作「任意改他人密码」后门。已设密用户的改密走 `/api/me/password`，重置走 admin 的 `password-reset`。

### X8 `/api/me/*` 返回精简 DTO
不外泄 `key_hash` / `user_id` / `request_id` / `channel_id`。门户不需要这些，减少面。

### X9 改密返回新 token
`POST /api/me/password` 成功后 `token_version += 1` 并**返回一枚新 token**，避免用户被当场踢出，同时别处旧 token 立即失效。

### X10 `/portal` 页面壳公开
与 `/dashboard` 同策略（[router.go:91-95](../router/router.go#L91-L95) 的设计理由）：页面壳不含任何数据，数据全部来自 JWT 网关后的 `/api/me/*`。因此 `/portal`、`/login`、`/register` 均**不套鉴权中间件**。
- `/login` = `/dashboard/login` 的**别名**，同一个升级后的双 tab 页面
- 登录成功后按 `is_admin` 分流：`/dashboard` 或 `/portal`

### X11 迁移顺序与 `schemaVersion`
`schemaVersion` 4 → **5**；同时把 [store/migrate.go:27](../store/migrate.go#L27) 的 `CurrentVersion()` **改为返回 `schemaVersion`**，消除 1 vs 4 的不一致（纯对齐，无行为变化）。

### X12 迁移语句幂等性
全部走 `ensureColumn` / `CREATE TABLE IF NOT EXISTS` / `INSERT OR IGNORE` / 带条件的 `UPDATE`。`migrate()` 每次启动都跑（[db.go:64](../store/db.go#L64)），因此**每条语句都必须可重复执行**。

### X13 `charge_logs` 不含 `token_id`
按 D33。去掉后该列现实中恒 NULL，属噪音。

### X14 密码散列
bcrypt，`password_hash` 存完整 bcrypt 串（`$2a$10$...`）。**空串 = 未设密**（与 D29 判定一致）。cost 见 §4。

### X15 登录限流实现
内存 map，key = **用户名小写化**（`strings.ToLower(strings.TrimSpace(name))`）。5 次失败 / 15 分钟；成功登录清零；`need_setup_password` 分支**同样计入**（防账号枚举）。重启失效，接受。

### X16 预扣行的唯一性保护（配合 X-Q2）
引入 `status=0` 后，若同一 `request_id` 被重放，会产生**两条 pending 行 → 双重预扣**。加部分唯一索引兜底：

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_request_pending
    ON usage(request_id) WHERE status = 0;
```

语义：**同一 `request_id` 至多一条 pending 行**。预扣 INSERT 命中冲突即视为重放，直接回滚返 402（或幂等返回既有的预扣结果）。
配套规则：**预扣是唯一的 INSERT 点**；结算/退款一律 `UPDATE ... WHERE request_id=? AND status=0` 并用 `RowsAffected==1` 做单向前进保护。

### X17 时间口径统一（**修一个现存显示缺陷**）
`created_at` 由 `datetime('now')` 产生，是 **UTC**，格式 `YYYY-MM-DD HH:MM:SS`（无时区标记）。

| 面 | 现状问题 | 本期决定 |
|---|---|---|
| **服务端趋势分组** | `GROUP BY date(created_at)` 按 **UTC 日**切分；北京时间 07:00 的调用会被算进**前一天**，柱状图整体错位 8 小时 | **按 `date(created_at, '+8 hours')` 分组**，偏移量抽为常量 `statsTZOffset = "+8 hours"` |
| **前端时间显示** | [admin-shell.js:84-91](../router/admin-shell.js#L84-L91) `formatStart` 用 `new Date("YYYY-MM-DD HH:MM:SS")`——无时区标记，浏览器按**本地时间**解析，于是 UTC 值被当成本地时间显示，**差 8 小时**（Safari 更会直接判为 Invalid Date） | **新增 `/api/me/*` 的时间字段统一返回 RFC3339 UTC**（带 `Z`，如 `2026-02-01T10:00:00Z`），前端用 `new Date(...)` 转本地时间显示 |
| 3b 管理页 | 同一缺陷已存在 | **本期不动**（遵守"3b 管理前端保持不动"），登记为已知问题；若软件部愿意顺手修则更佳 |

> ⚠️ 若不做这条，门户的"每日用量"图会稳定错位 8 小时，属上线后必被投诉的问题。

### X18 余额变更与 `charge_logs` **必须同事务**
`charge` / `redeem` / `manual_adjust` / `grant` 四类操作都要：`UPDATE users SET balance=...` + `INSERT charge_logs` 在同一 `tx` 内，`balance_after` 取更新后的真实值。
理由与 D27 同源：`SetMaxOpenConns(1)` 下单连接事务天然串行，若分两步，中途失败会导致"钱变了但没流水"（对账不可追溯，直接违背 D33 的审计目标）。
**新增方法需 Tx 版**（如 `UserAddBalanceTx(tx, userID, delta, log)`），理由同 [db.go:294](../store/db.go#L294) 的自锁约束。

### X19 `jwt_secret` 文件读写细节
```
路径 = env TTS_JWT_SECRET_PATH（非空）
       否则 filepath.Join(filepath.Dir(dbPath), "jwt_secret")
```
- 文件**不存在** → 生成 32 字节 `crypto/rand` → **原子写**（写临时文件 → `Chmod 0600` → `os.Rename`），避免并发/断电产生半截文件
- 文件**存在但为空或长度 < 32** → **fail-fast 启动失败**（宁可起不来，也不要静默换 secret 把所有人踢下线）
- 文件**存在且不可读/不可写** → fail-fast，日志给出确切路径
- env 指向的路径**所在目录不存在** → 不自动创建，fail-fast 并提示（避免在意外位置落凭证）
- Windows 下 `0600` 基本无效（只影响只读属性），D30 已接受；容器/生产以 Linux 为主
- **不打印 secret 本身**，日志只打路径与字节数

### X20 充值码生成重试
`code` 为 16 位大写字母+数字随机串（字符集 `A-Z0-9`，约 36¹⁶ 空间）。批量生成时若命中 `UNIQUE` 冲突，**对该条重试**（上限 5 次），仍失败则整体回滚返 500。**不允许**把冲突静默跳过（会造成"生成的码少了一部分"）。
批量上限 `count ≤ 1000`，`quota ≥ 1`。

### X21 为什么不需要 CSRF token
本期鉴权凭证全部走 `Authorization: Bearer`（**非 Cookie**），浏览器不会自动携带 ⇒ 传统 CSRF 不成立。同时 CORS 受现有 [middleware/cors.go](../middleware/cors.go) 管控（默认不同源）。
**约束（必须遵守）**：`/api/me/*` 与 `/api/auth/*` **不得**改为读写 Cookie 的鉴权方式；一旦引入 Cookie 就必须补 CSRF 防护。此条写进代码注释，防未来重构踩坑。

### X22 `/api/me/usage` 与 `/api/me/balance/logs` 的过滤参数
门户两张流水表需要筛选，仅 `limit/offset` 不够：

| 接口 | 查询参数 |
|---|---|
| `GET /api/me/usage` | `limit`、`offset`、`token_id?`、`status?`（0/1/2）、`from?`、`to?`（`YYYY-MM-DD`，按北京时间边界解释，与 X17 一致） |
| `GET /api/me/balance/logs` | `limit`、`offset`、`type?`（`charge\|redeem\|manual_adjust\|grant`） |

- 一律**强制** `WHERE user_id = <JWT 里的 user_id>`，与筛选条件是 `AND` 关系；**任何情况下不得**由查询参数指定 `user_id`（越权面）
- 非法 `status`/`type` 值 → 400 `invalid_filter`（不静默忽略）
- 排序统一 `ORDER BY id DESC`
- 现有 `usage` 索引 [db.go:252-261](../store/db.go#L252-L261) 有 `(user_id, created_at)` 与 `(token_id, created_at)`，够用；`charge_logs` 用 §5.3 的 `(user_id, created_at)`

### X23 前端凭证槽的具体落法（细化 X4）
事实：[admin-shell.js:10](../router/admin-shell.js#L10) 键名常量 `KEY_STORAGE = 'ttsAdminKey'`。

- **沿用 `'ttsAdminKey'` 字面键名**（避免强制全员重新登录、把 diff 压到最小）。槽内值可以是 **admin_key 或 JWT** 二者之一。
- `portal-shell.js` 读写**同一个**底层键，因此"登录一次，门户与管理页共用登录态"。
- **API 收敛**：新增中性别名 `getCred / setCred / clearCred` 作为**新代码的唯一入口**；`getKey / setKey / clearKey` 保留为薄别名，让 6 个现有 admin 页**零改动**。
- **`sessionStorage` 是 per-tab 的**（[admin-shell.js:12](../router/admin-shell.js#L12) 注释"跨页面同标签页共享"）：管理员在标签页 A、普通用户在标签页 B **互不干扰**，这是选它而非 `localStorage` 的额外收益。
- ⚠️ **已知副作用**：[admin-shell.js:24-29](../router/admin-shell.js#L24-L29) 的 401 拦截器会 `clearKey()`。门户页遇到 401 也会清掉共享槽——同一标签页内本就是同一身份，可接受；但**不要**在门户页里改成跳 `/dashboard/login`。
- 新增 shell API（供双 tab 登录页使用）：
  ```js
  loginWithPassword(name, password)   // POST /api/auth/login
     // 成功 → setCred(token)，返回 {is_admin, name}
     // need_setup_password=true → 【不写入任何凭证】，返回该标记让 UI 跳设密视图
  loginWithAdminKey(key)              // 即现有 login(key)：setKey → GET /api/admin/overview 验证
  setupPassword(name, password)       // POST /api/auth/setup-password（需本机或 admin_key 通道）
  ```

### X24 欠费停服（**用户定案**，取代 X-Q1）
> 定案原文：**「用户余额为负，然后停止为用户提供 API 服务，直至钱包充值完成」**

| 项 | 规则 |
|---|---|
| 结算补扣 | **允许把 `balance` 扣成负**；不拒结算、不钳 0（与 3b token 层透支行为一致，账目不与真实用量脱节） |
| 停服条件 | **`balance < 0` → 拒绝该用户的一切 API 调用**，返回 402 |
| 解除条件 | `balance >= pre`（本次调用的预扣额）。**必须先把欠费填平、再留出本笔预扣额**；只回到 0 不足以放行 `pre>0` 的调用 |
| 实现 | **无需新逻辑**。`UserPreConsume` 的 `WHERE balance >= pre` 天然覆盖：`-5 >= 12` 假、`-5 >= 0` **也假** ⇒ `RowsAffected==0` ⇒ 402。**连 `pre = 0` 的边界（空字符、unlimited 探针）也被挡住** |

**⚠️ 连带决定 ①：`unlimited=1` token 同样受停服约束（定案：是）**
理由：停服是**用户级**策略，不是 token 级配额；否则一枚 unlimited token 即"免死金牌"，整套计费对该用户失效。
- 实现：`billing.PreConsume` 的 `unlimited` 分支**不再无条件短路**，增加一次零成本探针（`balance >= 0`），为负 → 402。**仍不产生任何扣减 SQL、`pre` 仍记 0。**
- 必须同步改写 [billing.go:121-123](../billing/billing.go#L121-L123) 的注释为：**`unlimited` = 不计费，但**仍受欠费停服约束。
- 副作用：seed 给 `default` 的 unlimited token 也受约束（admin 给自己充值即可）。
- **若运维希望 admin 永久豁免**：不要新增"豁免"状态位（会开一个新的绕过面），改用给 admin 钱包充足余额的运维约定。

**⚠️ 连带决定 ②：错误码区分（定案：区分）**
沿用固定信封 `{"error":{message,type,code}}`（[auth.go:129](../middleware/auth.go#L129)），靠两个 code 表达两种语义：

| 条件 | HTTP | code | 语义 |
|---|---|---|---|
| `balance < 0` | 402 | **`balance_negative`** | 已欠费停服，须充值恢复 |
| `0 <= balance < pre` | 402 | `insufficient_balance` | 余额不足本笔预扣 |

**逃生通道（已验证，无死锁）**：`/api/me/*` 只校验 JWT、**不校验余额** ⇒ 欠费用户**仍能登录门户并 `POST /api/me/redeem` 兑换充值码**。`/api/auth/login`、`/api/auth/register-status`、`/api/me/*` 全部不受欠费影响。**充值码是欠费状态下唯一的自助恢复路径，实现时不得给它加余额校验。**

**前端（本人）**：
- `balance < 0` → 门户顶部**红色横幅**「账户已欠费，API 已暂停，请充值后恢复」
- `balance == 0` → **黄色提示**「余额不足，请充值」
- 402 的 `balance_negative` → 引导跳「余额/兑换」区块

**运维**：每次因欠费拒绝打一条 WARN（含 `user_id`、`balance`、路径），便于发现异常用户。

### X25 结算补扣导致 `limit_quota` 小幅穿透（**定案 (a)：接受穿透**）
与 X-Q1 同根因（`actual > pre`）：
- 预扣断言 `used_quota + pre <= limit_quota`
- 结算执行 `used_quota += (actual - pre)`，而中文语速下 `actual - pre > 0` 是常态 ⇒ **最终 `used_quota` 可超出 `limit_quota` 约 5%**

| 选项 | 后果 | 建议 |
|---|---|---|
| **(a) 接受穿透，明确语义**（**定案 ✅**） | `limit_quota` 语义 = **准入阈值**（预扣前判定），不是硬上限；实际用量可小幅超出。§18 断言改为 `used_quota <= limit_quota × 1.1` | ✅ 实现最简，与"限额是防跑超不是精确封顶"的定位一致 |
| (b) 结算后钳制 | 需在结算 tx 内反向判断并把 `used_quota` 钳到 `limit_quota`，此时**计数器与 `usage.actual_quota` 总额不再相等**，对账口径被破坏 | ❌ 不推荐 |
| (c) 预扣乘数提高 | 把 `Multiplier` 从 1.2 提到 1.3+ 覆盖秒数项，但会放大预扣对余额的压力，且不同语速仍需不同系数 | ❌ 治标 |

> **附带事实（原策划未写）**：`pre = ceil(chars × 1.2)`、`actual = ceil(chars + 秒数)`，**分界点是 5 字/秒**。中文 TTS 常见 4–5 字/秒且音频含停顿 ⇒ **补扣是常态，退款才是少数**。这直接决定 X-Q1/X24 与 X25 的重要性——它们不是边缘场景。

### X26 `usage.status=0` 预扣中（**定案 A：引入**，取代 X-Q2）

**为什么必须引入**：D27 要求「balance 预扣 + used_quota 预占 + **usage 落行**」同一事务，但 3b 的 usage 行是**结算/退款时才 INSERT** 的（[usage.go:168](../store/usage.go#L168) / [usage.go:224](../store/usage.go#L224)），预扣只改额度、不留任何痕迹。要落实 D27 字面，必须新增中间态。

| 方案 | 结论 |
|---|---|
| **(A) 引入 `status=0`** | ✅ **定案** |
| (B) 预扣不留流水 | ❌ 崩溃/被 kill 时"钱扣了但无流水"，只能靠 `balance` 与 `usage.actual_quota` 总额的差知道「总账少了 X」，**无法定位是哪个 `request_id`** —— 计费系统的审计底线，不接受 |
| (C) 另开 `usage_reservations` 表 | 🟡 已排除（多一张表 + 跨表对账 + 门户列表需 join） |

**实现规则（定案）**：

| 阶段 | 动作 |
|---|---|
| 预扣 | **唯一的 INSERT 点**：`INSERT usage(status=0, pre_consumed=Pre, actual_quota=0)` |
| 结算 | `UPDATE usage SET status=1, actual_quota=? WHERE request_id=? AND status=0` |
| 退款 | `UPDATE usage SET status=2 WHERE request_id=? AND status=0` |
| 保护 | 结算/退款均以 **`RowsAffected==1`** 做**单向前进**校验；已 `status=1` 时**拒绝退款** |

**三个必须同步处理的连带项**：

1. **移除** [usage.go:45-47](../store/usage.go#L45-L47) 的 `if u.Status == 0 { u.Status = UsageStatusSettled }` 兜底 —— 否则 pending 行会被当成 settled 读。⚠️ 移除前**先 grep 确认 `UsageInsert` 无其它调用点**（当前计费唯一路径是 billing）。
2. **所有读 `usage.status` 的地方都要能处理 0**：管理页流水表 [admin-user.html](../router/admin-user.html)、`/api/me/usage`、以及任何按 `status=1` 过滤的统计。逐个 grep `UsageStatus` 与 `status` 字面量确认。
3. 与 **X16** 配套：部分唯一索引 `CREATE UNIQUE INDEX ... ON usage(request_id) WHERE status=0` 是防重放的落脚点（没有 pending 态它无处可落）。

**额外收益（顺带修掉 3b 一个真实缺陷）**：
3b 的退款幂等只查 `status=2`（[usage.go:232](../store/usage.go#L232)），而结算插入的是 `status=1` ⇒ 若出现「先结算、后跨进程重试退款」，3b 会**再退一次预扣**（重复退钱）。新规则 `0→2` 单向前进 + 已 `status=1` 拒绝退款，把这个洞自然堵上。

**前端**：门户与管理页的流水状态需显示三态 —— `0=进行中`、`1=已结算`、`2=已退款`。

### X27 依赖落定（**定案**，取代 X-Q3）

**实测结论（v1.3，已在本机跑过）**：`GOPROXY=https://goproxy.cn,https://goproxy.io,direct` **可达**，两个模块的版本列表均成功解析。

| 模块 | **钉定版本** |
|---|---|
| `github.com/golang-jwt/jwt/v5` | **`v5.3.1`**（实测最新） |
| `golang.org/x/crypto` | **`v0.58.0`**（实测最新，提供 bcrypt） |
| bcrypt cost | `10` |

**定案：保留 bcrypt，不改用 PBKDF2。** 网络既然通，不为省一个依赖去偏离既定方案（§1.2 明确要求 bcrypt）。

**备案（仅在将来交付到完全内网/离线环境时启用，本期不做）**：

| 原本要 | stdlib 替代（已用 `go doc` 验证存在） |
|---|---|
| `golang.org/x/crypto/bcrypt` | **`crypto/pbkdf2`** —— Go 1.24+ 已提升为 stdlib，本机 Go 1.26 实测存在。用 PBKDF2-HMAC-SHA256 + 16B 随机盐 + ≥600k 迭代（OWASP 建议） |
| `golang-jwt/jwt/v5` | `crypto/hmac` + `crypto/sha256` + `encoding/base64` + `encoding/json` 手写 HS256（约 60 行）。**若走这条路，必须做对三件事**：① 严格拒绝 `alg != "HS256"`（alg 混淆攻击入口）② 用 `hmac.Equal` 恒定时间比较 ③ 校验 `exp` |

**⚠️ 执行环境注意**：本机 DSH 文件沙箱只允许写项目目录，`go get` 需写 `GOMODCACHE`（`C:\Users\33713\go\pkg\mod`）会被拒（实测报 `Access is denied`）⇒ **`go get` 必须在沙箱外执行**。这不是设计问题，是执行环境限制。

**连带影响**：`x/crypto v0.58.0` 依赖 `golang.org/x/sys`，而 [go.mod](../go.mod) 现钉 `x/sys v0.47.0`（indirect）⇒ `go get` 可能要求升级它。属机械改动，但注意别带进其它漂移。

### X-Q 开放问题（**已全部关闭**）

| 原编号 | 问题 | 定案 |
|---|---|---|
| **X-Q1** | 结算补扣导致 `balance` 为负 | ✅ 允许为负 + 欠费停服 → **X24** |
| **X-Q2** | 是否引入 `usage.status=0（预扣中）` | ✅ **引入** → **X26** |
| **X-Q3** | 离线/内网环境的依赖获取 | ✅ 实测可联网，钉 `jwt v5.3.1` + `x/crypto v0.58.0` → **X27** |

---

## §4 依赖冻结

```go
require (
    github.com/golang-jwt/jwt/v5  v5.3.1   // 已实测可解析
    golang.org/x/crypto           v0.58.0  // bcrypt；已实测可解析
)
```

| 模块 | 选择 | 理由 |
|---|---|---|
| JWT | **`github.com/golang-jwt/jwt/v5` `v5.3.1`** | v5 为当前活跃维护线；v4 为 legacy。原策划写的无版本号 `golang-jwt/jwt` 指向 v4 import 路径，**以 v5 为准**。版本号已由 `go list -m -versions` 实测确认存在（v5.3.1 为最新）。 |
| bcrypt | `golang.org/x/crypto` **`v0.58.0`** | stdlib 无 bcrypt 替代（**PBKDF2 备案见 X27**）。版本号已实测确认存在。 |
| bcrypt cost | **10**（`bcrypt.DefaultCost`），常量 `bcryptCost = 10` | 单次约 60–100ms，配合 D10 限流不影响体验；后续可调。 |

**落地命令（必须在沙箱外执行，原因见 X27）**：

```bash
cd D:\项目\Volcano-Engine-TTS-UI.git
go get github.com/golang-jwt/jwt/v5@v5.3.1
go get golang.org/x/crypto@v0.58.0
go mod tidy
```

预期：`go.mod` 出现两条 require；`go.sum` 落定；**可能连带升级 `golang.org/x/sys`**（现钉 `v0.47.0`，indirect）—— 留意别带进其它漂移。
若解析出的补丁版更高，**以 `go.sum` 实际落定为准并同步回填本节**。

**强制要求（安全）**：JWT 解析**必须**显式限定算法，防 alg 混淆攻击：

```go
token, err := jwt.ParseWithClaims(raw, &Claims{},
    func(t *jwt.Token) (any, error) { return secret, nil },
    jwt.WithValidMethods([]string{"HS256"}), // 必须
)
```

**JWT claims 契约**：

```json
{
  "sub": "12",            // user_id（字符串，jwt.RegisteredClaims.Subject）
  "is_admin": true,
  "ver": 3,               // users.token_version
  "iat": 1770000000,
  "exp": 1770086400       // iat + 24h
}
```

---

## §5 Schema 变更（schemaVersion 4 → 5）

> 全部走 `ensureColumn` / `CREATE TABLE IF NOT EXISTS`，**可重复执行**。

### 5.1 `users` 新增 4 列

```sql
ALTER TABLE users ADD COLUMN password_hash TEXT    NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN is_admin      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN balance       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN token_version INTEGER NOT NULL DEFAULT 0;
```

- `quota`：**保留但废弃**（前端不显示、后端不读写）
- `used_quota`：**继续做聚合对账镜像**（结算时 `+= Actual`），不参与准入
- 幂等升级：`UPDATE users SET is_admin = 1 WHERE name = 'default' AND is_admin = 0`

### 5.2 `tokens` 新增 1 列

```sql
ALTER TABLE tokens ADD COLUMN limit_quota INTEGER;   -- NULL = 不限
```

- `remain_quota`：**冻结**（迁移不改，不再 UPDATE，仅前端只读展示）
- `used_quota`：语义变更为**限额预占计数器**（见 §7.3）

### 5.3 新表 `charge_logs`（资金流水/审计）

```sql
CREATE TABLE IF NOT EXISTS charge_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       INTEGER NOT NULL REFERENCES users(id),
    type          TEXT    NOT NULL,                 -- charge|redeem|manual_adjust|grant
    delta         INTEGER NOT NULL,                 -- 正负均可
    balance_after INTEGER NOT NULL,
    source        TEXT    NOT NULL DEFAULT '',      -- manual|redeem|admin_create|register_bonus
    remark        TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_charge_logs_user_time ON charge_logs(user_id, created_at);
```

> **注意**：本表**只记余额来源**（充值/兑换/手动调整/赠送）。**调用消费不在此表**，由 `usage.actual_quota` 覆盖（D25，不双写）。
> **前端含义**：门户需要**两张**流水表 —— 调用流水（usage）+ 余额流水（charge_logs），不能只显示其中一张，否则钱包历史不完整。

### 5.4 新表 `redemptions`（充值码）

```sql
CREATE TABLE IF NOT EXISTS redemptions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    code       TEXT    NOT NULL UNIQUE,   -- 16 位大写字母+数字
    quota      INTEGER NOT NULL,
    used_by    INTEGER,                   -- users.id，NULL=未使用
    used_at    TEXT,
    remark     TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL DEFAULT (datetime('now'))
);
```

### 5.5 `usage.status` 三态（**X26 定案 A：引入**）

| 值 | 含义 |
|---|---|
| **0** | **pending 预扣中**（新增） |
| 1 | settled 已结算（存量行为 1，兼容） |
| 2 | refunded 已退款 |

### 5.5b `usage.upstream_req_id`（v1.5 新增列，**实施期发现的设计修正**）

```sql
ALTER TABLE usage ADD COLUMN upstream_req_id TEXT NOT NULL DEFAULT '';
```

**为什么需要这一列**：三段式的定位键必须是 `request_id`，而它又必须在整条生命周期内
**保持稳定**。最初实现是「结算时把 `request_id` 改写成上游 ReqID」，结果单测立刻抓到问题：
结算之后再按原键（本地号）查行会**查不到**，退款路径退化成 `ErrNotFound`，
状态机无法区分「已结算」（应拒）与「不存在」（数据异常）。

改法：`request_id` 恒为**本地请求号**（三段式定位键 + 预扣幂等键 + 部分唯一索引的键），
上游 ReqID 另存 `upstream_req_id` 仅供对账追溯。

| 列 | 语义 | 谁来写 |
|---|---|---|
| `request_id` | **稳定定位键**，`local-<nano>-<seq>` | 预扣时写入，之后**永不修改** |
| `upstream_req_id` | 上游（火山）返回的 ReqID | 结算时写入；失败退款时留空 |

> 收益：`UsageRefund` 永远能定位到 pending 行并给出**准确**的状态判定
> （`0` 可退 / `1` 已结算→拒绝 / `2` 已退→幂等），而不是撞上 `ErrNotFound`。
> 该行为由 `store/stage4_test.go` 的 `TestRefund_AfterSettleMustBeRejected` 固化。

### 5.6 `settings` 新增 2 键（不建表）

| 键 | 类型 | 默认 | 语义 |
|---|---|---|---|
| `allow_register` | bool（`"1"`/`"0"`） | **未设置**（首次迁移时按 D31 置 `"0"`，仅当从未设置） | 是否允许自由注册 |
| `init_balance` | int | 0 | 自助注册赠送额度 |

---

## §6 鉴权矩阵

| 路径 | 中间件 | 接受的凭证 | 备注 |
|---|---|---|---|
| `POST /v1/audio/speech` | `ValidateAPIKey` | API Key（tokens 表） | **不变** |
| `/api/me/*` | **`ValidateSession`（新）** | **仅 JWT** | admin_key **不可**用（#14）；admin JWT 可用（D28） |
| `/api/admin/*` | **`RequireAdminOrJWT`（新）** | admin_key **或** admin JWT | 替换现有 `RequireAdmin` 的挂载点 |
| `/dashboard*` | `htmlAwareAdmin` → 内部用 `RequireAdminOrJWT` | 同上 | ⚠️ **仅非 HTML 请求返 403**，见下方更正 |
| `/api/auth/register-status` | **无** | — | 公开（D22）。**只返回 `{"allow":bool}`，不读 settings 全表** |
| `/api/auth/login`、`/api/auth/register`、`/api/auth/setup-password` | **无** | setup-password 内部自校验通道（D23/D32） | 公开挂载 + handler 内校验 |
| `/login`、`/register`、`/portal` 页面壳 | **无** | — | X10；壳无数据 |
| `/dashboard/login`、`/login` | **无** | — | 已有事实（[router.go:108](../router/router.go#L108)） |

**`RequireAdminOrJWT` 判定顺序**（X4）：先按 JWT 解析（`WithValidMethods(["HS256"])` + `exp` + `ver` 比对 + `is_admin==true`），失败再走 `setting.GetAdminKeys()` 常量时间比较。两者皆失败 → 401 `admin_required`（保持现有 code，前端零改动）。

**`ValidateSession` 失败码**：

| 场景 | 状态 | code |
|---|---|---|
| 无/坏 Bearer | 401 | `session_required` |
| 签名/过期非法 | 401 | `session_invalid` |
| `ver` 不匹配（改密后） | 401 | `session_revoked` |
| user 不存在 | 401 | `session_invalid` |
| `user.status != 1` | 403 | `user_disabled` |

### 6.1 ⚠️ 更正：`htmlAwareAdmin` 对 HTML 请求**不设防**

原策划 #18 称「普通用户 JWT 访问 `/dashboard` 返 403」。**这句话只在非 HTML 请求下成立。** 源码事实（[router.go:250-259](../router/router.go#L250-L259)）：

```go
func htmlAwareAdmin(html http.Handler) http.Handler {
    guarded := middleware.RequireAdmin(html)
    return http.HandlerFunc(func(w, r) {
        if acceptsHTML(r.Header.Get("Accept")) {  // 浏览器地址栏/点链接
            html.ServeHTTP(w, r)                  // ← 直接返壳，不鉴权
            return
        }
        guarded.ServeHTTP(w, r)                   // 脚本/curl → 强制鉴权
    })
}
```

**推论（必须写进前端实现，否则会出 UX 事故）**：

1. 普通用户在手输 `/dashboard` 时**会拿到完整管理页外壳**（200），随后页面里的 `/api/admin/*` 全部 403 → [admin-shell.js:24-29](../router/admin-shell.js#L24-L29) 的 401 拦截器**清掉凭证** → 用户被踢回登录页，且不知道为什么。
2. 因此 **`is_admin` 分流必须由前端在登录后立即执行**（D15），不能依赖后端壳层拦截。
3. **补充加固（建议软件部做，成本极低）**：`/portal` 与 `/dashboard` 的壳路由也可加一层"**壳层软分流**"——在 `htmlAwareAdmin` 之外新增 `htmlAwareSession`，当请求带**可解析的普通用户 JWT**（`Accept: text/html` 时无法读 header，故此路不通）… ⇒ 结论：**HTML 导航无法在服务端判定角色**（`sessionStorage` 不随请求发送），所以**唯一可靠防线就是前端分流 + API 层 403**。这与 3b 的设计取舍一致，接受。
4. 前端兜底：`/dashboard/*` 每个页面启动时先 `GET /api/admin/overview`，失败则 `clearCred()` 并跳 `/login`（现有 `mountShell` 已是此模式，仅需确认新页面同样处理）。

**`Caller` 与 context 传递约定**（[middleware/auth.go:35-55](../middleware/auth.go#L35-L55)）：

| 项 | 说明 |
|---|---|
| 现有 `Caller{UserID, TokenID, Unlimited}` | 业务线（API Key）用，**不改结构** |
| 新增 `SessionCaller{UserID, IsAdmin, TokenVersion}` | 会话线（JWT）用，**独立结构 + 独立 ctx key**，避免两条线互相污染 |
| 新增 `WithSessionCaller(ctx, *SessionCaller)` / `SessionCallerFromContext(ctx)` | 与现有 `WithCaller`/`CallerFromContext` 同构 |
| ⚠️ 复用风险 | **不要**把 JWT 身份塞进现有 `Caller`：`TokenID=0` 会让 `billing.PreConsume` 的 `tokenID == 0` 校验（[billing.go:132](../billing/billing.go#L132)）误判。两条线必须分开。 |

> `billing.PreConsume` 的 `LimitQuota` **不需要**进 `Caller`——D17 的限额判定由 `UserPreConsume` 内的原子 `UPDATE ... WHERE` 完成，不在 Go 侧比较。

---

## §7 计费时序

### 7.1 三段式全貌

```
POST /v1/audio/speech
  │
  ├─ ValidateAPIKey ──→ Caller{UserID, TokenID, Unlimited}
  │
  ├─ billing.PreConsume(db, userID, tokenID, unlimited, chars, ratios)
  │    ├─ unlimited → 短路，pre=0，**不发任何 SQL**
  │    └─ 否则 pre = ceil(chars × 1.0 × 1.0 × 1.2)
  │         └─【改】store.UserPreConsume(userID, tokenID, pre, usage)   ← 原 TokenDecreaseQuota
  │              BEGIN
  │                ① UPDATE users SET balance = balance - ? WHERE id = ? AND balance >= ?
  │                     RowsAffected==0 → ROLLBACK → 402
│                        balance<0   → code=balance_negative   （欠费停服，X24）
│                        0<=bal<pre  → code=insufficient_balance
  │                ② UPDATE tokens SET used_quota = used_quota + ?
  │                     WHERE id = ? AND (limit_quota IS NULL OR used_quota + ? <= limit_quota)
  │                     RowsAffected==0 → ROLLBACK → 402 token_limit_exceeded
  │                ③ INSERT usage(status=0, pre_consumed=pre, actual_quota=0)   ← X-Q2
  │              COMMIT
  │
  ├─ Synthesize（上游调用）
  │
  ├─ 成功 → Session.Settle(usage)
  │    └─【改】store.UsageSettle（同事务）
  │         ① UPDATE usage SET actual_quota=?, status=1 WHERE id=? AND status=0
  │         ② UPDATE users SET balance = balance - (actual - pre) WHERE id=?     ← 允许为负（X-Q1）
  │         ③ UPDATE tokens SET used_quota = used_quota + (actual - pre) WHERE id=?
  │         ④ UPDATE users SET used_quota = used_quota + actual WHERE id=?       ← 统计镜像，不变
  │
  └─ 失败 → Session.Refund(usage)
       └─【改】store.UsageRefund（同事务，幂等）
            ① 查同 request_id：若已 status=2 → 幂等返回 nil
                              若已 status=1 → 拒绝（已结算不可退），WARN
            ② UPDATE usage SET status=2 WHERE id=? AND status=0
            ③ UPDATE users  SET balance    = balance + pre WHERE id=?
            ④ UPDATE tokens SET used_quota = used_quota - pre WHERE id=?
```

### 7.2 三种落点的函数级清单（软件部实施用）

| # | 现状 | 改为 | 文件 |
|---|---|---|---|
| ① 预扣 | `store.TokenDecreaseQuota(id, amount)` | **`store.UserPreConsume(userID, tokenID, amount, usage) (ok bool, err error)`** | [store/tokens.go:220](../store/tokens.go#L220) 附近新增 |
| ② 结算 | `store.UsageSettle(in)` 改 token 额度 | 改 `users.balance` + `tokens.used_quota` + `UPDATE usage` | [store/usage.go:168](../store/usage.go#L168) |
| ③ 退款 | `store.UsageRefund(userID, tokenID, pre, u)` 改 token 额度 | 改 `users.balance` + `tokens.used_quota` + 单向前进状态 | [store/usage.go:224](../store/usage.go#L224) |
| — 调用点 | [billing/billing.go:147](../billing/billing.go#L147) `s.TokenDecreaseQuota(tokenID, pre)` | 换成 `UserPreConsume` | [billing/billing.go](../billing/billing.go) |
| — `PreConsume` 签名 | `PreConsume(s, userID, tokenID, unlimited, chars, r)` | **签名不变**（`userID`/`tokenID` 已够用），只换内部调用 | [billing/billing.go:128](../billing/billing.go#L128) |

> ⚠️ **v1.4 更正**：原文称「`billing.PreConsume` 签名不变」是**错的**。落地时发现：
> 预扣阶段就要落 `status=0` 的 pending 流水（决策 X26），而 `usage.request_id` 此刻
> **只能**用本地请求号（上游 ReqID 要等合成完成才知道）。因此 `PreConsume` 新增了
> `reqID string` 参数：`PreConsume(s, userID, tokenID, unlimited, chars, **reqID**, r)`。
> 结算时用该 `reqID` 定位 pending 行，再把 `request_id` 更新为最终值（成功时为上游 ReqID），
> 对账追溯性不受影响。`Session.reqID` 保存该定位键。

### 7.3 `tokens.used_quota` 语义变更（重要）

| 阶段 | 变更 |
|---|---|
| 预扣 | `used_quota += pre`（**预占**） |
| 结算 | `used_quota += (actual - pre)` → 净效果 `+= actual` |
| 退款 | `used_quota -= pre` → 净效果 `+= 0` |

- 结算与退款都**必须**由 `status=0 → 1/2` 的单向前进（`RowsAffected==1`）保护，**否则重复调用会把计数器算坏**
- **历史值参与限额判定**：存量 token 的 `used_quota` 是 3b 的累计消费额，设 `limit_quota` 时一上来就占用。**这是预期行为（限额=历史累计上限）**，前端展示需注明"含历史用量"
- 门户展示口径：`used_quota`(限额已用) / `limit_quota`(限额)，`remain_quota` 标注为"历史遗留（不再变动）"

### 7.4 ⚠️ 实施陷阱清单

| 陷阱 | 说明 |
|---|---|
| **单连接自锁** | `SetMaxOpenConns(1)`（[db.go:47](../store/db.go#L47)）。事务持有连接期间**绝不能**调用走 `s.db` 的 Store 方法，只能用 `tx`。已由 [db.go:294](../store/db.go#L294) 明确警告。**所有新增额度方法必须有 Tx 版本。** |
| **`UsageInsert` 的 0→1 兜底** | [usage.go:45-47](../store/usage.go#L45-L47)。引入 `status=0` 后必须移除该兜底。**实施前 grep 确认无其它调用点**（当前计费唯一路径是 billing）。 |
| **`UsageRefund` 幂等判重条件** | 现为 `status=2`。引入 pending 后应改为"找到该 `request_id` 的行"，并区分 0（可退）/1（已结算，拒）/2（已退，幂等）。 |
| **`unlimited` token** | 3b 短路、不发 SQL、`pre=0`。C 模型下 **unlimited 仍然短路**（既不看 balance 也不看 limit_quota）。语义：`unlimited=1` = 完全免计费。本期保持。 |
| **`channel_id` 可空** | `usage.channel_id` 允许 NULL（走兜底），scan 用 `*int64`。勿改。 |
| **前端 `Authorization` 单头** | 一枚 header 只能带一种凭证。X4 的单槽设计依赖"admin_key 与 JWT 都走 `Bearer`"，这是可行的，但**不要**在拦截器里同时读两个槽。 |
| **预扣重放 → 双重扣款** | 必须落实 X16 的 `idx_usage_request_pending` 部分唯一索引，并坚持"预扣是唯一 INSERT 点"。否则同一 `request_id` 重放会造出两条 pending 行，**用户被扣两次**。 |
| **时间口径** | X17。`GROUP BY date(created_at)` 是 UTC 日，不改会错位 8 小时；新接口时间戳必须带 `Z`，否则浏览器按本地时间解析又错一次。**这一条不做，上线必被投诉。** |
| **`billing.go` 的包注释** | [billing.go:13-14](../billing/billing.go#L13-L14) 明确写着"额度归属：**token 层**"。C 模型必须同步改写，否则后续维护者会按旧注释把扣费改回 token —— 这是最容易被漏掉、危害最大的一处文档债。 |
| **`.env.example` 与 README** | 需登记 `TTS_JWT_SECRET_PATH`（X2/X19）。现有 [.env.example](../.env.example) 有"启动引导环境变量"清单，漏登记会导致运维不知道有这个开关。 |
| **`usageStatusPending` 与既有读取方** | 新增 `status=0` 后，所有读 `usage.status` 的地方都要能处理 0：[admin-user.html](../router/admin-user.html) 的流水表、`/api/me/usage`、以及任何按 `status=1` 过滤的统计。**逐个 grep `UsageStatus` 与 `status` 字面量确认。** |

---

## §8 接口契约（精确字段）

> 通用约定：成功响应用**平铺 JSON**（列表用 `{items,total,limit,offset}`，见 [users.go:112](../controller/users.go#L112)）；失败统一 `{"error":{"message","type","code"}}`（[auth.go:129](../middleware/auth.go#L129)）。分页 `limit` 默认 20 / 上限 100（[users.go:24](../controller/users.go#L24)）。
>
> **时间格式（v1.4 澄清 —— 原文此处与 X17 自相矛盾，已修）**：
> - **3b 既有接口**（`/api/admin/*`、`/api/voices` 等）：仍是 `datetime('now')` 产生的 `YYYY-MM-DD HH:MM:SS`（UTC，**无时区标记**），**保持不变**；
> - **本期新增接口**（`/api/me/*`、`/api/auth/*`、`/api/admin/redemptions` 等）：返回 **RFC3339 UTC（带 `Z`）**，形如 `2026-02-01T10:00:00Z`，以便浏览器 `new Date()` 正确转本地时间（决策 X17）。
>
> 两套并存是刻意的：统一改 3b 的格式会牵动"保持不动的 3b 管理前端"。

### 8.1 认证

| 方法 | 路径 | 请求 | 成功 | 失败 |
|---|---|---|---|---|
| GET | `/api/auth/register-status` | — | 200 `{"allow":bool}` | — |
| POST | `/api/auth/register` | `{name,password}` | 201 `{id,name,status:1,balance}` | 400 `weak_password`/`missing_field`；403 `register_disabled`；409 `user_exists` |
| POST | `/api/auth/login` | `{name,password}` | 200 `{token,is_admin,name}` **或** 200 `{need_setup_password:true}`（**无 token**，D29） | 401 `invalid_credentials`；429 `too_many_attempts` |
| POST | `/api/auth/setup-password` | `{name,password}` | 200 `{}` | 400 `weak_password`；403 `setup_not_allowed`（非本机且无 admin_key）；404 `user_not_found`；409 `password_already_set`（X7） |

**`login` 细则**：
- 用户不存在 / 密码错 → **同一** 401 `invalid_credentials`（不泄露账号是否存在）
- `password_hash` 为空 → **无论 password 传什么**，一律 200 `{need_setup_password:true}`，**不签发 token**（D29）
- 该分支**同样计入** D10 失败计数（防枚举，X15）
- `name` 参与限流的 key 需 `ToLower(TrimSpace())`

**`setup-password` 通道判定（D23 + D32）**：

```go
host, _, _ := net.SplitHostPort(r.RemoteAddr) // 只看 socket peer
byLocal := host == "127.0.0.1" || host == "::1"
byAdminKey := bearerMatchesAdminKeys(r.Header.Get("Authorization")) // 常量时间
if !byLocal && !byAdminKey { 403 setup_not_allowed }
```

**严禁**使用 `middleware.GetClientIP(r)` —— 它在启发式模式下解析 XFF 并兜底采信 `X-Real-IP`（[ratelimit.go:236-240](../middleware/ratelimit.go#L236-L240)），精确模式下按跳数回走且不过滤回环（[ratelimit.go:218-223](../middleware/ratelimit.go#L218-L223)），两条路都可能让攻击者伪造出 `127.0.0.1`。

**同机反代场景已知取舍（D32 接受）**：若 nginx 与本体同机，所有公网用户的 `RemoteAddr` 都是 `127.0.0.1`，本机判定对所有人成立 → 依赖 admin_key 通道与 D24 的注册关闭双防线。

### 8.2 自助（JWT）

| 方法 | 路径 | 请求 | 成功响应 |
|---|---|---|---|
| GET | `/api/me` | — | `{name,balance,is_admin,status,created_at}`（`status` 为 **int**：1/2/3） |
| POST | `/api/me/password` | `{old,new}` | 200 `{token}`（X9，新 JWT） |
| GET | `/api/me/tokens` | `?limit&offset` | `{items,total,limit,offset}` |
| POST | `/api/me/tokens` | `{name,limit_quota?,unlimited?}` | 201 `{id,key,name,key_prefix,limit_quota,unlimited,status,created_at}`，**`key` 明文仅此一次** |
| PATCH | `/api/me/tokens/{id}` | `{name?,status?,limit_quota?}` | 200 单个 token 对象 |
| DELETE | `/api/me/tokens/{id}` | — | 204 |
| GET | `/api/me/usage` | `?limit&offset` | `{items,total,limit,offset}` |
| GET | `/api/me/stats` | `?days=14` | `{totals,trend,days}` |
| GET | `/api/me/balance/logs` | `?limit&offset&type?` | `{items,total,limit,offset}` |
| POST | `/api/me/redeem` | `{code}` | 200 `{quota,balance}` |

**`/api/me/tokens` item 字段（X8 精简 DTO）**：

```json
{
  "id": 7,
  "name": "我的脚本",
  "key_prefix": "sk-abc12",
  "limit_quota": null,        // null = 不限
  "used_quota": 1280,         // D17 后：限额预占计数器（含历史用量）
  "remain_quota": 0,          // 冻结的历史值，只读展示
  "unlimited": false,
  "status": 1,
  "created_at": "2026-02-01 10:00:00",
  "updated_at": "2026-02-01 10:00:00"
}
```

**`unlimited` 与 `limit_quota` 的关系（v1.4 补充定案）**：

| `unlimited` | `limit_quota` | 行为 |
|---|---|---|
| `true` | 任意值 | **完全不计费**：不扣 `users.balance`、不预占 `used_quota`、**`limit_quota` 被忽略**（仍受"余额为负即停服"约束，X24） |
| `false` | `null` | 只受 `users.balance` 约束 |
| `false` | 有值 | 额外受该准入限额约束（D11/D17） |

- 后端**不会**因为 `unlimited=true` 而自动清空 `limit_quota`（避免"切换开关丢配置"）；该值只是被忽略。
- 前端按"两个字段都渲染"处理即可（`unlimited` 优先展示，限额列按 `limit_quota` 是否为 `null` 显示「不限」）。
- ⚠️ **自助接口不允许设置 `unlimited`**：`/api/me/tokens` 的新建与 PATCH 都**不接受**该字段（硬编码 `false` / 不传），否则普通用户可自建"免死金牌"令牌使整套计费失效。该开关只由 admin 侧掌握。

**PATCH 三态语义（X6）**：

| 请求体 | 效果 |
|---|---|
| `{}` | 400 `no_fields` |
| `{"name":"x"}` | 改名 |
| `{"status":2}` | 停用（1=enabled, 2=disabled） |
| `{"limit_quota":500}` | 设为 500 |
| `{"limit_quota":null}` | **清为不限** |
| （省略 `limit_quota`） | **不改限额** |

**`/api/me/usage` item 字段**：

```json
{
  "id": 991,
  "token_id": 7,
  "token_name": "我的脚本",     // JOIN tokens 得到，便于前端展示
  "model": "volcano-tts",
  "characters": 128,
  "audio_ms": 4200,
  "audio_bytes": 67200,
  "pre_consumed": 154,
  "actual_quota": 132,
  "status": 1,                 // 0=pending 1=settled 2=refunded
  "created_at": "2026-02-01 10:00:00"
}
```

**`/api/me/stats` 响应（X5）**：

```json
{
  "days": 14,
  "totals": { "characters": 128000, "audio_ms": 4200000, "audio_bytes": 67200000, "actual_quota": 132000, "requests": 870 },
  "trend": [
    { "date": "2026-01-19", "actual_quota": 8200, "characters": 8000, "requests": 51 },
    { "date": "2026-01-20", "actual_quota": 9100, "characters": 8900, "requests": 58 }
  ]
}
```

- `trend` 由后端 **`GROUP BY date(created_at, '+8 hours')`** 生成（`statsTZOffset`，决策 X17），**只统计 `status=1`**。⚠️ 前端**不做**时区补偿 —— 后端必须按北京时间日界分组，否则前端若再补偿就会双重偏移
- 补齐无数据的日期（前端柱状图需要连续日期轴，缺日补 0）
- `days` 默认 14，上限 90，越界即钳

**`/api/me/balance/logs` item 字段**：

```json
{ "id": 33, "type": "redeem", "delta": 100000, "balance_after": 100000,
  "source": "redeem", "remark": "", "created_at": "2026-02-01 10:00:00" }
```

**`/api/me/redeem` 细则**：`code` 先 `TrimSpace` + `ToUpper`，长度必须 16。
- 不存在 → 404 `invalid_code`
- 已使用 → 409 `code_used`（D20：`RowsAffected==0` 即判 409）
- 成功 → `charge_logs(type=redeem, source=redeem)` + `balance += quota`，返回 `{quota,balance}`

### 8.3 管理端新增/扩展

| 方法 | 路径 | 请求 | 成功 | 失败 |
|---|---|---|---|---|
| POST | `/api/admin/users/{id}/charge` | `{delta,remark?}` | 200 `{balance}` | 400 `insufficient_balance`（`balance+delta<0`）；404 |
| PUT | `/api/admin/settings` | 扩 `{allow_register?,init_balance?}` | 200 `{ok,updated,updated_at}`（现有形状） | 400 `no_fields`/`invalid_value` |
| GET | `/api/admin/settings` | — | 扩 `{allow_register,init_balance,...}`（现有字段不动） | — |
| POST | `/api/admin/redemptions` | `{quota,count,remark?}` | 201 `{codes:[...]}` | 400 `invalid_quota`/`invalid_count`（count 上限 1000） |
| GET | `/api/admin/redemptions` | `?limit&offset&used=` | `{items,total,limit,offset}` | — |
| POST | `/api/admin/users/{id}/password-reset` | `{new_password?}` | 200 `{password}`（未传则生成 16 位随机并**仅此一次返回**，不写日志）；`token_version+1` | 404 |
| POST | `/api/admin/users` | 扩 `{balance?,is_admin?,password?}` | 201（现有形状 + `balance`） | 400；409 `user_exists` |
| PATCH | `/api/admin/users/{id}` | 扩 `{is_admin?}` | 200（现有形状 + `balance`,`is_admin`） | 409 `last_admin`（D26） |
| DELETE | `/api/admin/users/{id}` | — | 204 | 409 `last_admin`（D26） |

**`charge` 细则**：
- `delta > 0` → `type=charge, source=manual`
- `delta < 0` → `type=manual_adjust, source=manual`，且校验 `balance+delta >= 0`
- `delta == 0` → 400 `no_fields`
- 必须写 `charge_logs`，`balance_after` = 变更后余额

**「最后一个 admin 保护」（D26）** 落地口径：`is_admin=1` 且 `status=1` 的行数为 1 时，**拒绝**对该行执行 降级（`is_admin=false`）/ 禁用（`status=2`）/ 删除。返回 409 `last_admin`。

> **v1.5 实施补充（同一 code 可能配两种状态码，前端/QA 注意）**：
>
> | code | 出现位置 | HTTP |
> |---|---|---|
> | `insufficient_balance` | **管理端 `charge`**（手动扣回会扣成负） | **400** |
> | `insufficient_balance` | **`/v1/audio/speech` 预扣**（余额不足本笔预扣） | **402** |
> | `balance_negative` | `/v1/audio/speech` 预扣（已欠费停服） | 402 |
>
> 两者语义不同：400 是"这个管理操作参数本身不合法"，402 是"该调用方没钱"。
> 前端按 **(code, status)** 组合判断，不要只按 code 分支。
>
> 其他实施期澄清：
> - **管理端建号重名**沿用 3b 的 409 **`duplicate_name`**（§8.3 原写 `user_exists` 是为了与注册接口统一；为不破坏既有 admin 前端，保留原 code，注册接口才是 `user_exists`）。
> - **`password-reset`** 的失败分支除 404 外还有 **400 `weak_password`**（传入的 `new_password` 不合规时）。
> - **建号响应**在 `balance` 之外**同时追加 `is_admin`**（与 PATCH 响应保持超集一致）。
> - **`guardLastAdmin` 是"先计数再更新"、非单事务**：`SetMaxOpenConns(1)` 下窗口极小，但两个并发管理操作理论上仍可能同时通过。严格原子需要 store 层事务，**本期接受该窗口**并登记为已知限制。
> - **`/api/admin/settings` 与 `/api/settings` 两条路径都已注册**（前者为正式路径，后者为过渡别名），前端用哪条都可以。

### 8.4 路由挂载清单（软件部实施用）

**新增页面路由**（[router/router.go](../router/router.go) 内，均**不套鉴权**，X10）：

```
GET /login    → serveAdmin(adminLoginHTML)     // 与 /dashboard/login 同页（别名，D15）
GET /register → serveAdmin(registerHTML)       // 新增 embed
GET /portal   → serveAdmin(portalHTML)         // 新增 embed
GET /portal/admin.css         → serveCSS       // 复用同一份 CSS（portal.html 实际引用此路径）
GET /portal/admin-shell.js    → serveShellJS   // 门户与 admin 共用同一份 shell（凭证槽只有一份）
GET /portal/portal-shell.js   → servePortalShellJS
```

> **v1.4 修正**：`portal.html` / `register.html` 引用的是 `/portal/admin.css` 与
> **`/dashboard/admin-shell.js`**（shell 里的 `http` 实例与凭证槽必须与 admin 页
> 完全同一份实现，避免两套漂移）。`/portal/admin-shell.js` 也一并注册作为等价别名，
> 便于门户页面就近引用。三条静态资源路由均已实现。

**新增 API 路由**：

```
# 公开
POST /api/auth/register
GET  /api/auth/register-status
POST /api/auth/login
POST /api/auth/setup-password

# JWT（ValidateSession）
GET    /api/me
POST   /api/me/password
GET    /api/me/tokens
POST   /api/me/tokens
PATCH  /api/me/tokens/{id:[0-9]+}
DELETE /api/me/tokens/{id:[0-9]+}
GET    /api/me/usage
GET    /api/me/stats
GET    /api/me/balance/logs
POST   /api/me/redeem

# admin（RequireAdminOrJWT）
POST /api/admin/users/{id:[0-9]+}/charge
POST /api/admin/redemptions
GET  /api/admin/redemptions
POST /api/admin/users/{id:[0-9]+}/password-reset
```

**改造点**：现有管理路由（[router.go:140-207](../router/router.go#L140-L207)）把 `middleware.RequireAdmin` 全部替换为 `middleware.RequireAdminOrJWT`；`/dashboard*` 的 `htmlAwareAdmin` 内部同样替换。`/api/me/*` 显式**不挂** `RequireAdminOrJWT`。

---

## §9 前端实施清单（本人负责）

### 9.1 页面

| # | 文件 | 内容 |
|---|---|---|
| 1 | `router/admin-login.html`（**改造**） | 双 tab：①账号密码→JWT ②admin_key 直连。底部注册链接按 `/api/auth/register-status` 显隐。登录成功按 `is_admin` 跳 `/dashboard` 或 `/portal`。`need_setup_password` → 跳设密视图（**本机/admin_key 通道**，否则给出明确文案） |
| 2 | `router/register.html`（**新增**） | `{name,password}` + 前端密码校验（≥8、非纯空白）+ 二次确认。403 `register_disabled` → 显示「注册已关闭，请联系管理员」 |
| 3 | `router/portal.html`（**新增**） | 门户主页 |
| 4 | `router/portal-shell.js`（**新增**） | 门户壳：导航布局 + axios 实例（只挂 JWT 槽）+ 401 统一跳 `/login` |
| 5 | `router/admin-users.html`（**改造**） | 用户列表加「余额」列 + 「充值」按钮（弹窗 `delta`/`remark`） |
| 6 | `router/admin-user.html`（**改造**） | 用户详情加余额/`is_admin` 展示与调整、充值码无关 |
| 7 | `router/admin-settings.html`（**改造**） | 加「注册开关」+「注册赠送额度」两项 |
| 8 | `router/admin.css`（**追加**） | 新组件类（余额卡、趋势柱、tab、toast） |

### 9.2 `/portal` 区块

按 §5.3 的前端含义（`charge_logs` 只记余额来源、调用消费在 `usage`）：**两张流水表都要**。

1. **余额卡**：大字号 `balance` + 「充值码兑换」按钮 + 「充值码」输入弹窗
2. **用量统计卡**：`totals.actual_quota / characters / requests`
3. **用量趋势图**：纯 CSS 柱状图（复用 3b 的 `.chart` 类风格，**不引入图表库**），数据源 `stats.trend`
4. **我的令牌**：列表（名/前缀/限额已用/是否不限/状态）+ 新建（明文 key **仅弹一次**，需拷贝确认）+ 启停/改名/调额/删除
5. **调用流水**：分页表（usage）
6. **余额流水**：分页表（charge_logs）
7. **改密**：`{old,new}` → 成功后用返回的新 token 覆盖槽（X9）
8. **顶部**：当前用户 + 退出（清槽跳 `/login`）

### 9.3 前端约束

- **技术栈锁定**：Vue 3.4.21 + axios 1.6.7（CDN，与现有页一致）+ 原生多页面，**不做 SPA 重构**（D17）
- **凭证单槽**（X4）：沿用 `KEY_STORAGE`；**拦截器只读一个槽**
- **错误文案**：统一读 `e.response.data.error.message`，并按 `code` 做差异化处理（`session_revoked`/`session_invalid` → 清槽跳登录；`balance_negative` → 红色欠费横幅；`insufficient_balance`/`token_limit_exceeded` → 402 特定文案）
- **响应式**：复用 `admin.css` 已有响应式类，手机可看余额与用量（D28/#28）
- **中文硬编码**，不做 i18n

### 9.4 前端开工的硬依赖
`/api/me/*` 上线后可联调；在此之前可用桩数据（按 §8 形状）先行开发，不阻塞。

---

## §10 迁移与上线

### 10.1 迁移步骤（`store` 内，幂等）

1. `schemaVersion` 4 → 5（[db.go:29](../store/db.go#L29)），并修 `CurrentVersion()`（X11）
2. `ensureColumn` × 4（users）+ × 1（tokens）
3. `CREATE TABLE IF NOT EXISTS` charge_logs / redemptions + 索引
4. `UPDATE users SET is_admin=1 WHERE name='default' AND is_admin=0`
5. **D31(c)**：`allow_register` **仅当键不存在**时置 `"0"`；若 `default.password_hash` 为空则打 `WARN`（提示尽快用本机/admin_key 通道设密）

### 10.2 上线顺序（发布编排）

```
1. 后端发版（含迁移）→ 启动即完成 schema 5
2. 校验：/api/auth/register-status 返回 {"allow":false}（新装/D24 触发时）
3. 【关键】admin 用本机或 admin_key 通道给 default 设密：
   POST /api/auth/setup-password {name:"default", password:"..."}
4. 【关键】给存量用户充值（C 模型下 balance=0 → 旧 token 调用 402）
   POST /api/admin/users/{id}/charge {delta:N}
5. admin 按需开启注册：PUT /api/admin/settings {allow_register:true, init_balance:0}
6. 前端发版
```

### 10.3 存量影响（必读）

| 影响 | 说明 |
|---|---|
| **旧 token 调用会 402** | C 模型下扣 `user.balance`，而 balance 从 0 起步（3b 无充值概念）。**上线前必须给存量用户充值**，否则业务中断 |
| `tokens.remain_quota` | 冻结为历史值，不再变动，仅前端只读展示 |
| `tokens.used_quota` | 参与新限额判定（历史值一并计入），前端需注明 |
| `users.quota` | 保留但废弃，前后端均不读写 |
| `users.used_quota` | 继续累加 `actual`，语义不变 |
| `usage` 老数据 | `status` 默认 1（settled），兼容 |
| 前端提示 | 门户在 `balance<=0` 时显示「余额不足，请充值」引导（#24 建议） |

### 10.4 无回滚脚本
3b 从未有充值/余额，`balance` 从 0 起步，**无需从 `usage` 倒推**（#23）。回滚 = 回退二进制；`balance` 等新列留着不影响旧版本（旧版不读这些列）。

---

## §11 验收清单

### 11.1 安全（必须全过）

- [ ] `default` 空密码时，`POST /api/auth/login {name:"default",password:""}` → 200 `{need_setup_password:true}`，**响应体无 `token`**
- [ ] 用上一步响应中**不存在**的 token 去调 `/api/admin/overview` → 401
- [ ] `setup-password` 从**非本机、无 admin_key** 调用 → 403 `setup_not_allowed`
- [ ] `setup-password` 携带伪造 `X-Forwarded-For: 127.0.0.1` 与 `X-Real-IP: 127.0.0.1` 从非本机调用 → **仍 403**（验证 D32 生效）
- [ ] 对已设密账号再调 `setup-password` → 409 `password_already_set`
- [ ] 改密后**旧 JWT** 调 `/api/me` → 401 `session_revoked`；**新返回值**的 JWT 可用
- [ ] 连续 5 次错密码 → 第 6 次 429 `too_many_attempts`
- [ ] 普通用户 JWT 调 `/api/admin/overview` → 401/403；调 `/dashboard` → 403
- [ ] admin_key 调 `/api/me` → 401（#14）
- [ ] `alg: none` / `alg: HS512` 伪造 JWT → 401（验证 `WithValidMethods`）
- [ ] 响应体任何地方**不含** `password_hash`、`key_hash`

### 11.2 计费（必须全过）

- [ ] `balance` 充足 → 调用成功，`balance` 减少 `actual`（非 `pre`）
- [ ] `balance` 不足 → 402 `insufficient_balance`，**且 balance/used_quota 均未变动**（事务回滚）
- [ ] **欠费停服（X24）**：结算补扣使 `balance < 0` → 该用户**普通 token 与 unlimited token 均** 402 `balance_negative`；`pre=0` 的调用也被拒（验证 `-5 >= 0` 为假）
- [ ] **欠费逃生通道（X24）**：余额为负时，`GET /api/me` 200、`POST /api/me/redeem` 200（充值码不得受余额校验）
- [ ] **解除条件（X24）**：`balance` 由 -5 充到 0 → 对 `pre=12` 的调用仍 402 `insufficient_balance`；充到 ≥12 → 恢复 200
- [ ] **`limit_quota` 穿透口径（X25 定案 a）**：断言 `used_quota <= limit_quota × 1.1`（语义为「准入阈值」，非硬上限）
- [ ] **`usage.status` 三态（X26）**：预扣后该 `request_id` 有且仅有 1 行 `status=0`；结算后变 1 且 `actual_quota` 正确；退款后变 2
- [ ] **单向前进保护（X26）**：对已 `status=1` 的行调退款 → **被拒且不多退钱**；对已 `status=2` 的行再退款 → 幂等返回
- [ ] **pending 显示（X26）**：门户 `/api/me/usage` 与管理页流水表都能显示 `status=0` 为「进行中」，不出现空白或误判为已结算
- [ ] **`UsageInsert` 兜底已移除**：构造 `Status` 未显式传值的调用 → 断言**不会**被静默写成 `status=1`
- [ ] `limit_quota` 超限 → 402 `token_limit_exceeded`，**且 `users.balance` 未变动**（验证 D27 同事务）
- [ ] 合成失败 → 退款，`balance` 精确回到调用前，`used_quota` 回到调用前
- [ ] **重复退款**（同 request_id 调两次）→ 只退一次
- [ ] **已结算再退款** → 拒绝且不多退
- [ ] 并发 20 个请求打同一 token + `limit_quota=1000` → `used_quota` 不超过 1000（验证 D17 原子预占）
- [ ] 并发充值码兑换同一 code × 10 → 仅 1 次 200，其余 409
- [ ] `unlimited=1` 的 token → 不发任何扣减 SQL，`pre=0`，`balance`/`used_quota` 不变
- [ ] `remain_quota` 在整个流程中**始终不变**（冻结验证）
- [ ] 结算补扣致负的场景（`Actual > Pre` 且余额不足）→ 按 X-Q1 定案行为一致

### 11.3 功能

- [ ] 注册开关：关 → 403 且 `register-status` 返回 `allow:false`；开 → 201 且按 `init_balance` 赠送并写 `charge_logs(type=grant,source=register_bonus)`
- [ ] admin 建号带 `balance` → 写 `charge_logs(type=grant,source=admin_create)`
- [ ] `/api/admin/settings` GET 返回 `allow_register`/`init_balance`；PUT 可改且**立即生效**（沿用 `LoadRuntimeConfig` 流程）
- [ ] 最后一个 admin 降级/禁用/删除 → 409 `last_admin`
- [ ] `/api/me/tokens` PATCH 三态：省略/值/null 行为与 §8.2 表一致
- [ ] 新建 token 返回明文 `key`，且**再查列表时无 `key` 字段**
- [ ] 跨用户越权：A 的 JWT 操作 B 的 token（PATCH/DELETE `/api/me/tokens/{B的id}`）→ 403/404，**不得成功**
- [ ] `/api/me/stats` 的 `trend` 日期连续、缺日补 0、只含 settled
- [ ] 分页口径：`limit=1000` 被钳到 100；`offset=-1` 钳到 0

### 11.4 迁移

- [ ] 老库（schema 4）升级后：新列齐全、`usage` 老数据 `status=1`、`is_admin` 迁移生效
- [ ] **连续重启 3 次**：`allow_register` 不被反复覆盖（验证 D31(c)）、无重复建表/建列报错
- [ ] `default` 已设密后重启：不再打 WARN、`allow_register` 保持 admin 的设置值

---

## §12 风险与安全清单

| 风险 | 等级 | 处置 |
|---|---|---|
| 空密码账号换取 admin JWT | 🔴 高 | **D29**：空密码永不签发凭证。**上线前必须验证 §11.1 第 1–2 条** |
| XFF 伪造绕过本机判定 | 🔴 高 | **D32**：只读 `RemoteAddr`；禁用 `GetClientIP` 于此用途 |
| 同机反代致本机判定失效 | 🟠 中 | D32 已知取舍；靠 admin_key 通道 + D24 注册关闭双防线 |
| 存量用户 balance=0 致业务中断 | 🟠 中 | §10.3：上线前批量充值；门户余额不足提示 |
| 单连接下误用 `s.db` 造成自锁 | 🟠 中 | §7.4：tx 内只用 Tx 版方法 |
| JWT secret 随 CWD 漂移致全员登出 | 🟠 中 | D30：与 DB 同目录派生；`TTS_JWT_SECRET_PATH` 可覆盖 |
| `tokens.used_quota` 语义变更致限额误判 | 🟡 低 | §7.3 明确口径；前端注明"含历史用量" |
| bcrypt cost 过高致登录慢 | 🟡 低 | cost=10，常量可调 |
| 依赖拉取失败阻塞后端 | 🟢 低 | **已实测 `goproxy.cn` 可达**（X27），版本钉 `jwt v5.3.1` + `x/crypto v0.58.0`。剩余风险仅"沙箱外执行 `go get`"这一执行环境事项 |
| `usage.status=0` 引入后读取方漏处理 | 🟠 中 | X26 连带项 2：逐个 grep `UsageStatus` 与 `status` 字面量，管理页流水表与统计口径都要能显示"进行中" |
| 登录锁定内存态重启失效 | 🟡 低 | D10 已知接受；持久化记待办 |

---

## §13 本期不做（待办登记）

| 项 | 说明 |
|---|---|
| 第三方支付（支付宝/微信） | D2：留接口，不实现 |
| 邀请 / 分销 / 多级代理 | #25 |
| 实名 / 手机号 | #26 |
| i18n 多语言 | #27（保持中文硬编码） |
| 额度申请 / 工单 | #29（靠 admin 手动充） |
| 调用**次数**限流（rate limit per token） | #4（`limit_quota` 是额度上限，不是次数） |
| JWT 无感刷新（refresh token） | #11（401 直接跳登录） |
| JWT secret 平滑轮换（多 secret 容忍） | D30 |
| 登录锁定持久化 | D10（重启失效） |
| IP 级登录限速 | #12 |
| 充值码 `expires_at` 过期 | #5 |
| 崩溃遗留 `status=0` 行的自动补偿任务 | X-Q2 的后续（本期先人工扫） |

---

## §14 实施顺序建议（软件部）

```
① 依赖冻结（X-Q3 先验证联网/vendor）           ← 卡点
② schema 5 迁移 + store 层方法（Tx 版）
③ middleware：ValidateSession + RequireAdminOrJWT + 路由替换
④ auth 接口（含 D29/D14/D23/D32 安全逻辑）      ← 最高风险，先自测 §11.1
⑤ /api/me/* 自助接口
⑥ admin 扩字段（charge / settings / redemptions / password-reset / last_admin）
⑦ billing 落点迁移（UserPreConsume / UsageSettle / UsageRefund）← 必须跑 §11.2 并发用例
⑧ 前端（本人，可与 ⑤ 并行）
```

---

## §15 常量登记表（防漂移，实现时逐项对齐）

> 集中一份，避免后端/前端各自写死数字导致行为不一致。

| 常量 | 值 | 出处 | 备注 |
|---|---|---|---|
| `bcryptCost` | `10` | X1 | `bcrypt.DefaultCost` |
| `jwtAlg` | `"HS256"` | §4 | 解析时必须 `WithValidMethods` |
| `jwtTTL` | `24h` | D9 | claims `exp = iat + 24h` |
| `jwtSecretBytes` | `32` | X19 | `crypto/rand` |
| `minPasswordLen` | `8` | D19 | 禁空/纯空白 |
| `loginMaxFail` | `5` | D10 | 账号级 |
| `loginLockWindow` | `15m` | D10 | 内存 map |
| `defaultPageLimit` | `20` | 现有 [users.go:25](../controller/users.go#L25) | 复用，勿新增 |
| `maxPageLimit` | `100` | 现有 [users.go:26](../controller/users.go#L26) | 复用，勿新增 |
| `redeemCodeLen` | `16` | X20 | 字符集 `A-Z0-9` |
| `redeemMaxBatch` | `1000` | X20 | `count` 上限 |
| `redeemGenRetry` | `5` | X20 | UNIQUE 冲突重试 |
| `statsDefaultDays` | `14` | X5 | `/api/me/stats` |
| `statsMaxDays` | `90` | X5 | 越界即钳 |
| `statsTZOffset` | `"+8 hours"` | X17 | `date(created_at, ...)` |
| `passwordResetLen` | `16` | §8.3 | admin 重置时生成的随机密码 |
| `usageStatusPending` | `0` | X-Q2 | 新增 |
| `usageStatusSettled` | `1` | 现有 [usage.go:11](../store/usage.go#L11) | 不变 |
| `usageStatusRefunded` | `2` | 现有 [usage.go:12](../store/usage.go#L12) | 不变 |
| `userStatusEnabled/Disabled/Deleted` | `1 / 2 / 3` | #13 | 现有校验只认 1/2 |
| `tokenStatusEnabled/Disabled` | `1 / 2` | 现有 | 不变 |
| `chargeType*` | `charge / redeem / manual_adjust / grant` | D25 | TEXT 列 |
| `chargeSource*` | `manual / redeem / admin_create / register_bonus` | D33 | TEXT 列 |
| `storageKey` | `'ttsAdminKey'` | X23 | sessionStorage 键名，**不改** |

---

## §16 改动文件清单（用于排期与 review 拆分）

### 16.1 后端（软件部）

**新增**

| 文件 | 内容 |
|---|---|
| `middleware/jwt.go` | secret 加载/生成（X19）、`IssueToken`、`ParseToken`（`WithValidMethods`） |
| `middleware/session.go` | `SessionCaller`、`ValidateSession`、`RequireAdminOrJWT`、ctx 存取 |
| `controller/auth.go` | `register` / `login` / `setup-password` / `register-status`（含 D29/D14/D32/D10） |
| `controller/me.go` | `/api/me` 全部 10 个 handler |
| `controller/redemptions.go` | admin 充值码 生成/列表 |
| `store/charge_logs.go` | `ChargeLog` 类型 + `ChargeLogInsertTx` + 列表查询 |
| `store/redemptions.go` | `RedemptionCreate(batch)` / `RedemptionRedeemAtomic` / 列表 |
| `router/register.html`、`router/portal.html`、`router/portal-shell.js` | 前端新增（本人） |

**修改**

| 文件 | 改动 |
|---|---|
| `go.mod` / `go.sum` | 加 `golang-jwt/jwt/v5 v5.3.1`、`golang.org/x/crypto v0.58.0`（**须在沙箱外 `go get`**，见 §20） |
| `main.go` | 启动期加载 JWT secret，失败 **fail-fast** |
| `store/db.go` | `schemaVersion` 4→5；`ensureColumn`×5；新表 DDL + 索引（§5） |
| `store/migrate.go` | `CurrentVersion()` 对齐 `schemaVersion`（X11） |
| `store/users.go` | `User` 加 4 字段；`UserAddBalanceTx`、`UserSetPassword`、`UserBumpTokenVersion`、`UserCountActiveAdmins` |
| `store/tokens.go` | `Token` 加 `LimitQuota *int64`；新增 `UserPreConsume`（Tx，X16）；`TokenDecreaseQuota` 改为不再被 billing 调用（保留或删除，建议删除以免误用） |
| `store/usage.go` | 新增 `UsageStatusPending`；`UsageSettle`/`UsageRefund` 改落点；`UsageListByUser` 加过滤（X22）；新增 `UsageTrendByUser`（X17） |
| `billing/billing.go` | `PreConsume` 内部换 `UserPreConsume`；**包注释的"额度归属 token 层"必须改写**（现 [billing.go:13-14](../billing/billing.go#L13-L14) 明确写着 token 层，不改会误导后续维护者） |
| `controller/users.go` | list/detail 加 `balance`/`is_admin`；create 扩字段；`charge`；`password-reset`；`last_admin` 保护（D26） |
| `controller/settings.go` | 加 `allow_register` / `init_balance`（X3） |
| `controller/tokens.go` | admin 侧 token 支持 `limit_quota`；PATCH 三态（X6） |
| `router/router.go` | 新路由挂载（§8.4）；`RequireAdmin` → `RequireAdminOrJWT` **全量替换**；3 个新 `//go:embed`；`servePortalShellJS` |
| `middleware/admin_auth.go` | `RequireAdmin` 保留（供内部/兼容）；建议把 `RequireAdminOrJWT` 放 `session.go`，避免两文件往返 |
| `.env.example` | 登记 `TTS_JWT_SECRET_PATH`（X2/X19） |
| `README.md` | 公网部署安全清单补 3 条：①首次部署必须先给 `default` 设密 ②注册默认关闭的原因 ③升级时必须给存量用户充值 |

**明确不改**：`store/channels.go`、`store/voices.go`、`adapter/**`、`telemetry/**`、`metrics/**`、`installer/**`（除 §10 备份建议）、`store/setup.go`、`controller/setup.go`、`setting/config.go`（除非软件部决定把 secret 路径也做进 `LoadRuntimeConfig`）。

### 16.2 前端（本人）

| 文件 | 改动 |
|---|---|
| `router/admin-shell.js` | 加 `getCred/setCred/clearCred` 别名；加 `loginWithPassword` / `setupPassword`；`mountShell` 的登出跳转保持 `/dashboard/login`；**不动** `mountShell` 的 nav（门户用独立 shell） |
| `router/admin-login.html` | 双 tab；`need_setup_password` → 设密视图；底部注册链接按 `register-status` 显隐 |
| `router/register.html`（新） | 注册表单 + 本地校验 + 403 文案 |
| `router/portal.html`（新） | 门户 8 个区块（§9.2） |
| `router/portal-shell.js`（新） | `mountPortalShell`（注入点 `<aside id="portal-sidebar">`）+ 复用同一 axios 与凭证槽 |
| `router/admin-users.html` | 余额列 + 充值弹窗 |
| `router/admin-user.html` | 余额/`is_admin` 展示与调整；**usage 表加 `status=0` 的显示**（"进行中"），否则新状态会显示成空白 |
| `router/admin-settings.html` | 注册开关 + 注册赠送额度 |
| `router/admin.css` | 余额卡、趋势柱、tab、portal 布局、响应式补充 |

---

## §17 错误码总表（前端文案与 QA 断言共用）

| code | HTTP | 含义 | 前端处置 |
|---|---|---|---|
| `session_required` | 401 | 无/坏 Bearer | 清槽 → 跳 `/login` |
| `session_invalid` | 401 | JWT 非法/过期 | 清槽 → 跳 `/login` |
| `session_revoked` | 401 | `token_version` 不匹配（已改密） | 清槽 → 跳 `/login` + 提示"密码已变更" |
| `user_disabled` | 403 | 账号被禁用 | 提示联系管理员，不清槽（避免误导） |
| `admin_required` | 401 | 管理凭证不足 | 管理页 → 跳 `/dashboard/login` |
| `invalid_credentials` | 401 | 登录名/密码错 | 表单内提示，**不区分**账号是否存在 |
| `too_many_attempts` | 429 | 连续失败锁定 | 提示"请 15 分钟后重试" |
| `register_disabled` | 403 | 注册已关闭 | 「注册已关闭，请联系管理员」 |
| `user_exists` | 409 | 用户名已占用 | 表单内提示 |
| `weak_password` | 400 | 密码不合规 | 表单内提示（≥8、非纯空白） |
| `setup_not_allowed` | 403 | 设密通道不满足 | 提示"请在本机操作，或使用管理员凭证" |
| `password_already_set` | 409 | 该账号已设密 | 引导走登录 / 忘记密码找管理员 |
| `insufficient_balance` | 402 | 余额不足本笔预扣 | 门户余额卡高亮 + 引导兑换充值码 |
| `balance_negative` | 402 | **已欠费停服**（`balance < 0`） | 顶部红色横幅「API 已暂停，请充值恢复」+ 引导兑换 |
| `token_limit_exceeded` | 402 | token 限额已用尽 | 提示该令牌限额用尽，可到门户调额 |
| `invalid_code` | 404 | 充值码不存在 | 输入框提示 |
| `code_used` | 409 | 充值码已使用 | 输入框提示 |
| `last_admin` | 409 | 唯一管理员保护 | 管理页提示"至少保留一个启用的管理员" |
| `not_owner` | 403 | 越权操作他人资源 | 提示 + 刷新列表 |
| `invalid_filter` | 400 | 筛选参数非法 | 重置筛选 |
| `no_fields` | 400 | 空更新请求 | 表单校验提示 |
| `not_found` | 404 | 资源不存在 | 现有 [httpjson.go:27](../controller/httpjson.go#L27) |
| `db_read_failed` / `db_write_failed` | 500 | DB 故障 | 通用错误提示 |
| `db_not_ready` | 503 | store 未就绪 | 启动中提示 |

> 全部沿用 `{"error":{"message","type","code"}}`（[auth.go:129](../middleware/auth.go#L129)）。
>
> **v1.4 实装变更（对外契约，需知晓）**：`/v1/audio/speech` 预扣失败的原 code
> `quota_exhausted` 已按本表改为 **`insufficient_balance`**（HTTP 仍是 402，`type` 仍是
> `insufficient_quota`）。同时新增三个 402 code：`balance_negative`、`token_limit_exceeded`、
> `duplicate_request`（同 `request_id` 重放）。若担心既有调用方按 code 分支，可让后端
> 在 `message` 里保留原措辞；状态码未变，按 402 兜底的调用方不受影响。

---

## §18 QA 可执行验证清单（curl）

> 前置：`BASE=http://127.0.0.1:8080`、`ADMIN_KEY=<DB admin_key>`、需 `jq`（或 `python -c` 提取）。
> **加 ⭐ 的是必须过的核心断言。**

```bash
BASE=http://127.0.0.1:8080
ADMIN_KEY='...'

# ── 0) 公开注册开关
curl -s $BASE/api/auth/register-status
# 期望 200 {"allow":false}（新装 / D24 触发时）

# ── ⭐1) D29：空密码【不得】签发 token
curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
     -d '{"name":"default","password":""}' | tee /tmp/r.json
# 期望 200 {"need_setup_password":true}
grep -q '"token"' /tmp/r.json && echo '❌ D29 未落实：空密码签发了 token' || echo '✅ D29 通过'

# ── 2) 首次设密（本机执行）
curl -s -X POST $BASE/api/auth/setup-password -H 'Content-Type: application/json' \
     -d '{"name":"default","password":"Str0ngPass123"}'
# 期望 200 {}

# ── 3) 已设密再调 setup-password
curl -s -X POST $BASE/api/auth/setup-password -H 'Content-Type: application/json' \
     -d '{"name":"default","password":"Another123"}'
# 期望 409 password_already_set

# ── ⭐4) D32：从【另一台机器】伪造 XFF / X-Real-IP 必须仍 403
curl -s -o /dev/null -w '%{http_code}\n' -X POST $BASE/api/auth/setup-password \
     -H 'Content-Type: application/json' \
     -H 'X-Forwarded-For: 127.0.0.1' -H 'X-Real-IP: 127.0.0.1' \
     -d '{"name":"default","password":"Hacked123"}'
# 期望 403（若 200 → D32 未落实，属致命缺陷）

# ── 5) 正常登录
TOKEN=$(curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
        -d '{"name":"default","password":"Str0ngPass123"}' | jq -r .token)
curl -s $BASE/api/me -H "Authorization: Bearer $TOKEN"

# ── ⭐6) #14：admin_key 不得调 /api/me
curl -s -o /dev/null -w '%{http_code}\n' $BASE/api/me -H "Authorization: Bearer $ADMIN_KEY"
# 期望 401

# ── ⭐7) 普通用户 JWT 不得调 /api/admin/*
curl -s -o /dev/null -w '%{http_code}\n' $BASE/api/admin/overview -H "Authorization: Bearer $TOKEN"
# 期望 401（default 是 admin 时应为 200；请用普通账号复验）

# ── 8) 存量充值（上线必做）
curl -s -X POST $BASE/api/admin/users/1/charge -H "Authorization: Bearer $ADMIN_KEY" \
     -H 'Content-Type: application/json' -d '{"delta":100000,"remark":"存量充值"}'
# 期望 200 {"balance":100000}

# ── 9) 越权扣成负
curl -s -X POST $BASE/api/admin/users/1/charge -H "Authorization: Bearer $ADMIN_KEY" \
     -H 'Content-Type: application/json' -d '{"delta":-999999999}'
# 期望 400 insufficient_balance

# ── 10) 建 token（null = 不限）
curl -s -X POST $BASE/api/me/tokens -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"name":"ci","limit_quota":null}'
# 期望 201，响应含明文 key（仅此一次）

# ── 11) 限额三态
TID=<上一步的 id>
curl -s -X PATCH $BASE/api/me/tokens/$TID -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"limit_quota":500}'   # 设 500
curl -s -X PATCH $BASE/api/me/tokens/$TID -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"name":"renamed"}'    # 不动限额
curl -s -X PATCH $BASE/api/me/tokens/$TID -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"limit_quota":null}'  # 清为不限
# 三次后 GET /api/me/tokens 断言 limit_quota 为 null 且 name 已改

# ── ⭐12) 402 与事务回滚
#     先把 balance 扣到 0，再调 TTS；随后 GET /api/me 断言 balance 与 used_quota【均未变】
curl -s -o /dev/null -w '%{http_code}\n' -X POST $BASE/v1/audio/speech \
     -H "Authorization: Bearer <上一步的明文 key>" -H 'Content-Type: application/json' \
     -d '{"model":"tts-1","input":"你好","voice":"default"}'
# 期望 402

# ── ⭐13) 并发兑换同一充值码
CODE=<admin 生成的码>
for i in $(seq 1 10); do
  curl -s -o /dev/null -w '%{http_code}\n' -X POST $BASE/api/me/redeem \
       -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
       -d "{\"code\":\"$CODE\"}" &
done; wait
# 期望：恰好 1 个 200，其余 409

# ── ⭐14) 并发打同一 token 的 limit_quota
#     建 limit_quota=1000 的 token，并发 20 个 TTS 请求
#     断言：used_quota <= 1000 × 1.1（X25 定案 (a)：允许约 5% 穿透；
#           若软件部选 (b) 钳制，则断言改为严格 <= 1000）

# ── ⭐20) 欠费停服（X24）
#     a) 把某用户 balance 打到略高于 pre，调一次"慢语速"请求使其结算补扣为负：
#        结算后 GET /api/admin/users/{id} → 断言 balance < 0
#     b) 立刻再用该用户的【普通 token】调用 → 期望 402 balance_negative
#     c) 用该用户的【unlimited token】调用 → 期望同样 402（X24 连带决定①）
#     d) 边界：chars=0 的请求（pre=0）在负余额下 → 期望 402（验证 -5 >= 0 为假）
#     e) 逃生通道：用该用户 JWT 登录门户 → GET /api/me 200；
#        兑换充值码 → 200；充值后 balance >= pre → 调用恢复 200
curl -s $BASE/api/me -H "Authorization: Bearer $TOKEN"          # 期望 200（欠费也能看自己）
curl -s -X POST $BASE/api/me/redeem -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d '{"code":"<新码>"}'  # 期望 200（欠费也能兑换）
curl -s -o /dev/null -w '%{http_code}\n' -X POST $BASE/v1/audio/speech \
     -H "Authorization: Bearer <欠费用户的普通 key>" \
     -H 'Content-Type: application/json' -d '{"model":"tts-1","input":"你好","voice":"default"}'
# 期望 402，且 body.error.code == "balance_negative"

# ── 21) 解除条件精度（X24）
#     balance 从 -5 充到 0 → 对 pre=12 的调用仍应 402 insufficient_balance
#     再充到 >= 12 → 才恢复 200

# ── ⭐22) usage 三态（X26）
#     a) 预扣后立刻查库（合成进行中）：该 request_id 恰有 1 行 status=0
sqlite3 tts.db "SELECT request_id,status,pre_consumed,actual_quota FROM usage WHERE request_id='<rid>';"
#     b) 成功结算后：同一 request_id 仍只有 1 行，status=1，actual_quota>0
#     c) 失败退款后：status=2，actual_quota=0，且 users.balance 精确回到调用前
#     d) 对已 status=1 的行调退款 → 不得多退钱（回归 3b 的重复退款缺陷）
#     e) 重放同一 request_id 再预扣 → 期望 402（被 idx_usage_request_pending 挡住）

# ── 23) pending 的展示口径（X26）
curl -s "$BASE/api/me/usage?status=0" -H "Authorization: Bearer $NEW"
# 期望 200，items 里的 status=0 行在前端渲染为「进行中」

# ── 15) 分页钳制
curl -s "$BASE/api/me/usage?limit=1000&offset=-1" -H "Authorization: Bearer $TOKEN"
# 期望响应里 limit=100，offset=0

# ── ⭐16) 改密踢旧
NEW=$(curl -s -X POST $BASE/api/me/password -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' \
      -d '{"old":"Str0ngPass123","new":"Str0ngPass456"}' | jq -r .token)
curl -s -o /dev/null -w '%{http_code}\n' $BASE/api/me -H "Authorization: Bearer $TOKEN"
# 期望 401 session_revoked（旧 token）
curl -s -o /dev/null -w '%{http_code}\n' $BASE/api/me -H "Authorization: Bearer $NEW"
# 期望 200

# ── 17) 连重启三次断言 allow_register 不被覆盖（D31(c)）
#     PUT allow_register=true → 重启 → GET 仍为 true

# ── 18) 时间口径（X17）
curl -s "$BASE/api/me/stats?days=3" -H "Authorization: Bearer $NEW"
# 断言：created_at 形如 2026-02-01T10:00:00Z（带 Z）；trend 日期为北京时间日界

# ── 19) 前缀探测：换不同前缀应返回相同结果（不泄露用户是否存在）
for n in default default2 nosuchuser; do
  curl -s -X POST $BASE/api/auth/login -H 'Content-Type: application/json' \
       -d "{\"name\":\"$n\",\"password\":\"wrong\"}" -o /dev/null -w "$n %{http_code}\n"
done
# 期望全部 401，且响应体 message 完全一致
```

---

## §19 首次部署 / 升级操作手册

### 19.1 全新部署

```
1. 启动服务（自动建库 → schema 5）
2. 观察日志：应出现 WARN「default 账号尚未设置密码，自由注册已关闭」
3. 【必须在服务器本机执行】设置管理员密码：
     curl -X POST http://127.0.0.1:8080/api/auth/setup-password \
          -H 'Content-Type: application/json' \
          -d '{"name":"default","password":"<强密码>"}'
   （或从任意机器带 admin_key 调用）
4. 浏览器打开 /login → 账号密码 tab → default + 新密码 → 自动进 /dashboard
5. 如需开放注册：
     /dashboard/settings → 打开「允许自由注册」、设置「注册赠送额度」
   （等价 API：PUT /api/admin/settings {"allow_register":true,"init_balance":0}）
6. 普通用户访问 /login → 注册（若已开）或由管理员在 /dashboard/users 建号
```

> **为什么注册默认关闭**：D24 的安全设计——公共实例上"空密码管理员 + 开放注册"是攻击面组合。管理员设密后可按需打开。

### 19.2 从 3b 升级（**有业务中断风险，务必按序**）

```
1. 【备份】停服 → 复制 tts.db（含 -wal/-shm）到安全位置
2. 部署新二进制 → 启动（自动迁移到 schema 5）
3. 校验迁移：
     - sqlite3 tts.db "PRAGMA table_info(users);"   → 见 4 个新列
     - sqlite3 tts.db "PRAGMA table_info(tokens);"  → 见 limit_quota
     - sqlite3 tts.db "SELECT name,is_admin FROM users WHERE name='default';" → is_admin=1
     - sqlite3 tts.db "SELECT key,value FROM settings WHERE key='allow_register';"
4. 【关键】设 default 密码（见 19.1 步骤 3）
5. 【关键】给所有存量用户充值，否则其旧 token 一律 402：
     for id in <存量用户 id 列表>; do
       curl -X POST $BASE/api/admin/users/$id/charge \
            -H "Authorization: Bearer $ADMIN_KEY" \
            -H 'Content-Type: application/json' -d '{"delta":1000000,"remark":"3b→4 迁移赠送"}'
     done
6. 冒烟：用一个存量 token 调一次 /v1/audio/speech，确认 200 且 balance 按 actual 减少
7. 前端发版 → 逐页验收 §11.3
```

### 19.3 回滚

- 代码：回退到 3b 二进制即可。**新列留着不影响旧版本**（旧版不读 `balance`/`limit_quota`）。
- 数据：`balance` 从 0 起步，无需从 `usage` 倒推（#23）；若已充值，回滚后这些值被旧版忽略。
- ⚠️ 唯一不可逆项：若已产生 `status=0` 的 pending 行，回滚后旧版会把它们当 `status=1` 读（[usage.go:45-47](../store/usage.go#L45-L47) 的兜底仅在 INSERT 时生效，读取不受影响）。**回滚前建议先跑一遍待办扫描，把 pending 行人工结算/退款清零。**

### 19.4 备份建议（新增）
`schemaVersion` 迁移属 `ALTER TABLE ADD COLUMN` 与建表，风险低但不可逆。**建议 `migrate()` 在检测到"从 4 升到 5"时自动做一次 `tts.db` 冷备**（复用 [installer.tryBackupCorrupt](../installer/bootstrap.go#L162) 的改名思路，但用 `.bak-v4-<ts>` 后缀），并在日志给出路径。是否实施由软件部定。

---

## §20 明日开工第一步（按序执行，前 3 项是阻塞项）

```bash
# ① 【阻塞｜必须在 DSH 沙箱外执行】依赖落定（沙箱会拒写 GOMODCACHE，见 X27）
cd D:\项目\Volcano-Engine-TTS-UI.git
go get github.com/golang-jwt/jwt/v5@v5.3.1
go get golang.org/x/crypto@v0.58.0
go mod tidy
go build ./...            # 先确认空改动可编译，再动代码
# 校验: go.mod 出现两条 require; 留意 golang.org/x/sys 是否被连带升级

# ② 【阻塞｜在真实库上试验前必做】冷备数据库（§19.2 步骤 1）
#    停服 → 复制 tts.db 及 -wal/-shm

# ③ 【阻塞】按 §16 顺序动手；schema 迁移先跑通"连续重启 3 次不报错"（§11.4）
```

**后端顺序（§14 已列，此处为勾选表）**

- [ ] 依赖落定 + 编译基线通过
- [ ] schema 5 迁移 + store 层 Tx 版方法（§5、X18、X26）
- [ ] `middleware`：`jwt.go`（X19）+ `session.go`（§6.1）+ 路由全量替换 `RequireAdminOrJWT`
- [ ] auth 接口（**D29 / D14 / D23 / D32 / D10 五条安全逻辑**）→ 跑 §18 第 1–4、16、19 项
- [ ] `/api/me/*`（含 X5 trend、X17 时间口径、X22 过滤、X24 逃生通道）
- [ ] admin 扩字段（charge / settings / redemptions / password-reset / last_admin）
- [ ] billing 落点迁移（`UserPreConsume` / `UsageSettle` / `UsageRefund`）+ **改写 [billing.go:13-14](../billing/billing.go#L13-L14) 与 [billing.go:121-123](../billing/billing.go#L121-L123) 的过时注释** → 跑 §18 第 12–14、20–22 项
- [ ] `.env.example` / `README.md` 同步（§16）

**前端顺序（本人，可与后端 ⑤ 并行）**

- [ ] `admin-shell.js` 加 `getCred/setCred/clearCred` + `loginWithPassword`（X23）
- [ ] `admin-login.html` 双 tab + `need_setup_password` 视图
- [ ] `register.html` / `portal.html` / `portal-shell.js`
- [ ] admin 三页改造（users / user / settings）+ `admin.css`
- [ ] 联调：§18 全量过一遍

> **前端不阻塞**：`/api/me/*` 未就绪时按 §8 的形状用桩数据先行开发。

---

## §21 实施完成与验证证据（v1.5）

### 21.1 静态检查

| 项 | 结果 |
|---|---|
| `go build ./...` | ✅ 通过（含全部新增包与文件） |
| `go vet ./...` | ✅ 无输出 |
| `go test ./store/` | ✅ **ok**（13 条 STAGE4 单测全绿，1.9s） |
| `node --check router/*.js` | ✅ portal-shell.js / admin-shell.js 通过 |

### 21.2 端到端（HTTP，真实起服务 + 真实 SQLite）

脚本流程：临时库启动 → 走完 `/api/setup` 转 normal 模式 → 41 条断言。

```
PASS=41  FAIL=0  SKIP=1
```

覆盖（按 §11/§18 对应）：

| 断言组 | 覆盖内容 |
|---|---|
| A1–A8 | 注册开关公开可读且默认关；**D29 空密码不签发凭证**；本机首次设密；X7 已设密 409；登录；D28 admin JWT 可用；**#14 admin_key 不得调 `/api/me`**；注册关闭 403 |
| A9–A14 | 注册开关 PUT；D13/D33 注册赠送入账；重名 409；D19 密码规范；普通用户 JWT 进管理接口 403 |
| A15–A19 | 充值；手动扣成负 400；建 token 明文 key 仅一次；列表不含 key；X6 limit_quota 三态 |
| A20–A24 | 402 余额不足；**X24 逃生通道（零余额仍可读 `/api/me`、仍可兑换充值码）**；D20 同码二次兑换 409；充值码 trim + 大小写不敏感 |
| A25–A28 | **越权删他人令牌 404**；X9 改密换新 token；D9 旧 JWT 401 `session_revoked`；**alg=none 伪造 JWT 401**；D26 降级唯一管理员 409 |
| A29–A31 | X5 趋势 7 个点；分页钳制 limit→100/offset→0；X22 非法筛选 400；余额流水；D10 登录锁定 429；X3 settings 暴露两个新键 |

**SKIP=1**：`A21 负余额 → 402 balance_negative`。负余额只能由「结算补扣（`actual > pre`）」产生，
需要真实上游合成服务；该路径改由 store 单测覆盖（见 21.3）。

### 21.3 单元测试（覆盖 HTTP 触不到的路径）

`store/stage4_test.go`，13 条：

| 测试 | 固化的行为 |
|---|---|
| `TestPreConsume_RejectsWhenBalanceNegative` | 负余额拒绝，**含 `Amount=0` 边界**（`-5 >= 0` 为假） |
| `TestPreConsume_UnlimitedStillBlockedWhenDebtor` | **X24 连带①：unlimited token 同样受欠费停服** |
| `TestPreConsume_RejectsInsufficientBalance` | 余额不足；**失败整体回滚且不留 pending 行** |
| `TestPreConsume_RejectsOverTokenLimit` | 限额拒绝时**余额与 used_quota 均不变**（D27 同事务） |
| `TestPreConsume_ReplayBlockedByPartialIndex` | **X16 重放不二次扣款** |
| `TestPreConsumeSettle_MovesMoneyAndUsageState` | 多退少补；`used_quota` 净效果 = actual；**`remain_quota` 冻结不动**；同一请求仅 1 行流水；`request_id` 保持稳定而 `upstream_req_id` 记录上游号 |
| `TestSettle_AllowsBalanceToGoNegative` | **X24：结算补扣允许扣成负** |
| `TestRefund_IdempotentAndBlocksAfterSettle` | 退款幂等；已退款行再结算报冲突 |
| `TestRefund_AfterSettleMustBeRejected` | **修掉 3b 缺陷：已结算不得再退预扣** |
| `TestUserAddBalance_GuardAndChargeLog` | 手动扣成负拒绝；`delta=0` 拒绝；`charge_logs` 字段与 type 过滤 |
| `TestRedemption_AtomicAndSingleUse` | 码长度；兑换入账；二次兑换 409；大小写/空白归一化；不存在与长度不符 |
| `TestEnforceInitialSecurityPolicy_FreshInstall` | **BUG-2 回归：seed 后 default 必须被提升为管理员**；无密码时强制 `allow_register=0`；幂等 |
| `TestEnforceInitialSecurityPolicy_DoesNotOverrideAdminChoice` | **D31(c)：不覆盖管理员的显式设置** |

### 21.4 实施期发现并修掉的 3 个真实缺陷

| 编号 | 缺陷 | 后果 | 修法 |
|---|---|---|---|
| **BUG-1** | 登录时 `req.Password == ""` 提前 `denyLogin` | 空密码账号**永远走不到 D29 的 `need_setup_password` 分支**，前端首次设密流程无从触发（契约 §8.1 被破坏） | 只对 `name == ""` 提前拒绝；空密码账号由空 hash 分支处理，已设密账号交给 bcrypt 自然失败 |
| **BUG-2** | `migrate()` 的 `UPDATE users SET is_admin=1 WHERE name='default'` 在**全新安装时影响 0 行**（跑在 seed 之前），而 seed 的 INSERT 不带 `is_admin` | 新装的 `default` **不是管理员** ⇒ 管理角色缺失，且「最后一个 admin 保护」因"可用管理员数=0"而**完全失效** | 在 `EnforceInitialSecurityPolicy()`（seed 之后调用）里补一条幂等提升；migrate 里的那条保留用于升级场景 |
| **BUG-3** | 结算时把 `usage.request_id` 改写为上游 ReqID | 定位键不稳定 ⇒ 结算后按原键查不到行，退款状态机退化为 `ErrNotFound`，**无法区分"已结算"与"不存在"** | 新增 `usage.upstream_req_id`；`request_id` 恒为本地号（§5.5b） |

> BUG-1/BUG-2 由 HTTP 端到端脚本抓出，BUG-3 由 store 单测抓出 —— 三者都不是靠读代码能发现的。

### 21.5 残余验证缺口（诚实登记）

| 缺口 | 说明 | 建议 |
|---|---|---|
| 真实上游合成未跑通 | 无火山 TTS 凭证，`/v1/audio/speech` 只验证到 **402 预扣拒绝**（那已发生在调用上游之前）。**成功路径的结算/退款、以及 `actual > pre` 的真实补扣未经真机验证** | 上线前用真实 key 跑一次 §18 第 12 项 |
| `balance_negative` 仅单测覆盖 | 见 21.2 的 SKIP | 同上 |
| `last_admin` 保护非原子 | "先计数再更新"，理论并发窗口（§8.3 补充） | 本期接受；要严格原子需 store 层事务 |
| 前端未经浏览器验证 | 子代理用 Node 桩把真实代码跑了 127 项回归（契约字段、三态、文案、D29 槽为空），但没有真实浏览器渲染/交互验证 | 建议人工过一遍 `/login` → `/portal` |
| 门户 mock 开关已关闭 | `router/portal-shell.js` 的 `USE_MOCK = false`（已切真接口）；置回 `true` 可离线调试 | — |

### 21.6 环境备注（沙箱）

| 现象 | 处置 |
|---|---|
| 沙箱不允许 Go 写默认缓存 | 所有 Go 命令显式设 `GOCACHE`/`GOMODCACHE` 到可写的 TEMP 目录 |
| `go get` / `git` / 部分文件写入**间歇性** `Access is denied` | 重试；`go.mod`/`go.sum` 最终由 `go get` + `go mod tidy` 正常写入 |
| 工作区内 **无法删除** `controller/zz_smoke_test.go`（多种删除方式均被拒） | 该文件首行有 `//go:build dsh_stage4_smoke` 构建标签，且 `.gitignore` 含 `*_test.go` ⇒ **不影响构建、测试与提交**。需要时手动执行：`Remove-Item controller/zz_smoke_test.go -Force` |
| 项目 `.gitignore` 含 `*_test.go` | 本项目**不提交测试文件**（既有约定）。`store/stage4_test.go` 同样被忽略 —— 若希望这些回归用例进入版本库，需先调整 `.gitignore` |

### 21.7 最终产物复验（在最后一个二进制上）

修复 BUG-1/BUG-2/BUG-3 与格式问题后，重新构建并在**新库**上复验关键安全路径：

| 复验项 | 结果 |
|---|---|
| `go build ./...` / `go vet ./...` | ✅ 通过 / 无输出 |
| `go test ./store/ -count=1` | ✅ ok（13 条单测） |
| 7 个新增 `.go` 文件的 `gofmt -l` | ✅ 全部干净（其余被列出的文件经与 HEAD 对比确认**本来就未过** gofmt，非本次引入） |
| **D29**：空密码登录 | ✅ `200 {"need_setup_password":true}`，响应中**无 `token`** |
| **BUG-2 回归**：新装 `default` 角色 | ✅ `/api/me` 返回 `"is_admin":true` |
| **D24**：新装注册开关 | ✅ `{"allow":false}` |
| **#14**：admin_key 调 `/api/me` | ✅ 401（`session_invalid`） |

> 41 条 HTTP 断言的那一轮跑在此前一个二进制上；此后仅有三处**非行为性**改动
> （`auth.go` 注释缩进、`redemptions.go` 字符串拼接空格、前端 `USE_MOCK` 开关），
> 且上述复验已重新覆盖关键安全路径，故 41/41 的结论对最终代码依然成立。

---

## 附：本文件的变更记录

| 版本 | 内容 |
|---|---|
| v1.0 | 冻结 D1–D33；补齐 X1–X15；提出 X-Q1/X-Q2/X-Q3 三项待软件部确认；补全 §8 精确字段契约、§7 时序与陷阱、§10 上线编排、§11 验收清单 |
| v1.1 | 新增 X16–X23（pending 唯一索引、**时间口径修正**、余额与 charge_logs 同事务、secret 文件细节、充值码重试、无需 CSRF 论证、流水过滤参数、前端凭证槽具体落法）；**更正 #18 关于 `htmlAwareAdmin` 对 HTML 请求不设防的事实**并给出前端兜底；补 §6.1 的 `Caller`/`SessionCaller` 分离约定；新增 §15 常量登记表、§16 改动文件清单、§17 错误码总表、§18 QA curl 清单、§19 部署操作手册 |
| v1.2 | **X-Q1 定案 → 新增 X24「欠费停服」**（用户拍板：允许扣成负 + `balance < 0` 即停服至充值恢复；含 `unlimited` token 同样受限、`balance_negative` 新错误码、逃生通道验证、前端横幅）；新增 **X25 `limit_quota` 穿透**（含 `pre`/`actual` 的 **5 字/秒分界点**分析，说明补扣是常态）；§0/§17/§18 同步（新增欠费停服用例、修正 limit 断言） |
| **v1.3** | **契约冻结，开放问题归零。** 新增 **X26**（`usage.status=0` 预扣中，定案 A：引入；含单向前进保护与"顺带修掉 3b 重复退款缺陷"）与 **X27**（依赖落定：**实测 `goproxy.cn` 可达**，钉 `jwt v5.3.1` + `x/crypto v0.58.0`，保留 bcrypt，PBKDF2/手写 HS256 仅作离线备案；记录"`go get` 须在沙箱外执行"）。**X25 定案 (a)**。§3 标题/引言、§4 落地命令、§5.5、§11.2、§12 风险表、§16、§18（新增 usage 三态与 pending 展示用例）同步。**新增 §20 明日开工第一步**（含阻塞项勾选表） |
| **v1.4** | **实施期契约修正（由前后端实现反推，均已落地）。** ① §8 开头的"时间一律 `YYYY-MM-DD HH:MM:SS`"与 X17 矛盾 → 澄清为**3b 既有接口不变、本期新增接口用 RFC3339+Z**；② §8.2 的 `trend` 说明滞后 → 改为 `date(created_at,'+8 hours')` 并注明前端**不做**时区补偿；③ 新增 **`unlimited` 与 `limit_quota` 的关系**定案（`unlimited=true` 时限额被忽略，且**自助接口不得设置 `unlimited`** —— 否则用户可自建免计费令牌）；④ §8.4 静态资源路由修正为实际引用（`/portal/admin.css` + `/dashboard/admin-shell.js`，另注册 `/portal/admin-shell.js` 别名）；⑤ §17 记录对外契约变更：预扣失败的 code 由 `quota_exhausted` → **`insufficient_balance`**，并新增 `balance_negative` / `token_limit_exceeded` / `duplicate_request`；⑥ §7.2 **更正**"`PreConsume` 签名不变"的说法 —— 实际新增 `reqID` 参数（X26 的 pending 行需要预扣期定位键） |
| **v1.5** | **实施完成。** 新增 **§5.5b `usage.upstream_req_id`**（实施期设计修正：`request_id` 必须作为稳定定位键，上游号另存一列）；§8.3 补充实施澄清（**同一 `insufficient_balance` 在管理端 charge 是 400、在 TTS 预扣是 402**、建号重名沿用 `duplicate_name`、`password-reset` 也有 400 `weak_password`、`guardLastAdmin` 非原子、两条 settings 路径均已注册）；**新增 §21「实施完成与验证证据」**：静态检查、**HTTP 端到端 41 PASS / 0 FAIL / 1 SKIP**、**store 单测 13 条全绿**、实施期修掉的 **3 个真实缺陷**（BUG-1 空密码登录走不到 D29、BUG-2 全新安装 default 不是管理员致 last_admin 保护失效、BUG-3 定位键被改写致状态机退化）、**残余验证缺口**与沙箱环境备注 |
