const state = {
  accessToken: localStorage.getItem('maas_access_token') || '',
  csrfToken: localStorage.getItem('maas_csrf_token') || ''
};

const $ = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (state.accessToken) headers.Authorization = `Bearer ${state.accessToken}`;
  if (state.csrfToken && options.method && options.method !== 'GET') headers['X-CSRF-Token'] = state.csrfToken;
  if (options.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
  const response = await fetch(path, { ...options, headers });
  if (response.status === 401) { logout(); throw new Error('登录已过期'); }
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(payload.error || '请求失败');
  return payload;
}

function showApp() { $('login-view').classList.add('hidden'); $('app-view').classList.remove('hidden'); }
function showLogin(message = '') { $('app-view').classList.add('hidden'); $('login-view').classList.remove('hidden'); $('login-error').textContent = message; }
function logout() { state.accessToken = ''; state.csrfToken = ''; localStorage.removeItem('maas_access_token'); localStorage.removeItem('maas_csrf_token'); showLogin(); }
function formatDate(value) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '--'; }

async function login(event) {
  event.preventDefault(); $('login-error').textContent = '';
  try {
    const result = await api('/api/auth/login', { method: 'POST', body: JSON.stringify({ username: $('username').value, password: $('password').value }) });
    state.accessToken = result.access_token; state.csrfToken = result.csrf_token;
    localStorage.setItem('maas_access_token', state.accessToken); localStorage.setItem('maas_csrf_token', state.csrfToken);
    showApp(); await refreshDashboard();
  } catch (error) { $('login-error').textContent = error.message; }
}

async function refreshDashboard() {
  try {
    const [me, summary, tenants, audit] = await Promise.all([api('/api/auth/me'), api('/api/admin/summary'), api('/api/admin/tenants'), api('/api/admin/audit')]);
    $('identity').textContent = me.principal.id;
    $('stat-tenants').textContent = summary.tenants; $('stat-audits').textContent = summary.audit_events; $('stat-usage').textContent = summary.usage_events; $('stat-redis').textContent = summary.redis ? '在线' : '未配置';
    $('api-status').textContent = 'API 在线'; $('api-status').classList.add('online');
    $('tenant-rows').innerHTML = tenants.length ? tenants.map((tenant) => `<tr><td><code>${escapeHTML(String(tenant.ID))}</code></td><td>${escapeHTML(tenant.Name || tenant.Slug)}</td><td><span class="status-tag">${escapeHTML(String(tenant.Status))}</span></td><td>${formatDate(tenant.CreatedAt)}</td></tr>`).join('') : '<tr><td colspan="4" class="empty">暂无租户</td></tr>';
    $('audit-list').innerHTML = audit.length ? audit.slice(0, 8).map((entry) => `<div class="audit-item"><span class="audit-dot"></span><div><b>${escapeHTML(entry.Action)}</b><span>${escapeHTML(entry.ResourceType)} / ${escapeHTML(entry.ResourceID)}</span></div><time>${formatDate(entry.CreatedAt)}</time></div>`).join('') : '<p class="empty">暂无审计事件</p>';
  } catch (error) { $('api-status').textContent = error.message; $('api-status').classList.remove('online'); }
}

async function createTenant(event) {
  event.preventDefault(); $('tenant-error').textContent = '';
  try { await api('/api/admin/tenants', { method: 'POST', body: JSON.stringify({ id: $('tenant-id').value, slug: $('tenant-slug').value, name: $('tenant-name').value }) }); event.target.reset(); await refreshDashboard(); }
  catch (error) { $('tenant-error').textContent = error.message; }
}

function escapeHTML(value) { return String(value).replace(/[&<>'"]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[char])); }

$('login-form').addEventListener('submit', login); $('tenant-form').addEventListener('submit', createTenant); $('refresh').addEventListener('click', refreshDashboard); $('logout').addEventListener('click', logout);
if (state.accessToken) api('/api/auth/me').then(() => { showApp(); refreshDashboard(); }).catch(() => showLogin());
