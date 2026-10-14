const TOKEN_KEY = "random-image-api.admin-token";
const REMEMBER_KEY = "random-image-api.remember-token";
const MODE_KEY = "random-image-api.mode";
const DOWNLOAD_ORIGINAL_KEY = "random-image-api.download-original";
const THEME_KEY = "random-image-api.theme";
const PAGE_SIZE_KEY = "random-image-api.page-size";

const MAX_CLIENT_FILE_SIZE = 50 * 1024 * 1024;
const ALLOWED_EXTENSIONS = [".jpg", ".jpeg", ".png", ".webp"];
const UPLOAD_CONCURRENCY = 3;
const GALLERY_CACHE_LIMIT = 40;

const state = {
    type: "desktop",
    page: 1,
    size: 24,
    total: 0,
    items: [],
    query: "",
    selected: new Set(),
    files: [],
    uploading: false,
    mode: "thumb",
    downloadOriginal: false,
};

const tabPages = { desktop: 1, mobile: 1 };
const tabTotals = { desktop: null, mobile: null };
const galleryCache = new Map();
const itemById = new Map();

let currentRole = "guest";

const publicConfig = {
    guestToken: "",
    whitelistActive: false,
    guestApiOriginalLimit: 0,
    guestApiThumbLimit: 0,
    guestDownloadOriginalLimit: 0,
    guestDownloadThumbLimit: 0,
};

const $ = (id) => document.getElementById(id);

function on(id, event, handler, options) {
    const el = $(id);
    if (!el) return null;
    el.addEventListener(event, handler, options);
    return el;
}

function escapeHTML(v) {
    return String(v)
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;")
        .replaceAll('"', "&quot;")
        .replaceAll("'", "&#039;");
}

function escapeAttr(v) {
    return escapeHTML(v).replaceAll("`", "&#096;");
}

function formatBytes(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    if (bytes < 1024 * 1024 * 1024) return `${(bytes / 1024 / 1024).toFixed(2)} MB`;
    return `${(bytes / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function debounce(fn, wait) {
    let timer = null;
    return (...args) => {
        clearTimeout(timer);
        timer = setTimeout(() => fn(...args), wait);
    };
}

/* ===== 主题 ===== */

const darkQuery = window.matchMedia("(prefers-color-scheme: dark)");

function applyTheme() {
    const saved = localStorage.getItem(THEME_KEY) || "auto";
    const dark = saved === "dark" || (saved === "auto" && darkQuery.matches);
    document.documentElement.dataset.theme = dark ? "dark" : "light";

    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = dark ? "#090b16" : "#f3f4fb";
}

function toggleTheme() {
    const current = document.documentElement.dataset.theme;
    localStorage.setItem(THEME_KEY, current === "dark" ? "light" : "dark");
    applyTheme();
}

darkQuery.addEventListener("change", () => {
    if (!localStorage.getItem(THEME_KEY) || localStorage.getItem(THEME_KEY) === "auto") applyTheme();
});

/* ===== 弹窗 / 提示 ===== */

let lastFocusedElement = null;

function showModal(element, visible) {
    if (!element) return;

    if (visible) {
        lastFocusedElement =
            document.activeElement instanceof HTMLElement ? document.activeElement : null;
        element.classList.remove("hidden");
        element.removeAttribute("inert");
        element.setAttribute("aria-hidden", "false");
        document.body.classList.add("modal-open");
        return;
    }

    if (document.activeElement?.blur) document.activeElement.blur();

    element.classList.add("hidden");
    element.setAttribute("inert", "");
    element.setAttribute("aria-hidden", "true");

    if (!document.querySelector(".modal:not(.hidden)")) {
        document.body.classList.remove("modal-open");
    }

    if (lastFocusedElement?.focus && document.contains(lastFocusedElement)) {
        try { lastFocusedElement.focus({ preventScroll: true }); } catch {}
    }
    lastFocusedElement = null;
}

function isModalOpen(id) {
    return !$(id)?.classList.contains("hidden");
}

function showToast(message, type = "") {
    const toast = $("toast");
    if (!toast) return;

    toast.textContent = message;
    toast.classList.toggle("is-error", type === "error");
    toast.classList.add("show");

    clearTimeout(showToast.timer);
    showToast.timer = setTimeout(() => toast.classList.remove("show"), 2400);
}

function askConfirm({ title, message, okText = "确定", danger = false, cancel = true }) {
    const dialog = $("confirmDialog");
    if (!dialog || typeof dialog.showModal !== "function") {
        return Promise.resolve(window.confirm(message));
    }

    $("confirmTitle").textContent = title;
    $("confirmMessage").textContent = message;

    const ok = $("confirmOk");
    ok.textContent = okText;
    ok.className = danger ? "btn danger" : "btn primary";

    const cancelBtn = $("confirmCancel");
    cancelBtn.hidden = !cancel;

    dialog.returnValue = "";
    return new Promise((resolve) => {
        dialog.addEventListener("close", () => resolve(dialog.returnValue === "ok"), { once: true });
        dialog.showModal();
    });
}

/* ===== 公开配置 ===== */

async function loadPublicConfig() {
    try {
        const resp = await fetch("/api/public-config", { cache: "no-store" });
        if (!resp.ok) return;

        const payload = await resp.json();
        const p = payload?.data || {};

        publicConfig.guestToken = String(p.guest_token || "");
        publicConfig.whitelistActive = Boolean(p.whitelist_active);
        publicConfig.guestApiOriginalLimit = Number(p.guest_api_original_limit) || 0;
        publicConfig.guestApiThumbLimit = Number(p.guest_api_thumb_limit) || 0;
        publicConfig.guestDownloadOriginalLimit = Number(p.guest_download_original_limit) || 0;
        publicConfig.guestDownloadThumbLimit = Number(p.guest_download_thumb_limit) || 0;

        renderPublicConfig();
    } catch (err) {
        console.warn("[app] public-config 加载失败", err);
    }
}

function renderPublicConfig() {
    const whitelistStrip = $("whitelistStrip");
    if (whitelistStrip) whitelistStrip.hidden = !publicConfig.whitelistActive;

    const strip = $("visitorStrip");
    if (!strip || !publicConfig.guestToken) return;

    strip.hidden = false;

    const quota = (value) => (value > 0 ? value : "∞");
    setText("guestTokenDisplay", publicConfig.guestToken);
    setText("guestApiThumb", quota(publicConfig.guestApiThumbLimit));
    setText("guestApiOriginal", quota(publicConfig.guestApiOriginalLimit));
    setText("guestDownloadThumb", quota(publicConfig.guestDownloadThumbLimit));
    setText("guestDownloadOriginal", quota(publicConfig.guestDownloadOriginalLimit));
}

function setText(id, value) {
    const el = $(id);
    if (el) el.textContent = value;
}

async function ensureGuestTokenLoaded() {
    if (publicConfig.guestToken) return publicConfig.guestToken;
    await loadPublicConfig();
    return publicConfig.guestToken;
}

/* ===== 密钥 ===== */

function apiToken() {
    const input = $("tokenInput");
    return input ? input.value.trim() : "";
}

function saveToken() {
    const remember = $("rememberKey")?.checked || false;
    const token = apiToken();

    if (remember && token) {
        localStorage.setItem(TOKEN_KEY, token);
        localStorage.setItem(REMEMBER_KEY, "1");
        return;
    }

    localStorage.removeItem(TOKEN_KEY);
    localStorage.setItem(REMEMBER_KEY, remember ? "1" : "0");
}

function restoreToken() {
    const remember = localStorage.getItem(REMEMBER_KEY) === "1";
    const saved = localStorage.getItem(TOKEN_KEY);

    const rememberEl = $("rememberKey");
    if (rememberEl) rememberEl.checked = remember;

    const tokenEl = $("tokenInput");
    if (tokenEl) tokenEl.value = remember && saved ? saved : "";
}

async function detectRole() {
    const token = apiToken();

    if (!token) {
        currentRole = "guest";
        applyRoleUI();
        return;
    }

    try {
        const resp = await fetch("/api/whoami", {
            headers: { Authorization: `Bearer ${token}` },
            cache: "no-store",
        });
        const data = resp.ok ? await resp.json() : null;
        currentRole = data?.data?.role === "admin" ? "admin" : "guest";
    } catch {
        currentRole = "guest";
    }

    applyRoleUI();
}

async function probeRole(token) {
    if (!token) return { role: "empty" };

    await ensureGuestTokenLoaded();
    if (publicConfig.guestToken && token === publicConfig.guestToken) return { role: "guest" };

    try {
        const resp = await fetch("/api/whoami", {
            headers: { Authorization: `Bearer ${token}` },
            cache: "no-store",
        });
        if (!resp.ok) return { role: "invalid" };

        const data = await resp.json();
        const role = data?.data?.role;
        if (role === "admin") return { role: "admin" };
        if (data?.data?.reason === "whitelist_blocked") return { role: "blocked" };
        return { role: "invalid" };
    } catch {
        return { role: "invalid" };
    }
}

function applyRoleUI() {
    const isAdmin = currentRole === "admin";

    document.body.classList.toggle("is-admin", isAdmin);
    document.body.classList.toggle("is-guest", !isAdmin);

    setText("currentRoleLabel", isAdmin ? "管理员 · 修改密钥" : "访客 · 设置密钥");
    applyModeUI();
}

/* ===== API 模式 ===== */

function restoreMode() {
    state.mode = localStorage.getItem(MODE_KEY) === "original" ? "original" : "thumb";
    applyModeUI();
}

function applyModeUI() {
    document.querySelectorAll(".mode-btn").forEach((btn) => {
        btn.classList.toggle("active", btn.dataset.mode === state.mode);
    });
    updateAPIBox();
}

function setMode(mode) {
    if (mode !== "thumb" && mode !== "original") return;
    state.mode = mode;
    localStorage.setItem(MODE_KEY, mode);
    applyModeUI();
}

function restoreDownloadOriginal() {
    state.downloadOriginal = localStorage.getItem(DOWNLOAD_ORIGINAL_KEY) === "1";
    const el = $("downloadOriginal");
    if (el) el.checked = state.downloadOriginal;
}

function saveDownloadOriginal() {
    localStorage.setItem(DOWNLOAD_ORIGINAL_KEY, state.downloadOriginal ? "1" : "0");
}

function apiAutoURL() {
    const width = Math.max(1, window.innerWidth);
    const params = new URLSearchParams({ type: "auto", w: String(width) });

    if (state.mode === "original") {
        params.set("mode", "original");
        const token = apiToken();
        if (token && currentRole === "admin") params.set("token", token);
    }

    return `${location.origin}/api/random?${params}`;
}

function updateAPIBox() {
    const url = apiAutoURL();

    const apiElement = $("apiUrl");
    if (apiElement) {
        apiElement.textContent = url;
        apiElement.title = url;
    }

    document.querySelectorAll("#randomApiLink, #randomHeroLink").forEach((link) => {
        link.href = url;
    });

    const width = Math.max(1, window.innerWidth);
    setText("currentWidth", `${width}px`);
    setText("currentDeviceType", width <= 768 ? "Mobile" : "Desktop");
}

async function copyText(text, button) {
    if (!text) {
        showToast("没有可复制的内容");
        return false;
    }

    const originalHTML = button ? button.innerHTML : "";
    const flash = (label) => {
        if (!button) return;
        button.innerHTML = label;
        setTimeout(() => { button.innerHTML = originalHTML; }, 1200);
    };

    if (navigator.clipboard && window.isSecureContext) {
        try {
            await navigator.clipboard.writeText(text);
            showToast("已复制到剪贴板 ✨");
            flash("✓");
            return true;
        } catch {}
    }

    try {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        ta.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;opacity:0";
        document.body.appendChild(ta);
        ta.select();
        ta.setSelectionRange(0, ta.value.length);
        const ok = document.execCommand("copy");
        ta.remove();
        if (ok) {
            showToast("已复制到剪贴板 ✨");
            flash("✓");
            return true;
        }
    } catch {}

    showToast("复制失败，请手动复制", "error");
    return false;
}

/* ===== 卡片 ===== */

function pickGridURL(item) {
    return item.grid_url || item.thumbnail_url || item.url;
}

function pickPreviewURL(item) {
    return item.thumbnail_url || item.url;
}

function matchesQuery(item) {
    if (!state.query) return true;
    const q = state.query.toLowerCase();
    return (
        String(item.filename).toLowerCase().includes(q) ||
        String(item.original_name).toLowerCase().includes(q)
    );
}

function imageCard(item) {
    const checked = state.selected.has(item.id) ? "checked" : "";
    const caption = `${item.original_name} · ${item.width}×${item.height} · ${formatBytes(item.size)}`;
    const previewURL = pickPreviewURL(item);
    const smallTag = item.has_thumbnail === false
        ? ' <span class="small-tag" title="此图较小，未单独生成缩略图，下载即为原始文件">小图</span>'
        : "";

    return `
    <article class="image-card" data-card-id="${item.id}">
      <div class="thumb-wrap">
        <img class="thumb" loading="lazy" decoding="async"
          src="${escapeAttr(pickGridURL(item))}" alt="${escapeAttr(item.original_name)}"
          data-preview="${escapeAttr(previewURL)}" data-caption="${escapeAttr(caption)}">
        <label class="check" title="选择这张图片">
          <input type="checkbox" data-select-id="${item.id}" ${checked} aria-label="选择 ${escapeAttr(item.filename)}">
          <svg class="icon"><use href="#i-check"/></svg>
        </label>
        <span class="badge">${escapeHTML(String(item.type).toUpperCase())}</span>
        <div class="thumb-actions">
          <button class="chip-btn" type="button" data-preview="${escapeAttr(previewURL)}" data-caption="${escapeAttr(caption)}">
            <svg class="icon"><use href="#i-expand"/></svg><span>预览</span>
          </button>
          <button class="chip-btn" type="button" data-download-id="${item.id}">
            <svg class="icon"><use href="#i-download"/></svg><span>下载</span>
          </button>
        </div>
      </div>
      <div class="image-body">
        <div class="filename" title="${escapeAttr(item.filename)}">${escapeHTML(item.filename)}</div>
        <div class="image-meta" title="${escapeAttr(item.original_name)}">
          ${item.width} × ${item.height} · ${formatBytes(item.size)}${smallTag}
        </div>
      </div>
    </article>`;
}

/* ===== 画廊 ===== */

function galleryCacheKey(type, page, size) {
    return `${type}:${page}:${size}`;
}

function clearGalleryCache() {
    galleryCache.clear();
}

function cacheGalleryResult(type, page, size, result) {
    const key = galleryCacheKey(type, page, size);
    if (galleryCache.size >= GALLERY_CACHE_LIMIT) {
        galleryCache.delete(galleryCache.keys().next().value);
    }
    galleryCache.set(key, result);
}

async function fetchGalleryPage(type, page, size) {
    const params = new URLSearchParams({ type, page: String(page), size: String(size) });
    const response = await fetch(`/api/images?${params}`, {
        cache: "no-store",
        headers: { Accept: "application/json, image/webp" },
    });
    const result = await response.json();
    if (!response.ok || !result.success) throw new Error(result.message || "加载失败");
    return result;
}

function renderSkeletons(count = 8) {
    const gallery = $("gallery");
    if (!gallery) return;
    gallery.innerHTML = Array.from(
        { length: count },
        () => `<div class="skeleton skeleton-card" aria-hidden="true"></div>`,
    ).join("");
}

function renderGallery() {
    const gallery = $("gallery");
    if (!gallery) return;

    const visible = state.items.filter(matchesQuery);

    if (!state.items.length) {
        gallery.innerHTML = `<div class="empty">当前分类还没有图片</div>`;
    } else if (!visible.length) {
        gallery.innerHTML = `<div class="empty">没有匹配「${escapeHTML(state.query)}」的图片</div>`;
    } else {
        gallery.innerHTML = visible.map(imageCard).join("");
    }

    renderStats(visible.length);
}

function renderStats(visibleCount) {
    const maxPage = Math.max(1, Math.ceil(state.total / state.size));
    const matched = state.query ? ` · 匹配 ${visibleCount}/${state.items.length}` : "";

    setText("galleryStats", state.total
        ? `${state.total} 张图片 · 第 ${state.page}/${maxPage} 页${matched}`
        : "还没有图片");
    setText("pageInfo", `${state.page} / ${maxPage}`);

    const prev = $("prevPage");
    if (prev) prev.disabled = state.page <= 1;
    const next = $("nextPage");
    if (next) next.disabled = state.page >= maxPage;

    setText("currentTypeBadge", state.type === "mobile" ? "Mobile" : "Desktop");
    const badge = $("currentTypeBadge");
    if (badge) {
        badge.classList.toggle("desktop", state.type === "desktop");
        badge.classList.toggle("mobile", state.type === "mobile");
    }

    tabTotals[state.type] = state.total;
    setText("desktopCount", tabTotals.desktop ?? "-");
    setText("mobileCount", tabTotals.mobile ?? "-");
}

function applyGalleryResult(result) {
    state.total = Number(result.data.total) || 0;
    state.items = Array.isArray(result.data.items) ? result.data.items : [];

    itemById.clear();
    for (const item of state.items) itemById.set(item.id, item);

    const maxPage = Math.max(1, Math.ceil(state.total / state.size));
    if (state.page > maxPage) state.page = maxPage;
    tabPages[state.type] = state.page;

    const visibleIds = new Set(state.items.map((item) => item.id));
    for (const id of state.selected) {
        if (!visibleIds.has(id)) state.selected.delete(id);
    }

    renderGallery();
    updateSelectionUI();
}

async function loadGallery({ force = false } = {}) {
    if (force) clearGalleryCache();

    const { type, page, size } = state;
    const cached = galleryCache.get(galleryCacheKey(type, page, size));

    if (cached) {
        applyGalleryResult(cached);
        schedulePrefetch();
        return;
    }

    renderSkeletons(size > 24 ? 12 : 8);

    try {
        const result = await fetchGalleryPage(type, page, size);
        cacheGalleryResult(type, page, size, result);
        applyGalleryResult(result);
        schedulePrefetch();
    } catch (error) {
        const gallery = $("gallery");
        if (gallery) gallery.innerHTML = `<div class="empty">加载失败：${escapeHTML(error.message)}</div>`;
        setText("galleryStats", "加载失败");
    }
}

let prefetchTimer = null;

function schedulePrefetch() {
    clearTimeout(prefetchTimer);
    prefetchTimer = setTimeout(prefetchAdjacentPages, 600);
}

function prefetchAdjacentPages() {
    const { type, page, size, total } = state;
    const maxPage = Math.max(1, Math.ceil(total / size));

    for (const target of [page + 1, page - 1]) {
        if (target < 1 || target > maxPage) continue;
        if (galleryCache.has(galleryCacheKey(type, target, size))) continue;
        fetchGalleryPage(type, target, size)
            .then((result) => cacheGalleryResult(type, target, size, result))
            .catch(() => {});
    }
}

async function refreshTabCounts() {
    const other = state.type === "desktop" ? "mobile" : "desktop";
    try {
        const result = await fetchGalleryPage(other, 1, 1);
        tabTotals[other] = Number(result.data.total) || 0;
        setText(other === "desktop" ? "desktopCount" : "mobileCount", tabTotals[other]);
    } catch {}
}

function updateSelectionUI() {
    const count = state.selected.size;

    setText("selectionInfo", count ? `已选 ${count} 张` : "未选择");
    document.body.classList.toggle("has-selection", count > 0);

    const del = $("batchDelete");
    if (del) del.disabled = count === 0;
    const dl = $("batchDownload");
    if (dl) dl.disabled = count === 0;
}

/* ===== 上传 ===== */

function createFileKey(file) {
    return [file.name, file.size, file.lastModified].join("|");
}

function addFiles(fileList) {
    let added = 0;
    let skipped = 0;

    for (const file of Array.from(fileList || [])) {
        const lowerName = file.name.toLowerCase();
        const ext = lowerName.includes(".") ? lowerName.slice(lowerName.lastIndexOf(".")) : "";

        if (!ALLOWED_EXTENSIONS.includes(ext) || file.size <= 0 || file.size > MAX_CLIENT_FILE_SIZE) {
            skipped++;
            continue;
        }

        const key = createFileKey(file);
        if (state.files.some((item) => item.key === key)) continue;

        state.files.push({ key, file, status: "pending", message: "等待上传", progress: 0, xhr: null });
        added++;
    }

    renderFileList();
    if (skipped) showToast(`已跳过 ${skipped} 个不支持或超限的文件`, "error");
    else if (added) showToast(`已添加 ${added} 张图片`);
}

function removeFile(key) {
    const index = state.files.findIndex((item) => item.key === key);
    if (index === -1) return;

    if (state.files[index].status === "uploading") {
        state.files[index].xhr?.abort();
        return;
    }

    state.files.splice(index, 1);
    renderFileList();
}

function renderFileList() {
    const fileList = $("fileList");
    const fileSummary = $("fileSummary");
    const startUploadBtn = $("startUpload");

    if (!state.files.length) {
        if (fileList) fileList.innerHTML = "";
        if (fileSummary) fileSummary.textContent = "未选择文件";
        if (startUploadBtn) startUploadBtn.disabled = true;
        return;
    }

    const totalSize = state.files.reduce((sum, item) => sum + item.file.size, 0);
    const uploadingCount = state.files.filter((i) => i.status === "uploading").length;
    const pendingCount = state.files.filter((i) => i.status === "pending").length;
    const failedCount = state.files.filter((i) => i.status === "failed" || i.status === "cancelled").length;

    let summary = `已选 ${state.files.length} 张 · ${formatBytes(totalSize)}`;
    if (failedCount > 0) summary += ` · ${failedCount} 张待重试`;

    if (fileSummary) fileSummary.textContent = summary;
    if (startUploadBtn) startUploadBtn.disabled = uploadingCount > 0 || pendingCount === 0;
    if (!fileList) return;

    fileList.innerHTML = state.files.map((item) => {
        let action = "";

        if (item.status === "pending") {
            action = `<button type="button" data-remove-file="${escapeAttr(item.key)}" title="移除">×</button>`;
        } else if (item.status === "uploading") {
            action = `<button type="button" data-cancel-file="${escapeAttr(item.key)}" title="取消上传">取消</button>`;
        } else if (item.status === "success") {
            action = `<span class="file-tick">✓</span>`;
        } else {
            action = `<button type="button" data-retry-file="${escapeAttr(item.key)}" title="重新上传">↻</button>
                      <button type="button" data-remove-file="${escapeAttr(item.key)}" title="移除">×</button>`;
        }

        const progress = item.status === "uploading"
            ? `<div class="file-progress" role="progressbar" aria-valuenow="${Math.round(item.progress || 0)}">
                 <div class="file-progress-bar" style="width:${Math.max(0, Math.min(100, item.progress || 0))}%"></div>
               </div>`
            : "";

        return `
      <div class="file-row file-${item.status}" data-file-key="${escapeAttr(item.key)}">
        <div class="file-row-main">
          <div class="file-name-wrap">
            <div class="file-name" title="${escapeAttr(item.file.name)}">${escapeHTML(item.file.name)}</div>
            <div class="file-message">${escapeHTML(item.message || "")}</div>
          </div>
          <div class="file-status">${formatBytes(item.file.size)}</div>
          <div class="file-action">${action}</div>
        </div>
        ${progress}
      </div>`;
    }).join("");
}

function uploadOne(item, token) {
    item.status = "uploading";
    item.message = "上传中…";
    item.progress = 0;
    renderFileList();

    return new Promise((resolve) => {
        let settled = false;
        const finish = (value) => {
            if (settled) return;
            settled = true;
            item.xhr = null;
            renderFileList();
            resolve(value);
        };

        const xhr = new XMLHttpRequest();
        item.xhr = xhr;

        const form = new FormData();
        form.append("file", item.file);

        xhr.open("POST", "/api/upload");
        xhr.setRequestHeader("Authorization", `Bearer ${token}`);

        xhr.upload.onprogress = (event) => {
            if (!event.lengthComputable) return;
            const percent = Math.round((event.loaded / event.total) * 100);
            item.progress = percent;
            item.message = `上传中 ${percent}%`;

            const row = document.querySelector(`[data-file-key="${CSS.escape(item.key)}"]`);
            if (!row) return;

            const bar = row.querySelector(".file-progress-bar");
            if (bar) bar.style.width = `${percent}%`;
            const msg = row.querySelector(".file-message");
            if (msg) msg.textContent = item.message;
        };

        xhr.onload = () => {
            let result = {};
            try { result = JSON.parse(xhr.responseText || "{}"); } catch {}

            if (xhr.status === 401) {
                item.status = "failed";
                item.message = "密钥错误";
                finish(false);
                return;
            }
            if (xhr.status === 403) {
                item.status = "failed";
                item.message = "无权限";
                finish(false);
                return;
            }
            if (xhr.status < 200 || xhr.status >= 300 || !result.success) {
                item.status = "failed";
                item.message = result.message || `HTTP ${xhr.status}`;
                finish(false);
                return;
            }

            item.status = "success";
            item.message = "上传完成";
            item.progress = 100;
            finish(true);
        };

        xhr.onerror = () => { item.status = "failed"; item.message = "网络错误"; finish(false); };
        xhr.ontimeout = () => { item.status = "failed"; item.message = "请求超时"; finish(false); };
        xhr.onabort = () => { item.status = "cancelled"; item.message = "已取消"; finish(false); };

        xhr.send(form);
    });
}

async function startUpload() {
    if (state.uploading) return;

    const resultBox = $("uploadResult");

    if (currentRole !== "admin") {
        if (resultBox) resultBox.textContent = "当前身份不是管理员，请先设置密钥。";
        showToast("请先设置管理员密钥", "error");
        return;
    }

    const token = apiToken();
    if (!token) {
        if (resultBox) resultBox.textContent = "请输入密钥。";
        return;
    }

    saveToken();

    const queue = state.files.filter((item) => item.status === "pending");
    if (!queue.length) {
        if (resultBox) resultBox.textContent = "没有待上传的图片。";
        return;
    }

    state.uploading = true;
    $("clearFiles")?.setAttribute("disabled", "");
    $("fileInput")?.setAttribute("disabled", "");

    let cursor = 0;
    let successCount = 0;
    let failedCount = 0;
    let cancelledCount = 0;

    const report = () => {
        if (resultBox) {
            resultBox.textContent = `处理中：${successCount} 成功 · ${failedCount} 失败 · ${cancelledCount} 取消`;
        }
    };

    const worker = async () => {
        while (cursor < queue.length) {
            const item = queue[cursor++];
            const ok = await uploadOne(item, token);

            if (ok) successCount++;
            else if (item.status === "cancelled") cancelledCount++;
            else failedCount++;

            report();
        }
    };

    await Promise.all(
        Array.from({ length: Math.min(UPLOAD_CONCURRENCY, queue.length) }, worker),
    );

    state.uploading = false;
    $("clearFiles")?.removeAttribute("disabled");
    $("fileInput")?.removeAttribute("disabled");
    renderFileList();

    if (resultBox) {
        resultBox.textContent = `完成：${successCount} 成功 · ${failedCount} 失败 · ${cancelledCount} 取消`;
    }
    showToast(successCount ? `已上传 ${successCount} 张图片` : "上传失败", successCount ? "" : "error");

    if (successCount > 0) {
        clearGalleryCache();
        await Promise.all([loadGallery({ force: true }), refreshTabCounts()]);
    }
}

async function batchDelete() {
    const ids = Array.from(state.selected);
    if (!ids.length) return;

    if (currentRole !== "admin") {
        showToast("访客没有删除权限", "error");
        return;
    }

    const token = apiToken();
    if (!token) {
        showToast("请先设置密钥", "error");
        return;
    }

    const confirmed = await askConfirm({
        title: "删除图片",
        message: `确定删除选中的 ${ids.length} 张图片吗？此操作不可撤销。`,
        okText: "删除",
        danger: true,
    });
    if (!confirmed) return;

    saveToken();

    try {
        const response = await fetch("/api/images/delete/batch", {
            method: "POST",
            headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
            body: JSON.stringify({ ids }),
        });

        const result = await response.json().catch(() => ({}));

        if (response.status === 401) { showToast("密钥错误", "error"); return; }
        if (response.status === 403) { showToast("无权限", "error"); return; }
        if (!response.ok && response.status !== 207) {
            showToast(result.message || "批量删除失败", "error");
            return;
        }

        const deleted = result.data?.deleted ?? 0;
        const failed = result.data?.failed ?? [];
        state.selected.clear();

        clearGalleryCache();
        await Promise.all([loadGallery({ force: true }), refreshTabCounts()]);

        if (failed.length) {
            await askConfirm({
                title: `已删除 ${deleted} 张`,
                message: `以下图片删除失败：\n${failed.join("\n")}`,
                okText: "知道了",
                cancel: false,
            });
        } else {
            showToast(`已删除 ${deleted} 张图片`);
        }
    } catch (error) {
        showToast(`删除失败：${error.message}`, "error");
    }
}

async function batchDownload() {
    const ids = Array.from(state.selected);
    if (!ids.length) return;

    const token = apiToken();
    if (!token) {
        showToast("请先设置密钥", "error");
        return;
    }

    saveToken();

    const body = { ids, mode: state.downloadOriginal ? "original" : "thumb" };

    try {
        const response = await fetch("/api/images/download/batch", {
            method: "POST",
            headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
            body: JSON.stringify(body),
        });

        if (!response.ok) {
            const result = await response.json().catch(() => ({}));
            const fallback = response.status === 401 ? "密钥错误" : "批量下载失败";
            showToast(result.message || fallback, "error");
            return;
        }

        const blob = await response.blob();
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = `random-images-${Date.now()}.zip`;
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
        showToast(`正在下载 ${ids.length} 张图片`);
    } catch (error) {
        showToast(`下载失败：${error.message}`, "error");
    }
}

async function downloadSingle(item) {
    if (!item) return;

    const token = apiToken();
    if (!token) {
        showToast("请先设置密钥", "error");
        return;
    }

    const qs = state.downloadOriginal ? "?mode=original" : "";

    let response;
    try {
        response = await fetch(item.download_url + qs, {
            headers: { Authorization: `Bearer ${token}` },
        });
    } catch (err) {
        showToast(`下载失败：${err.message}`, "error");
        return;
    }

    if (!response.ok) {
        const data = await response.json().catch(() => ({}));
        const fallback = response.status === 401 ? "密钥错误" : `下载失败（HTTP ${response.status}）`;
        showToast(data.message || fallback, "error");
        return;
    }

    let filename = item.original_name || item.filename || "image";
    if (!state.downloadOriginal) {
        const base = filename.replace(/\.[^.]+$/, "");
        filename = (base || "image") + ".jpg";
    }

    const blobURL = URL.createObjectURL(await response.blob());
    const a = document.createElement("a");
    a.href = blobURL;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(blobURL), 1000);
}

/* ===== 预览 ===== */

const clamp = (v, min, max) => Math.min(Math.max(v, min), max);
const previewState = { items: [], index: 0, zoom: 1, origin: "50% 50%", token: 0 };

function collectPreviewItems() {
    return Array.from(document.querySelectorAll("#gallery img.thumb[data-preview]")).map((img) => ({
        url: img.dataset.preview,
        caption: img.dataset.caption || "",
        alt: img.alt || "图片预览",
    }));
}

function applyPreviewZoom() {
    const img = $("previewImg");
    if (!img) return;
    img.style.transformOrigin = previewState.origin;
    img.style.transform = previewState.zoom > 1 ? `scale(${previewState.zoom})` : "";
    img.classList.toggle("zoomed", previewState.zoom > 1);
}

function resetPreviewZoom() {
    previewState.zoom = 1;
    previewState.origin = "50% 50%";
    applyPreviewZoom();
}

function setPreviewZoom(zoom, cx, cy) {
    const img = $("previewImg");
    if (!img) return;

    const next = clamp(zoom, 1, 4);

    if (next > 1 && cx != null && cy != null) {
        const rect = img.getBoundingClientRect();
        if (rect.width && rect.height) {
            const x = clamp(((cx - rect.left) / rect.width) * 100, 0, 100);
            const y = clamp(((cy - rect.top) / rect.height) * 100, 0, 100);
            previewState.origin = `${x}% ${y}%`;
        }
    } else if (next === 1) {
        previewState.origin = "50% 50%";
    }

    previewState.zoom = next;
    applyPreviewZoom();
}

function preloadPreviewImage(index) {
    const item = previewState.items[index];
    if (!item) return;
    const img = new Image();
    img.decoding = "async";
    img.src = item.url;
}

function showPreviewAt(index) {
    const total = previewState.items.length;
    if (!total) return;

    const next = clamp(index, 0, total - 1);
    const item = previewState.items[next];

    previewState.index = next;
    previewState.token += 1;
    const token = previewState.token;

    const img = $("previewImg");
    const stage = $("previewStage");
    if (!img || !stage) return;

    resetPreviewZoom();
    stage.classList.add("loading");
    $("previewError")?.classList.remove("show");

    img.onload = () => { if (token === previewState.token) stage.classList.remove("loading"); };
    img.onerror = () => {
        if (token !== previewState.token) return;
        stage.classList.remove("loading");
        $("previewError")?.classList.add("show");
    };

    img.src = item.url;
    img.alt = item.alt || "图片预览";

    if (img.complete && img.naturalWidth > 0) stage.classList.remove("loading");

    setText("previewCaption", item.caption);
    setText("previewCounter", total > 1 ? `${next + 1} / ${total}` : "");

    const prev = $("previewPrev");
    if (prev) prev.disabled = next <= 0;
    const nextBtn = $("previewNext");
    if (nextBtn) nextBtn.disabled = next >= total - 1;

    preloadPreviewImage(next - 1);
    preloadPreviewImage(next + 1);
}

function previewStep(delta) {
    if (previewState.items.length <= 1) return;
    const next = previewState.index + delta;
    if (next < 0 || next >= previewState.items.length) return;
    showPreviewAt(next);
}

function openPreview(url, caption) {
    const items = collectPreviewItems();
    let index = items.findIndex((item) => item.url === url);

    if (index === -1) {
        items.push({ url, caption, alt: caption || "图片预览" });
        index = items.length - 1;
    }

    previewState.items = items;
    showPreviewAt(index);
    showModal($("previewModal"), true);
}

function closePreview() {
    showModal($("previewModal"), false);

    previewState.items = [];
    previewState.index = 0;
    previewState.token += 1;

    const img = $("previewImg");
    if (img) {
        img.onload = null;
        img.onerror = null;
        img.removeAttribute("src");
    }

    $("previewStage")?.classList.remove("loading");
    $("previewError")?.classList.remove("show");
    setText("previewCaption", "");
    setText("previewCounter", "");
    resetPreviewZoom();
}

/* ===== 选择 ===== */

function selectAllVisible() {
    document.querySelectorAll("[data-select-id]").forEach((input) => {
        input.checked = true;
        state.selected.add(Number(input.dataset.selectId));
    });
    updateSelectionUI();
}

function clearSelection() {
    state.selected.clear();
    document.querySelectorAll("[data-select-id]").forEach((input) => { input.checked = false; });
    updateSelectionUI();
}

function clearFiles() {
    if (state.uploading) return;
    state.files = [];
    const input = $("fileInput");
    if (input) input.value = "";
    setText("uploadResult", "");
    renderFileList();
}

/* ===== 密钥弹窗 ===== */

function openTokenModal() {
    showModal($("tokenModal"), true);
    setTimeout(() => {
        const input = $("tokenInput");
        if (input) { input.focus(); input.select(); }
    }, 60);
}

function closeTokenModal() {
    showModal($("tokenModal"), false);
    const result = $("tokenResult");
    if (result) {
        result.textContent = "";
        result.classList.remove("is-error", "is-success");
    }
}

function setTokenResult(message, type = "") {
    const result = $("tokenResult");
    if (!result) return;
    result.textContent = message;
    result.classList.remove("is-error", "is-success");
    if (type) result.classList.add(`is-${type}`);
}

async function handleSaveToken() {
    const token = apiToken();

    if (!token) {
        saveToken();
        await detectRole();
        setTokenResult("已切换为访客身份", "success");
        setTimeout(closeTokenModal, 600);
        return;
    }

    const res = await probeRole(token);

    if (res.role === "admin" || res.role === "guest") {
        saveToken();
        currentRole = res.role;
        applyRoleUI();
        setTokenResult(res.role === "admin" ? "✓ 管理员密钥已保存" : "✓ 访客密钥已保存", "success");
        setTimeout(closeTokenModal, 600);
        return;
    }

    if (res.role === "blocked") {
        setTokenResult("密钥正确，但当前 IP / 来源不在白名单。请在白名单内访问，或生成临时密钥。", "error");
        return;
    }

    setTokenResult("无效的密钥", "error");
}

async function handleClearToken() {
    const input = $("tokenInput");
    if (input) input.value = "";
    saveToken();
    await detectRole();
    setTokenResult("已切换为访客身份", "success");
}

async function handleGenerateTempToken() {
    const btn = $("generateTempToken");
    if (!btn) return;

    btn.disabled = true;
    try {
        const resp = await fetch("/api/temp-token", { method: "POST" });
        const data = await resp.json().catch(() => ({}));
        showToast(resp.ok ? "临时密钥已生成，请查看服务器日志" : data.message || "生成失败", resp.ok ? "" : "error");
    } catch (err) {
        showToast("生成失败：" + err.message, "error");
    } finally {
        btn.disabled = false;
    }
}

/* ===== 事件 ===== */

document.addEventListener("click", async (event) => {
    const modeBtn = event.target.closest(".mode-btn");
    if (modeBtn) { setMode(modeBtn.dataset.mode); return; }

    const previewTarget = event.target.closest("[data-preview]");
    if (previewTarget) {
        openPreview(previewTarget.dataset.preview, previewTarget.dataset.caption || "");
        return;
    }

    const downloadBtn = event.target.closest("[data-download-id]");
    if (downloadBtn) {
        const item = itemById.get(Number(downloadBtn.dataset.downloadId));
        if (item) await downloadSingle(item);
        else showToast("找不到图片，请刷新列表", "error");
        return;
    }

    const removeButton = event.target.closest("[data-remove-file]");
    if (removeButton) { removeFile(removeButton.dataset.removeFile); return; }

    const cancelFile = event.target.closest("[data-cancel-file]");
    if (cancelFile) {
        state.files.find((entry) => entry.key === cancelFile.dataset.cancelFile)?.xhr?.abort();
        return;
    }

    const retryFile = event.target.closest("[data-retry-file]");
    if (retryFile) {
        if (state.uploading) return;
        const item = state.files.find((entry) => entry.key === retryFile.dataset.retryFile);
        if (!item) return;
        item.status = "pending";
        item.message = "等待上传";
        item.progress = 0;
        renderFileList();
        await startUpload();
        return;
    }

    const checkbox = event.target.closest("[data-select-id]");
    if (checkbox) {
        const id = Number(checkbox.dataset.selectId);
        if (checkbox.checked) state.selected.add(id);
        else state.selected.delete(id);
        updateSelectionUI();
        return;
    }

    const tab = event.target.closest(".tab");
    if (tab && !tab.classList.contains("active")) {
        tabPages[state.type] = state.page;

        document.querySelectorAll(".tab").forEach((el) => el.classList.toggle("active", el === tab));

        state.type = tab.dataset.type;
        state.page = tabPages[state.type] || 1;
        state.total = 0;
        state.items = [];
        state.selected.clear();
        updateSelectionUI();

        await loadGallery();
    }
});

on("themeToggle", "click", toggleTheme);
on("openToken", "click", openTokenModal);

on("openUpload", "click", () => {
    if (currentRole !== "admin") {
        showToast("请先设置管理员密钥", "error");
        openTokenModal();
        return;
    }
    showModal($("uploadModal"), true);
});

on("openTokenFromUpload", "click", () => {
    showModal($("uploadModal"), false);
    openTokenModal();
});

on("closeToken", "click", closeTokenModal);
on("saveToken", "click", handleSaveToken);
on("clearToken", "click", handleClearToken);

on("rememberKey", "change", () => {
    if (currentRole === "admin" || currentRole === "guest") saveToken();
});

on("tokenInput", "keydown", (event) => {
    if (event.key !== "Enter") return;
    event.preventDefault();
    $("saveToken")?.click();
});

on("copyGuestToken", "click", async (event) => {
    const token = await ensureGuestTokenLoaded();
    if (!token) { showToast("访客密钥加载失败", "error"); return; }
    await copyText(token, event.currentTarget);
});

on("generateTempToken", "click", handleGenerateTempToken);
on("closeUpload", "click", () => { if (!state.uploading) showModal($("uploadModal"), false); });
on("startUpload", "click", startUpload);
on("selectAll", "click", selectAllVisible);
on("clearSelection", "click", clearSelection);
on("clearFiles", "click", clearFiles);

on("fileInput", "change", (event) => {
    addFiles(event.target.files);
    event.target.value = "";
});

on("dropZone", "click", () => { if (!state.uploading) $("fileInput")?.click(); });

on("dropZone", "dragover", (event) => {
    event.preventDefault();
    $("dropZone")?.classList.add("dragging");
});

on("dropZone", "dragleave", () => $("dropZone")?.classList.remove("dragging"));

on("dropZone", "drop", (event) => {
    event.preventDefault();
    $("dropZone")?.classList.remove("dragging");
    if (!state.uploading) addFiles(event.dataTransfer.files);
});

on("batchDelete", "click", batchDelete);
on("batchDownload", "click", batchDownload);

on("refreshBtn", "click", async () => {
    clearGalleryCache();
    await Promise.all([loadGallery({ force: true }), refreshTabCounts()]);
    showToast("已刷新");
});

on("copyApi", "click", async (event) => {
    await copyText(apiAutoURL(), event.currentTarget);
});

on("downloadOriginal", "change", (event) => {
    state.downloadOriginal = event.target.checked;
    saveDownloadOriginal();
});

on("searchInput", "input", debounce((event) => {
    state.query = event.target.value.trim();
    renderGallery();
}, 140));

on("pageSize", "change", async (event) => {
    const size = Number(event.target.value) || 24;
    state.size = size;
    state.page = 1;
    localStorage.setItem(PAGE_SIZE_KEY, String(size));
    await loadGallery();
});

on("closePreview", "click", closePreview);
on("previewPrev", "click", (event) => { event.stopPropagation(); previewStep(-1); });
on("previewNext", "click", (event) => { event.stopPropagation(); previewStep(1); });

on("previewImg", "click", (event) => {
    if (previewState.zoom > 1) { setPreviewZoom(1); return; }
    setPreviewZoom(2.2, event.clientX, event.clientY);
});

on("previewStage", "wheel", (event) => {
    if (previewState.zoom <= 1) return;
    event.preventDefault();
    setPreviewZoom(previewState.zoom + (event.deltaY < 0 ? 0.25 : -0.25), event.clientX, event.clientY);
}, { passive: false });

let previewTouch = null;

on("previewStage", "touchstart", (event) => {
    previewTouch = event.touches.length === 1
        ? { x: event.touches[0].clientX, y: event.touches[0].clientY }
        : null;
}, { passive: true });

on("previewStage", "touchend", (event) => {
    if (!previewTouch) return;

    const touch = event.changedTouches[0];
    const start = previewTouch;
    previewTouch = null;

    if (!touch || previewState.zoom > 1) return;

    const dx = touch.clientX - start.x;
    const dy = touch.clientY - start.y;
    if (Math.abs(dx) < 50 || Math.abs(dx) < Math.abs(dy)) return;

    previewStep(dx < 0 ? 1 : -1);
}, { passive: true });

on("prevPage", "click", async () => {
    if (state.page <= 1) return;
    state.page -= 1;
    tabPages[state.type] = state.page;
    await loadGallery();
});

on("nextPage", "click", async () => {
    const maxPage = Math.max(1, Math.ceil(state.total / state.size));
    if (state.page >= maxPage) return;
    state.page += 1;
    tabPages[state.type] = state.page;
    await loadGallery();
});

$("tokenModal")?.querySelector(".modal-backdrop")?.addEventListener("click", closeTokenModal);
$("uploadModal")?.querySelector(".modal-backdrop")?.addEventListener("click", () => {
    if (!state.uploading) showModal($("uploadModal"), false);
});
$("previewModal")?.querySelector(".modal-backdrop")?.addEventListener("click", closePreview);

window.addEventListener("resize", debounce(updateAPIBox, 150));

const updateScrollState = () => document.body.classList.toggle("is-scrolled", window.scrollY > 8);
window.addEventListener("scroll", updateScrollState, { passive: true });

window.addEventListener("keydown", (event) => {
    if (isModalOpen("previewModal")) {
        if (event.key === "Escape") closePreview();
        else if (event.key === "ArrowLeft") { event.preventDefault(); previewStep(-1); }
        else if (event.key === "ArrowRight") { event.preventDefault(); previewStep(1); }
        return;
    }

    if (event.key === "Escape") {
        if (isModalOpen("tokenModal")) closeTokenModal();
        else if (isModalOpen("uploadModal") && !state.uploading) showModal($("uploadModal"), false);
        return;
    }

    const tag = event.target?.tagName;
    if (event.key === "/" && tag !== "INPUT" && tag !== "SELECT" && tag !== "TEXTAREA") {
        event.preventDefault();
        $("searchInput")?.focus();
    }
});

/* ===== 启动 ===== */

applyTheme();
updateAPIBox();
restoreToken();
restoreMode();
restoreDownloadOriginal();

const savedSize = Number(localStorage.getItem(PAGE_SIZE_KEY));
if (savedSize && $("pageSize")?.querySelector(`option[value="${savedSize}"]`)) {
    state.size = savedSize;
    $("pageSize").value = String(savedSize);
}

renderFileList();
renderSkeletons(8);
updateSelectionUI();
updateScrollState();

loadGallery();
refreshTabCounts();
void detectRole();
void loadPublicConfig();
