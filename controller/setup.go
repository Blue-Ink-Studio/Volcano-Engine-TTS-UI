package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/volcano-tts/tts-api/common"
	"github.com/volcano-tts/tts-api/installer"
	"github.com/volcano-tts/tts-api/middleware"
	"github.com/volcano-tts/tts-api/setting"
	"github.com/volcano-tts/tts-api/store"
)

// SetupAPIState 是 setup 控制器需要的状态:
//   - Store:  db 访问,可能为 nil(自愈回退后 store 已关闭,等待重新 setup)
//   - DBPath: 用于安装完成时写 lock
type SetupAPIState struct {
	Store  *store.Store
	DBPath string
	Token  string
}

// 全局 setup 状态,在 main.go 启动时通过 SetSetupState 注入。
// 进程内只有一个二进制实例,全局变量是合适的。
var setupState SetupAPIState

// SetSetupState 注入 setup 控制器所需的 store + dbPath,启动期调用一次。
func SetSetupState(s *store.Store, dbPath string) {
	setupState.Store = s
	setupState.DBPath = dbPath
}

// GetSetupStore 供 router/main 注入的 store 访问函数。
func GetSetupStore() *store.Store { return setupState.Store }

// GetSetupDBPath 供 router/main 注入的 dbPath 访问函数。
func GetSetupDBPath() string { return setupState.DBPath }

// SetupStatusHandler GET /api/setup/status
// 始终返回当前模式,无论安装与否;用于部署探针 + 引导页判断。
func SetupStatusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	resp := map[string]any{
		"installed": installer.GetMode() == installer.ModeNormal,
		"mode":      installer.GetMode().String(),
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// SetupPrefillHandler 与 prefillFromEnv 已在 STAGE5 **删除**。
//
// 原因(契约 §7):setup 只收 `{token, admin:{name,password}}`,不再有
// api_key / auth_key / 音色等需要预填的字段 —— 预填功能失去意义,
// 而且它会继续把旧 env(BYTEDANCE_TTS_*)的形状暴露给前端,容易误导。
// 路由侧的注册也一并移除(main/router)。

// SetupRequestBody 是 POST /api/setup 的请求体(STAGE5 契约 §7)。
//
// STAGE5 起 setup **只做一件事:注册超管账号**。
// 渠道凭证 / 音色全部移出安装流程 —— 本系统是多渠道 hub,
// 在建站阶段绑死一个上游 api_key + 音色是单渠道时代的遗产,建站后到
// 渠道管理页动态添加即可。
type SetupRequestBody struct {
	Token string `json:"token"`
	Admin struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	} `json:"admin"`
}

// SetupSubmitHandler POST /api/setup
//
// STAGE5 流程:校验 token → 校验超管用户名/口令 → **单事务**创建超管 + 写基础配置
// → 写 lock → 切正常模式。必须在安装模式才接受;装完后永久 404。
//
// 不再设置 api_key / auth_key / voices —— 渠道建站后到渠道管理页添加(契约 §7)。
func SetupSubmitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 安装完成后此端点永久关闭(防止被误触)
	if installer.GetMode() != installer.ModeSetup {
		http.NotFound(w, r)
		return
	}

	// 解析 body
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var body SetupRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error", "bad_request")
		return
	}

	// token 校验(常量时间比较防计时攻击)
	if setting.SetupToken == "" || !secureEqualString(body.Token, setting.SetupToken) {
		log.Printf("[setup] token 校验失败 - 客户端=%s", middleware.GetClientIP(r))
		middleware.SendJSONError(w, http.StatusUnauthorized, "invalid setup token", "authentication_error", "invalid_token")
		return
	}

	// 超管用户名:必填 + 长度上限(与注册接口同口径 maxUserNameLen)
	adminName := strings.TrimSpace(body.Admin.Name)
	if adminName == "" {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"admin.name is required", "invalid_request_error", "missing_name")
		return
	}
	if len([]rune(adminName)) > maxUserNameLen {
		middleware.SendJSONError(w, http.StatusBadRequest,
			"admin.name is too long", "invalid_request_error", "invalid_name")
		return
	}
	// 超管口令沿用 D19 规范(>=8、非纯空白)—— 空密码的超管等于没有超管
	if err := ValidatePassword(body.Admin.Password); err != nil {
		middleware.SendJSONError(w, http.StatusBadRequest,
			err.Error(), "invalid_request_error", "weak_password")
		return
	}

	// 取 store:必须为非 nil(自愈回退后 store 是 nil,这种状态下不接 setup,要求重启)
	s := GetSetupStore()
	if s == nil {
		middleware.SendJSONError(w, http.StatusServiceUnavailable, "database not ready, please restart service", "configuration_error", "db_not_ready")
		return
	}

	hash, err := HashPassword(body.Admin.Password)
	if err != nil {
		log.Printf("[setup] hash owner password: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError,
			"hash password failed", "server_error", "internal_error")
		return
	}

	// 基础配置:只写"已初始化"与安装时间。
	//
	// 刻意**不写** api_key / auth_key / default_speaker —— 它们属于"渠道级配置",
	// 由渠道管理页承担(契约 §2/§7)。这正是 STAGE5 要消除的架构矛盾:
	// setup 不该把一个上游的凭证焊进建站流程。
	settingsKV := map[string]string{
		"initialized":  "1",
		"installed_at": time.Now().UTC().Format(time.RFC3339),
	}

	// **DB 写入单事务**(D5):超管 + 基础配置一起提交,任一失败整体回滚。
	ownerID, err := s.SetupCreateOwner(
		store.User{Name: adminName, Status: 1, PasswordHash: hash},
		settingsKV)
	if err != nil {
		log.Printf("[setup] 创建超管失败,事务回滚 - 错误=%v", err)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			middleware.SendJSONError(w, http.StatusConflict,
				"admin name already exists", "invalid_request_error", "user_exists")
		case errors.Is(err, store.ErrInvalid):
			middleware.SendJSONError(w, http.StatusBadRequest,
				err.Error(), "invalid_request_error", "bad_request")
		default:
			middleware.SendJSONError(w, http.StatusInternalServerError,
				fmt.Sprintf("failed to apply setup: %v", err),
				"server_error", "db_write_failed")
		}
		return
	}
	log.Printf("[setup] 超管已创建 user_id=%d name=%q(role=owner, user_group=default, balance=0)",
		ownerID, adminName)

	// 写 lock(原子):从这一刻起,/api/setup 永久关闭。
	//
	// 顺序刻意为"先提交 DB、再建 lock":两者无法放进同一个事务(文件 vs SQLite),
	// 而"DB 已提交但 lock 没写成"是可恢复的(重启会再判一次并允许重跑),
	// 反过来"lock 写了但 DB 没提交"会把实例锁死在"装好了但没超管"的状态。
	if err := installer.CreateLock(GetSetupDBPath()); err != nil {
		log.Printf("[setup] 写 lock 失败: %v", err)
		middleware.SendJSONError(w, http.StatusInternalServerError, "failed to create install lock", "server_error", "lock_write_failed")
		return
	}
	// 切到正常模式(本进程内)
	installer.SetMode(installer.ModeNormal)
	log.Printf("[setup] 安装完成!后续请求将进入正常模式(请用刚创建的超管登录)")

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"message":  "installed",
		"redirect": "/login", // STAGE5(D7):装完去统一登录页,用超管登录
		"owner":    adminName,
	})
}

// validateSetupSettings 与 validateSetupVoices 已在 STAGE5 删除。
//
// 旧的 setup 要求 api_key / auth_key / default_resource_id / default_speaker 四项必填,
// 并要求至少 1 条音色 —— 那是"单渠道建站"的约束。STAGE5 把这些全部移出安装流程后,
// 这两条校验不再有任何输入可校验(setup 只收超管账号)。

// secureEqualString wraps common.SecureEqualString 保持向后兼容(原文件内已有调用)。
func secureEqualString(a, b string) bool { return common.SecureEqualString(a, b) }
