/* portal-shell.js — 普通用户门户共享脚本
   加载方式: <script src="/portal/portal-shell.js"></script>
   暴露到 window.portal: { USE_MOCK, http, mountPortalShell, isAdminKey, logout,
                           toast, formatQty, formatInt, fmtTime, fmtDate, todayStr, shiftDate }

   职责(§9.1 / §16.2):
   1. 复用 admin-shell.js 的**同一个** axios 实例与**同一个**凭证槽(X4/X23),
      因此「登录一次,门户与管理页共用登录态」。这里**不新建 axios 实例**。
   2. mountPortalShell():渲染 <aside id="portal-sidebar"> 与移动端导航。
   3. 401/403 处置:清槽 → 跳 /login(X23:门户不跳 /dashboard/login)。
   4. USE_MOCK:后端 /api/me/* 未就绪时用桩数据(形状严格照 §8),置 false 即走真实接口。
      桩数据由一个 axios adapter 实现,请求路径与组件代码完全一致,
      所以切换开关时**不需要改任何字段名或调用点**。
*/
(function () {
    // ===== 桩数据开关(交付说明:就是这一行)=====
    // v1.4:后端 /api/me/* 已实现并通过端到端验收,改为走真实接口。
    // 置回 true 可在没有后端时用桩数据调试(形状严格照 §8,只替换 axios adapter)。
    const USE_MOCK = false;

    // sessionStorage 键名与 admin-shell.js 完全一致(§15 storageKey: 'ttsAdminKey',不改)
    const KEY_STORAGE = 'ttsAdminKey';
    const admin = window.admin;
    if (!admin) {
        console.error('[portal-shell] 必须先加载 /dashboard/admin-shell.js');
        return;
    }
    // 复用同一实例:拦截器只读一个槽,门户与管理页共享登录态
    const http = admin.http;
    const { toast } = admin;

    // ===== 表单/展示辅助 =====
    const formatInt = (n) => (n === null || n === undefined || n === '') ? '—' : Number(n).toLocaleString('zh-CN');
    // 额度没有单位,统一按整数展示,避免误导
    const formatQty = (n) => (n === null || n === undefined) ? '—' : Number(n).toLocaleString('zh-CN');

    // `/api/me/*` 的时间戳是 RFC3339 UTC(带 Z),用 new Date() 转本地显示(X17)
    const fmtTime = (v) => admin.formatRFC3339(v, true);
    const fmtDate = (v) => admin.formatRFC3339(v, false);
    const pad2 = (n) => String(n).padStart(2, '0');
    const todayStr = () => {
        const d = new Date();
        return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
    };
    const shiftDate = (days) => {
        const d = new Date();
        d.setDate(d.getDate() + days);
        return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
    };

    // 槽内值是不是 admin_key(门户接口只认 JWT;#14)
    // 判据:JWT 恒为三段 `xxx.yyy.zzz`(header.payload.signature);
    // admin_key 默认前缀 `sk-`,不满足三段结构。宁可多跳一次登录页,也不让门户静默 401。
    const isAdminKey = (cred) => {
        const c = (cred === undefined || cred === null) ? (sessionStorage.getItem(KEY_STORAGE) || '') : String(cred);
        if (!c) return false;
        if (c.split('.').length === 3) return false;
        return true;
    };

    // 登出:清槽 → 统一登录页(X10:/login 与 /dashboard/login 同页)
    const logout = () => {
        admin.clearCred();
        location.href = '/login';
    };

    // ===== 「401 保留凭证」保护 =====
    // 场景:改密接口(POST /api/me/password)在「原密码不正确」时按 §8 返 401 invalid_credentials,
    // 而 admin-shell.js 的 401 拦截器会清掉共享凭证槽 —— 用户只是打错一次旧密码就被登出。
    // 做法:调用方在请求前 beginCredProtect(),本模块在 401 后把槽值原样写回;
    // 真正的会话失效(session_revoked / session_invalid)由调用方按 code 判定后自行 logout。
    let protectCred = null;
    const beginCredProtect = () => {
        protectCred = admin.getCred();
        return () => {
            if (protectCred) admin.setCred(protectCred);
            protectCred = null;
        };
    };

    // ===== 门户侧边栏 =====
    // 注入点 <aside id="portal-sidebar"></aside>(§16.2 指定的命名)
    // options: { activeNav, name, balance, isAdmin, onReload }
    const NAV = [
        { key: 'overview', label: '概览', icon: '▦', href: '/portal' },
        { key: 'tokens', label: '我的令牌', icon: '🔑', href: '/portal#tokens' },
        { key: 'usage', label: '用量与流水', icon: '▤', href: '/portal#usage' },
        { key: 'billing', label: '余额与充值', icon: '¥', href: '/portal#balance' },
    ];

    const mountPortalShell = (options = {}) => {
        const slot = document.getElementById('portal-sidebar');
        if (!slot) return {};
        const activeNav = options.activeNav || 'overview';
        const name = options.name || '';
        const balance = options.balance;
        const isAdmin = !!options.isAdmin;
        const balText = (balance === null || balance === undefined) ? '—' : formatInt(balance);
        const balClass = (typeof balance === 'number' && balance < 0) ? ' text-danger' : '';

        slot.outerHTML = `
<aside class="sidebar portal-side">
    <div class="brand">
        <div class="brand-logo">TTS</div>
        <div>
            <div class="brand-name">用户门户</div>
            <div class="brand-version">/portal · 自助控制台</div>
        </div>
    </div>
    <div class="portal-user">
        <div class="portal-user-avatar">${admin.escapeHTML((name || '?').slice(0, 1).toUpperCase())}</div>
        <div class="portal-user-meta">
            <div class="portal-user-name" title="${admin.escapeHTML(name)}">${admin.escapeHTML(name || '未登录')}</div>
            <div class="portal-user-bal">余额 <span class="mono${balClass}">${balText}</span></div>
        </div>
    </div>
    <nav class="nav portal-nav">
        ${NAV.map(n => `
        <a class="nav-item${activeNav === n.key ? ' active' : ''}" href="${n.href}">
            <span class="nav-icon">${n.icon}</span><span>${n.label}</span>
        </a>`).join('')}
        ${isAdmin ? `<a class="nav-item" href="/dashboard"><span class="nav-icon">☷</span><span>管理后台</span></a>` : ''}
    </nav>
    <div class="sidebar-foot">
        <div class="theme-row" id="theme-seg" title="主题:自动 / 亮 / 暗">
            <button class="theme-btn" data-theme-pref="auto"><span class="ic">◐</span><span>自动</span></button>
            <button class="theme-btn" data-theme-pref="light"><span class="ic">☀</span><span>亮</span></button>
            <button class="theme-btn" data-theme-pref="dark"><span class="ic">☾</span><span>暗</span></button>
        </div>
        <div class="sidebar-foot-row">
            <button class="btn-icon" id="portal-reload" title="刷新">↻ 刷新</button>
            <button class="btn-icon" id="portal-logout" title="退出登录">⏻ 退出</button>
        </div>
    </div>
</aside>`;

        const btnReload = document.getElementById('portal-reload');
        const btnLogout = document.getElementById('portal-logout');
        if (btnReload) btnReload.addEventListener('click', () => {
            if (typeof options.onReload === 'function') options.onReload();
            else location.reload();
        });
        if (btnLogout) btnLogout.addEventListener('click', logout);

        const renderThemeUI = () => {
            const pref = admin.getThemePref();
            admin.applyTheme();
            document.querySelectorAll('#theme-seg .theme-btn').forEach(b => {
                b.classList.toggle('active', b.dataset.themePref === pref);
            });
        };
        document.querySelectorAll('#theme-seg .theme-btn').forEach(btn => {
            btn.addEventListener('click', () => {
                admin.setThemePref(btn.dataset.themePref);
                renderThemeUI();
            });
        });
        renderThemeUI();
        return {};
    };

    // ===== 桩数据(§8 字段形状)=====
    // 说明:只是把 axios 的 adapter 换掉,组件里的 URL / 参数 / 字段读取与真实后端完全一致。
    const mockState = (() => {
        const now = Date.now();
        const iso = (msAgo) => new Date(now - msAgo).toISOString().replace(/\.\d{3}Z$/, 'Z');
        const tokenNames = ['我的脚本', '测试环境', 'CI 流水线'];
        const tokens = tokenNames.map((nm, i) => ({
            id: i + 1,
            name: nm,
            key_prefix: 'sk-' + ['a1b2c', 'd3e4f', 'g5h6i'][i],
            limit_quota: i === 0 ? null : (i === 1 ? 5000 : 100000),
            used_quota: i === 0 ? 1280 : (i === 1 ? 5420 : 38210),
            remain_quota: 0,
            unlimited: false,
            status: i === 2 ? 2 : 1,
            created_at: iso((i + 1) * 86400000 * 5),
            updated_at: iso(3600000),
        }));
        let usageIdSeq = 0;
        const usage = [];
        const models = ['volcano-tts', 'seed-tts-2.0-standard'];
        for (let i = 0; i < 46; i++) {
            const t = tokens[i % 2]; // 只用启用中的两个令牌
            const chars = 40 + ((i * 37) % 320);
            const st = i % 17 === 5 ? 0 : (i % 23 === 7 ? 2 : 1);
            const pre = Math.ceil(chars * 1.2);
            const actual = st === 2 ? 0 : Math.ceil(chars + ((i * 13) % 60));
            usage.push({
                id: 9000 - i,
                token_id: t.id,
                token_name: t.name,
                model: models[i % 2],
                characters: chars,
                audio_ms: chars * 230,
                audio_bytes: chars * 5200,
                pre_consumed: pre,
                actual_quota: actual,
                status: st,
                request_id: 'req_' + (100000 + i * 7).toString(16),
                created_at: iso(i * 5400000 + 60000),
            });
        }
        usageIdSeq = 9000;
        const charge_logs = [
            { id: 12, type: 'grant', delta: 50000, balance_after: 50000, source: 'register_bonus', remark: '注册赠送', created_at: iso(86400000 * 20) },
            { id: 13, type: 'redeem', delta: 100000, balance_after: 150000, source: 'redeem', remark: '充值码 ABCD1234EFGH5678', created_at: iso(86400000 * 14) },
            { id: 14, type: 'manual_adjust', delta: -20000, balance_after: 130000, source: 'manual', remark: '管理员手动调整', created_at: iso(86400000 * 6) },
            { id: 15, type: 'charge', delta: 30000, balance_after: 160000, source: 'manual', remark: '工单补偿', created_at: iso(86400000 * 2) },
        ];
        return {
            user: {
                name: 'demo-user',
                balance: 160000,
                is_admin: false,
                status: 1,
                created_at: iso(86400000 * 21),
            },
            tokens, usage, charge_logs, usageIdSeq,
            usedCode: 'ABCD1234EFGH5678',
            password: 'Str0ngPass123',
        };
    })();

    const codeSuffix = () => {
        const cs = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
        let s = '';
        for (let i = 0; i < 8; i++) s += cs[Math.floor(Math.random() * cs.length)];
        return s;
    };

    const mockError = (status, code, message) => {
        const err = new Error(message);
        err.response = { status, data: { error: { message, type: 'mock', code } } };
        return Promise.reject(err);
    };

    const mockHandler = (cfg) => {
        // ⚠️ adapter 收到的 cfg.url 已经由 axios 合并了 baseURL('/api'),
        //    所以这里先剥掉前缀再匹配,否则所有桩接口都会 404。
        let url = String(cfg.url || '').split('?')[0];
        if (url.startsWith('/api/')) url = url.slice(4);
        else if (url === '/api') url = '/';
        const method = (cfg.method || 'get').toLowerCase();
        const params = cfg.params || {};
        const body = typeof cfg.data === 'string' ? JSON.parse(cfg.data || '{}') : (cfg.data || {});
        const ok = (data, status) => Promise.resolve({
            data, status: status || 200, statusText: 'OK',
            headers: {}, config: cfg, request: {},
        });

        // ---- /api/me ----
        if (url === '/me' && method === 'get') return ok({ ...mockState.user });

        // ---- /api/me/password ----
        if (url === '/me/password' && method === 'post') {
            if (!body.old || body.old !== mockState.password) return mockError(401, 'invalid_credentials', '原密码不正确');
            if (!body.new || String(body.new).trim().length < 8) return mockError(400, 'weak_password', '密码长度至少 8 位且不能为纯空白');
            mockState.password = body.new;
            admin.setCred('mock.jwt.token');
            return ok({ token: 'mock.jwt.token' });
        }

        // ---- /api/me/tokens ----
        if (url === '/me/tokens' && method === 'get') {
            const limit = Number(params.limit) || 20;
            const offset = Number(params.offset) || 0;
            return ok({ items: mockState.tokens.slice(offset, offset + limit), total: mockState.tokens.length, limit, offset });
        }
        if (url === '/me/tokens' && method === 'post') {
            if (!body.name || !String(body.name).trim()) return mockError(400, 'missing_field', '缺少 name');
            const id = Math.max(0, ...mockState.tokens.map(t => t.id)) + 1;
            const plain = 'sk-' + codeSuffix() + codeSuffix().toLowerCase();
            const limitQuota = (body.limit_quota === undefined || body.limit_quota === null) ? null : Number(body.limit_quota);
            const item = {
                id, name: String(body.name).trim(), key_prefix: plain.slice(0, 8),
                limit_quota: limitQuota, used_quota: 0, remain_quota: 0,
                unlimited: !!body.unlimited, status: 1,
                created_at: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
                updated_at: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
            };
            mockState.tokens.unshift({ ...item });
            // 明文 key **仅此一次**返回;列表中不含该字段(§8.2)
            return ok({ ...item, key: plain }, 201);
        }
        const tokenMatch = url.match(/^\/me\/tokens\/(\d+)$/);
        if (tokenMatch) {
            const id = Number(tokenMatch[1]);
            const t = mockState.tokens.find(x => x.id === id);
            if (!t) return mockError(404, 'not_found', '令牌不存在');
            if (method === 'patch') {
                const keys = Object.keys(body || {});
                if (keys.length === 0) return mockError(400, 'no_fields', '没有需要更新的字段');
                // 三态(X6):字段缺失=不改;值为 null=清为不限;数字=设定
                if (Object.prototype.hasOwnProperty.call(body, 'limit_quota')) {
                    t.limit_quota = body.limit_quota === null ? null : Number(body.limit_quota);
                }
                if (Object.prototype.hasOwnProperty.call(body, 'name')) t.name = String(body.name).trim();
                if (Object.prototype.hasOwnProperty.call(body, 'status')) t.status = Number(body.status);
                if (Object.prototype.hasOwnProperty.call(body, 'unlimited')) t.unlimited = !!body.unlimited;
                t.updated_at = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
                return ok({ ...t });
            }
            if (method === 'delete') {
                mockState.tokens = mockState.tokens.filter(x => x.id !== id);
                return ok('', 204);
            }
        }

        // ---- /api/me/usage ----
        if (url === '/me/usage' && method === 'get') {
            const limit = Math.min(100, Math.max(1, Number(params.limit) || 20));
            const offset = Math.max(0, Number(params.offset) || 0);
            let rows = mockState.usage.slice();
            if (params.token_id) rows = rows.filter(r => r.token_id === Number(params.token_id));
            if (params.status !== undefined && params.status !== '' && params.status !== null) {
                const st = Number(params.status);
                if (![0, 1, 2].includes(st)) return mockError(400, 'invalid_filter', 'status 非法');
                rows = rows.filter(r => r.status === st);
            }
            if (params.from) rows = rows.filter(r => r.created_at.slice(0, 10) >= params.from);
            if (params.to) rows = rows.filter(r => r.created_at.slice(0, 10) <= params.to);
            return ok({ items: rows.slice(offset, offset + limit), total: rows.length, limit, offset });
        }

        // ---- /api/me/balance/logs ----
        if (url === '/me/balance/logs' && method === 'get') {
            const limit = Math.min(100, Math.max(1, Number(params.limit) || 20));
            const offset = Math.max(0, Number(params.offset) || 0);
            let rows = mockState.charge_logs.slice().sort((a, b) => b.id - a.id);
            if (params.type) {
                if (!['charge', 'redeem', 'manual_adjust', 'grant'].includes(params.type)) {
                    return mockError(400, 'invalid_filter', 'type 非法');
                }
                rows = rows.filter(r => r.type === params.type);
            }
            return ok({ items: rows.slice(offset, offset + limit), total: rows.length, limit, offset });
        }

        // ---- /api/me/stats ----
        if (url === '/me/stats' && method === 'get') {
            const reqDays = Number(params.days) || 14;
            const days = Math.min(90, Math.max(1, reqDays));
            const settled = mockState.usage.filter(u => u.status === 1);
            const totals = {
                characters: settled.reduce((s, u) => s + u.characters, 0),
                audio_ms: settled.reduce((s, u) => s + u.audio_ms, 0),
                audio_bytes: settled.reduce((s, u) => s + u.audio_bytes, 0),
                actual_quota: settled.reduce((s, u) => s + u.actual_quota, 0),
                requests: settled.length,
            };
            // 日期轴按本地日补齐(缺日补 0),与服务端 X17 的北京时间日界一致
            const byDay = {};
            settled.forEach(u => {
                const d = new Date(u.created_at);
                const key = `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
                if (!byDay[key]) byDay[key] = { date: key, actual_quota: 0, characters: 0, requests: 0 };
                byDay[key].actual_quota += u.actual_quota;
                byDay[key].characters += u.characters;
                byDay[key].requests += 1;
            });
            const trend = [];
            for (let i = days - 1; i >= 0; i--) {
                const key = shiftDate(-i);
                trend.push(byDay[key] || { date: key, actual_quota: 0, characters: 0, requests: 0 });
            }
            return ok({ days, totals, trend });
        }

        // ---- /api/me/redeem ----
        if (url === '/me/redeem' && method === 'post') {
            const code = String(body.code || '').trim().toUpperCase();
            if (code.length !== 16) return mockError(404, 'invalid_code', '充值码不存在');
            if (code === mockState.usedCode) return mockError(409, 'code_used', '该充值码已被使用');
            const quota = 100000;
            mockState.user.balance += quota;
            mockState.charge_logs.push({
                id: Math.max(0, ...mockState.charge_logs.map(l => l.id)) + 1,
                type: 'redeem', delta: quota, balance_after: mockState.user.balance,
                source: 'redeem', remark: '充值码 ' + code,
                created_at: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'),
            });
            return ok({ quota, balance: mockState.user.balance });
        }

        return mockError(404, 'not_found', '未实现的桩接口: ' + method + ' ' + url);
    };

    if (USE_MOCK) {
        // 只替换 adapter:URL / 参数 / 字段读取与真实后端完全一致,
        // 因此把上面的 USE_MOCK 改成 false 即可直接走真接口,无需改任何调用点。
        http.defaults.adapter = (cfg) => new Promise((resolve) => {
            setTimeout(() => { resolve(mockHandler(cfg)); }, 120);
        });
    }

    // 401 后按需恢复凭证(必须注册在 admin-shell.js 的清槽拦截器**之后**,
    // 因为 axios 的响应拦截器按注册顺序执行)
    http.interceptors.response.use(r => r, err => {
        if (protectCred && err.response && err.response.status === 401) {
            admin.setCred(protectCred);
        }
        return Promise.reject(err);
    });

    // 暴露
    window.portal = {
        USE_MOCK,
        http,
        mountPortalShell,
        isAdminKey,
        logout,
        beginCredProtect,
        toast,
        formatInt,
        formatQty,
        fmtTime,
        fmtDate,
        todayStr,
        shiftDate,
        // 桩数据直通(便于控制台手工验证)
        mockState: USE_MOCK ? mockState : undefined,
    };
})();
