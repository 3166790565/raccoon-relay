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

// uid 形如 pu-01a1d03b-c02e-…，面板上只要分辨得出是哪个号，取首尾即可。
// 截得太短（6+4）会让同一批导入的号看起来一模一样，所以多留几位。
function shortUID(uid) {
  const s = String(uid || '');
  return s.length > 18 ? s.slice(0, 12) + '…' + s.slice(-4) : s;
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
  logs: () => ensureLogFacets().then(() => loadLogs()),
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
// 号池页的筛选、勾选状态都放在 STATE.pool 里：账号列表会被定时/手动刷新，
// 刷新不该把用户正在看的筛选条件和勾选清掉。
const POOL = {
  status: 'all', cool: 'all', q: '', sel: new Set(), fold: false,
  peakCredits: 1, creditStopBelow: 0,
};

async function loadAccounts() {
  try {
    const r = await api('/api/accounts');
    STATE.accounts = r.accounts || [];
    // 账号被删后它的 id 不该继续留在勾选里，否则批量操作会一直报「跳过 1 个」。
    const alive = new Set(STATE.accounts.map((a) => a.id));
    POOL.sel.forEach((id) => { if (!alive.has(id)) POOL.sel.delete(id); });
    renderAccounts();
  } catch (e) {
    $('tableMsg').textContent = e.message;
  }
}

// 限流相关：账号级冷却 与 模型级罚单 是两回事。
// 状态列只反映账号级；「限流」列汇总模型级罚单，两者都算「已限流」。
function modelCoolsOf(a) {
  return Array.isArray(a.modelCools) ? a.modelCools : [];
}

function isLimited(a) {
  const st = a.status || 'online';
  if (st === 'cooling' || st === 'exhausted' || st === 'relogin') return true;
  if (a.coolUntil > Date.now()) return true;
  return modelCoolsOf(a).some((m) => m.until > Date.now());
}

function filteredAccounts() {
  const q = POOL.q.trim().toLowerCase();
  return STATE.accounts.filter((a) => {
    if (POOL.status === 'enabled' && !a.enabled) return false;
    if (POOL.status === 'disabled' && a.enabled) return false;
    if (POOL.cool === 'normal' && isLimited(a)) return false;
    if (POOL.cool === 'cooling' && !isLimited(a)) return false;
    if (!q) return true;
    return [a.name, a.uid, a.note, a.platformUid].some((v) => String(v || '').toLowerCase().includes(q));
  });
}

// 剩余时间：30 分钟以内显示「还有 N 分钟」，否则显示到几点几分。
function fmtLeft(ms) {
  const d = ms - Date.now();
  if (d <= 0) return '已到期';
  if (d < 60 * 60 * 1000) return `还有 ${Math.max(1, Math.ceil(d / 60000))} 分钟`;
  return fmtTime(ms) + ' 恢复';
}

// 有效期：把 expires_at 显示成「N 天后」。没有订阅到期时间的号（多数个人号）
// 用 token 到期做兜底，否则这一列会对一大半的号显示「-」。
function expireInfo(a) {
  const now = Date.now();
  if (a.expiresAt > 0) {
    const days = Math.ceil((a.expiresAt - now) / 86400000);
    if (days < 0) return { text: '已过期', cls: 'bad' };
    return { text: days === 0 ? '今天到期' : `${days} 天后`, cls: days <= 3 ? 'warn' : '' };
  }
  if (a.tokenExp > 0) {
    const days = Math.ceil((a.tokenExp - now) / 86400000);
    if (days < 0) return { text: '令牌过期', cls: 'bad' };
    return { text: `${days} 天后`, cls: '', title: `access_token 到期：${fmtTime(a.tokenExp)}` };
  }
  return { text: '-', cls: '' };
}

// 余额列：和参考图一样是「额度条 + 可用值」两行。
// 小浣熊的号没有套餐上限可查，所以上面的条表示余额占全池最高余额的比例
// （用来一眼看出哪些号快见底），下面那行才是真正的数字。
function renderQuotaCell(a) {
  const credits = a.credits;
  const known = credits >= 0;
  const peak = POOL.peakCredits || 1;
  const pct = known ? Math.max(2, Math.min(100, (credits / peak) * 100)) : 0;
  const low = known && POOL.creditStopBelow > 0 && credits <= POOL.creditStopBelow;
  return `<div class="quota">
    <div class="quota-row">
      <span class="q-label">${known ? '余额' : '未知'}</span>
      <span class="q-val ${low ? 'bad' : known ? 'ok' : 'dim'}">${known ? fmtCredits(credits) + ' 积分' : '未查询'}</span>
    </div>
    <span class="mini-bar ${low ? 'low' : ''}"><i style="width:${pct}%"></i></span>
  </div>`;
}

function renderAccounts() {
  // 余额条的参照系：全池已知余额的最大值。每次渲染前算一次，
  // 避免每行都去遍历一遍列表。
  POOL.peakCredits = STATE.accounts.reduce((m, a) => (a.credits > m ? a.credits : m), 0) || 1;
  renderPoolStats();
  const tbody = $('accTable').querySelector('tbody');
  const rows = filteredAccounts();
  const total = STATE.accounts.length;

  $('poolEmpty').hidden = rows.length > 0 || total === 0;
  $('poolEmpty').textContent = total === 0 ? '还没有账号，去「添加账号」页导入' : '当前筛选下没有账号';
  $('poolFootInfo').textContent = total === 0
    ? '共 0 条'
    : `共 ${total} 条${rows.length === total ? '（全部显示）' : ` · 筛选出 ${rows.length} 条`}`;

  if (!rows.length) {
    tbody.innerHTML = '';
    syncSelUI();
    return;
  }

  tbody.innerHTML = rows.map((a, i) => {
    const st = a.status || 'online';
    const lowCredit = st === 'exhausted' && !a.coolUntil;
    const stLabel = lowCredit ? '低积分' : (STATUS_LABEL[st] || esc(st));
    const cools = modelCoolsOf(a).filter((m) => m.until > Date.now());
    const exp = expireInfo(a);
    const checked = POOL.sel.has(a.id);

    // 限流列：账号级冷却显示原因 + 剩余时间；只有模型级罚单时显示「N 个模型」。
    let coolCell;
    if (st === 'cooling' || (st === 'exhausted' && a.coolUntil)) {
      coolCell = `<div class="coolbox warn">
        <span class="cb-main">${esc(a.coolReason || '限流中')}</span>
        <span class="cb-sub">${esc(fmtLeft(a.coolUntil || 0))}</span>
      </div>`;
    } else if (lowCredit) {
      coolCell = `<div class="coolbox bad">
        <span class="cb-main">${esc(a.coolReason || '积分低于阈值')}</span>
        <span class="cb-sub">待余额恢复</span>
      </div>`;
    } else if (st === 'relogin') {
      coolCell = `<div class="coolbox bad"><span class="cb-main">凭证失效</span><span class="cb-sub">需重新登录</span></div>`;
    } else if (cools.length) {
      coolCell = `<div class="coolbox info" title="点击查看限流明细">
        <span class="cb-main">${cools.length} 个模型</span>
        <span class="cb-sub">最早 ${esc(fmtLeft(Math.min(...cools.map((m) => m.until))))}</span>
      </div>`;
    } else if (a.deadReason) {
      coolCell = `<div class="coolbox bad"><span class="cb-main">${esc(a.deadReason)}</span></div>`;
    } else {
      coolCell = '<span class="dim">—</span>';
    }

    return `<tr data-id="${esc(a.id)}" class="${checked ? 'sel' : ''}">
      <td class="c-check"><input type="checkbox" class="row-check" ${checked ? 'checked' : ''}></td>
      <td class="c-idx"><span class="idx">#${i + 1}</span></td>
      <td class="c-name">
        <div class="acc-name" title="${esc(a.uid)}">${esc(a.name || shortUID(a.uid))}</div>
        <div class="acc-sub">${esc(shortUID(a.uid))}${a.note ? ' · ' + esc(a.note) : ''}</div>
        ${a.deadReason ? `<div class="acc-sub bad">${esc(a.deadReason)}</div>` : ''}
      </td>
      <td class="c-enable">
        <label class="switch" title="${a.enabled ? '点击停用' : '点击启用'}">
          <input type="checkbox" class="row-enable" ${a.enabled ? 'checked' : ''}>
          <span></span>
        </label>
      </td>
      <td class="c-status"><span class="pill ${esc(st)}${lowCredit ? ' lowcredit' : ''}">
        <i class="dot"></i>${stLabel}</span></td>
      <td class="c-cool">${coolCell}</td>
      <td class="c-expire"><span class="${exp.cls}" ${exp.title ? `title="${esc(exp.title)}"` : ''}>${esc(exp.text)}</span></td>
      <td class="c-balance" title="${a.creditsAt ? '余额更新于 ' + fmtTime(a.creditsAt) : ''}">${renderQuotaCell(a)}</td>
      <td class="c-ops">
        <button class="ghost tiny" data-act="balance">余额</button>
        <button class="ghost tiny" data-act="settings">设置</button>
        <button class="ghost tiny more" data-act="more" title="更多操作">…</button>
      </td>
    </tr>`;
  }).join('');

  bindRowEvents(tbody);
  syncSelUI();
}

function bindRowEvents(tbody) {
  tbody.querySelectorAll('tr[data-id]').forEach((tr) => {
    const id = tr.dataset.id;

    tr.querySelector('.row-check').addEventListener('change', (e) => {
      if (e.target.checked) POOL.sel.add(id); else POOL.sel.delete(id);
      tr.classList.toggle('sel', e.target.checked);
      syncSelUI();
    });

    tr.querySelector('.row-enable').addEventListener('change', async (e) => {
      const want = e.target.checked;
      try {
        await api('/api/accounts/' + encodeURIComponent(id), {
          method: 'PATCH', body: JSON.stringify({ enabled: want }),
        });
        const acc = STATE.accounts.find((a) => a.id === id);
        if (acc) acc.enabled = want;
        toast(want ? '已启用' : '已停用');
      } catch (err) {
        e.target.checked = !want; // 失败回滚开关，别让界面显示成已生效
        toast(err.message, true);
      }
      // 开关是乐观更新：先按本地状态重画统计，不重新拉列表，避免正在看的位置被刷掉。
      renderPoolStats();
    });

    tr.querySelectorAll('button[data-act]').forEach((btn) => {
      btn.addEventListener('click', (ev) => {
        ev.stopPropagation();
        const act = btn.dataset.act;
        if (act === 'more') openRowMenu(btn, id);
        else if (act === 'settings') openAccountSettings(id);
        else accountAction(act, id);
      });
    });

    const coolBox = tr.querySelector('.coolbox.info');
    if (coolBox) {
      coolBox.addEventListener('click', () => openCoolDetail(coolBox, id));
    }
  });
}

// 顶部筛选按钮上的数字：一直反映全量统计，不随筛选结果变化。
function renderPoolStats() {
  const total = STATE.accounts.length;
  const enabled = STATE.accounts.filter((a) => a.enabled).length;
  const limited = STATE.accounts.filter(isLimited).length;
  const setB = (sel, vals) => {
    const btns = document.querySelectorAll(sel + ' button');
    btns.forEach((b, i) => {
      const b2 = b.querySelector('b');
      if (b2) b2.textContent = vals[i] ?? 0;
    });
  };
  setB('#pfStatus', [total, enabled, total - enabled]);
  setB('#pfCool', [total, total - limited, limited]);
}

function syncSelUI() {
  const rows = filteredAccounts();
  const selCount = POOL.sel.size;
  const allChecked = rows.length > 0 && rows.every((a) => POOL.sel.has(a.id));
  $('selAll').checked = allChecked;
  $('selAllHead').checked = allChecked;
  $('selAll').indeterminate = !allChecked && rows.some((a) => POOL.sel.has(a.id));
  $('selAllHead').indeterminate = $('selAll').indeterminate;
  $('selInfo').textContent = selCount
    ? `已选 ${selCount} 个 · 共 ${rows.length} 个`
    : `共 ${rows.length} 个`;
  $('clearSelBtn').disabled = selCount === 0;
  $('batchToggleBtn').disabled = selCount === 0;
}

function setSelectAll(on) {
  const rows = filteredAccounts();
  if (on) rows.forEach((a) => POOL.sel.add(a.id));
  else rows.forEach((a) => POOL.sel.delete(a.id));
  renderAccounts();
}

['selAll', 'selAllHead'].forEach((id) => {
  $(id).addEventListener('change', (e) => setSelectAll(e.target.checked));
});
$('clearSelBtn').addEventListener('click', () => { POOL.sel.clear(); renderAccounts(); });

// 筛选条：点击切换，再点一次回到「全部」。
const FILTERS = {
  pfStatus: 'status',
  pfCool: 'cool',
};
Object.entries(FILTERS).forEach(([elId, key]) => {
  $(elId).addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    const v = btn.dataset.v;
    POOL[key] = POOL[key] === v && v !== 'all' ? 'all' : v;
    $(elId).querySelectorAll('button').forEach((b) => b.classList.toggle('on', b.dataset.v === POOL[key]));
    renderAccounts();
  });
});

$('pfSearch').addEventListener('input', (e) => { POOL.q = e.target.value; renderAccounts(); });
$('foldFiltersBtn').addEventListener('click', () => {
  POOL.fold = !POOL.fold;
  $('poolToolbar').classList.toggle('folded', POOL.fold);
  $('foldFiltersBtn').textContent = POOL.fold ? '展开' : '折叠';
});

// ---------- 弹层：限流明细 / 更多操作 / 账号设置 ----------
// 用一个共享的浮层，点击别处或按 Esc 关闭；浮层挂在 body 上避免被表格的
// overflow 裁掉（表格横向滚动 + 弹层是常见组合）。
const POP = { el: null, anchor: null };

function closePop() {
  if (POP.el) { POP.el.remove(); POP.el = null; POP.anchor = null; }
  document.removeEventListener('mousedown', onDocDown, true);
  document.removeEventListener('keydown', onPopKey, true);
}
function onDocDown(e) {
  if (POP.el && !POP.el.contains(e.target) && !(POP.anchor && POP.anchor.contains(e.target))) closePop();
}
function onPopKey(e) { if (e.key === 'Escape') closePop(); }

function openPop(anchor, html, { width = 280 } = {}) {
  closePop();
  const el = document.createElement('div');
  el.className = 'pop';
  el.style.width = width + 'px';
  el.innerHTML = html;
  document.body.appendChild(el);

  const r = anchor.getBoundingClientRect();
  const w = width;
  let left = r.right - w;
  if (left < 8) left = 8;
  if (left + w > window.innerWidth - 8) left = window.innerWidth - w - 8;
  let top = r.bottom + 6;
  // 下方放不下就翻到上方
  const h = el.offsetHeight;
  if (top + h > window.innerHeight - 8) top = Math.max(8, r.top - h - 6);
  el.style.left = left + 'px';
  el.style.top = top + 'px';

  POP.el = el;
  POP.anchor = anchor;
  setTimeout(() => {
    document.addEventListener('mousedown', onDocDown, true);
    document.addEventListener('keydown', onPopKey, true);
  }, 0);
  return el;
}

function openCoolDetail(anchor, id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  const cools = modelCoolsOf(acc).filter((m) => m.until > Date.now());
  // 每条明细压成三行内的紧凑块：模型名 + 恢复倒计时是主行，
  // 原因单独一行截断（参考图里也是这么排的），「清除」固定贴右侧。
  const rows = cools.map((m) => `
    <div class="pd-row">
      <div class="pd-main">${esc(m.model)}</div>
      <div class="pd-meta">
        <span class="pd-reason" title="${esc(m.reason || '')}">${esc(m.reason || '上游限流')}</span>
        <span class="warn nowrap">${esc(fmtLeft(m.until))}</span>
      </div>
      <button class="ghost tiny pd-clear" data-uncool="${esc(m.model)}">清除</button>
    </div>`).join('');

  const el = openPop(anchor, `
    <div class="pop-head">${esc(acc.name || shortUID(acc.uid))}</div>
    <div class="pop-sub">限流明细</div>
    <div class="pop-body">
      ${rows || '<p class="hint">该账号当前没有模型级限流</p>'}
      ${acc.coolUntil && acc.coolUntil > Date.now() ? `
        <div class="pd-row">
          <div class="pd-main">账号级冷却</div>
          <div class="pd-meta"><span class="pd-reason">${esc(acc.coolReason || '')}</span>
            <span class="warn nowrap">${esc(fmtLeft(acc.coolUntil))}</span></div>
          <button class="ghost tiny pd-clear" data-uncool="">解除</button>
        </div>` : ''}
    </div>
    <div class="pop-foot">
      <span class="hint">${cools.length} 个模型限流中 · 到恢复时间自动解除</span>
      <button class="ghost tiny" id="popUncoolAll">全部解除</button>
    </div>`, { width: 330 });

  el.querySelectorAll('[data-uncool]').forEach((b) => {
    b.addEventListener('click', async () => {
      try {
        if (b.dataset.uncool) {
          await api(`/api/accounts/${encodeURIComponent(id)}/uncool`, {
            method: 'POST', body: JSON.stringify({ model: b.dataset.uncool }),
          });
        } else {
          await api(`/api/accounts/${encodeURIComponent(id)}/uncool`, { method: 'POST', body: '{}' });
        }
        closePop();
        toast('已解除限流');
        await Promise.all([loadAccounts(), loadStatus()]);
      } catch (e) { toast(e.message, true); }
    });
  });
  el.querySelector('#popUncoolAll').addEventListener('click', async () => {
    try {
      for (const m of cools) {
        await api(`/api/accounts/${encodeURIComponent(id)}/uncool`, {
          method: 'POST', body: JSON.stringify({ model: m.model }),
        });
      }
      // 账号级冷却也一起清，否则「全部解除」之后状态列仍显示冷却中。
      await api(`/api/accounts/${encodeURIComponent(id)}/uncool`, { method: 'POST', body: '{}' });
      closePop();
      toast('已全部解除');
      await Promise.all([loadAccounts(), loadStatus()]);
    } catch (e) { toast(e.message, true); }
  });
}

function openRowMenu(anchor, id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  const el = openPop(anchor, `
    <div class="pop-menu">
      <button data-m="balance">查余额</button>
      <button data-m="refresh">刷新凭据</button>
      <button data-m="checkin">签到</button>
      <button data-m="uncool">解除冷却</button>
      <button data-m="toggle">${acc.enabled ? '停用账号' : '启用账号'}</button>
      <div class="sep"></div>
      <button data-m="delete" class="danger">删除账号</button>
    </div>`, { width: 160 });

  el.querySelectorAll('[data-m]').forEach((b) => {
    b.addEventListener('click', () => { closePop(); accountAction(b.dataset.m, id); });
  });
}

function openAccountSettings(id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  // 账号 id 是 "acc-<hex>"，不含需要转义的字符；用属性遍历而不是拼选择器，
  // 免得旧浏览器上 CSS.escape 不存在时整页脚本挂掉。
  const row = [...document.querySelectorAll('tr[data-id]')].find((tr) => tr.dataset.id === id);
  const anchor = (row && row.querySelector('.c-ops')) || document.body;
  const el = openPop(anchor, `
    <div class="pop-head">账号设置</div>
    <div class="pop-sub">${esc(acc.name || '')} · ${esc(shortUID(acc.uid))}</div>
    <div class="pop-body">
      <div class="field"><label>备注名</label><input id="asName" value="${esc(acc.name || '')}"></div>
      <div class="field"><label>备注</label><input id="asNote" value="${esc(acc.note || '')}" placeholder="可留空"></div>
      <div class="field"><label>优先级</label><input id="asPriority" type="number" step="1" value="${Number(acc.priority) || 0}"></div>
      <p class="hint">uid：${esc(acc.uid)}<br>最近一次使用：${acc.lastUsed ? fmtTime(acc.lastUsed) : '从未'}<br>
        今日签到：${STATE.today && acc.lastCheckinDay === STATE.today ? '已签到' : esc(acc.lastCheckinDay || '未签到')}</p>
    </div>
    <div class="pop-foot">
      <button class="ghost tiny" id="asCancel">取消</button>
      <button class="primary tiny" id="asSave">保存</button>
    </div>`, { width: 320 });

  el.querySelector('#asCancel').addEventListener('click', closePop);
  el.querySelector('#asSave').addEventListener('click', async () => {
    try {
      await api('/api/accounts/' + encodeURIComponent(id), {
        method: 'PATCH',
        body: JSON.stringify({
          name: el.querySelector('#asName').value.trim(),
          note: el.querySelector('#asNote').value.trim(),
          priority: Math.round(Number(el.querySelector('#asPriority').value) || 0),
        }),
      });
      closePop();
      toast('已保存');
      await Promise.all([loadAccounts(), loadStatus()]);
    } catch (e) { toast(e.message, true); }
  });
}

// 轻提示：页面右下角一行，3 秒淡出。比 alert 打断少，适合行内操作。
let toastTimer = null;
function toast(msg, isErr = false) {
  let el = $('toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'toast';
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.className = 'toast show' + (isErr ? ' err' : '');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = 'toast'; }, 3000);
}

async function accountAction(act, id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  $('tableMsg').textContent = '';
  try {
    if (act === 'delete') {
      if (!confirm(`确认删除账号「${acc.name}」？`)) return;
      await api('/api/accounts/' + encodeURIComponent(id), { method: 'DELETE' });
      toast('已删除');
    } else if (act === 'toggle') {
      await api('/api/accounts/' + encodeURIComponent(id), {
        method: 'PATCH', body: JSON.stringify({ enabled: !acc.enabled }),
      });
      toast(acc.enabled ? '已停用' : '已启用');
    } else if (act === 'uncool') {
      const r = await api(`/api/accounts/${encodeURIComponent(id)}/uncool`, { method: 'POST', body: '{}' });
      toast(r.message || '已解除冷却');
    } else if (act === 'balance') {
      const r = await api(`/api/accounts/${encodeURIComponent(id)}/balance`, { method: 'POST' });
      toast(`余额 ${fmtCredits(r.credits)} 积分`);
    } else {
      const r = await api(`/api/accounts/${encodeURIComponent(id)}/${act}`, { method: 'POST' });
      toast(r.message || '完成');
    }
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    toast(e.message, true);
    $('tableMsg').textContent = e.message;
  }
}

// ---------- 批量操作 ----------
$('batchToggleBtn').addEventListener('click', (e) => {
  const el = openPop(e.currentTarget, `
    <div class="pop-menu">
      <button data-b="enable">批量启用</button>
      <button data-b="disable">批量停用</button>
      <button data-b="checkin">批量签到</button>
      <button data-b="uncool">批量解除冷却</button>
      <div class="sep"></div>
      <button data-b="delete" class="danger">批量删除</button>
    </div>`, { width: 170 });
  el.querySelectorAll('[data-b]').forEach((b) => {
    b.addEventListener('click', () => { closePop(); batchAction(b.dataset.b); });
  });
});

async function batchAction(action) {
  const ids = [...POOL.sel];
  if (!ids.length) return;
  const label = {
    enable: '启用', disable: '停用', checkin: '签到', uncool: '解除冷却', delete: '删除',
  }[action] || action;
  if (action === 'delete' && !confirm(`确认删除选中的 ${ids.length} 个账号？不可恢复。`)) return;
  if (action === 'checkin' && !confirm(`对选中的 ${ids.length} 个账号执行签到？`)) return;
  $('tableMsg').textContent = `${label}中…`;
  try {
    const r = await api('/api/accounts/batch', {
      method: 'POST', body: JSON.stringify({ action, ids }),
    });
    toast(`${label}完成：成功 ${r.done || 0}${r.failed ? `，失败 ${r.failed}` : ''}`);
    if (action === 'delete') POOL.sel.clear();
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    toast(e.message, true);
    $('tableMsg').textContent = e.message;
  }
}

// ---------- 查询余额 ----------
async function queryBalances(ids = []) {
  const n = ids.length || filteredAccounts().filter((a) => a.enabled).length;
  if (!n) { toast('没有可查询的账号'); return; }
  $('tableMsg').textContent = `正在查询 ${n} 个账号的余额…`;
  $('refreshAllCreditsBtn').disabled = true;
  try {
    const r = await api('/api/accounts/balances', {
      method: 'POST', body: JSON.stringify({ ids }),
    });
    toast(`余额查询完成：成功 ${r.done || 0}${r.failed ? `，失败 ${r.failed}` : ''}`);
    $('tableMsg').textContent = '';
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) {
    toast(e.message, true);
    $('tableMsg').textContent = e.message;
  } finally {
    $('refreshAllCreditsBtn').disabled = false;
  }
}

$('refreshAllCreditsBtn').addEventListener('click', () => {
  // 有勾选就只查勾选的，没勾选查全部启用账号——和参考图的「查询余额」一致。
  queryBalances([...POOL.sel]);
});

$('refreshListBtn').addEventListener('click', async () => {
  $('tableMsg').textContent = '';
  await Promise.all([loadAccounts(), loadStatus()]);
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
    // 号池页的余额条要标红「低于暂停阈值」的号，这里顺手把阈值带过去。
    POOL.creditStopBelow = s.creditStopEnable ? (s.creditStopBelow ?? 0) : 0;
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

// ---------- 调用日志 ----------
// 服务端分页 + 筛选：日志可能有几十万条，一次只取一页，筛选条件交给 SQL。
// 条件全部放在 LOG 里，翻页只改 page，切页回来也能保持上次的视图。
const LOG = {
  range: 'today', status: '', failedOnly: false, account: '', model: '', api: '', q: '',
  page: 1, pageSize: 50, total: 0, failed: 0, pages: 1, seq: 0, facetsLoaded: false,
};

function logRangeBounds() {
  // 区间按浏览器本地时区的自然日切分，和用户看到的「今天」一致。
  const now = new Date();
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  switch (LOG.range) {
    case 'today': return { since: startOfToday, until: 0, label: '今天' };
    case 'yesterday': return { since: startOfToday - day, until: startOfToday, label: '昨天' };
    case '7d': return { since: startOfToday - 6 * day, until: 0, label: '近 7 天' };
    case '30d': return { since: startOfToday - 29 * day, until: 0, label: '近 30 天' };
    default: return { since: 0, until: 0, label: '全部时间' };
  }
}

function fmtClock(ms) {
  if (!ms) return '-';
  const d = new Date(ms);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function fmtDuration(ms) {
  const v = Number(ms) || 0;
  if (v < 1000) return `${v}毫秒`;
  if (v < 60000) return `${(v / 1000).toFixed(1)}秒`;
  const m = Math.floor(v / 60000);
  return `${m}分${Math.round((v % 60000) / 1000)}秒`;
}

// 大数字压缩成「1.2万 / 3.4k」，避免长数字把列撑开。
function fmtNum(n) {
  const v = Number(n) || 0;
  if (v >= 10000) return (v / 10000).toFixed(1).replace(/\.0$/, '') + '万';
  return String(v);
}

// 用量列：主行是速度（输出 token / 耗时），副行是输入输出明细。
// 真实请求的速度在几十到几百 tok/s，离线补写的脏数据可能算出离谱值，所以压成万/k。
function fmtTokens(l) {
  const total = Number(l.totalTokens) || 0;
  const p = Number(l.promptTokens) || 0;
  const c = Number(l.completionTokens) || 0;
  if (!total && !p && !c) return '<span class="dim">-</span>';
  const ms = Number(l.ms) || 0;
  const head = ms > 0 && c > 0
    ? `${fmtNum(Math.round(c / (ms / 1000)))} tok/s`
    : `${fmtNum(total)} tok`;
  return `<div class="tok"><b>${head}</b><span>输入 ${fmtNum(p)} · 输出 ${fmtNum(c)}</span></div>`;
}

function logStatusCell(l) {
  const code = Number(l.status) || 0;
  if (code >= 500) return `<span class="lpill bad">${code || 'ERR'}</span>`;
  if (code >= 400) return `<span class="lpill warn">${code}</span>`;
  if (code >= 200) return `<span class="lpill ok">${code}</span>`;
  return `<span class="lpill">${code || '-'}</span>`;
}

function logAccountCell(l) {
  const name = l.accountName || '';
  const uid = l.accountUID || l.accountId || '';
  if (!name && !uid) return '<span class="dim">-</span>';
  return `<div class="log-acc">
    <div class="acc-name" title="${esc(uid)}">${esc(name || shortUID(uid))}</div>
    <div class="acc-sub">${esc(shortUID(uid))}</div>
  </div>`;
}

function logQuery() {
  const b = logRangeBounds();
  const p = new URLSearchParams();
  if (b.since) p.set('since', b.since);
  if (b.until) p.set('until', b.until);
  const status = LOG.failedOnly ? 'fail' : LOG.status;
  if (status) p.set('status', status);
  if (LOG.account) p.set('account', LOG.account);
  if (LOG.model) p.set('model', LOG.model);
  if (LOG.api) p.set('api', LOG.api);
  if (LOG.q.trim()) p.set('q', LOG.q.trim());
  p.set('page', LOG.page);
  p.set('pageSize', LOG.pageSize);
  return p;
}

function renderLogRows(rows) {
  const tbody = $('logTable').querySelector('tbody');
  $('logEmpty').hidden = rows.length > 0;
  $('logEmpty').textContent = '当前筛选下没有调用记录';
  if (!rows.length) { tbody.innerHTML = ''; return; }
  tbody.innerHTML = rows.map((l) => {
    const err = l.error || '';
    return `<tr data-id="${esc(l.id)}" class="${l.status >= 400 ? 'is-fail' : ''}">
      <td class="lc-time"><div class="log-time">${esc(fmtClock(l.ts))}</div></td>
      <td class="lc-account">${logAccountCell(l)}</td>
      <td class="lc-status">${logStatusCell(l)}</td>
      <td class="lc-model"><div class="log-model" title="${esc(l.model)}">${esc(l.model || '-')}</div></td>
      <td class="lc-api"><span class="tag">${esc(l.api || '-')}</span></td>
      <td class="lc-ms"><span class="log-ms">${esc(fmtDuration(l.ms))}</span></td>
      <td class="lc-tokens">${fmtTokens(l)}</td>
      <td class="lc-error"><div class="log-err" title="${esc(err)}">${esc(err) || '<span class="dim">-</span>'}</div></td>
      <td class="lc-detail"><button class="ghost tiny log-detail" data-id="${esc(l.id)}">详情</button></td>
    </tr>`;
  }).join('');
}

function renderLogFoot() {
  const pages = Math.max(1, LOG.pages);
  const b = logRangeBounds();
  const failText = LOG.failed > 0 ? ` · <span class="bad">${LOG.failed} 失败</span>` : '';
  $('logCount').innerHTML = `${LOG.total} 条${failText}`;
  $('logRangeInfo').innerHTML = `${esc(b.label)} · 共 ${LOG.total} 条${failText}`;
  $('logPageInfo').textContent = `第 ${LOG.page} / ${pages} 页`;
  $('lfPrev').disabled = LOG.page <= 1;
  $('lfNext').disabled = LOG.page >= pages;
  $('lfJump').max = String(pages);
  if (document.activeElement !== $('lfJump')) $('lfJump').value = String(LOG.page);
}

async function loadLogs() {
  const seq = ++LOG.seq;
  $('logTableMsg').textContent = '';
  try {
    const r = await api('/api/logs?' + logQuery().toString());
    if (seq !== LOG.seq) return; // 快速翻页/改筛选时丢弃过期响应
    LOG.total = r.total || 0;
    LOG.failed = r.failed || 0;
    LOG.pages = r.pages || 1;
    // 数据变少（被清理或换了筛选）时当前页可能越界，退到最后一页重取。
    if (LOG.page > LOG.pages) { LOG.page = LOG.pages; return loadLogs(); }
    LOG.page = r.page || LOG.page;
    LOG_ROWS = r.logs || [];
    renderLogRows(LOG_ROWS);
    renderLogFoot();
  } catch (e) {
    if (seq === LOG.seq) $('logTableMsg').textContent = e.message;
  }
}

// 筛选下拉依赖当前号池和历史流水里的取值，数据少时带来即可。
async function ensureLogFacets() {
  try {
    const [a, f] = await Promise.all([
      api('/api/accounts'),
      LOG.facetsLoaded ? Promise.resolve(null) : api('/api/logs/facets'),
    ]);
    const accSel = $('lfAccount');
    const keep = accSel.value;
    accSel.innerHTML = '<option value="">全部账号</option>' + (a.accounts || []).map((x) =>
      `<option value="${esc(x.id)}">${esc(x.name || shortUID(x.uid))}</option>`).join('');
    accSel.value = keep;

    if (f) {
      const fill = (id, items, keepV) => {
        const sel = $(id);
        sel.innerHTML = `<option value="">全部${id === 'lfModel' ? '模型' : '接口'}</option>` +
          items.map((v) => `<option value="${esc(v)}">${esc(v)}</option>`).join('');
        sel.value = keepV;
      };
      fill('lfModel', f.models || [], $('lfModel').value);
      fill('lfAPI', f.apis || [], $('lfAPI').value);
      LOG.facetsLoaded = true;
    }
  } catch { /* 下拉拉不到不影响主列表 */ }
}

function gotoLogPage(p) {
  LOG.page = Math.max(1, Math.min(LOG.pages || 1, p));
  loadLogs();
}

// 任一筛选变化都回到第 1 页，否则会停在一个不存在的页上。
function applyLogFilter() {
  LOG.page = 1;
  loadLogs();
}

// 筛选控件是用户自己改的，不回写 value，否则选中的过滤条件没法点「取消」。
// 只有重置时统一同步一次。
function syncLogFilterUI() {
  document.querySelectorAll('#lfRange button').forEach((b) => b.classList.toggle('on', b.dataset.v === LOG.range));
  $('lfStatus').value = LOG.status;
  $('lfAccount').value = LOG.account;
  $('lfModel').value = LOG.model;
  $('lfAPI').value = LOG.api;
  $('lfFailedOnly').checked = LOG.failedOnly;
  $('lfSearch').value = LOG.q;
}

document.querySelectorAll('#lfRange button').forEach((b) => {
  b.addEventListener('click', () => {
    LOG.range = b.dataset.v;
    document.querySelectorAll('#lfRange button').forEach((x) => x.classList.toggle('on', x === b));
    applyLogFilter();
  });
});
$('lfStatus').addEventListener('change', () => { LOG.status = $('lfStatus').value; applyLogFilter(); });
$('lfAccount').addEventListener('change', () => { LOG.account = $('lfAccount').value; applyLogFilter(); });
$('lfModel').addEventListener('change', () => { LOG.model = $('lfModel').value; applyLogFilter(); });
$('lfAPI').addEventListener('change', () => { LOG.api = $('lfAPI').value; applyLogFilter(); });
$('lfFailedOnly').addEventListener('change', () => { LOG.failedOnly = $('lfFailedOnly').checked; applyLogFilter(); });
$('lfPageSize').addEventListener('change', () => { LOG.pageSize = Number($('lfPageSize').value) || 50; applyLogFilter(); });
$('lfPrev').addEventListener('click', () => gotoLogPage(LOG.page - 1));
$('lfNext').addEventListener('click', () => gotoLogPage(LOG.page + 1));
$('lfJump').addEventListener('change', () => gotoLogPage(Number($('lfJump').value) || 1));
$('lfReset').addEventListener('click', () => {
  Object.assign(LOG, { range: 'today', status: '', failedOnly: false, account: '', model: '', api: '', q: '' });
  syncLogFilterUI();
  applyLogFilter();
});
let logSearchTimer = null;
$('lfSearch').addEventListener('input', () => {
  clearTimeout(logSearchTimer);
  const v = $('lfSearch').value;
  logSearchTimer = setTimeout(() => { LOG.q = v; applyLogFilter(); }, 300);
});
// 回车立刻查，不再等防抖
$('lfSearch').addEventListener('keydown', (e) => {
  if (e.key !== 'Enter') return;
  clearTimeout(logSearchTimer);
  LOG.q = $('lfSearch').value;
  applyLogFilter();
});
// 行内「详情」用事件委托：分页重渲染后不需要重新绑定
$('logTable').addEventListener('click', (e) => {
  const btn = e.target.closest('.log-detail');
  if (btn) openLogDetail(btn);
});

$('logRefreshBtn').addEventListener('click', () => {
  LOG.facetsLoaded = false;
  ensureLogFacets().then(() => loadLogs());
});

// 自动刷新：只在「还在日志页 + 标签页可见」时轮询，切走或最小化就停，
// 既不白烧请求，也不会在别的页面上把后台刷成一片红。
const AUTO = { on: false, sec: 10, timer: null, inflight: false };

function autoRefreshTick() {
  // 上一次还没回来就跳过本轮，避免慢网络下请求叠起来。
  if (!AUTO.on || AUTO.inflight || STATE.page !== 'logs' || document.hidden) return;
  AUTO.inflight = true;
  Promise.resolve(loadLogs()).finally(() => { AUTO.inflight = false; });
}

function syncAutoRefresh() {
  clearInterval(AUTO.timer);
  AUTO.timer = null;
  if (AUTO.on) AUTO.timer = setInterval(autoRefreshTick, AUTO.sec * 1000);
  $('lfAutoDot').classList.toggle('on', AUTO.on);
  localStorage.setItem('logAutoRefresh', JSON.stringify({ on: AUTO.on, sec: AUTO.sec }));
  // 刚打开时先补一次，不用干等一个周期
  if (AUTO.on) autoRefreshTick();
}

$('lfAuto').addEventListener('change', () => { AUTO.on = $('lfAuto').checked; syncAutoRefresh(); });
$('lfAutoSec').addEventListener('change', () => {
  AUTO.sec = Number($('lfAutoSec').value) || 10;
  syncAutoRefresh();
});

// 从后台切回来时立刻补一次，而不是等下一个周期。
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && AUTO.on) autoRefreshTick();
});

// 恢复上次的开关状态（默认关闭，避免一进来就打后端）。
try {
  const saved = JSON.parse(localStorage.getItem('logAutoRefresh') || '{}');
  if (saved.on) {
    AUTO.on = true;
    AUTO.sec = Number(saved.sec) || 10;
    $('lfAuto').checked = true;
    $('lfAutoSec').value = String(AUTO.sec);
  }
} catch { /* 存储被清或格式坏了就用默认值 */ }
syncAutoRefresh();

// 「清理」不在这里直接删数据：跳到运行日志页的清理入口，那里有确认和策略说明。
$('loadLogsBtn').addEventListener('click', () => {
  switchPage('syslogs');
  toast('已跳到运行日志的清理入口');
});

// 列设置：允许把不常看的列藏起来，选择存在 localStorage。
const LOG_COLS = [['time', '时间'], ['account', '账号'], ['status', '状态'], ['model', '模型'],
  ['api', '接口'], ['ms', '用时'], ['tokens', '用量'], ['error', '错误']];
const LOG_HIDDEN = new Set(JSON.parse(localStorage.getItem('logHiddenCols') || '[]'));

function applyLogCols() {
  LOG_COLS.forEach(([k]) => $('logTable').classList.toggle('hide-' + k, LOG_HIDDEN.has(k)));
  localStorage.setItem('logHiddenCols', JSON.stringify([...LOG_HIDDEN]));
}
applyLogCols();

$('logColumnsBtn').addEventListener('click', (e) => {
  const html = `<div class="pop-menu">
    ${LOG_COLS.map(([k, label]) => `<label class="pop-check">
      <input type="checkbox" data-col="${k}" ${LOG_HIDDEN.has(k) ? '' : 'checked'}>
      <span>${label}</span></label>`).join('')}
  </div>`;
  const el = openPop(e.currentTarget, html, { width: 160 });
  el.querySelectorAll('[data-col]').forEach((cb) => {
    cb.addEventListener('change', () => {
      if (cb.checked) LOG_HIDDEN.delete(cb.dataset.col); else LOG_HIDDEN.add(cb.dataset.col);
      applyLogCols();
    });
  });
});

// 详情弹层：单条请求的完整信息，错误全文在这里展开（表格里只显示一行）。
let LOG_ROWS = [];
function openLogDetail(anchor) {
  const id = anchor.dataset.id;
  const l = LOG_ROWS.find((x) => String(x.id) === String(id));
  if (!l) return;
  const kv = (k, v, cls = '') => `<div class="ld-row"><span class="ld-k">${k}</span><span class="ld-v ${cls}">${v}</span></div>`;
  openPop(anchor, `
    <div class="pop-head">请求详情</div>
    <div class="pop-sub">${esc(fmtClock(l.ts))} · ${esc(l.model || '-')}</div>
    <div class="pop-body">
      ${kv('状态', logStatusCell(l))}
      ${kv('接口', esc(l.api || '-'))}
      ${kv('账号', esc(l.accountName || shortUID(l.accountUID || l.accountId) || '-'))}
      ${kv('UID', esc(l.accountUID || l.accountId || '-'))}
      ${kv('用时', esc(fmtDuration(l.ms)))}
      ${kv('输入 token', String(l.promptTokens || 0))}
      ${kv('输出 token', String(l.completionTokens || 0))}
      ${kv('总 token', String(l.totalTokens || 0))}
      ${l.error ? `<div class="ld-row col"><span class="ld-k">错误</span><pre class="ld-err">${esc(l.error)}</pre></div>` : ''}
    </div>
    <div class="pop-foot"><span class="hint">#${esc(l.id)}</span>
      <button class="ghost tiny" id="ldClose">关闭</button></div>`, { width: 380 });
  document.getElementById('ldClose').addEventListener('click', closePop);
}

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
