/* admin 共享脚本 — 多个 admin-* 页面共用 + 统一登录页
   加载方式: <script src="/dashboard/admin-shell.js"></script>
   暴露到 window.admin: { http, getKey, setKey, clearKey, getCred, setCred, clearCred,
                           toast, dismissToast, mountShell,
                           formatUptime, shortPath, formatBytes, formatStart, formatRFC3339, formatInt,
                           login, loginWithPassword, setupPassword, logout,
                           applyTheme, getThemePref, setThemePref, resolveTheme, escapeHTML }
   用法:
     const { http, toast, mountShell } = window.admin;
     const { apiKey, overview } = mountShell({ activeNav: 'voices' });

   STAGE4 变更(契约 docs/STAGE4_PORTAL_BILLING_PLAN.md §3 X4 / X23):
   - 仍是**单一凭证槽** sessionStorage['ttsAdminKey'],槽内可存 admin_key **或** JWT 二者之一。
     本文件是唯一读该槽的地方,axios 拦截器只读这一个槽。
   - 新增中性 API getCred/setCred/clearCred 作为新代码入口;
     getKey/setKey/clearKey 保留为薄别名(6 个既有 admin 页零改动)。
   - formatStart 保持原样(X17:3b 管理页本期不动);
     门户用的时间格式化请用 formatRFC3339(见下)。
*/
(function () {
    const KEY_STORAGE = 'ttsAdminKey';

    // === 凭证持久化(sessionStorage 单槽,跨页面同标签页共享)===
    // 槽内值可以是 admin_key,也可以是 JWT —— 两者都走 `Authorization: Bearer <槽值>`。
    const getCred = () => sessionStorage.getItem(KEY_STORAGE) || '';
    const setCred = (k) => { if (k) sessionStorage.setItem(KEY_STORAGE, k); else sessionStorage.removeItem(KEY_STORAGE); };
    const clearCred = () => sessionStorage.removeItem(KEY_STORAGE);

    // 旧 API 别名(薄封装,行为完全一致)
    const getKey = getCred;
    const setKey = setCred;
    const clearKey = clearCred;

    // === HTTP client(401 自动清凭证,业务层 redirect 到登录)===
    // 只读**一个**槽:不要在这里同时读 admin_key 与 JWT 两个槽(X4/§7.4)。
    const http = axios.create({ baseURL: '/api' });
    http.interceptors.request.use(c => {
        const k = getCred();
        if (k) c.headers.Authorization = 'Bearer ' + k;
        return c;
    });
    http.interceptors.response.use(r => r, err => {
        if (err.response && err.response.status === 401) {
            clearCred();
        }
        return Promise.reject(err);
    });

    // === Toasts(全局单例)===
    let toasts = [];
    let toastSeq = 0;
    let toastStackEl = null;
    const ensureToastStack = () => {
        if (toastStackEl) return toastStackEl;
        toastStackEl = document.createElement('div');
        toastStackEl.className = 'toast-stack';
        document.body.appendChild(toastStackEl);
        return toastStackEl;
    };
    const renderToasts = () => {
        const stack = ensureToastStack();
        stack.innerHTML = toasts.map(t => `
            <div class="toast toast-${t.type}">
                <span class="toast-icon">${t.type === 'ok' ? '✓' : t.type === 'err' ? '!' : '⚠'}</span>
                <span class="toast-msg">${escapeHTML(t.msg)}</span>
                <button class="toast-close" data-id="${t.id}">×</button>
            </div>
        `).join('');
        stack.querySelectorAll('.toast-close').forEach(btn => {
            btn.addEventListener('click', () => dismissToast(Number(btn.dataset.id)));
        });
    };
    const toast = (type, msg, ttl) => {
        const id = ++toastSeq;
        toasts.push({ id, type, msg });
        renderToasts();
        if (ttl !== 0) setTimeout(() => dismissToast(id), ttl || 3200);
    };
    const dismissToast = (id) => {
        toasts = toasts.filter(t => t.id !== id);
        renderToasts();
    };

    // === HTML escape(toast msg 兜底)===
    const escapeHTML = (s) => String(s).replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    }[c]));

    // === format helpers ===
    const formatUptime = (s) => {
        if (!s) return '—';
        const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
        return h > 0 ? `${h}h ${m}m` : `${m}m`;
    };
    const shortPath = (p) => p ? p.split(/[\\/]/).pop() : '';
    const formatBytes = (n) => {
        if (n === null || n === undefined) return '—';
        if (n < 1024) return n + ' B';
        if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
        return (n / 1024 / 1024).toFixed(2) + ' MB';
    };
    // ⚠️ 保持原样不动(X17):3b 管理页的数据是 `YYYY-MM-DD HH:MM:SS`(UTC 但无时区标记)。
    const formatStart = (iso) => {
        if (!iso) return '—';
        try {
            const d = new Date(iso);
            const pad = (n) => String(n).padStart(2, '0');
            return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
        } catch (e) { return '—'; }
    };

    // 整数千分位(余额/额度展示;STAGE4 门户与管理页共用)
    const formatInt = (n) => (n === null || n === undefined || n === '') ? '—' : Number(n).toLocaleString('zh-CN');

    // === RFC3339 时间格式化(X17,门户新代码专用)===
    // `/api/me/*` 的时间戳统一是带 `Z` 的 RFC3339 UTC(如 2026-02-01T10:00:00Z),
    // new Date(...) 会正确解析成 UTC,再按浏览器本地时区显示 —— 与旧 formatStart
    // 把 `YYYY-MM-DD HH:MM:SS` 当本地时间解析的行为不同,所以另写一个函数。
    const formatRFC3339 = (iso, withSeconds) => {
        if (!iso) return '—';
        const d = new Date(iso);
        if (isNaN(d.getTime())) return '—';
        const pad = (n) => String(n).padStart(2, '0');
        const base = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
        return withSeconds ? `${base}:${pad(d.getSeconds())}` : base;
    };

    // === 主题系统(明暗自适应 + 手动锁定) ===
    // 偏好存 localStorage 'ttsTheme': auto | light | dark
    // auto 跟随系统 prefers-color-scheme(监听变化); light/dark 手动锁定。
    // 每个 admin-* 页 <head> 最前有一段内联脚本在 CSS 加载前设 data-theme(防 FOUC)。
    const THEME_KEY = 'ttsTheme';
    const getThemePref = () => { try { return localStorage.getItem(THEME_KEY) || 'auto'; } catch (e) { return 'auto'; } };
    const setThemePref = (v) => { try { localStorage.setItem(THEME_KEY, v); } catch (e) {} };
    const resolveTheme = (pref) => pref === 'auto'
        ? (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
        : pref;
    const applyTheme = () => {
        document.documentElement.setAttribute('data-theme', resolveTheme(getThemePref()));
    };
    // 系统主题变化时,仅 auto 模式跟随(手动锁定不跳)
    if (window.matchMedia) {
        const mq = window.matchMedia('(prefers-color-scheme: dark)');
        const onSysTheme = () => { if (getThemePref() === 'auto') applyTheme(); };
        if (mq.addEventListener) mq.addEventListener('change', onSysTheme);
        else if (mq.addListener) mq.addListener(onSysTheme);
    }

    // === Sidebar 渲染(每个 admin-* 页面共用)===
    // 占位符 <aside id="admin-sidebar"></aside> 由 shell 填入。
    // options: { activeNav, overview, voiceCount, channelCount, userCount, corsConfigured, onReload, onLogout }
    const mountShell = (options = {}) => {
        const slot = document.getElementById('admin-sidebar');
        if (!slot) return {};
        const activeNav = options.activeNav || '';
        const ov = options.overview || {};
        const voiceCount = options.voiceCount != null ? options.voiceCount : '';
        const channelCount = options.channelCount != null ? options.channelCount : '';
        const userCount = options.userCount != null ? options.userCount : '';
        const corsConfigured = !!options.corsConfigured;
        const themePref = getThemePref();
        slot.outerHTML = `
<aside class="sidebar">
    <div class="brand">
        <div class="brand-logo">TTS</div>
        <div>
            <div class="brand-name">控制台</div>
            <div class="brand-version">v${escapeHTML(ov.version || '—')} · ${escapeHTML(ov.mode || '...')}</div>
        </div>
    </div>
    <nav class="nav">
        <div class="nav-group">
            <div class="nav-label">概览</div>
            <a class="nav-item${activeNav === 'dashboard' ? ' active' : ''}" href="/dashboard">
                <span class="nav-icon">▦</span><span>仪表盘</span>
            </a>
        </div>
        <div class="nav-group">
            <div class="nav-label">管理</div>
            <a class="nav-item${activeNav === 'voices' ? ' active' : ''}" href="/dashboard/voices">
                <span class="nav-icon">♪</span><span>音色</span>
                <span class="nav-badge">${voiceCount}</span>
            </a>
            <a class="nav-item${activeNav === 'channels' ? ' active' : ''}" href="/dashboard/channels">
                <span class="nav-icon">⇄</span><span>渠道</span>
                <span class="nav-badge">${channelCount}</span>
            </a>
            <a class="nav-item${activeNav === 'users' ? ' active' : ''}" href="/dashboard/users">
                <span class="nav-icon">☷</span><span>用户</span>
                <span class="nav-badge">${userCount}</span>
            </a>
            <a class="nav-item${activeNav === 'settings' ? ' active' : ''}" href="/dashboard/settings">
                <span class="nav-icon">⚙</span><span>设置</span>
                ${corsConfigured ? '' : '<span class="nav-dot" title="CORS 未配置"></span>'}
            </a>
        </div>
        <div class="nav-group">
            <div class="nav-label">观测</div>
            <a class="nav-item${activeNav === 'status' ? ' active' : ''}" href="/dashboard/status">
                <span class="nav-icon">♥</span><span>详细状态</span>
            </a>
        </div>
        <div class="nav-group">
            <div class="nav-label">我的</div>
            <a class="nav-item" href="/portal">
                <span class="nav-icon">◈</span><span>用户门户</span>
            </a>
        </div>
    </nav>
    <div class="sidebar-foot">
        <div class="theme-row" id="theme-seg" title="主题:自动 / 亮 / 暗">
            <button class="theme-btn${themePref === 'auto' ? ' active' : ''}" data-theme-pref="auto"><span class="ic">◐</span><span>自动</span></button>
            <button class="theme-btn${themePref === 'light' ? ' active' : ''}" data-theme-pref="light"><span class="ic">☀</span><span>亮</span></button>
            <button class="theme-btn${themePref === 'dark' ? ' active' : ''}" data-theme-pref="dark"><span class="ic">☾</span><span>暗</span></button>
        </div>
        <div class="sidebar-foot-row">
            <button class="btn-icon" id="btn-reload" title="刷新">↻ 刷新</button>
            <button class="btn-icon" id="btn-logout" title="登出">⏻ 登出</button>
        </div>
    </div>
</aside>`;
        const btnReload = document.getElementById('btn-reload');
        const btnLogout = document.getElementById('btn-logout');
        if (btnReload) btnReload.addEventListener('click', () => {
            if (typeof options.onReload === 'function') options.onReload();
            else location.reload();
        });
        if (btnLogout) btnLogout.addEventListener('click', () => {
            clearCred();
            if (typeof options.onLogout === 'function') options.onLogout();
            else location.href = '/dashboard/login';
        });

        // 主题切换(自动/亮/暗) + 即时生效
        const renderThemeUI = () => {
            const pref = getThemePref();
            applyTheme();
            document.querySelectorAll('#theme-seg .theme-btn').forEach(b => {
                b.classList.toggle('active', b.dataset.themePref === pref);
            });
        };
        document.querySelectorAll('#theme-seg .theme-btn').forEach(btn => {
            btn.addEventListener('click', () => {
                setThemePref(btn.dataset.themePref);
                renderThemeUI();
            });
        });
        renderThemeUI();
        return {};
    };

    // === Login helper(login 页用)===
    // ① admin_key 直连:先写槽,再拿 /api/admin/overview 验证;失败清槽。
    const login = async (key) => {
        setCred(key);
        try {
            await http.get('/admin/overview');
            return true;
        } catch (e) {
            clearCred();
            throw e;
        }
    };

    // ② 账号密码 → JWT(X23)
    //   成功 → 写槽(返回的 token),返回 { token, is_admin, name }
    //   need_setup_password=true → **绝不写入任何凭证**,按原样返回 D29 标记
    //   其它失败 → 原样抛出(不写槽)
    const loginWithPassword = async (name, password) => {
        const r = await http.post('/auth/login', { name: name, password: password });
        const d = r.data || {};
        if (d.need_setup_password) {
            clearCred();          // 保险:确保槽内没有残留凭证
            return d;
        }
        if (!d.token) {
            throw new Error('登录响应缺少 token');
        }
        setCred(d.token);
        return d;
    };

    // ③ 首次设密(X23 / D23 / D32)
    //   需要本机来源或有效 admin_key;此处用当前槽内的凭证做 auth(若槽内是 admin_key 即可过通道)。
    //   注意:该接口地址在 axios 的 baseURL '/api' 之下 ⇒ POST /api/auth/setup-password。
    const setupPassword = (name, password) =>
        http.post('/auth/setup-password', { name: name, password: password });

    // ④ 登出:清槽 + 跳统一登录页
    const logout = (redirect) => {
        clearCred();
        location.href = redirect || '/login';
    };

    // 暴露
    window.admin = {
        http,
        // 凭证槽(新代码用 Cred,旧代码用 Key 别名)
        getCred, setCred, clearCred,
        getKey, setKey, clearKey,
        toast, dismissToast,
        mountShell, login, loginWithPassword, setupPassword, logout,
        applyTheme, getThemePref, setThemePref, resolveTheme,
        formatUptime, shortPath, formatBytes, formatStart, formatRFC3339, formatInt,
        escapeHTML,
    };
})();
