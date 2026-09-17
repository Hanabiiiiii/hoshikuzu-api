const TOKEN_KEY = "random-image-api.admin-token";
const REMEMBER_KEY = "random-image-api.remember-token";
const MODE_KEY = "random-image-api.mode";
const DOWNLOAD_ORIGINAL_KEY = "random-image-api.download-original";
const MAX_CLIENT_FILE_SIZE = 50 * 1024 * 1024;

const ALLOWED_EXTENSIONS = [".jpg", ".jpeg", ".png", ".webp"];

const state = {
    type: "desktop",
    page: 1,
    size: 15,
    total: 0,
    selected: new Set(),
    files: [],
    uploading: false,
    mode: "thumb",
    downloadOriginal: false,
};

let currentRole = "guest";

const publicConfig = {
    guestToken: "",
    whitelistActive: false,
    guestApiOriginalLimit: 0,
    guestApiThumbLimit: 0,
    guestDownloadOriginalLimit: 0,
    guestDownloadThumbLimit: 0,
};

const tabPages = { desktop: 1, mobile: 1 };
const GALLERY_CACHE_LIMIT = 40;
const galleryCache = new Map();

const $ = (id) => document.getElementById(id);

function on(id, event, handler, options) {
    const el = $(id);
    if (!el) {
        console.warn(`[app] 缺少元素 #${id}，跳过 ${event} 绑定`);
        return null;
    }
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

/* ===== Modal ===== */

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

    if (
        document.activeElement &&
        element.contains(document.activeElement) &&
        typeof document.activeElement.blur === "function"
    ) {
        document.activeElement.blur();
    }

    element.classList.add("hidden");
    element.setAttribute("inert", "");
    element.setAttribute("aria-hidden", "true");
    document.body.classList.remove("modal-open");

    if (
        lastFocusedElement &&
        document.contains(lastFocusedElement) &&
        typeof lastFocusedElement.focus === "function"
    ) {
        try { lastFocusedElement.focus({ preventScroll: true }); } catch {}
    }

    lastFocusedElement = null;
}

/* ===== 公开配置 ===== */

async function loadPublicConfig() {
    try {
        const resp = await fetch("/api/public-config", { cache: "no-store" });
        if (!resp.ok) {
            console.error(`[app] /api/public-config 返回 HTTP ${resp.status}`);
            return;
        }

        const data = await resp.json();
        const p = data?.data || {};

        publicConfig.guestToken = String(p.guest_token || "");
        publicConfig.whitelistActive = Boolean(p.whitelist_active);
        publicConfig.guestApiOriginalLimit = Number(p.guest_api_original_limit) || 0;
        publicConfig.guestApiThumbLimit = Number(p.guest_api_thumb_limit) || 0;
        publicConfig.guestDownloadOriginalLimit = Number(p.guest_download_original_limit) || 0;
        publicConfig.guestDownloadThumbLimit = Number(p.guest_download_thumb_limit) || 0;

        console.log("[app] public-config 已加载", publicConfig);
        renderPublicConfig();
    } catch (err) {
        console.error("[app] 加载 public-config 失败", err);
    }
}

function renderPublicConfig() {
    const whitelistStrip = $("whitelistStrip");
    if (whitelistStrip) {
        whitelistStrip.hidden = !publicConfig.whitelistActive;
    }

    const strip = $("visitorStrip");
    if (strip && publicConfig.guestToken) {
        strip.hidden = false;

        if ($("guestTokenDisplay")) $("guestTokenDisplay").textContent = publicConfig.guestToken;
        if ($("guestApiThumb")) $("guestApiThumb").textContent = publicConfig.guestApiThumbLimit > 0 ? publicConfig.guestApiThumbLimit : "∞";
        if ($("guestApiOriginal")) $("guestApiOriginal").textContent = publicConfig.guestApiOriginalLimit > 0 ? publicConfig.guestApiOriginalLimit : "∞";
        if ($("guestDownloadThumb")) $("guestDownloadThumb").textContent = publicConfig.guestDownloadThumbLimit > 0 ? publicConfig.guestDownloadThumbLimit : "∞";
        if ($("guestDownloadOriginal")) $("guestDownloadOriginal").textContent = publicConfig.guestDownloadOriginalLimit > 0 ? publicConfig.guestDownloadOriginalLimit : "∞";
    }
}

async function ensureGuestTokenLoaded() {
    if (publicConfig.guestToken) return publicConfig.guestToken;
    await loadPublicConfig();
    return publicConfig.guestToken;
}

/* ===== Token ===== */

function apiToken() {
    const input = $("tokenInput");
    return input ? input.value.trim() : "";
}

function saveToken() {
    const rememberEl = $("rememberKey");
    const remember = rememberEl ? rememberEl.checked : false;
    const token = apiToken();

    if (remember && token) {
        localStorage.setItem(TOKEN_KEY, token);
        localStorage.setItem(REMEMBER_KEY, "1");
    } else {
        localStorage.removeItem(TOKEN_KEY);
        localStorage.removeItem(REMEMBER_KEY);
    }
}

function restoreToken() {
    const remember = localStorage.getItem(REMEMBER_KEY) === "1";
    const saved = localStorage.getItem(TOKEN_KEY);
    const rememberEl = $("rememberKey");
    const tokenEl = $("tokenInput");
    if (rememberEl) rememberEl.checked = remember;
    if (tokenEl) tokenEl.value = remember && saved ? saved : "";
}

/* ===== 角色识别 ===== */

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

        if (!resp.ok) {
            currentRole = "guest";
        } else {
            const data = await resp.json();
            currentRole = data?.data?.role === "admin" ? "admin" : "guest";
        }
    } catch {
        currentRole = "guest";
    }

    applyRoleUI();
}

async function probeRole(token) {
    if (!token) return { role: "empty" };

    await ensureGuestTokenLoaded();

    if (publicConfig.guestToken && token === publicConfig.guestToken) {
        return { role: "guest" };
    }

    try {
        const resp = await fetch("/api/whoami", {
            headers: { Authorization: `Bearer ${token}` },
            cache: "no-store",
        });
        if (!resp.ok) return { role: "invalid" };

        const data = await resp.json();
        const role = data?.data?.role;
        const reason = data?.data?.reason;

        if (role === "admin") return { role: "admin" };
        if (reason === "whitelist_blocked") return { role: "blocked" };
        return { role: "invalid" };
    } catch {
        return { role: "invalid" };
    }
}

function applyRoleUI() {
    const isAdmin = currentRole === "admin";

    document.body.classList.toggle("is-admin", isAdmin);
    document.body.classList.toggle("is-guest", !isAdmin);

    const roleLabel = $("currentRoleLabel");
    if (roleLabel) {
        roleLabel.textContent = isAdmin
            ? "管理员 · 点击修改密钥"
            : "访客 · 点击设置密钥";
    }

    applyModeUI();
}

/* ===== 加速 / 原图 模式 ===== */

function restoreMode() {
    const saved = localStorage.getItem(MODE_KEY);
    state.mode = saved === "original" ? "original" : "thumb";
    applyModeUI();
}

function saveMode() {
    localStorage.setItem(MODE_KEY, state.mode);
}

function applyModeUI() {
    document.querySelectorAll(".mode-btn").forEach((btn) => {
        btn.classList.toggle("active", btn.dataset.mode === state.mode);
    });

    const link = $("randomApiLink");
    if (link) link.href = apiAutoURL();

    updateAPIBox();
}

function setMode(mode) {
    if (mode !== "thumb" && mode !== "original") return;
    state.mode = mode;
    saveMode();
    applyModeUI();
}

/* ===== 下载原图开关 ===== */

function restoreDownloadOriginal() {
    const saved = localStorage.getItem(DOWNLOAD_ORIGINAL_KEY) === "1";
    state.downloadOriginal = saved;
    const el = $("downloadOriginal");
    if (el) el.checked = saved;
}

function saveDownloadOriginal() {
    localStorage.setItem(
        DOWNLOAD_ORIGINAL_KEY,
        state.downloadOriginal ? "1" : "0",
    );
}

/* ===== API URL ===== */

function apiAutoURL() {
    const width = Math.max(1, window.innerWidth);
    const modeQS = state.mode === "original" ? "&mode=original" : "";

    let tokenQS = "";
    if (state.mode === "original" && currentRole === "admin") {
        const token = apiToken();
        if (token) {
            tokenQS = `&token=${encodeURIComponent(token)}`;
        }
    }

    return `${location.origin}/api/random?type=auto&w=${width}${modeQS}${tokenQS}`;
}

function updateAPIBox() {
    const width = Math.max(1, window.innerWidth);

    const apiElement = $("apiUrl");
    if (apiElement) {
        const url = apiAutoURL();
        apiElement.textContent = url;
        apiElement.title = url;
    }

    const widthElement = $("currentWidth");
    if (widthElement) widthElement.textContent = `${width}px`;

    const typeElement = $("currentDeviceType");
    if (typeElement) typeElement.textContent = width <= 768 ? "Mobile" : "Desktop";
}

/* ===== Toast ===== */

function showToast(message) {
    const toast = $("toast");
    if (!toast) return;

    toast.textContent = message;
    toast.classList.add("show");

    clearTimeout(showToast.timer);
    showToast.timer = setTimeout(() => toast.classList.remove("show"), 2200);
}

/* ===== 复制 ===== */

async function copyText(text, sourceButton) {
    if (!text) {
        showToast("没有可复制的内容");
        return false;
    }

    const originalHTML = sourceButton ? sourceButton.innerHTML : "";
    const flash = (label) => {
        if (!sourceButton) return;
        sourceButton.innerHTML = label;
        window.setTimeout(() => { sourceButton.innerHTML = originalHTML; }, 1200);
    };

    if (navigator.clipboard && window.isSecureContext) {
        try {
            await navigator.clipboard.writeText(text);
            showToast("已复制到剪贴板 ✨");
            flash("✓ 已复制");
            return true;
        } catch (err) {
            console.warn("[app] navigator.clipboard 失败", err);
        }
    }

    try {
        const ta = document.createElement("textarea");
        ta.value = text;
        ta.setAttribute("readonly", "");
        ta.style.position = "fixed";
        ta.style.top = "0";
        ta.style.left = "0";
        ta.style.width = "1px";
        ta.style.height = "1px";
        ta.style.opacity = "0";
        document.body.appendChild(ta);
        ta.focus();
        ta.select();
        ta.setSelectionRange(0, ta.value.length);
        const ok = document.execCommand("copy");
        ta.remove();
        if (ok) {
            showToast("已复制到剪贴板 ✨");
            flash("✓ 已复制");
            return true;
        }
    } catch (err) {
        console.warn("[app] execCommand 失败", err);
    }

    try {
        window.prompt("复制下面的内容：", text);
        return true;
    } catch {
        showToast("复制失败，请手动复制");
        return false;
    }
}

/* ===== 图片卡片 ===== */

function pickDisplayURL(item) {
    return item.thumbnail_url || item.url;
}

/*
 * imageCard 渲染。
 *
 * has_thumbnail 为 false 表示此图较小（< 200KB），没有独立缩略图，
 * 列表展示和下载都直接用原文件。在 meta 行加一个"小图"标记提示用户。
 */
function imageCard(item) {
    const checked = state.selected.has(item.id) ? "checked" : "";
    const caption = `${item.original_name} · ${item.width}×${item.height} · ${formatBytes(item.size)}`;
    const displayUrl = pickDisplayURL(item);

    const smallTag = item.has_thumbnail === false
        ? ' <span class="small-tag" title="此图较小，未生成缩略图，下载即为原始文件">小图</span>'
        : '';

    return `
    <article class="image-card">
      <div class="thumb-wrap">
        <input class="check" type="checkbox" data-select-id="${item.id}" ${checked}
          aria-label="选择 ${escapeAttr(item.filename)}">
        <span class="badge">${item.type.toUpperCase()}</span>
        <img class="thumb" loading="lazy" decoding="async" fetchpriority="low"
          src="${escapeAttr(displayUrl)}" alt="${escapeAttr(item.original_name)}"
          data-preview="${escapeAttr(displayUrl)}" data-caption="${escapeAttr(caption)}">
      </div>
      <div class="image-body">
        <div class="filename" title="${escapeAttr(item.filename)}">${escapeHTML(item.filename)}</div>
        <div class="image-meta">${item.width} × ${item.height} · ${formatBytes(item.size)}${smallTag}</div>
        <div class="image-meta" title="${escapeAttr(item.original_name)}">原名：${escapeHTML(item.original_name)}</div>
        <div class="image-actions">
          <button class="btn" data-preview="${escapeAttr(displayUrl)}" data-caption="${escapeAttr(caption)}">预览</button>
          <button class="btn" data-download-id="${item.id}">下载</button>
        </div>
      </div>
    </article>`;
}

/* ===== 画廊 ===== */

function galleryCacheKey(type, page, size) {
    return `${type}:${page}:${size}`;
}
function clearGalleryCache() { galleryCache.clear(); }
function cacheGalleryResult(type, page, size, result) {
    const key = galleryCacheKey(type, page, size);
    if (galleryCache.size >= GALLERY_CACHE_LIMIT) {
        const oldest = galleryCache.keys().next().value;
        galleryCache.delete(oldest);
    }
    galleryCache.set(key, result);
}

async function fetchGalleryPage(type, page, size) {
    const params = new URLSearchParams({ type, page: String(page), size: String(size) });
    const response = await fetch(`/api/images?${params}`, { cache: "no-store" });
    const result = await response.json();
    if (!response.ok || !result.success) throw new Error(result.message || "加载失败");
    return result;
}

function applyGalleryResult(result) {
    state.total = Number(result.data.total);
    const maxPage = Math.max(1, Math.ceil(state.total / state.size));
    if (state.page > maxPage) state.page = maxPage;
    tabPages[state.type] = state.page;

    const visibleIds = new Set(result.data.items.map((i) => i.id));
    for (const id of state.selected) {
        if (!visibleIds.has(id)) state.selected.delete(id);
    }

    const stats = $("galleryStats");
    if (stats) stats.textContent = `${state.total} 张图片 · 第 ${state.page}/${maxPage} 页`;

    const pageInfo = $("pageInfo");
    if (pageInfo) pageInfo.textContent = `${state.page} / ${maxPage}`;

    const prev = $("prevPage");
    if (prev) prev.disabled = state.page <= 1;
    const next = $("nextPage");
    if (next) next.disabled = state.page >= maxPage;

    const gallery = $("gallery");
    if (gallery) {
        gallery.innerHTML = result.data.items.length
            ? result.data.items.map(imageCard).join("")
            : `<div class="empty">当前分类还没有图片。</div>`;
    }

    updateSelectionUI();
}

async function loadGallery({ force = false } = {}) {
    if (force) clearGalleryCache();

    const { type, page, size } = state;
    const key = galleryCacheKey(type, page, size);

    if (galleryCache.has(key)) {
        applyGalleryResult(galleryCache.get(key));
        schedulePrefetch();
        return;
    }

    const gallery = $("gallery");
    if (gallery) gallery.innerHTML = `<div class="empty">加载中...</div>`;

    try {
        const result = await fetchGalleryPage(type, page, size);
        cacheGalleryResult(type, page, size, result);
        applyGalleryResult(result);
        schedulePrefetch();
    } catch (error) {
        if (gallery) gallery.innerHTML = `<div class="empty">加载失败：${escapeHTML(error.message)}</div>`;
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
        const key = galleryCacheKey(type, target, size);
        if (galleryCache.has(key)) continue;
        fetchGalleryPage(type, target, size)
            .then((result) => cacheGalleryResult(type, target, size, result))
            .catch(() => {});
    }
}

function updateSelectionUI() {
    const count = state.selected.size;
    const info = $("selectionInfo");
    if (info) info.textContent = `已选 ${count}`;
    const del = $("batchDelete");
    if (del) del.disabled = count === 0;
    const dl = $("batchDownload");
    if (dl) dl.disabled = count === 0;
}

/* ===== 上传列表 ===== */

function createFileKey(file) {
    return [file.name, file.size, file.lastModified].join("|");
}

function addFiles(fileList) {
    const incoming = Array.from(fileList || []);
    for (const file of incoming) {
        const lowerName = file.name.toLowerCase();
        const ext = lowerName.includes(".")
            ? lowerName.slice(lowerName.lastIndexOf("."))
            : "";
        if (!ALLOWED_EXTENSIONS.includes(ext)) continue;
        if (file.size <= 0 || file.size > MAX_CLIENT_FILE_SIZE) continue;

        const key = createFileKey(file);
        if (state.files.some((item) => item.key === key)) continue;

        state.files.push({ key, file, status: "pending", message: "等待上传", progress: 0, xhr: null });
    }
    renderFileList();
}

function removeFile(key) {
    const index = state.files.findIndex((item) => item.key === key);
    if (index === -1) return;
    const item = state.files[index];
    if (item.status === "uploading") { item.xhr?.abort(); return; }
    state.files.splice(index, 1);
    renderFileList();
}

function renderFileList() {
    const fileList = $("fileList");
    const fileSummary = $("fileSummary");
    const startUpload = $("startUpload");

    if (!state.files.length) {
        if (fileList) fileList.innerHTML = "";
        if (fileSummary) fileSummary.textContent = "未选择文件";
        if (startUpload) startUpload.disabled = true;
        return;
    }

    const totalSize = state.files.reduce((sum, item) => sum + item.file.size, 0);
    const uploadingCount = state.files.filter((i) => i.status === "uploading").length;
    const pendingCount = state.files.filter((i) => i.status === "pending").length;
    const failedCount = state.files.filter((i) => i.status === "failed" || i.status === "cancelled").length;

    let summaryText = `已选择 ${state.files.length} 张 · ${formatBytes(totalSize)}`;
    if (failedCount > 0) summaryText += ` · ${failedCount} 张需重试`;

    if (fileSummary) fileSummary.textContent = summaryText;
    if (startUpload) startUpload.disabled = uploadingCount > 0 || pendingCount === 0;
    if (!fileList) return;

    fileList.innerHTML = state.files.map((item) => {
        const statusText = item.message || "";
        let action = "";
        if (item.status === "pending") {
            action = `<button class="remove-file" data-remove-file="${escapeAttr(item.key)}" title="移除">×</button>`;
        }
        if (item.status === "uploading") {
            action = `<button class="cancel-file" data-cancel-file="${escapeAttr(item.key)}" title="取消上传">取消</button>`;
        }
        if (item.status === "success") {
            action = `<span class="file-success">✓</span>`;
        }
        if (item.status === "failed" || item.status === "cancelled") {
            action = `<button class="retry-file" data-retry-file="${escapeAttr(item.key)}" title="重新上传">↻</button>
                      <button class="remove-file" data-remove-file="${escapeAttr(item.key)}" title="移除">×</button>`;
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
            <div class="file-message">${escapeHTML(statusText)}</div>
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
    item.message = "上传中...";
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
            if (row) {
                const bar = row.querySelector(".file-progress-bar");
                if (bar) bar.style.width = `${percent}%`;
                const msg = row.querySelector(".file-message");
                if (msg) msg.textContent = item.message;
            }
        };

        xhr.onload = () => {
            let result = {};
            try { result = JSON.parse(xhr.responseText || "{}"); } catch {}

            if (xhr.status === 401) { item.status = "failed"; item.message = "密钥错误"; item.progress = 0; finish(false); return; }
            if (xhr.status === 403) { item.status = "failed"; item.message = "无权限"; item.progress = 0; finish(false); return; }
            if (xhr.status < 200 || xhr.status >= 300 || !result.success) {
                item.status = "failed";
                item.message = result.message || `HTTP ${xhr.status}`;
                item.progress = 0;
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

    if (currentRole !== "admin") {
        const r = $("uploadResult");
        if (r) r.textContent = "当前身份不是管理员，请先设置密钥。";
        showToast("请先设置管理员密钥");
        return;
    }

    const token = apiToken();
    if (!token) {
        const r = $("uploadResult");
        if (r) r.textContent = "请输入密钥。";
        return;
    }

    saveToken();

    const pending = state.files.filter((i) => i.status === "pending");
    if (!pending.length) {
        const r = $("uploadResult");
        if (r) r.textContent = "没有待上传的图片。";
        return;
    }

    state.uploading = true;
    const cf = $("clearFiles"); const fi = $("fileInput"); const su = $("startUpload");
    if (cf) cf.disabled = true;
    if (fi) fi.disabled = true;
    if (su) su.disabled = true;

    let successCount = 0, failedCount = 0, cancelledCount = 0;

    for (const item of pending) {
        if (item.status !== "pending") continue;
        const success = await uploadOne(item, token);
        if (success) successCount++;
        else if (item.status === "cancelled") cancelledCount++;
        else failedCount++;

        const r = $("uploadResult");
        if (r) r.textContent = `处理中：${successCount} 成功 · ${failedCount} 失败 · ${cancelledCount} 取消`;
    }

    state.uploading = false;
    if (cf) cf.disabled = false;
    if (fi) fi.disabled = false;
    renderFileList();

    const r = $("uploadResult");
    if (r) r.textContent = `处理完成：${successCount} 成功 · ${failedCount} 失败 · ${cancelledCount} 取消`;

    if (successCount > 0) {
        clearGalleryCache();
        await loadGallery({ force: true });
    }
}

/* ===== 批量删除 ===== */

async function batchDelete() {
    const ids = Array.from(state.selected);
    if (!ids.length) return;

    if (currentRole !== "admin") {
        alert("访客没有删除权限。");
        return;
    }

    const token = apiToken();
    if (!token) { alert("请输入密钥。"); return; }

    if (!confirm(`确定删除选中的 ${ids.length} 张图片吗？`)) return;

    saveToken();

    try {
        const response = await fetch("/api/images/delete/batch", {
            method: "POST",
            headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
            body: JSON.stringify({ ids }),
        });

        const result = await response.json().catch(() => ({}));

        if (response.status === 401) { alert("密钥错误。"); return; }
        if (response.status === 403) { alert("无权限。"); return; }
        if (!response.ok && response.status !== 207) {
            alert(result.message || "批量删除失败");
            return;
        }

        const deleted = result.data?.deleted ?? 0;
        const failed = result.data?.failed ?? [];
        state.selected.clear();

        let message = `已删除 ${deleted} 张图片。`;
        if (failed.length) message += `\n失败：\n${failed.join("\n")}`;
        alert(message);

        clearGalleryCache();
        await loadGallery({ force: true });
    } catch (error) {
        alert(`删除失败：${error.message}`);
    }
}

/* ===== 批量下载 ===== */

async function batchDownload() {
    const ids = Array.from(state.selected);
    if (!ids.length) return;

    const token = apiToken();
    if (!token) { alert("请先设置密钥。"); return; }

    saveToken();

    const body = { ids, mode: state.downloadOriginal ? "original" : "thumb" };

    try {
        const response = await fetch("/api/images/download/batch", {
            method: "POST",
            headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
            body: JSON.stringify(body),
        });

        if (response.status === 401) { alert("密钥错误。"); return; }
        if (response.status === 429) {
            const result = await response.json().catch(() => ({}));
            alert(result.message || "今日配额已用完。");
            return;
        }
        if (!response.ok) {
            const result = await response.json().catch(() => ({}));
            alert(result.message || "批量下载失败");
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
    } catch (error) {
        alert(`下载失败：${error.message}`);
    }
}

/* ===== 单张下载 ===== */

async function downloadSingle(item) {
    if (!item) return;

    const token = apiToken();
    if (!token) {
        alert("请先设置密钥。");
        return;
    }

    const qs = state.downloadOriginal ? "?mode=original" : "";
    const url = item.download_url + qs;

    let response;
    try {
        response = await fetch(url, {
            headers: { Authorization: `Bearer ${token}` },
        });
    } catch (err) {
        alert(`下载失败：${err.message}`);
        return;
    }

    if (response.status === 401) { alert("密钥错误。"); return; }
    if (response.status === 403) { alert("无权限。"); return; }
    if (response.status === 429) {
        const data = await response.json().catch(() => ({}));
        alert(data.message || "今日配额已用完。");
        return;
    }
    if (!response.ok) {
        const data = await response.json().catch(() => ({}));
        alert(data.message || `下载失败（HTTP ${response.status}）`);
        return;
    }

    const disposition = response.headers.get("Content-Disposition") || "";
    const match = disposition.match(/filename="?([^";]+)"?/);
    const filename = match
        ? match[1]
        : (item.original_name || item.filename || "image");

    const blob = await response.blob();
    const blobURL = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = blobURL;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(blobURL), 1000);
}

/* ===== 图片预览 ===== */

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
            const x = ((cx - rect.left) / rect.width) * 100;
            const y = ((cy - rect.top) / rect.height) * 100;
            previewState.origin = `${clamp(x, 0, 100)}% ${clamp(y, 0, 100)}%`;
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

    const caption = $("previewCaption");
    if (caption) caption.textContent = item.caption;

    const counter = $("previewCounter");
    if (counter) counter.textContent = total > 1 ? `${next + 1} / ${total}` : "";

    const prev = $("previewPrev");
    if (prev) prev.disabled = next <= 0;
    const nextBtn = $("previewNext");
    if (nextBtn) nextBtn.disabled = next >= total - 1;

    preloadPreviewImage(next - 1);
    preloadPreviewImage(next + 1);
}

function previewStep(delta) {
    const total = previewState.items.length;
    if (total <= 1) return;
    const next = previewState.index + delta;
    if (next < 0 || next >= total) return;
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
    const c = $("previewCaption"); if (c) c.textContent = "";
    const pc = $("previewCounter"); if (pc) pc.textContent = "";
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
    const fi = $("fileInput"); if (fi) fi.value = "";
    const r = $("uploadResult"); if (r) r.textContent = "";
    renderFileList();
}

/* ===== 密钥弹窗 ===== */

function openTokenModal() {
    showModal($("tokenModal"), true);
    window.setTimeout(() => {
        const input = $("tokenInput");
        if (input) { input.focus(); input.select(); }
    }, 60);
}

function closeTokenModal() {
    showModal($("tokenModal"), false);
    const r = $("tokenResult");
    if (r) { r.textContent = ""; r.classList.remove("is-error", "is-success"); }
}

function setTokenResult(message, type = "") {
    const r = $("tokenResult");
    if (!r) return;
    r.textContent = message;
    r.classList.remove("is-error", "is-success");
    if (type === "error") r.classList.add("is-error");
    if (type === "success") r.classList.add("is-success");
}

async function handleSaveToken() {
    const input = $("tokenInput");
    const token = input ? input.value.trim() : "";

    if (!token) {
        saveToken();
        await detectRole();
        setTokenResult("已切换为访客身份", "success");
        window.setTimeout(() => closeTokenModal(), 600);
        return;
    }

    const res = await probeRole(token);

    if (res.role === "admin") {
        saveToken();
        currentRole = "admin";
        applyRoleUI();
        setTokenResult("✓ 管理员密钥已保存", "success");
        window.setTimeout(() => closeTokenModal(), 600);
        return;
    }

    if (res.role === "guest") {
        saveToken();
        currentRole = "guest";
        applyRoleUI();
        setTokenResult("✓ 访客密钥已保存", "success");
        window.setTimeout(() => closeTokenModal(), 600);
        return;
    }

    if (res.role === "blocked") {
        setTokenResult(
            "密钥正确，但当前 IP / 来源不在白名单。请从白名单访问，或点击生成临时密钥。",
            "error",
        );
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

/* ===== 临时密钥生成 ===== */

async function handleGenerateTempToken() {
    const btn = $("generateTempToken");
    if (!btn) return;

    const originalText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "生成中...";

    try {
        const resp = await fetch("/api/temp-token", { method: "POST" });
        const data = await resp.json().catch(() => ({}));

        if (!resp.ok) {
            showToast(data.message || "生成失败");
            return;
        }

        showToast("临时密钥已生成，请查看服务器日志");
    } catch (err) {
        showToast("生成失败：" + err.message);
    } finally {
        btn.disabled = false;
        btn.textContent = originalText;
    }
}

/* ===== 事件绑定 ===== */

document.addEventListener("click", async (event) => {
    const modeBtn = event.target.closest(".mode-btn");
    if (modeBtn) { setMode(modeBtn.dataset.mode); return; }

    const preview = event.target.closest("[data-preview]");
    if (preview && !event.target.closest("input")) {
        openPreview(preview.dataset.preview, preview.dataset.caption || "");
        return;
    }

    const downloadBtn = event.target.closest("[data-download-id]");
    if (downloadBtn) {
        const id = Number(downloadBtn.dataset.downloadId);
        const key = galleryCacheKey(state.type, state.page, state.size);
        const cached = galleryCache.get(key);
        const item = cached?.data?.items?.find((x) => x.id === id);
        if (!item) {
            alert("找不到图片，请刷新列表。");
            return;
        }
        await downloadSingle(item);
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

    const removeButton = event.target.closest("[data-remove-file]");
    if (removeButton) { removeFile(removeButton.dataset.removeFile); return; }

    const cancelFile = event.target.closest("[data-cancel-file]");
    if (cancelFile) {
        const item = state.files.find((entry) => entry.key === cancelFile.dataset.cancelFile);
        item?.xhr?.abort();
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

    const tab = event.target.closest(".tab");
    if (tab) {
        tabPages[state.type] = state.page;
        document.querySelectorAll(".tab").forEach((i) => i.classList.remove("active"));
        tab.classList.add("active");
        state.type = tab.dataset.type;
        state.page = tabPages[state.type] || 1;
        state.total = 0;
        state.selected.clear();

        const badge = $("currentTypeBadge");
        if (badge) {
            badge.textContent = state.type === "mobile" ? "Mobile" : "Desktop";
            badge.classList.remove("desktop", "mobile");
            badge.classList.add(state.type);
        }
        await loadGallery();
    }
});

on("openToken", "click", openTokenModal);

on("openUpload", "click", () => {
    if (currentRole !== "admin") {
        showToast("请先设置管理员密钥");
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
    const btn = event.currentTarget;
    const token = await ensureGuestTokenLoaded();
    if (!token) { showToast("访客密钥加载失败"); return; }
    await copyText(token, btn);
});

on("generateTempToken", "click", handleGenerateTempToken);

on("closeUpload", "click", () => {
    if (!state.uploading) showModal($("uploadModal"), false);
});

on("startUpload", "click", startUpload);
on("selectAll", "click", selectAllVisible);
on("clearSelection", "click", clearSelection);
on("clearFiles", "click", clearFiles);

on("fileInput", "change", () => {
    const input = $("fileInput");
    if (!input) return;
    addFiles(input.files);
    input.value = "";
});

on("dropZone", "click", () => {
    if (!state.uploading) $("fileInput")?.click();
});

on("dropZone", "dragover", (event) => {
    event.preventDefault();
    $("dropZone")?.classList.add("dragging");
});

on("dropZone", "dragleave", () => {
    $("dropZone")?.classList.remove("dragging");
});

on("dropZone", "drop", (event) => {
    event.preventDefault();
    $("dropZone")?.classList.remove("dragging");
    if (!state.uploading) addFiles(event.dataTransfer.files);
});

on("batchDelete", "click", batchDelete);
on("batchDownload", "click", batchDownload);

on("refreshBtn", "click", () => {
    clearGalleryCache();
    loadGallery({ force: true });
});

on("copyApi", "click", async (event) => {
    await copyText(apiAutoURL(), event.currentTarget);
});

on("downloadOriginal", "change", (event) => {
    state.downloadOriginal = event.target.checked;
    saveDownloadOriginal();
    const key = galleryCacheKey(state.type, state.page, state.size);
    const cached = galleryCache.get(key);
    if (cached) applyGalleryResult(cached);
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
    if (event.touches.length !== 1) { previewTouch = null; return; }
    previewTouch = { x: event.touches[0].clientX, y: event.touches[0].clientY };
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

window.addEventListener("resize", updateAPIBox);

window.addEventListener("keydown", (event) => {
    const previewOpen = !$("previewModal")?.classList.contains("hidden");
    if (previewOpen) {
        if (event.key === "Escape") closePreview();
        else if (event.key === "ArrowLeft") { event.preventDefault(); previewStep(-1); }
        else if (event.key === "ArrowRight") { event.preventDefault(); previewStep(1); }
        return;
    }

    const tokenOpen = !$("tokenModal")?.classList.contains("hidden");
    if (event.key === "Escape" && tokenOpen) { closeTokenModal(); return; }

    const uploadOpen = !$("uploadModal")?.classList.contains("hidden");
    if (event.key === "Escape" && uploadOpen && !state.uploading) {
        showModal($("uploadModal"), false);
    }
});

/* 启动 */
updateAPIBox();
restoreToken();
restoreMode();
restoreDownloadOriginal();
renderFileList();
loadGallery();
void detectRole();
void loadPublicConfig();