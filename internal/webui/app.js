'use strict';

const $ = (id) => document.getElementById(id);
const STATE = { accounts: [], loggedIn: false, page: '', loaded: new Set(), today: '' };

async function api(path, opts = {}) {
  const res = await fetch(path, {
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', ...(opts.headers || {}) },
    ...opts,
  });
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch { data = { message: text }; }
  if (!res.ok) {
    const err = new Error(data.message || `HTTP ${res.status}`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[c]);
}

function fmtTime(ms) {
  if (!ms) return '-';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getMonth() + 1}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function shortUID(uid) {
  const s = String(uid || '');
  return s.length > 10 ? s.slice(0, 6) + '…' + s.slice(-4) : s;
}

// 积分显示：一万以下用阿拉伯数字，一万及以上用「万」，保留一位小数（去掉多余的 .0）。
//   9600 → 9600 ，15000 → 1.5万 ，20000 → 2万
// 负数（未知余额）按 0 处理，不显示 -1。
function fmtCredits(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v < 0) return '-';
  if (v < 10000) return String(v);
  const w = v / 10000;
  // 四舍五入到一位小数；10000 以下不进位，避免 9999 显示成 1万
  return w.toFixed(1).replace(/\.0$/, '') + '万';
}

const STATUS_LABEL = { online: '在线', cooling: '冷却', exhausted: '积分耗尽', relogin: '需重登' };

// ---------- 登录 ----------
async function login() {
  const token = $('adminToken').value.trim();
  if (!token) return;
  $('loginErr').textContent = '';
  try {
    await api('/api/login', { method: 'POST', body: JSON.stringify({ token }) });
    showPanel();
  } catch (e) {
    $('loginErr').textContent = e.message;
  }
}

$('loginBtn').addEventListener('click', login);
$('adminToken').addEventListener('keydown', (e) => { if (e.key === 'Enter') login(); });
$('logoutBtn').addEventListener('click', () => {
  document.cookie = 'rc_admin=; Max-Age=0; path=/';
  location.reload();
});

// ---------- 添加账号内部 Tab 切换 ----------
document.querySelectorAll('#addTabs .tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('#addTabs .tab').forEach((b) => b.classList.toggle('active', b === btn));
    document.querySelectorAll('.tabpane').forEach((p) => p.classList.toggle('active', p.id === 'pane-' + btn.dataset.tab));
  });
});

// ---------- 顶层标签页切换 ----------
// 每个标签首次进入时才拉取数据；哈希记录当前页，刷新后停在原页。
const PAGE_LOADERS = {
  overview: () => Promise.resolve(),
  accounts: () => loadAccounts(),
  add: () => Promise.resolve(),
  keys: () => loadKeys(),
  logs: () => loadLogs(),
  syslogs: () => loadSysLogs(),
  settings: () => loadSettings(),
  update: () => loadUpdate(),
};

function switchPage(page, { force = false } = {}) {
  if (!PAGE_LOADERS[page]) page = 'overview';
  STATE.page = page;

  document.querySelectorAll('.navtab').forEach((b) => b.classList.toggle('active', b.dataset.page === page));
  document.querySelectorAll('.page').forEach((p) => p.classList.toggle('active', p.id === 'page-' + page));

  if (location.hash.slice(1) !== page) history.replaceState(null, '', '#' + page);
  window.scrollTo(0, 0);

  // 顶部状态条每个页面都要显示，所以每次切页都刷新一次。
  loadStatus();
  if (force || !STATE.loaded.has(page)) {
    // 加载成功才记为已加载：失败（网络抖动、服务重启）下次切回来会重试，
    // 而不是把空白页永久缓存下来。
    STATE.loaded.add(page);
    Promise.resolve()
      .then(() => PAGE_LOADERS[page]())
      .catch(() => STATE.loaded.delete(page));
  }
}

document.querySelectorAll('.navtab').forEach((btn) => {
  btn.addEventListener('click', () => switchPage(btn.dataset.page));
});
window.addEventListener('hashchange', () => switchPage(location.hash.slice(1)));

// ---------- 概览 ----------
async function loadStatus() {
  try {
    const s = await api('/api/status');
    const a = s.accounts || {};
    const sched = s.scheduler || {};
    const renewer = s.renewer || {};
    const chips = [
      `<span class="chip">账号 ${a.total || 0}</span>`,
      `<span class="chip ok">在线 ${a.online || 0}</span>`,
      `<span class="chip warn">冷却 ${a.cooling || 0}</span>`,
      `<span class="chip bad">需重登 ${a.relogin || 0}</span>`,
    ];
    $('chips').innerHTML = chips.join('');
    // 服务端时区的「今天」，号池的「今日签到」用它判断，不用浏览器时区
    STATE.today = (s.checkin || {}).today || '';
    if (STATE.page === 'accounts' && STATE.accounts.length) renderAccounts();

    // 积分总和：只要有一部分账号余额未知，就在括号里标注是按多少个号统计的，
    // 免得用户把「已知部分的和」误当成全池总额。
    const credTotal = a.creditsTotal || 0;
    const credKnown = a.creditsKnown || 0;
    const credText = credKnown === (a.total || 0)
      ? fmtCredits(credTotal)
      : `${fmtCredits(credTotal)}（${credKnown}/${a.total || 0} 个号有数据）`;

    $('overview').innerHTML = [
      ['总账号', a.total || 0],
      ['在线', a.online || 0],
      ['冷却/耗尽', (a.cooling || 0) + (a.exhausted || 0)],
      ['需重登', a.relogin || 0],
      ['积分总和', credText],
      ['今日待签到', (s.checkin || {}).due ?? 0],
      ['选号策略', s.strategy || '-'],
      ['自动签到', sched.enabled ? '开启' : '关闭'],
      ['上次签到', sched.lastResult || '-'],
      ['主动续期', renewer.enabled ? '开启' : '关闭'],
      ['上次续期', renewer.lastRun ? `${fmtTime(renewer.lastRun)} · 续期 ${renewer.renewed || 0}` : '-'],
    ].map(([k, v]) => `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join('');
  } catch (e) {
    if (e.status === 401) showLogin();
  }
}

// ---------- 账号 ----------
async function loadAccounts() {
  try {
    const r = await api('/api/accounts');
    STATE.accounts = r.accounts || [];
    renderAccounts();
  } catch (e) {
    $('tableMsg').textContent = e.message;
  }
}

function renderAccounts() {
  const tbody = $('accTable').querySelector('tbody');
  if (!STATE.accounts.length) {
    tbody.innerHTML = '<tr><td colspan="7" class="empty">还没有账号，去「添加账号」页导入</td></tr>';
    return;
  }
  // 用服务端时区的今天（/api/status 的 checkin.today），否则浏览器与服务器时区不同时会误判「已签」。
  const today = STATE.today;
  tbody.innerHTML = STATE.accounts.map((a) => {
    const st = a.status || 'online';
    // exhausted 且没有到期时间 = 积分低于阈值被暂停，等签到/余额回升，不是 402 硬耗尽
    const lowCredit = st === 'exhausted' && !a.coolUntil;
    const stLabel = lowCredit ? '低积分暂停' : (STATUS_LABEL[st] || esc(st));
    const cool = lowCredit
      ? esc(a.coolReason || '-')
      : (st === 'cooling' || st === 'exhausted'
        ? `${fmtTime(a.coolUntil)} · ${esc(a.coolReason || '')}`
        : esc(a.coolReason || '-'));
    const checked = today && a.lastCheckinDay === today ? '✓ 已签' : (a.lastCheckinDay || '-');
    return `<tr>
      <td>${esc(a.name)}${a.enabled ? '' : ' <span class="tag">停用</span>'}</td>
      <td title="${esc(a.uid)}">${esc(shortUID(a.uid))}</td>
      <td><span class="dot ${esc(st)}"></span>${stLabel}</td>
      <td title="${a.credits >= 0 ? a.credits : ''}">${fmtCredits(a.credits)}</td>
      <td class="small">${cool}</td>
      <td class="small">${esc(checked)}</td>
      <td class="ops">
        <button data-act="balance" data-id="${esc(a.id)}">查余额</button>
        <button data-act="refresh" data-id="${esc(a.id)}">刷新</button>
        <button data-act="checkin" data-id="${esc(a.id)}">签到</button>
        <button data-act="toggle" data-id="${esc(a.id)}">${a.enabled ? '停用' : '启用'}</button>
        <button data-act="delete" data-id="${esc(a.id)}" class="danger">删除</button>
      </td>
    </tr>`;
  }).join('');

  tbody.querySelectorAll('button[data-act]').forEach((btn) => {
    btn.addEventListener('click', () => accountAction(btn.dataset.act, btn.dataset.id));
  });
}

async function accountAction(act, id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  $('tableMsg').textContent = '';
  try {
    if (act === 'delete') {
      if (!confirm(`确认删除账号「${acc.name}」？`)) return;
      await api('/api/accounts/' + encodeURIComponent(id), { method: 'DELETE' });
      $('tableMsg').textContent = '已删除';
    } else if (act === 'toggle') {
      await api('/api/accounts/' + encodeURIComponent(id), {
        method: 'PATCH', body: JSON.stringify({ enabled: !acc.enabled }),
      });
      $('tableMsg').textContent = acc.enabled ? '已停用' : '已启用';
    } else {
      const r = await api(`/api/accounts/${encodeURIComponent(id)}/${act}`, { method: 'POST' });
      $('tableMsg').textContent = r.message || (act === 'balance' ? `积分 ${r.credits}` : '完成');
    }
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    $('tableMsg').textContent = e.message;
  }
}

$('refreshListBtn').addEventListener('click', async () => {
  $('tableMsg').textContent = '';
  await Promise.all([loadAccounts(), loadStatus()]);
});

$('checkinAllBtn').addEventListener('click', async () => {
  if (!confirm('对全部启用账号执行签到？')) return;
  $('tableMsg').textContent = '签到中…';
  try {
    const r = await api('/api/checkin/all', { method: 'POST' });
    $('tableMsg').textContent = `完成：成功 ${r.done || 0} / 共 ${r.total || 0}`;
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    $('tableMsg').textContent = e.message;
  }
});

$('exportBtn').addEventListener('click', () => {
  window.location.href = '/api/accounts/export';
});

// ---------- 一键清理失效账号 ----------
// 先拉一次预览，把会删掉哪些号明明白白列给用户；确认后只提交这份 id 清单，
// 后端会按最新状态复核一次，预览之后被救回来的号不会被误删。
$('purgeDeadBtn').addEventListener('click', async () => {
  $('tableMsg').textContent = '正在检查失效账号…';
  let dead;
  try {
    const r = await api('/api/accounts/dead');
    dead = r.accounts || [];
  } catch (e) {
    $('tableMsg').textContent = e.message;
    return;
  }
  if (!dead.length) {
    $('tableMsg').textContent = '没有失效账号（凭证失效或订阅已过期）';
    return;
  }
  const preview = dead.slice(0, 10)
    .map((a) => `  · ${a.name || shortUID(a.uid)}（${a.reason}）`)
    .join('\n');
  const more = dead.length > 10 ? `\n  … 另有 ${dead.length - 10} 个` : '';
  if (!confirm(`将删除以下 ${dead.length} 个失效账号，不可恢复：\n\n${preview}${more}\n\n继续？`)) {
    $('tableMsg').textContent = '已取消';
    return;
  }
  $('tableMsg').textContent = '删除中…';
  try {
    const r = await api('/api/accounts/dead/purge', {
      method: 'POST', body: JSON.stringify({ ids: dead.map((a) => a.id) }),
    });
    $('tableMsg').textContent = `已删除 ${r.deleted || 0} 个失效账号`
      + (r.skipped ? `，跳过 ${r.skipped} 个（期间已恢复）` : '');
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    $('tableMsg').textContent = e.message;
  }
});

// ---------- 登录 / 添加 ----------
$('openAuthBtn').addEventListener('click', async () => {
  try {
    const r = await api('/api/auth/authorize-url');
    window.open(r.url, '_blank', 'noopener');
    $('redirectHint').textContent = r.redirectEnabled
      ? '已启用回环回调，登录完成后回到本页刷新即可。'
      : '登录完成后请复制浏览器地址栏里的链接，粘贴到下面。';
  } catch (e) {
    $('exchangeMsg').textContent = e.message;
  }
});

$('exchangeBtn').addEventListener('click', async () => {
  const input = $('pasteInput').value.trim();
  if (!input) return;
  $('exchangeMsg').textContent = '兑换中…';
  try {
    const r = await api('/api/auth/exchange', { method: 'POST', body: JSON.stringify({ input }) });
    $('exchangeMsg').textContent = r.message + (r.uid ? `（uid ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      $('pasteInput').value = '';
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) {
    $('exchangeMsg').textContent = e.message;
  }
});

$('manualBtn').addEventListener('click', async () => {
  const body = {
    token: $('mToken').value.trim(),
    refreshToken: $('mRefresh').value.trim(),
    officeIdentity: $('mOffice').value.trim(),
    name: $('mName').value.trim(),
  };
  if (!body.token) { $('manualMsg').textContent = 'access_token 不能为空'; return; }
  $('manualMsg').textContent = '提交中…';
  try {
    const r = await api('/api/auth/manual', { method: 'POST', body: JSON.stringify(body) });
    $('manualMsg').textContent = r.message + (r.uid ? `（uid ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      ['mToken', 'mRefresh', 'mOffice', 'mName'].forEach((id) => { $(id).value = ''; });
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) {
    $('manualMsg').textContent = e.message;
  }
});

$('importFile').addEventListener('change', async (e) => {
  const f = e.target.files && e.target.files[0];
  if (!f) return;
  $('importText').value = await f.text();
});

$('importBtn').addEventListener('click', async () => {
  const raw = $('importText').value.trim();
  if (!raw) return;
  $('importMsg').textContent = '导入中…';
  $('importOut').textContent = '';
  try {
    const r = await api('/api/accounts/import', { method: 'POST', body: raw });
    let msg = `导入完成：成功 ${r.imported || 0} / 共 ${r.total || 0}`;
    if (r.skipped) {
      const parts = Object.entries(r.skippedProviders || {}).map(([k, v]) => `${k} ${v}`);
      msg += `（已过滤 ${r.skipped} 个${parts.length ? '：' + parts.join('、') : '（凭据不全）'}）`;
    }
    $('importMsg').textContent = msg;
    $('importOut').textContent = JSON.stringify(r.results || [], null, 2);
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    $('importMsg').textContent = e.message;
    if (e.data && e.data.results) $('importOut').textContent = JSON.stringify(e.data.results, null, 2);
  }
});

// ---------- Keys ----------
async function loadKeys() {
  try {
    const r = await api('/api/keys');
    const tbody = $('keyTable').querySelector('tbody');
    const keys = r.keys || [];
    if (!keys.length) {
      tbody.innerHTML = '<tr><td colspan="5" class="empty">还没有 API Key</td></tr>';
      return;
    }
    tbody.innerHTML = keys.map((k) => `<tr>
      <td>${esc(k.name)}</td><td><code>${esc(k.prefix)}…</code></td>
      <td class="small">${fmtTime(k.createdAt)}</td>
      <td class="small">${k.lastUsed ? fmtTime(k.lastUsed) : '从未'}</td>
      <td><button data-id="${esc(k.id)}" class="danger delkey">吊销</button></td>
    </tr>`).join('');
    tbody.querySelectorAll('.delkey').forEach((b) => {
      b.addEventListener('click', async () => {
        if (!confirm('吊销这个 Key？')) return;
        await api('/api/keys/' + encodeURIComponent(b.dataset.id), { method: 'DELETE' });
        loadKeys();
      });
    });
  } catch (e) {
    $('newKeyOut').textContent = e.message;
  }
}

$('createKeyBtn').addEventListener('click', async () => {
  try {
    const r = await api('/api/keys', { method: 'POST', body: JSON.stringify({ name: $('keyName').value.trim() }) });
    $('newKeyOut').textContent = '新 Key（只显示这一次）：\n' + r.key;
    $('keyName').value = '';
    loadKeys();
  } catch (e) {
    $('newKeyOut').textContent = e.message;
  }
});

// ---------- 设置 ----------
async function loadSettings() {
  try {
    const r = await api('/api/settings');
    const s = r.settings || {};
    $('setStrategy').value = s.strategy === 'round_robin' ? 'round_robin' : 'load_balance';
    $('setCheckinEnable').value = String(!!s.checkinEnable);
    $('setHour').value = s.checkinHour ?? 0;
    $('setMinute').value = s.checkinMinute ?? 10;
    $('setJitter').value = s.checkinJitterMinutes ?? 30;
    $('setRetryUntilExhausted').value = String(!!s.retryUntilExhausted);
    $('setMaxRetries').value = s.maxRetries ?? 3;
    $('setLogFileEnable').value = String(!!s.logFileEnable);
    $('setLogDir').value = s.logDir || '';
    $('setLogMaxSizeMB').value = s.logMaxSizeMB ?? 32;
    $('setLogMaxFiles').value = s.logMaxFiles ?? 10;
    $('setLogFileRetainDays').value = s.logFileRetainDays ?? 7;
    $('setLogDBRetainDays').value = s.logDBRetainDays ?? 30;
    $('setLogCleanEnable').value = String(!!s.logCleanEnable);
    $('setLogCleanIntervalMin').value = s.logCleanIntervalMin ?? 60;
    // 后端以秒/分钟存，界面按「天/小时」显示，方便直接读
    $('setRenewWindowDays').value = Math.round(((s.renewWindowSec ?? 259200) / 86400) * 100) / 100;
    $('setRenewIntervalHours').value = Math.round(((s.renewIntervalMin ?? 360) / 60) * 10) / 10;
    $('setRenewGapMS').value = s.renewGapMS ?? 1000;
    $('setModelCooldown').value = String(s.modelCooldown !== false);
    $('setCreditStopEnable').value = String(!!s.creditStopEnable);
    $('setCreditStopBelow').value = s.creditStopBelow ?? 0;
    $('setCreditResumeAbove').value = s.creditResumeAbove ?? 0;
  } catch (e) {
    $('settingsMsg').textContent = e.message;
  }
}

// 冷却设置单独保存：只提交冷却与积分阈值字段，避免和上面的运行设置互相覆盖。
$('saveCooldownBtn').addEventListener('click', async () => {
  const stopBelow = numOrNaN($('setCreditStopBelow'));
  const resumeAbove = numOrNaN($('setCreditResumeAbove'));
  if (!Number.isFinite(stopBelow) || !Number.isFinite(resumeAbove) || stopBelow < 0 || resumeAbove < 0) {
    $('cooldownMsg').textContent = '积分阈值请填写非负整数';
    return;
  }
  if (resumeAbove <= stopBelow) {
    // 相等也不行：暂停与解除会同时成立，账号状态每分钟翻一次。
    $('cooldownMsg').textContent = '解除阈值必须大于暂停阈值';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        modelCooldown: $('setModelCooldown').value === 'true',
        creditStopEnable: $('setCreditStopEnable').value === 'true',
        creditStopBelow: Math.round(stopBelow),
        creditResumeAbove: Math.round(resumeAbove),
      }),
    });
    $('cooldownMsg').textContent = '已保存，立即生效';
    await loadSettings();
    // 保存后账号状态可能立刻变化（被暂停/恢复），两个列表都刷一遍
    if (STATE.loaded.has('accounts')) loadAccounts();
    loadStatus();
  } catch (e) {
    $('cooldownMsg').textContent = e.message;
  }
});

// numOrNaN 把输入框转成数字；空串要当成缺失而不是 0——
// Number('') === 0 会把「清空输入」静默变成「关闭续期 / 签到时间归零」。
function numOrNaN(el) {
  const raw = String(el.value).trim();
  if (raw === '') return NaN;
  return Number(raw);
}

$('saveSettingsBtn').addEventListener('click', async () => {
  const hour = numOrNaN($('setHour'));
  const minute = numOrNaN($('setMinute'));
  const jitter = numOrNaN($('setJitter'));
  const maxRetries = numOrNaN($('setMaxRetries'));
  if ([hour, minute, jitter, maxRetries].some((n) => !Number.isFinite(n))) {
    $('settingsMsg').textContent = '签到时间与重试次数不能留空';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        strategy: $('setStrategy').value,
        checkinEnable: $('setCheckinEnable').value === 'true',
        checkinHour: Math.round(hour),
        checkinMinute: Math.round(minute),
        checkinJitterMinutes: Math.round(jitter),
        retryUntilExhausted: $('setRetryUntilExhausted').value === 'true',
        maxRetries: Math.round(maxRetries),
      }),
    });
    $('settingsMsg').textContent = '已保存';
    await loadSettings();
    loadStatus();
  } catch (e) {
    $('settingsMsg').textContent = e.message;
  }
});

// 续期设置单独保存：只提交续期字段，避免和上面的运行设置互相覆盖。
$('saveRenewSettingsBtn').addEventListener('click', async () => {
  const days = numOrNaN($('setRenewWindowDays'));
  const hours = numOrNaN($('setRenewIntervalHours'));
  const gap = numOrNaN($('setRenewGapMS'));
  if (!Number.isFinite(days) || !Number.isFinite(hours) || !Number.isFinite(gap)) {
    $('renewSettingsMsg').textContent = '请填写有效的数字（提前量/间隔填 0 表示关闭）';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        renewWindowSec: Math.round(days * 86400),
        renewIntervalMin: Math.round(hours * 60),
        renewGapMS: Math.round(gap),
      }),
    });
    $('renewSettingsMsg').textContent = '已保存，立即生效';
    await loadSettings();
    loadStatus();
  } catch (e) {
    $('renewSettingsMsg').textContent = e.message;
  }
});

$('runRenewBtn').addEventListener('click', async () => {
  $('renewSettingsMsg').textContent = '续期中…';
  try {
    const r = await api('/api/renew/run', { method: 'POST' });
    $('renewSettingsMsg').textContent =
      `检查 ${r.checked || 0}，续期 ${r.renewed || 0}，失败 ${r.failed || 0}，恢复 ${r.recovered || 0}`;
    await loadAccounts();
    loadStatus();
  } catch (e) {
    $('renewSettingsMsg').textContent = e.message;
  }
});

// 日志设置单独保存：只提交日志相关字段，避免和上面的运行设置互相覆盖。
$('saveLogSettingsBtn').addEventListener('click', async () => {
  const nums = ['setLogMaxSizeMB', 'setLogMaxFiles', 'setLogFileRetainDays',
    'setLogDBRetainDays', 'setLogCleanIntervalMin'].map((id) => numOrNaN($(id)));
  if (nums.some((n) => !Number.isFinite(n))) {
    $('logSettingsMsg').textContent = '日志数字项不能留空';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        logFileEnable: $('setLogFileEnable').value === 'true',
        logDir: $('setLogDir').value.trim(),
        logMaxSizeMB: Math.round(nums[0]),
        logMaxFiles: Math.round(nums[1]),
        logFileRetainDays: Math.round(nums[2]),
        logDBRetainDays: Math.round(nums[3]),
        logCleanIntervalMin: Math.round(nums[4]),
        logCleanEnable: $('setLogCleanEnable').value === 'true',
      }),
    });
    $('logSettingsMsg').textContent = '已保存';
    // 配置可能被后端归一化（比如空目录回落到默认），重新拉一次以显示实际生效值
    await loadSettings();
    if (STATE.loaded.has('syslogs')) loadSysLogs();
  } catch (e) {
    $('logSettingsMsg').textContent = e.message;
  }
});

// ---------- 日志 ----------
async function loadLogs() {
  try {
    const r = await api('/api/logs');
    const tbody = $('logTable').querySelector('tbody');
    const logs = r.logs || [];
    if (!logs.length) {
      tbody.innerHTML = '<tr><td colspan="7" class="empty">还没有调用记录</td></tr>';
      return;
    }
    tbody.innerHTML = logs.map((l) => `<tr>
      <td class="small">${fmtTime(l.TS)}</td>
      <td>${esc(l.API)}</td>
      <td class="small">${esc(l.Model)}</td>
      <td class="${l.Status >= 400 ? 'bad' : 'ok'}">${l.Status}</td>
      <td class="small">${l.TotalTokens || 0}</td>
      <td class="small">${l.MS || 0}ms</td>
      <td class="small err">${esc(l.Error || '')}</td>
    </tr>`).join('');
  } catch (e) {
    $('logTableMsg').textContent = e.message;
  }
}

$('loadLogsBtn').addEventListener('click', loadLogs);

// ---------- 运行日志（文件 + 自动清理） ----------
function fmtSize(n) {
  const v = Number(n) || 0;
  if (v < 1024) return v + ' B';
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + ' KB';
  return (v / 1024 / 1024).toFixed(2) + ' MB';
}

async function loadSysLogs() {
  try {
    const r = await api('/api/logs/manage');
    if (!r.ok) { $('syslogMsg').textContent = r.message || '日志管理未启用'; return; }
    const s = r.status || {};
    const c = s.config || {};

    $('logStats').innerHTML = [
      ['日志目录', c.logDir || '-'],
      ['文件日志', c.logFileEnable ? '开启' : '关闭'],
      ['日志文件', `${s.fileCount || 0} 个 · ${fmtSize(s.fileBytes)}`],
      ['单文件上限', `${c.logMaxSizeMB || 0} MB`],
      ['调用流水', s.usageCount >= 0 ? `${s.usageCount || 0} 条` : '未知'],
      ['签到记录', s.checkinCount >= 0 ? `${s.checkinCount || 0} 条` : '未知'],
      ['自动清理', c.logCleanEnable ? `开启 · 每 ${c.logCleanIntervalMin} 分钟` : '关闭'],
      ['保留策略', `文件 ${c.logFileRetainDays || 0} 天 / 流水 ${c.logDBRetainDays || 0} 天`],
    ].map(([k, v]) => `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join('');

    const lc = s.lastCleanResult || {};
    if (s.lastClean) {
      const parts = [`上次清理 ${fmtTime(s.lastClean)}`];
      parts.push(`删除文件 ${lc.removedFiles || 0} 个（${fmtSize(lc.freedBytes)}）`);
      parts.push(`流水 ${lc.removedUsage || 0} 条`);
      parts.push(`签到 ${lc.removedCheckin || 0} 条`);
      if (lc.error) parts.push('错误：' + lc.error);
      $('logLastClean').textContent = parts.join(' · ');
    } else {
      $('logLastClean').textContent = '尚未运行过清理';
    }

    const tbody = $('logFileTable').querySelector('tbody');
    const files = s.files || [];
    if (!files.length) {
      tbody.innerHTML = '<tr><td colspan="4" class="empty">日志目录里还没有文件</td></tr>';
      return;
    }
    tbody.innerHTML = files.map((f) => `<tr>
      <td>${esc(f.name)}${f.current ? ' <span class="tag">当前</span>' : ''}</td>
      <td class="small">${fmtSize(f.size)}</td>
      <td class="small">${fmtTime(f.modTime)}</td>
      <td><a href="/api/logs/files?download=${encodeURIComponent(f.name)}">下载</a></td>
    </tr>`).join('');
  } catch (e) {
    $('syslogMsg').textContent = e.message;
  }
}

$('refreshSyslogsBtn').addEventListener('click', async () => {
  $('syslogMsg').textContent = '';
  await loadSysLogs();
});

$('cleanLogsBtn').addEventListener('click', async () => {
  if (!confirm('立即按当前策略清理过期日志与流水？')) return;
  $('syslogMsg').textContent = '清理中…';
  try {
    const r = await api('/api/logs/clean', { method: 'POST' });
    const x = r.result || {};
    $('syslogMsg').textContent =
      `清理完成：文件 ${x.removedFiles || 0} 个（${fmtSize(x.freedBytes)}），` +
      `调用流水 ${x.removedUsage || 0} 条，签到记录 ${x.removedCheckin || 0} 条` +
      (x.error ? `；错误：${x.error}` : '');
    await loadSysLogs();
  } catch (e) {
    $('syslogMsg').textContent = e.message;
  }
});

// ---------- 版本更新 ----------
const UPDATE = { status: null, installing: false };

function fmtUpdateTime(ms) {
  if (!ms) return '-';
  return fmtTime(ms);
}

function renderUpdate() {
  const s = UPDATE.status || {};
  const modeText = s.mode === 'docker' ? 'Docker 容器' : '裸机二进制';
  const latestText = s.latest
    ? (s.updateAvailable ? `${esc(s.latest)}（可更新）` : `${esc(s.latest)}（已是最新）`)
    : (s.error ? '检查失败' : '未检查');

  $('updateGrid').innerHTML = [
    ['当前版本', esc(s.current || '-')],
    ['最新版本', latestText],
    ['运行方式', esc(modeText)],
    ['安装包', esc(s.asset || '-')],
    ['检查时间', esc(fmtUpdateTime(s.checkedAt))],
    ['安装时间', esc(fmtUpdateTime(s.installedAt))],
    ['自动重启', s.autoRestart ? '支持' : '需手动'],
    ['更新备份', s.backupAvailable ? '有，可回滚' : '无'],
  ].map(([k, v]) => `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${v}</div></div>`).join('');

  $('installUpdateBtn').hidden = !(s.updateAvailable && s.canSelfUpdate) || UPDATE.installing;
  $('rollbackBtn').hidden = !s.backupAvailable || UPDATE.installing;
  $('checkUpdateBtn').disabled = UPDATE.installing;

  const notes = s.notes || '';
  $('updateNotes').hidden = !notes;
  $('updateNotesBody').textContent = notes;

  let hint = '';
  if (s.error) hint = `检查失败：${s.error}`;
  else if (s.updateAvailable) {
    hint = s.canSelfUpdate
      ? '点「一键更新」会下载当前平台安装包并做 sha256 校验，完成后自动重启。'
      : '当前目录不可写，无法自更新。请确认数据目录权限，或用 UPDATE_TARGET 指定可写路径。';
  } else if (s.latest) {
    hint = '当前已是最新版本。';
  }
  if (s.mode === 'docker' && s.updateAvailable) {
    hint += ' 容器内更新落在数据卷 /data/bin/relay，重建容器后仍生效；想回到镜像版本删掉该文件即可。';
  }
  $('updateHint').textContent = hint;
}

async function loadUpdate() {
  $('updateErr').textContent = '';
  try {
    const r = await api('/api/update');
    UPDATE.status = r.status || { current: r.current, disabled: r.disabled };
    renderUpdate();
  } catch (e) {
    $('updateErr').textContent = e.message;
  }
}

$('checkUpdateBtn').addEventListener('click', async () => {
  $('updateMsg').textContent = '检查中…';
  $('updateErr').textContent = '';
  try {
    const r = await api('/api/update/check', { method: 'POST' });
    UPDATE.status = r.status || {};
    $('updateMsg').textContent = r.ok
      ? (UPDATE.status.updateAvailable ? `发现新版本 ${UPDATE.status.latest}` : '已是最新版本')
      : (r.message || '检查失败');
    renderUpdate();
  } catch (e) {
    $('updateMsg').textContent = '';
    $('updateErr').textContent = e.message;
  }
});

$('installUpdateBtn').addEventListener('click', async () => {
  const s = UPDATE.status || {};
  if (!confirm(`确认更新到 ${s.latest}？服务会在安装完成后自动重启，期间短暂中断。`)) return;
  UPDATE.installing = true;
  renderUpdate();
  $('updateMsg').textContent = '正在下载并校验安装包…';
  $('updateErr').textContent = '';
  try {
    const r = await api('/api/update/install', { method: 'POST' });
    $('updateMsg').textContent = r.message || '安装完成';
    UPDATE.status = r.status || s;
    // 安装完成后按钮必须一直锁住：哪怕服务没重启成功，也得刷新页面才能再点，
    // 否则用户会在等待重启的窗口里反复触发下载。
    UPDATE.installing = true;
    renderUpdate();
    if (!UPDATE.status.autoRestart) {
      // 不支持自动重启的平台：服务还在跑旧进程，告知手动重启即可，不轮询。
      UPDATE.installing = false;
      renderUpdate();
      $('updateMsg').textContent = (r.message || '安装完成') + ' 请手动重启服务。';
      return;
    }
    $('updateMsg').textContent = '已安装，等待服务重启…';
    await waitForRestart(s.latest);
    UPDATE.installing = false;
    renderUpdate();
  } catch (e) {
    UPDATE.installing = false;
    renderUpdate();
    $('updateMsg').textContent = '';
    $('updateErr').textContent = e.message;
  }
});

$('rollbackBtn').addEventListener('click', async () => {
  if (!confirm('回滚到上一个版本并重启？')) return;
  UPDATE.installing = true;
  renderUpdate();
  try {
    const r = await api('/api/update/rollback', { method: 'POST' });
    $('updateMsg').textContent = r.message || '已回滚';
    if (r.status) UPDATE.status = r.status;
    if (!(UPDATE.status || {}).autoRestart) {
      // 不支持自动重启：不要轮询（旧进程的 /health 会立刻应答，造成误判），
      // 也没有「新版本号」可比对，直接把按钮恢复可用。
      UPDATE.installing = false;
      renderUpdate();
      $('updateMsg').textContent = (r.message || '已回滚') + ' 请手动重启服务。';
      return;
    }
    await waitForRestart(null);
    UPDATE.installing = false;
    renderUpdate();
  } catch (e) {
    UPDATE.installing = false;
    renderUpdate();
    $('updateErr').textContent = e.message;
  }
});

// waitForRestart 轮询 /health，等到服务恢复后重载页面。
async function waitForRestart(expected) {
  const deadline = Date.now() + 90000;
  await new Promise((res) => setTimeout(res, 1500));
  while (Date.now() < deadline) {
    try {
      const res = await fetch('/health', { cache: 'no-store' });
      if (res.ok) {
        const st = await api('/api/update').catch(() => null);
        if (st && st.status) UPDATE.status = st.status;
        const nowVersion = (UPDATE.status || {}).current;
        if (!expected || nowVersion === expected) {
          location.reload();
          return;
        }
      }
    } catch { /* 重启窗口内连接失败是预期内的 */ }
    await new Promise((res) => setTimeout(res, 1500));
  }
  $('updateMsg').textContent = expected
    ? '服务已恢复，但版本号未变化：可能是重启没走到新二进制，请检查容器/进程。'
    : '服务未在预期时间内恢复，请手动检查容器/进程状态。';
}

// ---------- 启动 ----------
function showLogin() {
  $('loginCard').hidden = false;
  $('app').hidden = true;
  $('logoutBtn').hidden = true;
  // 重新登录后旧会话的数据已经作废，清掉「已加载」标记让它重新拉。
  STATE.loaded.clear();
}

function showPanel() {
  STATE.loggedIn = true;
  $('loginCard').hidden = true;
  $('app').hidden = false;
  $('logoutBtn').hidden = false;
  switchPage(location.hash.slice(1));
}

// 已登录（cookie 有效）则直接进面板
(async () => {
  try {
    await api('/api/status');
    showPanel();
  } catch {
    showLogin();
  }
})();
