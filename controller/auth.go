// controller/auth.go — STAGE4 认证:注册开关查询 / 注册 / 登录 / 首次设密。
//
// 本文件承载本期**最高风险**的几条安全逻辑:
//
//   - D29:password_hash 为空时,登录**永不签发任何凭证**。
//     否则空密码账号(seed 的 default 就是 is_admin=1)可被远程
//     直接换成管理员 JWT —— 而管理接口 RequireAdminOrJWT 接受 admin JWT,
//     等于把 D14/D24 建的两道防线一起绕过。
//   - D14 + D32:首次设密只允许「本机 socket peer」或「有效 admin_key」两条通道;
//     本机判定**只读 r.RemoteAddr**,忽略一切 X-Forwarded-For / X-Real-IP。
//   - D23 + X7:setup-password **只允许给未设密账号**,已设密返 409,
//     防止 admin_key 通道被当成"任意改他人密码"的后门。
//   - D10:账号级连续失败锁定(内存 map,重启失效)。
//   - D19:密码规范(>= 8、非纯空白),注册与改密共用 ValidatePassword。
package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/store"
)

// 常量(文档 §15 常量登记表)。
const (
	// MinPasswordLen 是密码最小长度(决策 D19)。
	MinPasswordLen = 8
	// bcryptCost 是 bcrypt 代价因子(决策 X1:10,单次约 60–100ms)。
	bcryptCost = 10
	// 登录防爆破(决策 D10):账号级连续失败 loginMaxFail 次 → 锁 loginLockWindow。
	loginMaxFail    = 5
	loginLockWindow = 15 * time.Minute
	// maxAuthBodyBytes 限制认证接口 body 大小,避免大包打满内存。
	maxAuthBodyBytes = 4 << 10
	// maxUserNameLen 是用户名长度上限(文档未规定,取宽松防御值)。
	maxUserNameLen = 64
)

// settings 键名(决策 X3:复用现有 settings 表,不新建表)。
const (
	// SettingAllowRegister:是否允许自由注册(bool)。
	SettingAllowRegister = "allow_register"
	// SettingInitBalance:自助注册赠送额度(int)。
	SettingInitBalance = "init_balance"
)

// ValidatePassword 校验密码规范(决策 D19):非空/非纯空白 + 长度 >= MinPasswordLen。
// 注册、改密、admin 重置三处共用,避免口径漂移。
func ValidatePassword(pw string) error {
	if strings.TrimSpace(pw) == "" {
		return errors.New("password must not be empty or whitespace only")
	}
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	return nil
}

// HashPassword 生成 bcrypt 串(决策 X14:绝不明文落库;空串表示未设密)。
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("controller: hash password: %w", err)
	}
	return string(b), nil
}

// CheckPassword 校验明文与 bcrypt 串是否匹配;hash 为空恒不匹配。
func CheckPassword(hash, pw string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ---- 登录失败锁定(STAGE6 安全加固) ----
//
// 原实现(D10)只有**账号维度**一把锁,存在两个可被未认证攻击者直接利用的问题:
//
//  1. **账号 DoS**:攻击者对任意用户名连续 5 次错误密码,就把那个账号锁 15 分钟。
//     锁的 key 是用户名 ⇒ 知道管理员账号名(用户名往往可枚举,甚至写在文档里)
//     就能把管理员锁在门外,反复执行即可长期拒绝服务。
//  2. **内存 OOM**:loginAttempts 是个只增不减的 map,失败的账号名**永不清理**。
//     海量随机用户名 ⇒ map 无限增长 ⇒ 进程被 OOM killer 干掉(100% DoS)。
//
// 现在是**三层防护**:
//
//   A. 账号维度锁(保留):同名账号连续失败仍锁定 —— 防"针对某账号的爆破"。
//   B. IP 维度锁(新增):同一 IP 无论打哪个账号,累计失败超阈值即锁定 ——
//      这是防"批量账号枚举/分布式爆破"的主力。注意它与 A 是**或**关系:
//      换 IP 可以绕过 A,但**绕不过 B 对该 IP 的累计计数**;
//      而换 IP 攻击同一账号会被 A 拦住。两层互补,不是二选一。
//   C. 容量与过期(新增):map 条目有上限,且过期条目会被定期清理 ——
//      杜绝"海量随机用户名撑爆内存"。
//
// ⚠️ 关于"改成 (用户名,IP) 组合键"的方案:它**单独用是不够的** ——
// 攻击者只要每换一个 IP 就只失败 1 次,组合键永远不触发锁定。
// 所以本实现保留纯账号键(A),另加 IP 键(B)。

type loginAttempt struct {
	fails      int
	lockedTill time.Time
	// lastSeen 用于惰性清理:条目最后一次被访问的时间。
	lastSeen time.Time
}

var (
	loginMu sync.Mutex
	// loginAttempts:账号维度(纯用户名,保留 D10 语义)。
	loginAttempts = map[string]*loginAttempt{}
	// loginIPAttempts:IP 维度(防批量枚举 / 换 IP 爆破)。
	loginIPAttempts = map[string]*loginAttempt{}
)

// 容量与阈值(STAGE6):
const (
	// loginMaxIPFail 是同一 IP 在窗口内允许的累计失败次数。
	// 比账号维度的 5 宽松 —— 因为一个 IP 后面可能是 NAT 出口的多个用户,
	// 太紧会误伤正常用户。20 次/15 分钟仍足以挡住暴力破解。
	loginMaxIPFail = 20
	// loginMaxEntries 是两张 map 各自的条目上限。
	// 超过后触发清理;若清理后仍超,则拒绝记录新条目(宁可漏记也不 OOM)。
	loginMaxEntries = 10000
	// loginCleanupInterval 是后台清理周期。
	loginCleanupInterval = 5 * time.Minute
)

func loginKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// loginIPKey 取客户端 IP 作为限流键。
//
// ⚠️ 用 middleware.GetClientIP 而不是 r.RemoteAddr:部署在反代后面时,
// RemoteAddr 恒是反代 IP ⇒ 所有用户共用一个桶 ⇒ 一个用户输错就把全站锁了。
// GetClientIP 已处理 XFF 与可信代理跳数(见 middleware/ratelimit.go)。
func loginIPKey(r *http.Request) string {
	return middleware.GetClientIP(r)
}

// loginLocked 返回 (是否被锁, 剩余时长)。
// 账号维度与 IP 维度任一被锁即算被锁。
func loginLocked(name string, ip string) (bool, time.Duration) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()

	var longest time.Duration
	check := func(m map[string]*loginAttempt, key string) {
		a := m[key]
		if a == nil {
			return
		}
		a.lastSeen = now
		if remain := a.lockedTill.Sub(now); remain > 0 {
			if remain > longest {
				longest = remain
			}
		}
	}
	check(loginAttempts, loginKey(name))
	check(loginIPAttempts, ip)
	return longest > 0, longest
}

// loginRecordFailure 记一次失败。
//
// ⚠️ "用户不存在"与"账号未设密"两个分支**也要**调用:否则攻击者可据响应差异
// 枚举账号是否存在(X15)。
//
// 账号维度达到 loginMaxFail → 锁该账号;
// IP 维度累计达到 loginMaxIPFail → 锁该 IP。
func loginRecordFailure(name string, ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	now := time.Now()

	// --- 账号维度 ---
	k := loginKey(name)
	a := loginAttempts[k]
	if a == nil {
		if len(loginAttempts) >= loginMaxEntries {
			loginCleanupLocked(now)
		}
		if len(loginAttempts) >= loginMaxEntries {
			// 清理后仍超限:跳过记录(不能为了记一条把内存撑爆)。
			// 代价:极端洪水下这条账号的计数不准 —— 可接受。
			log.Printf("[auth] 账号锁定表已达上限 %d,跳过记录(账号=%q)", loginMaxEntries, k)
		} else {
			a = &loginAttempt{}
			loginAttempts[k] = a
		}
	}
	if a != nil {
		a.fails++
		a.lastSeen = now
		if a.fails >= loginMaxFail {
			a.lockedTill = now.Add(loginLockWindow)
			a.fails = 0
			log.Printf("[auth] 账号 %q 连续失败达 %d 次,锁定至 %s", k, loginMaxFail, a.lockedTill.Format(time.RFC3339))
		}
	}

	// --- IP 维度 ---
	if ip != "" {
		ia := loginIPAttempts[ip]
		if ia == nil {
			if len(loginIPAttempts) >= loginMaxEntries {
				loginCleanupLocked(now)
			}
			if len(loginIPAttempts) >= loginMaxEntries {
				log.Printf("[auth] IP 失败表已达上限 %d,跳过记录(ip=%s)", loginMaxEntries, ip)
			} else {
				ia = &loginAttempt{}
				loginIPAttempts[ip] = ia
			}
		}
		if ia != nil {
			ia.fails++
			ia.lastSeen = now
			if ia.fails >= loginMaxIPFail {
				ia.lockedTill = now.Add(loginLockWindow)
				ia.fails = 0
				log.Printf("[auth] IP %s 累计失败达 %d 次,锁定至 %s", ip, loginMaxIPFail, ia.lockedTill.Format(time.RFC3339))
			}
		}
	}
}

// loginCleanupLocked 清理过期条目。**调用方必须已持有 loginMu**。
//
// 清理两种条目:
//   - 已过锁定时间且很久没被访问的 → 直接删;
//   - 超过锁定窗口仍活跃的 → 保留(它还在窗口内)。
func loginCleanupLocked(now time.Time) {
	for k, a := range loginAttempts {
		if now.After(a.lockedTill) && now.Sub(a.lastSeen) > loginLockWindow {
			delete(loginAttempts, k)
		}
	}
	for k, a := range loginIPAttempts {
		if now.After(a.lockedTill) && now.Sub(a.lastSeen) > loginLockWindow {
			delete(loginIPAttempts, k)
		}
	}
}

// loginReset 登录成功后清零计数(账号 + IP 两个维度都清)。
func loginReset(name string, ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(loginAttempts, loginKey(name))
	if ip != "" {
		delete(loginIPAttempts, ip)
	}
}

// startLoginCleanup 启动后台清理协程,定期回收过期条目。
//
// 为什么必须后台跑:惰性清理只在"有新失败进来"时触发 ——
// 攻击者打一轮就停,那些条目会一直留在内存里直到下次攻击。
// 后台清理保证"攻击停止 ⇒ 内存回落"。
//
// stopCh 用于测试与优雅退出;**传 nil 表示永不停止**
// (select 里的 nil channel 永远不就绪,等于禁用该分支)。
func startLoginCleanup(stopCh <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(loginCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case now := <-ticker.C:
				loginMu.Lock()
				before := len(loginAttempts) + len(loginIPAttempts)
				loginCleanupLocked(now)
				after := len(loginAttempts) + len(loginIPAttempts)
				loginMu.Unlock()
				if before != after {
					log.Printf("[auth] 清理登录失败记录: %d → %d 条", before, after)
				}
			}
		}
	}()
}

// StartLoginCleanup 是 startLoginCleanup 的对外入口(main 启动时调用)。
// stopCh 传 nil 表示随进程生命周期常驻。
func StartLoginCleanup(stopCh <-chan struct{}) {
	startLoginCleanup(stopCh)
}

// decodeAuthBody 解析请求体并限制大小。
func decodeAuthBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return false
	}
	return true
}

// ---- GET /api/auth/register-status(公开,D22) ----

// RegisterStatusHandler GET /api/auth/register-status
//
// **公开**接口,只返回 {"allow":bool} —— 刻意不读 settings 全表,避免泄露配置。
// 键未设置时回退 false,即"默认不允许自由注册"(安全默认,见文档 §19.1)。
func RegisterStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := GetAdminStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready", "configuration_error", "db_not_ready")
		return
	}
	allow, err := s.SettingsGetBool(SettingAllowRegister, false)
	if err != nil {
		log.Printf("[auth] register-status: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "read settings failed", "server_error", "db_read_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"allow": allow})
}

// ---- POST /api/auth/register ----

type authRegisterReq struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// RegisterHandler POST /api/auth/register → 201 {id,name,status,balance}
//
// 注册开关关闭 → 403 register_disabled(决策 D4)。
// 初始余额取 settings.init_balance(D13/D16:仅自助注册赠送),赠送写
// charge_logs(type=grant, source=register_bonus)(决策 D33)。
func RegisterHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}

	allow, err := s.SettingsGetBool(SettingAllowRegister, false)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError, "read settings failed", "server_error", "db_read_failed")
		return
	}
	if !allow {
		middleware.SendJSONError(w, http.StatusForbidden, "registration is disabled", "invalid_request_error", "register_disabled")
		return
	}

	var req authRegisterReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_field")
		return
	}
	if len([]rune(name)) > maxUserNameLen {
		middleware.SendJSONError(w, http.StatusBadRequest, "name is too long", "invalid_request_error", "invalid_name")
		return
	}
	if err := ValidatePassword(req.Password); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "weak_password")
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		log.Printf("[auth] register hash: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "hash password failed", "server_error", "internal_error")
		return
	}

	initBalance, err := s.SettingsGetInt(SettingInitBalance, 0)
	if err != nil || initBalance < 0 {
		initBalance = 0
	}

	uid, err := s.UserCreateWithBalance(
		store.User{Name: name, Status: 1, PasswordHash: hash},
		int64(initBalance),
		store.ChargeSourceRegisterBonus, "注册赠送")
	switch {
	case errors.Is(err, store.ErrDuplicate):
		middleware.SendJSONError(w, http.StatusConflict, "user already exists", "invalid_request_error", "user_exists")
		return
	case errors.Is(err, store.ErrInvalid):
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "bad_request")
		return
	case err != nil:
		log.Printf("[auth] register create %q: %v", name, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "create user failed", "server_error", "db_write_failed")
		return
	}

	u, err := s.UserGet(uid)
	if err != nil {
		middleware.SendJSONError(w, http.StatusInternalServerError, "read back failed", "server_error", "db_read_failed")
		return
	}
	log.Printf("[auth] 注册成功 user_id=%d name=%q 初始余额=%d", u.ID, u.Name, u.Balance)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": u.ID, "name": u.Name, "status": u.Status, "balance": u.Balance,
	})
}

// ---- POST /api/auth/login ----

type authLoginReq struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// LoginHandler POST /api/auth/login
//
// 三条关键语义:
//  1. 用户不存在 / 密码错 / 用户名为空 → **同一个** 401 invalid_credentials
//     (不泄露账号是否存在,也不泄露"哪个字段错")
//  2. password_hash 为空 → 200 {need_setup_password:true},**不签发任何凭证**(D29)
//  3. 被禁用账号 → 403 user_disabled
//
// 限流(STAGE6 加固):**账号维度 + IP 维度**双锁,先查锁定;
// 失败分支(含 1、2)全部计数;成功两个维度都清零。
func LoginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}

	var req authLoginReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	ip := loginIPKey(r)

	// 锁定检查放在最前:被锁后连"账号是否存在"都不再回答
	if locked, remain := loginLocked(name, ip); locked {
		secs := int(remain.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		middleware.SendJSONError(w, http.StatusTooManyRequests,
			"too many failed attempts; try again later", "rate_limit_error", "too_many_attempts")
		return
	}

	if name == "" {
		loginRecordFailure(name, ip)
		denyLogin(w)
		return
	}
	// ⚠️ 注意:这里**不能**因为 req.Password == "" 就提前拒绝。
	// 账号未设密时必须走到下面的 need_setup_password 分支(决策 D29),
	// 否则前端拿不到该标记,首次设密流程无从触发。
	// 已设密账号传空密码会在 CheckPassword 处自然失败(Bcrypt 比较空串必不匹配)。

	u, err := s.UserGetByName(name)
	if err != nil {
		// 不存在也计数:否则可用"是否被锁"区分账号存在性
		loginRecordFailure(name, ip)
		denyLogin(w)
		return
	}
	if u.Status != 1 {
		middleware.SendJSONError(w, http.StatusForbidden, "user disabled", "authentication_error", "user_disabled")
		return
	}

	// 🔒 D29:空密码账号**永不签发凭证**,只告知需要设密。
	// 不校验传入的 password —— 未设密的账号没有可校验的秘密。
	if u.PasswordHash == "" {
		loginRecordFailure(name, ip)
		log.Printf("[auth] 账号 %q 尚未设置密码,要求先经由本机/admin_key 通道设密", u.Name)
		writeJSON(w, http.StatusOK, map[string]any{"need_setup_password": true})
		return
	}

	if !CheckPassword(u.PasswordHash, req.Password) {
		loginRecordFailure(name, ip)
		denyLogin(w)
		return
	}

	tok, err := middleware.IssueSessionToken(u.ID, u.IsAdmin, u.TokenVersion)
	if err != nil {
		log.Printf("[auth] login issue token user=%d: %v", u.ID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "issue session failed", "server_error", "internal_error")
		return
	}
	loginReset(name, ip)
	log.Printf("[auth] 登录成功 user_id=%d name=%q is_admin=%v", u.ID, u.Name, u.IsAdmin)
	writeJSON(w, http.StatusOK, map[string]any{
		"token":    tok,
		"is_admin": u.IsAdmin,
		"name":     u.Name,
	})
}

// denyLogin 写统一的"凭证无效"响应 —— 所有失败原因共用同一 message,
// 使外部无法区分"账号不存在"与"密码错误"。
func denyLogin(w http.ResponseWriter) {
	middleware.SendJSONError(w, http.StatusUnauthorized, "invalid credentials", "authentication_error", "invalid_credentials")
}

// ---- POST /api/auth/setup-password ----

type authSetupPasswordReq struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// SetupPasswordHandler POST /api/auth/setup-password
//
// 通道(决策 D23 + D32):**本机来源** 或 **有效 admin_key**。二者皆不满足 → 403。
//
// 只允许给**未设密**的账号设密(X7):已设密 → 409 password_already_set。
//
//	改密请走 POST /api/me/password;管理员重置请走 admin 的 password-reset。
func SetupPasswordHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := requireStoreDB(w)
	if s == nil {
		return
	}

	// 通道校验:本机(只读 socket peer)或 admin_key(常量时间比较)
	byLocal := middleware.IsLocalRequest(r)
	byAdminKey := false
	if !byLocal {
		byAdminKey = middleware.MatchesAdminKey(middleware.BearerTokenOf(r))
	}
	if !byLocal && !byAdminKey {
		log.Printf("[auth] setup-password 拒绝:来源非本机且无有效 admin_key - 客户端=%s",
			middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusForbidden,
			"setup-password is only allowed from localhost or with a valid admin key",
			"authentication_error", "setup_not_allowed")
		return
	}

	var req authSetupPasswordReq
	if !decodeAuthBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		middleware.SendJSONError(w, http.StatusBadRequest, "name is required", "invalid_request_error", "missing_field")
		return
	}
	if err := ValidatePassword(req.Password); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "weak_password")
		return
	}

	u, err := s.UserGetByName(name)
	if err != nil {
		middleware.SendJSONError(w, http.StatusNotFound, "user not found", "invalid_request_error", "user_not_found")
		return
	}
	if u.PasswordHash != "" {
		// X7:防止把 admin_key 通道当成任意改密后门
		middleware.SendJSONError(w, http.StatusConflict,
			"password already set; use change-password or admin reset",
			"invalid_request_error", "password_already_set")
		return
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		log.Printf("[auth] setup-password hash: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "hash password failed", "server_error", "internal_error")
		return
	}
	if _, err := s.UserSetPassword(u.ID, hash); err != nil {
		log.Printf("[auth] setup-password set user=%d: %v", u.ID, err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "set password failed", "server_error", "db_write_failed")
		return
	}
	log.Printf("[auth] 首次设密成功 user_id=%d name=%q 通道=%s", u.ID, u.Name, setupChannel(byLocal))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func setupChannel(byLocal bool) string {
	if byLocal {
		return "localhost"
	}
	return "admin_key"
}
