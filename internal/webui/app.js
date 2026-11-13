'use strict';

const $ = (id) => document.getElementById(id);
const STATE = { accounts: [], loggedIn: false };

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
    STATE.loggedIn = true;
    $('loginCard').hidden = true;
    $('app').hidden = false;
    $('logoutBtn').hidden = false;
    loadAll();
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

// ---------- Tab 切换 ----------
document.querySelectorAll('.tab').forEach((btn) => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.tab').forEach((b) => b.classList.toggle('active', b === btn));
    document.querySelectorAll('.tabpane').forEach((p) => p.classList.toggle('active', p.id === 'pane-' + btn.dataset.tab));
  });
});

// ---------- 概览 ----------
async function loadStatus() {
  try {
    const s = await api('/api/status');
    const a = s.accounts || {};
    const sched = s.scheduler || {};
    const chips = [
      `<span class="chip">账号 ${a.total || 0}</span>`,
      `<span class="chip ok">在线 ${a.online || 0}</span>`,
      `<span class="chip warn">冷却 ${a.cooling || 0}</span>`,
      `<span class="chip bad">需重登 ${a.relogin || 0}</span>`,
    ];
    $('chips').innerHTML = chips.join('');

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
    tbody.innerHTML = '<tr><td colspan="7" class="empty">还没有账号，先在上面添加一个</td></tr>';
    return;
  }
  const today = new Date().toLocaleDateString('sv-SE'); // yyyy-mm-dd
  tbody.innerHTML = STATE.accounts.map((a) => {
    const st = a.status || 'online';
    const cool = st === 'cooling' || st === 'exhausted'
      ? `${fmtTime(a.coolUntil)} · ${esc(a.coolReason || '')}`
      : esc(a.coolReason || '-');
    const checked = a.lastCheckinDay === today ? '✓ 已签' : (a.lastCheckinDay || '-');
    return `<tr>
      <td>${esc(a.name)}${a.enabled ? '' : ' <span class="tag">停用</span>'}</td>
      <td title="${esc(a.uid)}">${esc(shortUID(a.uid))}</td>
      <td><span class="dot ${st}"></span>${STATUS_LABEL[st] || esc(st)}</td>
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

$('refreshListBtn').addEventListener('click', () => { loadAccounts(); loadStatus(); });

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
    $('importMsg').textContent = `导入完成：成功 ${r.imported || 0} / 共 ${r.total || 0}`;
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
    $('setStrategy').value = s.strategy || 'optimal';
    $('setCheckinEnable').value = String(!!s.checkinEnable);
    $('setHour').value = s.checkinHour ?? 0;
    $('setMinute').value = s.checkinMinute ?? 10;
    $('setJitter').value = s.checkinJitterMinutes ?? 30;
  } catch (e) {
    $('settingsMsg').textContent = e.message;
  }
}

$('saveSettingsBtn').addEventListener('click', async () => {
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        strategy: $('setStrategy').value,
        checkinEnable: $('setCheckinEnable').value === 'true',
        checkinHour: Number($('setHour').value),
        checkinMinute: Number($('setMinute').value),
        checkinJitterMinutes: Number($('setJitter').value),
      }),
    });
    $('settingsMsg').textContent = '已保存';
    loadStatus();
  } catch (e) {
    $('settingsMsg').textContent = e.message;
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
    $('tableMsg').textContent = e.message;
  }
}

$('loadLogsBtn').addEventListener('click', loadLogs);

// ---------- 启动 ----------
function showLogin() {
  $('loginCard').hidden = false;
  $('app').hidden = true;
  $('logoutBtn').hidden = true;
}

async function loadAll() {
  await Promise.all([loadStatus(), loadAccounts(), loadKeys(), loadSettings(), loadLogs()]);
}

// 已登录（cookie 有效）则直接进面板
(async () => {
  try {
    await api('/api/status');
    STATE.loggedIn = true;
    $('loginCard').hidden = true;
    $('app').hidden = false;
    $('logoutBtn').hidden = false;
    loadAll();
  } catch {
    showLogin();
  }
})();
