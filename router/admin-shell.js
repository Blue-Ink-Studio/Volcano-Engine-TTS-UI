/* admin 共享脚本 — 多个 admin-* 页面共用
   加载方式: <script src="/admin/admin-shell.js"></script>
   暴露到 window.admin: { http, getKey, setKey, clearKey, toast, dismissToast, mountShell,
                           formatUptime, shortPath, formatBytes, formatStart, logout }
   用法:
     const { http, toast, mountShell } = window.admin;
     const { apiKey, overview } = mountShell({ activeNav: 'voices' });
*/
(function () {
    const KEY_STORAGE = 'ttsAdminKey';

    // === Key 持久化(sessionStorage,跨页面同标签页共享)===
    const getKey = () => sessionStorage.getItem(KEY_STORAGE) || '';
    const setKey = (k) => { if (k) sessionStorage.setItem(KEY_STORAGE, k); else sessionStorage.removeItem(KEY_STORAGE); };
    const clearKey = () => sessionStorage.removeItem(KEY_STORAGE);

    // === HTTP client(401 自动清 key,业务层 redirect 到登录)===
    const http = axios.create({ baseURL: '/api' });
    http.interceptors.request.use(c => {
        const k = getKey();
        if (k) c.headers.Authorization = 'Bearer ' + k;
        return c;
    });
    http.interceptors.response.use(r => r, err => {
        if (err.response && err.response.status === 401) {
            clearKey();
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
    const formatStart = (iso) => {
        if (!iso) return '—';
        try {
            const d = new Date(iso);
            const pad = (n) => String(n).padStart(2, '0');
            return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
        } catch (e) { return '—'; }
    };

    // === Sidebar 渲染(每个 admin-* 页面共用)===
    // 占位符 <aside id="admin-sidebar"></aside> 由 shell 填入。
    // options: { activeNav: 'dashboard' | 'voices' | 'settings', overview, voiceCount, corsConfigured, onReload, onLogout }
    const mountShell = (options = {}) => {
        const slot = document.getElementById('admin-sidebar');
        if (!slot) return {};
        const activeNav = options.activeNav || '';
        const ov = options.overview || {};
        const voiceCount = options.voiceCount != null ? options.voiceCount : '';
        const corsConfigured = !!options.corsConfigured;
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
            <a class="nav-item${activeNav === 'dashboard' ? ' active' : ''}" href="/admin">
                <span class="nav-icon">▦</span><span>仪表盘</span>
            </a>
        </div>
        <div class="nav-group">
            <div class="nav-label">管理</div>
            <a class="nav-item${activeNav === 'voices' ? ' active' : ''}" href="/admin/voices">
                <span class="nav-icon">♪</span><span>音色</span>
                <span class="nav-badge">${voiceCount}</span>
            </a>
            <a class="nav-item${activeNav === 'settings' ? ' active' : ''}" href="/admin/settings">
                <span class="nav-icon">⚙</span><span>设置</span>
                ${corsConfigured ? '' : '<span class="nav-dot" title="CORS 未配置"></span>'}
            </a>
        </div>
    </nav>
    <div class="sidebar-foot">
        <button class="btn-icon" id="btn-reload" title="刷新">↻ 刷新</button>
        <button class="btn-icon" id="btn-logout" title="登出">⏻ 登出</button>
    </div>
</aside>`;
        const btnReload = document.getElementById('btn-reload');
        const btnLogout = document.getElementById('btn-logout');
        if (btnReload) btnReload.addEventListener('click', () => {
            if (typeof options.onReload === 'function') options.onReload();
            else location.reload();
        });
        if (btnLogout) btnLogout.addEventListener('click', () => {
            clearKey();
            if (typeof options.onLogout === 'function') options.onLogout();
            else location.href = '/admin/login';
        });
        return {};
    };

    // === Login helper(login 页用)===
    const login = async (key) => {
        setKey(key);
        try {
            await http.get('/admin/overview');
            return true;
        } catch (e) {
            clearKey();
            throw e;
        }
    };

    // 暴露
    window.admin = {
        http, getKey, setKey, clearKey,
        toast, dismissToast,
        mountShell, login,
        formatUptime, shortPath, formatBytes, formatStart,
        escapeHTML,
    };
})();
