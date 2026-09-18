const entryMode = window.location.pathname === '/portal' ? 'portal' : 'admin';
const expectedPrincipalType = entryMode === 'portal' ? 'tenant_user' : 'platform_admin';
const sessionKey = (name) => `maas_${entryMode}_${name}`;

const state = {
  accessToken: localStorage.getItem(sessionKey('access_token')) || '',
  csrfToken: localStorage.getItem(sessionKey('csrf_token')) || '',
  principalType: localStorage.getItem(sessionKey('principal_type')) || '',
  selectedTenant: null,
  adminWorkspace: 'overview',
  portalWorkspace: 'overview',
  portalSection: 'overview',
  usageOffset: 0,
  usageLimit: 20,
  usageTotal: 0,
  permissions: [],
  keyPoll: null,
  tenants: [],
  models: [],
  plans: [],
  skus: [],
  selectedPlanModels: new Set(),
  editingPlanID: '',
  editingSKUID: '',
  passwordResetMember: null
};

const $ = (id) => document.getElementById(id);
const pick = (object, ...keys) => keys.map((key) => object?.[key]).find((value) => value !== undefined && value !== null);

// Remove the former shared session so it cannot cross the two entry boundaries.
['maas_access_token', 'maas_csrf_token', 'maas_principal_type'].forEach((key) => localStorage.removeItem(key));

async function api(path, options = {}) {
  const headers = { ...(options.headers || {}) };
  if (state.accessToken) headers.Authorization = `Bearer ${state.accessToken}`;
  if (state.csrfToken && options.method && options.method !== 'GET') headers['X-CSRF-Token'] = state.csrfToken;
  if (options.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
  const response = await fetch(path, { ...options, headers });
  const payload = response.status === 204 ? {} : await response.json().catch(() => ({}));
  if (response.status === 401) {
    clearSession();
    showLogin('登录已过期');
    throw new Error('登录已过期');
  }
  if (!response.ok) throw new Error(payload.error || payload.detail || '请求失败');
  return payload;
}

function configureLoginEntry() {
  const portal = entryMode === 'portal';
  document.body.classList.toggle('portal-mode', portal);
  document.querySelectorAll('.admin-auth').forEach((node) => node.classList.toggle('hidden', portal));
  document.querySelectorAll('.tenant-auth').forEach((node) => node.classList.toggle('hidden', !portal));
  $('login-eyebrow').textContent = portal ? 'MAAS TENANT PORTAL' : 'MAAS CONTROL PLANE';
  $('login-title').textContent = portal ? '租户门户' : '平台管理后台';
  $('login-submit').textContent = portal ? '登录租户门户' : '登录管理后台';
  document.title = portal ? 'MaaS 租户门户' : 'MaaS 平台管理后台';
  $('login-error').textContent = '';
  document.body.classList.remove('entry-loading');
}

configureLoginEntry();

function showLogin(message = '') {
  $('app-view').classList.add('hidden');
  $('login-view').classList.remove('hidden');
  $('login-error').textContent = message;
}

function showSurface(type) {
  $('login-view').classList.add('hidden');
  $('app-view').classList.remove('hidden');
  $('admin-view').classList.toggle('hidden', type !== 'platform_admin');
  $('portal-view').classList.toggle('hidden', type !== 'tenant_user');
  $('surface-name').textContent = type === 'platform_admin' ? 'Control Plane' : 'Tenant Portal';
  if (type === 'platform_admin') setAdminWorkspace(state.adminWorkspace);
}

function setAdminWorkspace(name) {
  if (!['overview', 'tenants', 'catalog'].includes(name)) name = 'overview';
  state.adminWorkspace = name;
  document.querySelectorAll('[data-admin-workspace]').forEach((button) => button.classList.toggle('active', button.dataset.adminWorkspace === name));
  document.querySelectorAll('.admin-workspace').forEach((workspace) => workspace.classList.toggle('hidden', workspace.id !== `admin-workspace-${name}`));
}

function setPortalWorkspace(name, section = 'overview') {
  const button = document.querySelector(`[data-portal-workspace-button="${name}"]`);
  if (!button || button.classList.contains('hidden')) name = 'overview';
  state.portalWorkspace = name;
  state.portalSection = name === 'overview' && section === 'usage' ? 'usage' : 'overview';
  document.querySelectorAll('[data-portal-workspace-button]').forEach((node) => {
    const active = node.dataset.portalWorkspaceButton === name && (name !== 'overview' || state.portalSection === 'overview');
    node.classList.toggle('active', active);
  });
  document.querySelectorAll('[data-portal-section-link]').forEach((node) => node.classList.toggle('active', state.portalSection === node.dataset.portalSectionLink));
  document.querySelectorAll('[data-portal-workspace]').forEach((node) => node.classList.toggle('hidden', node.dataset.portalWorkspace !== name));
  const configActive = ['plan', 'keys', 'members'].includes(name);
  const logsActive = name === 'audit' || state.portalSection === 'usage';
  document.querySelector('[data-portal-menu-group="config"]')?.classList.toggle('active', configActive);
  document.querySelector('[data-portal-menu-group="logs"]')?.classList.toggle('active', logsActive);
}

function togglePortalMenu(name) {
  const group = document.querySelector(`[data-portal-menu-group="${name}"]`);
  const button = document.querySelector(`[data-portal-menu-toggle="${name}"]`);
  if (!group || !button) return;
  const collapsed = group.classList.toggle('collapsed');
  button.setAttribute('aria-expanded', String(!collapsed));
}

function showPortalUsage() {
  setPortalWorkspace('overview', 'usage');
  requestAnimationFrame(() => $('portal-usage-panel').scrollIntoView({ behavior: 'smooth', block: 'start' }));
}

function saveSession(result) {
  if (result.principal_type !== expectedPrincipalType) throw new Error('账号类型与当前登录入口不匹配');
  state.accessToken = result.access_token;
  state.csrfToken = result.csrf_token;
  state.principalType = result.principal_type;
  localStorage.setItem(sessionKey('access_token'), state.accessToken);
  localStorage.setItem(sessionKey('csrf_token'), state.csrfToken);
  localStorage.setItem(sessionKey('principal_type'), state.principalType);
}

function clearSession() {
  state.accessToken = '';
  state.csrfToken = '';
  state.principalType = '';
  state.selectedTenant = null;
  state.permissions = [];
  clearTimeout(state.keyPoll);
  localStorage.removeItem(sessionKey('access_token'));
  localStorage.removeItem(sessionKey('csrf_token'));
  localStorage.removeItem(sessionKey('principal_type'));
}

async function login(event) {
  event.preventDefault();
  $('login-error').textContent = '';
  try {
    let result;
    if (entryMode === 'portal') {
      result = await api('/api/portal/auth/login', { method: 'POST', body: JSON.stringify({ Tenant: $('tenant-slug').value, Email: $('member-email').value, Password: $('member-password').value }) });
    } else {
      result = await api('/api/auth/login', { method: 'POST', body: JSON.stringify({ Username: $('username').value, Password: $('password').value }) });
    }
    saveSession(result);
    showSurface(state.principalType);
    if (state.principalType === 'platform_admin') await refreshAdmin(); else await refreshPortal();
  } catch (error) {
    $('login-error').textContent = error.message;
  }
}

async function logout() {
  try {
    if (state.accessToken) await api(state.principalType === 'tenant_user' ? '/api/portal/auth/logout' : '/api/auth/logout', { method: 'POST' });
  } catch (_) {}
  clearSession();
  showLogin();
}

function formatDate(value) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '--'; }
function formatNumber(value) { return new Intl.NumberFormat('zh-CN').format(Number(value || 0)); }
function formatMoney(micros) { return `$${(Number(micros || 0) / 1_000_000).toFixed(4)}`; }
function formatRate(micros) { return `$${(Number(micros || 0) / 1_000_000).toFixed(6)}`; }
function usdInput(micros) { return (Number(micros || 0) / 1_000_000).toFixed(6).replace(/\.?0+$/, ''); }
function escapeHTML(value) { return String(value ?? '').replace(/[&<>'"]/g, (char) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', "'": '&#39;', '"': '&quot;' }[char])); }

function maskEmail(value) {
  const email = String(value ?? '');
  const separator = email.lastIndexOf('@');
  if (separator <= 0) return email;
  const local = email.slice(0, separator);
  const domain = email.slice(separator);
  if (local.length <= 2) return `${local.slice(0, 1)}****${domain}`;
  if (local.length <= 4) return `${local.slice(0, 1)}****${local.slice(-1)}${domain}`;
  return `${local.slice(0, 2)}****${local.slice(-2)}${domain}`;
}

async function refreshAdmin() {
  try {
    const [me, summary, tenants, audit, plans, skus, catalog] = await Promise.all([
      api('/api/auth/me'), api('/api/admin/summary'), api('/api/admin/tenants'), api('/api/admin/audit'),
      api('/api/admin/plans'), api('/api/admin/skus'),
      api('/api/admin/models').catch((error) => ({ models: [], error: error.message }))
    ]);
    $('identity').textContent = pick(me.principal, 'ID', 'id');
    $('stat-tenants').textContent = summary.tenants;
    $('stat-audits').textContent = summary.audit_events;
    $('stat-usage').textContent = summary.usage_events;
    $('stat-redis').textContent = summary.redis ? '在线' : '未配置';
    $('api-status').textContent = 'API 在线';
    $('api-status').classList.add('online');
    state.tenants = tenants;
    $('tenant-rows').innerHTML = tenants.length ? tenants.map(renderTenant).join('') : '<tr><td colspan="6" class="empty">暂无租户</td></tr>';
    renderAudit('admin-audit-list', audit.slice(0, 8));
    renderPlanCatalog(plans);
    renderSKUs(skus);
    renderModelCatalog(catalog);
  } catch (error) {
    $('api-status').textContent = error.message;
    $('api-status').classList.remove('online');
  }
}

function microsFromUSD(value) {
  const amount = Number(value);
  if (!Number.isFinite(amount) || amount < 0) throw new Error('金额必须为非负数');
  return Math.round(amount * 1_000_000);
}

function requireCanonicalModelID(value) {
  if (value === '*') return;
  if (!/^[^/\s]+\/\S+$/.test(value)) throw new Error('模型必须使用 provider/model 格式，或填写 *');
}

function renderPlanCatalog(rows) {
	state.plans = rows;
  $('plan-rows').innerHTML = rows.length ? rows.map((row) => {
    const plan = row.plan;
    const models = row.models || [];
    const visibleModels = models.slice(0, 4).map((model) => `<code>${escapeHTML(model)}</code>`).join('');
    const more = models.length > 4 ? `<span class="model-more">另有 ${formatNumber(models.length - 4)} 个</span>` : '';
	return `<tr><td><b>${escapeHTML(plan.name)}</b><span class="cell-note">${escapeHTML(plan.id)}</span></td><td>${formatMoney(plan.monthly_micros)}<span class="cell-note">含 ${formatMoney(plan.included_credit_micros)}</span></td><td>${plan.overage_policy === 'reject' ? '额度用尽后拒绝' : '允许超额'}<span class="cell-note">${formatNumber(plan.max_concurrent)} 并发</span></td><td><div class="model-summary">${visibleModels || '--'}${more}</div></td><td><button class="table-action plan-edit" data-id="${escapeHTML(plan.id)}">编辑</button></td></tr>`;
  }).join('') : '<tr><td colspan="5" class="empty">暂无套餐</td></tr>';
}

function renderSKUs(rows) {
	state.skus = rows;
  $('sku-rows').innerHTML = rows.length ? rows.map((row) => {
    const legacy = row.id !== '*' && !row.id.includes('/');
	return `<tr><td><code class="${legacy ? 'legacy-sku' : ''}">${escapeHTML(row.id)}</code>${legacy ? '<span class="cell-note">兼容旧 ID</span>' : ''}</td><td>${escapeHTML(row.name)}</td><td>${formatRate(row.prompt_micros_per_1m)}</td><td>${formatRate(row.completion_micros_per_1m)}</td><td><button class="table-action sku-edit" data-id="${escapeHTML(row.id)}">编辑</button></td></tr>`;
  }).join('') : '<tr><td colspan="5" class="empty">暂无模型成本</td></tr>';
}

function renderModelCatalog(payload) {
  state.models = Array.isArray(payload?.models) ? payload.models : [];
  const options = [{ id: '*', provider: '', model: '全部模型' }, ...state.models];
  $('model-options').innerHTML = options.map((item) => `<option value="${escapeHTML(item.id)}">${escapeHTML(item.provider ? `${item.provider} · ${item.model}` : item.model)}</option>`).join('');
  const groups = new Map();
  state.models.forEach((model) => {
    const key = modelFamily(model);
    const current = groups.get(key) || { label: modelFamilyLabel(key), count: 0 };
    current.count += 1;
    groups.set(key, current);
  });
  const preferred = ['claude', 'gpt', 'codex'];
  const groupOptions = [...groups.entries()].sort(([left], [right]) => {
    const leftRank = preferred.indexOf(left);
    const rightRank = preferred.indexOf(right);
    if (leftRank !== -1 || rightRank !== -1) return (leftRank === -1 ? 99 : leftRank) - (rightRank === -1 ? 99 : rightRank);
    return modelFamilyLabel(left).localeCompare(modelFamilyLabel(right), 'zh-CN');
  });
  $('plan-model-family').innerHTML = '<option value="">选择模型系列</option><option value="*">全部模型（*）</option>' + groupOptions.map(([key, group]) => `<option value="${escapeHTML(key)}">${escapeHTML(group.label)}（${formatNumber(group.count)}）</option>`).join('');
  ['model-catalog-status', 'plan-model-catalog-status'].forEach((id) => {
    const status = $(id);
    status.textContent = payload?.error ? '模型目录不可用' : `${formatNumber(state.models.length)} 个可选模型`;
    status.title = payload?.error || '来自 Bifrost 当前已配置 Provider';
    status.classList.toggle('online', !payload?.error);
  });
  renderPlanModelSelection();
}

function modelFamily(item) {
  const provider = String(item.provider || '').toLowerCase();
  const model = String(item.model || item.id || '').toLowerCase();
  if (model.includes('codex')) return 'codex';
  if (model.includes('claude')) return 'claude';
  if (provider === 'openai' && /^(gpt-|chatgpt-|o\d)/.test(model)) return 'gpt';
  return `provider:${provider || 'other'}`;
}

function modelFamilyLabel(key) {
  if (key === 'claude') return 'Claude';
  if (key === 'gpt') return 'GPT';
  if (key === 'codex') return 'Codex';
  if (key.startsWith('provider:')) {
    const provider = key.slice('provider:'.length);
    return provider === 'other' ? '其他模型' : `${provider} 其他模型`;
  }
  return key;
}

function addPlanModels(models) {
  if (models.includes('*')) {
    state.selectedPlanModels.clear();
    state.selectedPlanModels.add('*');
  } else {
    state.selectedPlanModels.delete('*');
    models.forEach((model) => state.selectedPlanModels.add(model));
  }
  renderPlanModelSelection();
}

function renderPlanModelSelection() {
  const models = [...state.selectedPlanModels].sort();
  $('plan-model-selection').innerHTML = models.length ? models.map((model) => `<span class="model-chip"><code>${escapeHTML(model)}</code><button type="button" data-remove-plan-model="${escapeHTML(model)}" title="移除 ${escapeHTML(model)}" aria-label="移除 ${escapeHTML(model)}">&times;</button></span>`).join('') : '<span class="empty-selection">尚未选择模型</span>';
}

function addManualPlanModel() {
  const model = $('plan-model-add').value.trim();
  if (!model) return;
  try {
    requireCanonicalModelID(model);
    addPlanModels([model]);
    $('plan-model-add').value = '';
    $('plan-error').textContent = '';
  } catch (error) {
    $('plan-error').textContent = error.message;
  }
}

function resetPlanEditor() {
	state.editingPlanID = '';
	$('plan-form').reset();
	$('plan-id').readOnly = false;
	$('plan-id').dataset.manual = 'false';
	$('plan-form-title').textContent = '新建套餐';
	$('plan-submit').textContent = '创建套餐';
	$('plan-cancel-edit').classList.add('hidden');
	state.selectedPlanModels.clear();
	renderPlanModelSelection();
	$('plan-error').textContent = '';
}

function editPlan(planID) {
	const details = state.plans.find((row) => row.plan.id === planID);
	if (!details) return;
	const plan = details.plan;
	state.editingPlanID = planID;
	$('plan-name').value = plan.name;
	$('plan-id').value = plan.id;
	$('plan-id').readOnly = true;
	$('plan-id').dataset.manual = 'true';
	$('plan-monthly').value = usdInput(plan.monthly_micros);
	$('plan-credit').value = usdInput(plan.included_credit_micros);
	$('plan-concurrency').value = plan.max_concurrent;
	$('plan-overage').value = plan.overage_policy;
	state.selectedPlanModels = new Set(details.models || []);
	renderPlanModelSelection();
	$('plan-form-title').textContent = `编辑套餐 / ${plan.name}`;
	$('plan-submit').textContent = '保存套餐';
	$('plan-cancel-edit').classList.remove('hidden');
	$('plan-error').textContent = '';
	$('plan-form').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

async function createPlan(event) {
  event.preventDefault();
  try {
    const planID = $('plan-id').value.trim();
    const maxConcurrent = Number($('plan-concurrency').value);
    const models = [...state.selectedPlanModels];
    if (!models.length) throw new Error('请至少选择一个允许使用的模型');
    models.forEach(requireCanonicalModelID);
    if (!Number.isSafeInteger(maxConcurrent) || maxConcurrent < 1) throw new Error('租户最大并发必须大于 0');
    const payload = {
      plan: {
        id: planID,
        name: $('plan-name').value.trim(),
        currency: 'USD',
        monthly_micros: microsFromUSD($('plan-monthly').value),
        included_credit_micros: microsFromUSD($('plan-credit').value),
        max_concurrent: maxConcurrent,
        overage_policy: $('plan-overage').value
      },
      models
    };
	const editing = state.editingPlanID !== '';
	await api('/api/admin/plans', { method: editing ? 'PUT' : 'POST', body: JSON.stringify(payload) });
	resetPlanEditor();
    await refreshAdmin();
  } catch (error) { $('plan-error').textContent = error.message; }
}

function resetSKUEditor() {
	state.editingSKUID = '';
	$('sku-form').reset();
	$('sku-id').readOnly = false;
	$('sku-submit').textContent = '新增成本定价';
	$('sku-cancel-edit').classList.add('hidden');
	$('sku-error').textContent = '';
}

function editSKU(skuID) {
	const sku = state.skus.find((row) => row.id === skuID);
	if (!sku) return;
	state.editingSKUID = skuID;
	$('sku-id').value = sku.id;
	$('sku-id').readOnly = true;
	$('sku-name').value = sku.name;
	$('sku-prompt').value = usdInput(sku.prompt_micros_per_1m);
	$('sku-completion').value = usdInput(sku.completion_micros_per_1m);
	$('sku-submit').textContent = '保存成本定价';
	$('sku-cancel-edit').classList.remove('hidden');
	$('sku-error').textContent = '';
	$('sku-form').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

async function upsertSKU(event) {
  event.preventDefault();
  try {
    const id = $('sku-id').value.trim();
    requireCanonicalModelID(id);
    await api('/api/admin/skus', { method: 'PUT', body: JSON.stringify({ id, name: $('sku-name').value.trim(), prompt_micros_per_1m: microsFromUSD($('sku-prompt').value), completion_micros_per_1m: microsFromUSD($('sku-completion').value) }) });
	resetSKUEditor();
    await refreshAdmin();
  } catch (error) { $('sku-error').textContent = error.message; }
}

function renderTenant(row) {
  const id = pick(row, 'ID', 'id');
  const slug = pick(row, 'Slug', 'slug') || '';
  const name = pick(row, 'Name', 'name') || slug || id;
  const status = pick(row, 'Status', 'status');
  const created = pick(row, 'CreatedAt', 'created_at');
  const nextStatus = status === 'active' ? 'suspended' : ['registered', 'suspended', 'trial'].includes(status) ? 'active' : '';
  const statusAction = nextStatus ? `<button class="table-action tenant-status ${nextStatus === 'suspended' ? 'danger-action' : ''}" data-id="${escapeHTML(id)}" data-name="${escapeHTML(name)}" data-next="${nextStatus}">${nextStatus === 'active' ? '启用' : '停用'}</button>` : '';
  return `<tr><td><code>${escapeHTML(id)}</code></td><td><code>${escapeHTML(slug)}</code></td><td>${escapeHTML(name)}</td><td><span class="status-tag status-${escapeHTML(status)}">${escapeHTML(status)}</span></td><td>${formatDate(created)}</td><td class="row-actions"><button class="table-action tenant-edit" data-id="${escapeHTML(id)}">查看/编辑</button><button class="table-action tenant-keys" data-id="${escapeHTML(id)}" data-name="${escapeHTML(name)}">密钥</button><button class="table-action tenant-members" data-id="${escapeHTML(id)}" data-name="${escapeHTML(name)}">成员</button><button class="table-action tenant-plan" data-id="${escapeHTML(id)}" data-name="${escapeHTML(name)}">套餐</button>${statusAction}</td></tr>`;
}

async function createTenant(event) {
  event.preventDefault();
  $('tenant-error').textContent = '';
  try {
    await api('/api/admin/tenants', { method: 'POST', body: JSON.stringify({ ID: $('tenant-id').value, Slug: $('new-tenant-slug').value, Name: $('tenant-name').value }) });
    event.target.reset();
    await refreshAdmin();
  } catch (error) { $('tenant-error').textContent = error.message; }
}

function openTenantEditor(tenantID) {
  const row = state.tenants.find((item) => pick(item, 'ID', 'id') === tenantID);
  if (!row) return;
  closeAdminDetails();
  const name = pick(row, 'Name', 'name') || '';
  state.selectedTenant = { id: tenantID, name: name || pick(row, 'Slug', 'slug') || tenantID };
  $('admin-tenant-title').textContent = `${state.selectedTenant.name} / 租户详情`;
  $('edit-tenant-id').value = tenantID;
  $('edit-tenant-status').value = pick(row, 'Status', 'status') || '';
  $('edit-tenant-slug').value = pick(row, 'Slug', 'slug') || '';
  $('edit-tenant-name').value = name;
  $('admin-tenant-edit-error').textContent = '';
  $('admin-tenant-panel').classList.remove('hidden');
  $('admin-tenant-panel').scrollIntoView({ behavior: 'smooth', block: 'start' });
}

async function updateTenantProfile(event) {
  event.preventDefault();
  if (!state.selectedTenant) return;
  const errorNode = $('admin-tenant-edit-error');
  errorNode.textContent = '';
  try {
    await api(`/api/admin/tenants/${encodeURIComponent(state.selectedTenant.id)}`, {
      method: 'PATCH',
      body: JSON.stringify({ slug: $('edit-tenant-slug').value.trim(), name: $('edit-tenant-name').value.trim() })
    });
    closeAdminDetails();
    state.selectedTenant = null;
    await refreshAdmin();
    $('tenant-error').textContent = '租户信息已更新';
  } catch (error) {
    errorNode.textContent = error.message;
  }
}

function openAdminDetail(kind, button) {
  setAdminWorkspace('tenants');
  closeAdminDetails();
  state.selectedTenant = { id: button.dataset.id, name: button.dataset.name };
  const panel = $(`admin-${kind}-panel`);
  panel.classList.remove('hidden');
  $(`admin-${kind}-title`).textContent = `${state.selectedTenant.name} / ${{ key: 'API 密钥', member: '成员', plan: '套餐' }[kind]}`;
  panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
  if (kind === 'key') loadKeys('admin');
  if (kind === 'member') loadMembers('admin');
  if (kind === 'plan') loadAdminPlans();
}

function closeAdminDetails() {
  clearTimeout(state.keyPoll);
  state.keyPoll = null;
  document.querySelectorAll('#admin-view .detail-panel').forEach((node) => node.classList.add('hidden'));
}

function keyBase(scope) {
  return scope === 'portal' ? '/api/portal/keys' : `/api/admin/tenants/${encodeURIComponent(state.selectedTenant.id)}/keys`;
}

async function loadKeys(scope) {
  clearTimeout(state.keyPoll);
  const errorNode = $(`${scope}-key-error`);
  try {
    const keys = await api(keyBase(scope));
    errorNode.textContent = '';
    $(`${scope}-key-rows`).innerHTML = keys.length ? keys.map((key) => renderKey(key, scope)).join('') : '<tr><td colspan="5" class="empty">暂无 API 密钥</td></tr>';
    if (keys.some((key) => key.status === 'pending' || key.status === 'revoking')) state.keyPoll = setTimeout(() => loadKeys(scope), 1200);
  } catch (error) { errorNode.textContent = error.message; }
}

function renderKey(key, scope) {
  let operation = '';
  if (scope === 'portal' && state.permissions.includes('key.reveal') && !['revoked', 'revoking'].includes(key.status)) operation += `<button class="table-action key-reveal" data-id="${escapeHTML(key.id)}" title="查看完整 API 密钥" aria-label="查看 ${escapeHTML(key.name)} 的完整 API 密钥">&#128065;</button>`;
  if (key.status === 'failed') operation += `<button class="table-action key-retry" data-scope="${scope}" data-id="${escapeHTML(key.id)}">重试</button>`;
  if (!['revoked', 'revoking'].includes(key.status)) operation += `<button class="table-action danger-action key-revoke" data-scope="${scope}" data-id="${escapeHTML(key.id)}">撤销</button>`;
  return `<tr><td><b class="key-name">${escapeHTML(key.name)}</b></td><td><code>${escapeHTML(key.secret_fingerprint)}</code></td><td><span class="status-tag status-${escapeHTML(key.status)}">${escapeHTML(statusLabel(key.status))}</span></td><td>${formatDate(key.created_at)}</td><td class="row-actions">${operation || '--'}</td></tr>`;
}

function statusLabel(status) { return ({ pending: '等待同步', active: '可用', failed: '失败', revoking: '撤销中', revoked: '已撤销', disabled: '已禁用' })[status] || status; }

async function createKey(event, scope) {
  event.preventDefault();
  const name = $(`${scope}-key-name`).value;
  try {
    const result = await api(keyBase(scope), { method: 'POST', body: JSON.stringify({ name }) });
    event.target.reset();
    showSecret(result.secret, 'API 密钥已创建', scope === 'portal' ? '请保存到应用部署环境；租户 Owner 可在门户再次查看。' : '请立即保存到租户的安全凭证库。', false);
    await loadKeys(scope);
  } catch (error) { $(`${scope}-key-error`).textContent = error.message; }
}

function showSecret(secret, title, note, revealed) {
  $('secret-dialog-title').textContent = title;
  $('secret-dialog-note').textContent = note;
  $('secret-value').value = secret;
  $('secret-value').type = revealed ? 'text' : 'password';
  $('toggle-secret').title = revealed ? '隐藏密钥' : '显示密钥';
  $('toggle-secret').setAttribute('aria-label', revealed ? '隐藏密钥' : '显示密钥');
  $('copy-status').textContent = '';
  $('secret-dialog').showModal();
}

async function revealKey(keyID) {
  try {
    const result = await api(`/api/portal/keys/${encodeURIComponent(keyID)}/reveal`, { method: 'POST' });
    showSecret(result.secret, 'API 密钥', '本次查看已记录在租户审计中。', true);
  } catch (error) {
    $('portal-key-error').textContent = error.message;
  }
}

async function mutateKey(scope, id, action) {
  try {
    await api(`${keyBase(scope)}/${encodeURIComponent(id)}${action === 'retry' ? '/retry' : ''}`, { method: action === 'retry' ? 'POST' : 'DELETE' });
    await loadKeys(scope);
  } catch (error) { $(`${scope}-key-error`).textContent = error.message; }
}

function memberBase(scope) { return scope === 'portal' ? '/api/portal/members' : `/api/admin/tenants/${encodeURIComponent(state.selectedTenant.id)}/members`; }

async function loadMembers(scope) {
  try {
    const rows = await api(memberBase(scope));
    $(`${scope}-member-error`).textContent = '';
    $(`${scope}-member-rows`).innerHTML = rows.length ? rows.map((row) => renderMember(row, scope)).join('') : '<tr><td colspan="5" class="empty">暂无成员</td></tr>';
  } catch (error) { $(`${scope}-member-error`).textContent = error.message; }
}

function renderMember(row, scope) {
  const role = row.role;
  const locked = scope === 'portal' && role === 'owner';
  const roles = (scope === 'admin' ? ['owner', 'admin', 'developer', 'viewer'] : ['admin', 'developer', 'viewer']).map((name) => `<option value="${name}" ${name === role ? 'selected' : ''}>${name}</option>`).join('');
  const roleControl = locked ? '<span class="status-tag role-owner">owner</span>' : `<select class="member-role" data-id="${escapeHTML(row.id)}" data-scope="${scope}">${roles}</select>`;
	let action = locked ? '--' : `<button class="table-action member-save" data-id="${escapeHTML(row.id)}" data-scope="${scope}">保存</button><button class="table-action danger-action member-toggle" data-id="${escapeHTML(row.id)}" data-scope="${scope}" data-status="${row.status}">${row.status === 'active' ? '禁用' : '启用'}</button>`;
	if (scope === 'admin') action = `<button class="table-action member-password" data-id="${escapeHTML(row.id)}" data-name="${escapeHTML(row.display_name)}" data-email="${escapeHTML(row.email)}">重置密码</button>${action === '--' ? '' : action}`;
  return `<tr><td><b>${escapeHTML(row.display_name)}</b><span class="cell-note">${escapeHTML(row.email)}</span></td><td>${roleControl}</td><td><span class="status-tag status-${escapeHTML(row.status)}">${escapeHTML(statusLabel(row.status))}</span></td><td>${formatDate(row.last_login_at)}</td><td class="row-actions">${action}</td></tr>`;
}

async function createMember(event, scope) {
  event.preventDefault();
  try {
    await api(memberBase(scope), { method: 'POST', body: JSON.stringify({ display_name: $(`${scope}-member-name`).value, email: $(`${scope}-member-email`).value, password: $(`${scope}-member-password`).value, role: $(`${scope}-member-role`).value }) });
    event.target.reset();
    await loadMembers(scope);
  } catch (error) { $(`${scope}-member-error`).textContent = error.message; }
}

async function updateMember(scope, id, values) {
  try {
    await api(`${memberBase(scope)}/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify(values) });
    await loadMembers(scope);
  } catch (error) { $(`${scope}-member-error`).textContent = error.message; }
}

function openMemberPasswordDialog(button) {
	state.passwordResetMember = { id: button.dataset.id, name: button.dataset.name, email: button.dataset.email };
	$('member-password-target').textContent = `${button.dataset.name} · ${button.dataset.email}`;
	$('member-password-form').reset();
	$('member-password-error').textContent = '';
	$('member-password-dialog').showModal();
}

function closeMemberPasswordDialog() {
	state.passwordResetMember = null;
	$('member-password-form').reset();
	$('member-password-error').textContent = '';
	if ($('member-password-dialog').open) $('member-password-dialog').close();
}

async function resetMemberPassword(event) {
	event.preventDefault();
	const target = state.passwordResetMember;
	const password = $('member-new-password').value;
	const confirmation = $('member-confirm-password').value;
	if (!target) return;
	if (password.length < 12) {
		$('member-password-error').textContent = '密码至少需要 12 位';
		return;
	}
	if (password !== confirmation) {
		$('member-password-error').textContent = '两次输入的密码不一致';
		return;
	}
	try {
		await api(`${memberBase('admin')}/${encodeURIComponent(target.id)}`, { method: 'PATCH', body: JSON.stringify({ password }) });
		const label = target.email;
		closeMemberPasswordDialog();
		await loadMembers('admin');
		$('admin-member-error').textContent = `${label} 的密码已重置，旧会话已失效`;
	} catch (error) {
		$('member-password-error').textContent = error.message;
	}
}

async function loadAdminPlans() {
	const currentName = $('admin-current-plan-name');
	const currentMeta = $('admin-current-plan-meta');
	const currentModels = $('admin-current-plan-models');
  try {
    const rows = await api('/api/admin/plans');
    $('admin-plan-select').innerHTML = rows.map((row) => `<option value="${escapeHTML(pick(row.plan, 'ID', 'id'))}">${escapeHTML(pick(row.plan, 'Name', 'name'))}</option>`).join('');
	currentName.textContent = '未分配套餐';
	currentMeta.textContent = '';
	currentModels.innerHTML = '';
	try {
	  const details = await api(`/api/admin/tenants/${encodeURIComponent(state.selectedTenant.id)}/plan`);
	  const plan = details.plan || {};
	  const planID = pick(plan, 'ID', 'id');
	  currentName.textContent = pick(plan, 'Name', 'name') || planID || '未分配套餐';
	  currentMeta.textContent = planID ? `${planID} · 每月额度 ${formatMoney(pick(plan, 'IncludedCreditMicros', 'included_credit_micros'))} · ${formatNumber(pick(plan, 'MaxConcurrent', 'max_concurrent'))} 并发` : '';
	  const models = details.models || [];
	  currentModels.innerHTML = models.length ? models.map((model) => `<span class="model-chip readonly"><code>${escapeHTML(model)}</code></span>`).join('') : '<span class="empty-selection">未配置允许模型</span>';
	  if (planID) $('admin-plan-select').value = planID;
	  $('admin-plan-error').textContent = '';
	} catch (error) {
	  $('admin-plan-error').textContent = error.message;
	}
  } catch (error) { $('admin-plan-error').textContent = error.message; }
}

async function setAdminPlan(event) {
  event.preventDefault();
  try {
    await api(`/api/admin/tenants/${encodeURIComponent(state.selectedTenant.id)}/plan`, { method: 'PUT', body: JSON.stringify({ plan_id: $('admin-plan-select').value }) });
	await loadAdminPlans();
    $('admin-plan-error').textContent = '已应用';
  } catch (error) { $('admin-plan-error').textContent = error.message; }
}

async function refreshPortal() {
  try {
    const me = await api('/api/portal/me');
    state.permissions = me.permissions || [];
    $('identity').textContent = me.member.display_name;
    $('portal-tenant-name').textContent = pick(me.tenant, 'Name', 'name') || pick(me.tenant, 'Slug', 'slug');
    $('portal-role').textContent = `${me.member.role} · ${maskEmail(me.member.email)}`;
    const canKeys = state.permissions.includes('key.manage');
    const canMembers = state.permissions.includes('member.manage');
    const canAudit = state.permissions.includes('audit.read');
    document.querySelector('[data-portal-workspace-button="keys"]').classList.toggle('hidden', !canKeys);
    document.querySelector('[data-portal-workspace-button="members"]').classList.toggle('hidden', !canMembers);
    document.querySelector('[data-portal-workspace-button="audit"]').classList.toggle('hidden', !canAudit);
    const [usage, plan, audit] = await Promise.all([
      api(portalUsagePath()),
      api('/api/portal/plan'),
      canAudit ? api('/api/portal/audit') : Promise.resolve([])
    ]);
    applyPortalUsage(usage);
    const planName = pick(plan.plan, 'Name', 'name') || '当前套餐';
    const credit = plan.credit || {};
    $('portal-remaining').textContent = formatMoney(credit.remaining_credit_micros);
    $('portal-plan-name').textContent = planName;
    $('portal-credit-included').textContent = formatMoney(credit.included_credit_micros);
    $('portal-credit-used').textContent = formatMoney(credit.used_credit_micros);
    $('portal-concurrency').textContent = formatNumber(credit.max_concurrent);
    $('portal-overage').textContent = credit.overage_policy === 'allow' ? '允许超额' : '额度用尽后拒绝';
    const models = plan.models || [];
    $('portal-model-list').innerHTML = models.length ? models.map((model) => `<span class="model-chip readonly"><code>${escapeHTML(model)}</code></span>`).join('') : '<span class="empty-selection">暂无可用模型</span>';
    renderAudit('portal-audit-list', audit.slice(0, 12));
    if (canKeys) await loadKeys('portal');
    if (canMembers) await loadMembers('portal');
    setPortalWorkspace(state.portalWorkspace, state.portalSection);
  } catch (error) {
    $('portal-role').textContent = error.message;
  }
}

function portalUsagePath() {
  const query = new URLSearchParams({ limit: String(state.usageLimit), offset: String(state.usageOffset) });
  const status = $('usage-status-filter').value;
  const model = $('usage-model-filter').value;
  if (status) query.set('status', status);
  if (model) query.set('model', model);
  return `/api/portal/usage?${query.toString()}`;
}

function applyPortalUsage(usage) {
  const summary = usage.summary || {};
  $('portal-requests').textContent = formatNumber(summary.requests);
  $('portal-tokens').textContent = formatNumber(summary.total_tokens);
  $('portal-amount').textContent = formatMoney(summary.amount_micros);
  state.usageTotal = Number(usage.total_count || 0);
  const selectedModel = $('usage-model-filter').value;
  const models = usage.models || [];
  $('usage-model-filter').innerHTML = '<option value="">全部模型</option>' + models.map((model) => `<option value="${escapeHTML(model)}">${escapeHTML(model)}</option>`).join('');
  $('usage-model-filter').value = models.includes(selectedModel) ? selectedModel : '';
  $('portal-usage-rows').innerHTML = usage.events?.length ? usage.events.map(renderUsage).join('') : '<tr><td colspan="7" class="empty">当前筛选条件下暂无用量</td></tr>';
  $('usage-count').textContent = `${formatNumber(state.usageTotal)} 条`;
  $('usage-page').textContent = `第 ${Math.floor(state.usageOffset / state.usageLimit) + 1} 页`;
  $('usage-prev').disabled = state.usageOffset === 0;
  $('usage-next').disabled = state.usageOffset + state.usageLimit >= state.usageTotal;
}

async function loadPortalUsage() {
  try {
    const usage = await api(portalUsagePath());
    applyPortalUsage(usage);
  } catch (error) {
    $('portal-usage-rows').innerHTML = `<tr><td colspan="7" class="empty">${escapeHTML(error.message)}</td></tr>`;
    $('usage-count').textContent = '加载失败';
  }
}

function usageStatusMeta(value) {
  const status = String(value || 'unknown').toLowerCase();
  if (status === 'success') return { label: '成功', className: 'status-success' };
  if (status === 'failed' || status === 'error') return { label: '失败', className: 'status-failed' };
  if (status === 'processing') return { label: '处理中', className: 'status-pending' };
  return { label: '未知', className: 'status-unknown' };
}

function formatDuration(value) {
  if (value === undefined || value === null || Number.isNaN(Number(value))) return '--';
  const milliseconds = Math.max(0, Number(value));
  return milliseconds < 1000 ? `${Math.round(milliseconds)} ms` : `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 2 : 1)} s`;
}

function renderUsage(row) {
  const status = usageStatusMeta(pick(row, 'Status', 'status'));
  const model = String(pick(row, 'Model', 'model') || '--');
  const separator = model.indexOf('/');
  const provider = separator > 0 ? model.slice(0, separator) : '';
  const modelName = separator > 0 ? model.slice(separator + 1) : model;
  const detail = state.permissions.includes('request_log.read')
    ? `<button class="table-action usage-detail" data-id="${escapeHTML(pick(row, 'ID', 'id'))}">详情</button>`
    : '--';
  const promptTokens = pick(row, 'PromptTokens', 'prompt_tokens');
  const completionTokens = pick(row, 'CompletionTokens', 'completion_tokens');
  return `<tr><td><b>${escapeHTML(modelName)}</b>${provider ? `<span class="cell-note">${escapeHTML(provider)}</span>` : ''}</td><td><span class="usage-tokens"><b>${formatNumber(promptTokens)}</b><span>/</span><b>${formatNumber(completionTokens)}</b></span></td><td><span class="usage-amount">${formatMoney(pick(row, 'AmountMicros', 'amount_micros'))}</span></td><td><span class="usage-duration">${formatDuration(pick(row, 'DurationMS', 'duration_ms'))}</span></td><td><span class="status-tag ${status.className}">${status.label}</span></td><td>${formatDate(pick(row, 'CreatedAt', 'created_at'))}</td><td>${detail}</td></tr>`;
}

function jsonBlock(title, value) {
  if (value === undefined || value === null || (typeof value === 'object' && !Object.keys(value).length)) return `<div class="empty detail-empty">暂无${escapeHTML(title)}内容</div>`;
  const json = JSON.stringify(value, null, 2);
  return `<div class="json-block"><div class="json-heading"><b>${escapeHTML(title)}</b><button class="table-action copy-json" type="button">复制</button></div><pre><code>${escapeHTML(json)}</code></pre></div>`;
}

function setLogTab(name) {
  document.querySelectorAll('[data-log-tab]').forEach((button) => button.classList.toggle('active', button.dataset.logTab === name));
  document.querySelectorAll('[data-log-tab-panel]').forEach((panel) => panel.classList.toggle('hidden', panel.dataset.logTabPanel !== name));
}

function renderRequestLogDetail(detail, usage = null) {
  const summary = detail.summary || {};
  $('request-log-dialog-title').textContent = summary.id ? `请求 ${summary.id}` : '请求详情';
  $('request-log-summary').innerHTML = [
    ['状态', summary.status || '--'], ['模型', summary.model || usage?.Model || '--'],
    ['Token', formatNumber(summary.total_tokens ?? usage?.TotalTokens)],
    ['延迟', summary.latency_ms == null ? '--' : `${Math.round(Number(summary.latency_ms))} ms`],
    ['费用', summary.cost == null ? (usage ? formatMoney(usage.AmountMicros) : '--') : `$${Number(summary.cost).toFixed(6)}`],
    ['时间', formatDate(summary.timestamp || usage?.CreatedAt)]
  ].map(([label, value]) => `<div><span>${escapeHTML(label)}</span><strong>${escapeHTML(value)}</strong></div>`).join('');
  const availability = detail.content_hidden ? '内容记录已关闭' : detail.content_available ? '内容可用' : '没有可展示的正文';
  $('request-log-tab-overview').innerHTML = `<dl class="request-meta"><div><dt>对象</dt><dd>${escapeHTML(summary.object || '--')}</dd></div><div><dt>Provider</dt><dd>${escapeHTML(summary.provider || '--')}</dd></div><div><dt>流式请求</dt><dd>${summary.stream ? '是' : '否'}</dd></div><div><dt>正文状态</dt><dd>${availability}</dd></div><div><dt>脱敏</dt><dd>${detail.redacted ? '已脱敏' : '无敏感字段'}</dd></div><div><dt>截断</dt><dd>${detail.truncated ? '部分内容已截断' : '否'}</dd></div></dl>`;
  $('request-log-tab-request').innerHTML = jsonBlock('请求', detail.request);
  $('request-log-tab-response').innerHTML = jsonBlock('响应', detail.response);
  $('request-log-tab-routing').innerHTML = `${jsonBlock('路由', detail.routing)}${jsonBlock('错误', detail.error)}`;
  $('request-log-dialog-state').classList.add('hidden');
  $('request-log-dialog-content').classList.remove('hidden');
  setLogTab('overview');
}

async function openRequestLogDetail(path) {
  $('request-log-dialog-state').textContent = '加载中...';
  $('request-log-dialog-state').classList.remove('hidden');
  $('request-log-dialog-content').classList.add('hidden');
  $('request-log-dialog').showModal();
  try {
    const result = await api(path);
    if (result.log_available === false) {
      $('request-log-dialog-title').textContent = '用量详情';
      $('request-log-dialog-state').textContent = '对应请求日志已过期或不可用，账单用量仍然保留。';
      return;
    }
    renderRequestLogDetail(result.log || result, result.usage || null);
  } catch (error) {
    $('request-log-dialog-state').textContent = error.message;
  }
}

function renderAudit(id, rows) {
  $(id).innerHTML = rows.length ? rows.map((entry) => `<div class="audit-item"><span class="audit-dot"></span><div><b class="audit-action">${escapeHTML(pick(entry, 'Action', 'action'))}</b><span class="audit-resource">${escapeHTML(pick(entry, 'ResourceType', 'resource_type'))} / ${escapeHTML(pick(entry, 'ResourceID', 'resource_id'))}</span></div><time class="audit-time">${formatDate(pick(entry, 'CreatedAt', 'created_at'))}</time></div>`).join('') : '<p class="empty">暂无审计事件</p>';
}

async function copySecret() {
	try { await navigator.clipboard.writeText($('secret-value').value); $('copy-status').textContent = '已复制'; }
  catch (_) { $('copy-status').textContent = '复制失败'; }
}

function toggleSecretVisibility() {
	const secret = $('secret-value');
	const reveal = secret.type === 'password';
	secret.type = reveal ? 'text' : 'password';
	$('toggle-secret').title = reveal ? '隐藏密钥' : '显示密钥';
	$('toggle-secret').setAttribute('aria-label', reveal ? '隐藏密钥' : '显示密钥');
}

function clearSecretDialog() {
	$('secret-value').value = '';
	$('secret-value').type = 'password';
	$('copy-status').textContent = '';
}

function closeSecretDialog() {
	clearSecretDialog();
	if ($('secret-dialog').open) $('secret-dialog').close();
}

$('login-form').addEventListener('submit', login);
$('logout').addEventListener('click', logout);
$('refresh-admin').addEventListener('click', refreshAdmin);
$('refresh-portal').addEventListener('click', refreshPortal);
$('tenant-form').addEventListener('submit', createTenant);
$('tenant-edit-form').addEventListener('submit', updateTenantProfile);
$('plan-form').addEventListener('submit', createPlan);
$('sku-form').addEventListener('submit', upsertSKU);
$('plan-cancel-edit').addEventListener('click', resetPlanEditor);
$('sku-cancel-edit').addEventListener('click', resetSKUEditor);
$('add-plan-model').addEventListener('click', addManualPlanModel);
$('plan-model-add').addEventListener('keydown', (event) => {
  if (event.key === 'Enter') {
    event.preventDefault();
    addManualPlanModel();
  }
});
$('plan-model-family').addEventListener('change', (event) => {
  const family = event.target.value;
  if (!family) return;
  const models = family === '*' ? ['*'] : state.models.filter((model) => modelFamily(model) === family).map((model) => model.id);
  addPlanModels(models);
  event.target.value = '';
});
$('plan-model-selection').addEventListener('click', (event) => {
  const button = event.target.closest('[data-remove-plan-model]');
  if (!button) return;
  state.selectedPlanModels.delete(button.dataset.removePlanModel);
  renderPlanModelSelection();
});
$('sku-id').addEventListener('change', () => {
  if ($('sku-name').value.trim()) return;
  const selected = state.models.find((model) => model.id === $('sku-id').value.trim());
  if (selected) $('sku-name').value = selected.model;
  if ($('sku-id').value.trim() === '*') $('sku-name').value = 'Default model cost';
});
$('plan-name').addEventListener('input', () => {
  if ($('plan-id').dataset.manual === 'true') return;
  const generated = $('plan-name').value.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
  if (generated) $('plan-id').value = generated;
});
$('plan-id').addEventListener('input', () => { $('plan-id').dataset.manual = $('plan-id').value ? 'true' : 'false'; });
$('sku-id').addEventListener('change', () => {
  const selected = state.models.find((model) => model.id === $('sku-id').value.trim());
  if (selected && !$('sku-name').value.trim()) $('sku-name').value = selected.model;
});
$('admin-key-form').addEventListener('submit', (event) => createKey(event, 'admin'));
$('portal-key-form').addEventListener('submit', (event) => createKey(event, 'portal'));
$('admin-member-form').addEventListener('submit', (event) => createMember(event, 'admin'));
$('portal-member-form').addEventListener('submit', (event) => createMember(event, 'portal'));
$('admin-plan-form').addEventListener('submit', setAdminPlan);
$('close-secret').addEventListener('click', closeSecretDialog);
$('toggle-secret').addEventListener('click', toggleSecretVisibility);
$('copy-secret').addEventListener('click', copySecret);
$('secret-dialog').addEventListener('close', clearSecretDialog);
$('member-password-form').addEventListener('submit', resetMemberPassword);
$('close-member-password').addEventListener('click', closeMemberPasswordDialog);
$('cancel-member-password').addEventListener('click', closeMemberPasswordDialog);
$('member-password-dialog').addEventListener('close', () => {
	state.passwordResetMember = null;
	$('member-password-form').reset();
});
document.querySelectorAll('.close-detail').forEach((button) => button.addEventListener('click', () => { closeAdminDetails(); state.selectedTenant = null; }));
document.querySelectorAll('[data-admin-workspace]').forEach((button) => button.addEventListener('click', () => setAdminWorkspace(button.dataset.adminWorkspace)));
document.querySelectorAll('[data-portal-workspace-button]').forEach((button) => button.addEventListener('click', () => {
  setPortalWorkspace(button.dataset.portalWorkspaceButton, 'overview');
  if (button.dataset.portalWorkspaceButton === 'overview') requestAnimationFrame(() => $('portal-workspace-overview').scrollIntoView({ behavior: 'smooth', block: 'start' }));
}));
document.querySelectorAll('[data-portal-menu-toggle]').forEach((button) => button.addEventListener('click', () => togglePortalMenu(button.dataset.portalMenuToggle)));
document.querySelectorAll('[data-portal-section-link="usage"]').forEach((button) => button.addEventListener('click', showPortalUsage));
$('usage-status-filter').addEventListener('change', async () => {
  state.usageOffset = 0;
  await loadPortalUsage();
});
$('usage-model-filter').addEventListener('change', async () => {
  state.usageOffset = 0;
  await loadPortalUsage();
});
$('usage-filter-reset').addEventListener('click', async () => {
  $('usage-status-filter').value = '';
  $('usage-model-filter').value = '';
  state.usageOffset = 0;
  await loadPortalUsage();
});
$('portal-usage-rows').addEventListener('click', (event) => {
  const button = event.target.closest('.usage-detail');
  if (button) openRequestLogDetail(`/api/portal/usage/${encodeURIComponent(button.dataset.id)}/detail`);
});
$('usage-prev').addEventListener('click', async () => {
  state.usageOffset = Math.max(0, state.usageOffset - state.usageLimit);
  await loadPortalUsage();
});
$('usage-next').addEventListener('click', async () => {
  if (state.usageOffset + state.usageLimit >= state.usageTotal) return;
  state.usageOffset += state.usageLimit;
  await loadPortalUsage();
});
$('close-request-log').addEventListener('click', () => $('request-log-dialog').close());
document.querySelectorAll('[data-log-tab]').forEach((button) => button.addEventListener('click', () => setLogTab(button.dataset.logTab)));
$('request-log-dialog').addEventListener('click', async (event) => {
  const button = event.target.closest('.copy-json');
  if (!button) return;
  try { await navigator.clipboard.writeText(button.closest('.json-block').querySelector('pre code').textContent); button.textContent = '已复制'; }
  catch (_) { button.textContent = '复制失败'; }
});
renderPlanModelSelection();

$('tenant-rows').addEventListener('click', (event) => {
  const button = event.target.closest('button[data-id]');
  if (!button) return;
  if (button.classList.contains('tenant-edit')) openTenantEditor(button.dataset.id);
  if (button.classList.contains('tenant-keys')) openAdminDetail('key', button);
  if (button.classList.contains('tenant-members')) openAdminDetail('member', button);
  if (button.classList.contains('tenant-plan')) openAdminDetail('plan', button);
  if (button.classList.contains('tenant-status')) updateTenantStatus(button.dataset.id, button.dataset.next);
});

async function updateTenantStatus(id, status) {
  try {
    await api(`/api/admin/tenants/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify({ status }) });
    await refreshAdmin();
  } catch (error) { $('tenant-error').textContent = error.message; }
}

document.addEventListener('click', (event) => {
	const planEdit = event.target.closest('.plan-edit');
	if (planEdit) { editPlan(planEdit.dataset.id); return; }
	const skuEdit = event.target.closest('.sku-edit');
	if (skuEdit) { editSKU(skuEdit.dataset.id); return; }
	const password = event.target.closest('.member-password');
	if (password) { openMemberPasswordDialog(password); return; }
  const reveal = event.target.closest('.key-reveal');
  if (reveal) { revealKey(reveal.dataset.id); return; }
  const retry = event.target.closest('.key-retry');
  if (retry) { mutateKey(retry.dataset.scope, retry.dataset.id, 'retry'); return; }
  const revoke = event.target.closest('.key-revoke');
  if (revoke && window.confirm('确认撤销这个 API 密钥？')) { mutateKey(revoke.dataset.scope, revoke.dataset.id, 'revoke'); return; }
  const save = event.target.closest('.member-save');
  if (save) {
    const select = document.querySelector(`.member-role[data-scope="${save.dataset.scope}"][data-id="${save.dataset.id}"]`);
    updateMember(save.dataset.scope, save.dataset.id, { role: select.value });
    return;
  }
  const toggle = event.target.closest('.member-toggle');
  if (toggle) updateMember(toggle.dataset.scope, toggle.dataset.id, { status: toggle.dataset.status === 'active' ? 'disabled' : 'active' });
});

if (state.accessToken && state.principalType !== expectedPrincipalType) {
  clearSession();
  showLogin('请使用当前入口对应的账号登录');
} else if (state.accessToken) {
  api('/api/auth/me').then((me) => {
    const principalType = pick(me.principal, 'Type', 'type');
    if (principalType !== expectedPrincipalType) throw new Error('账号类型与当前登录入口不匹配');
    state.principalType = principalType;
    showSurface(state.principalType);
    return state.principalType === 'platform_admin' ? refreshAdmin() : refreshPortal();
  }).catch(() => {
    clearSession();
    showLogin('登录已过期，请重新登录');
  });
}
