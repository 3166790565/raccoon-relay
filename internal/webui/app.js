'use strict';

const $ = (id) => document.getElementById(id);
const STATE = { accounts: [], loggedIn: false, page: '', loaded: new Set(), today: '', version: '' };

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
  report: () => loadReport(),
  accounts: () => loadAccounts(),
  checkin: () => loadCheckinCenter(),
  add: () => Promise.resolve(),
  models: () => loadModels(),
  keys: () => loadKeys(),
  logs: () => ensureLogFacets().then(() => loadLogs()),
  syslogs: () => loadSysLogs(),
  settings: () => loadSettings(),
  update: () => loadUpdate(),
};

// 顶栏面包屑的末段：与侧栏导航文字保持一致（导航里带图标，这里只取文字）。
const PAGE_TITLES = {
  overview: '概览', report: '报表', accounts: '号池', checkin: '签到中心',
  add: '添加账号', models: '模型', keys: 'API Keys', logs: '调用日志',
  syslogs: '运行日志', settings: '设置', update: '版本更新',
};

function setCrumb(page) {
  const el = $('crumb');
  if (!el) return;
  el.innerHTML = `<span>小浣熊反代</span><span class="sep">/</span>` +
    `<b class="cur">${esc(PAGE_TITLES[page] || page)}</b>`;
}

function switchPage(page, { force = false } = {}) {
  if (!PAGE_LOADERS[page]) page = 'overview';
  STATE.page = page;
  // 报表标签页每次进入都重新取数：用户切回来时想看到的是最新用量，
  // 不是上次离开时的旧图。滚动位置也会重置（switchPage 里已统一处理）。
  if (page === 'report') STATE.loaded.delete('report');

  document.querySelectorAll('.navtab').forEach((b) => b.classList.toggle('active', b.dataset.page === page));
  document.querySelectorAll('.page').forEach((p) => p.classList.toggle('active', p.id === 'page-' + page));
  setCrumb(page);

  if (location.hash.slice(1) !== page) history.replaceState(null, '', '#' + page);
  closeSidebar();

  // 每个页面自己是滚动容器（壳层固定视口），换页要把该页的滚动位置归零，
  // 否则从「调用日志」底部切到「概览」会停在半截。
  const cur = document.getElementById('page-' + page);
  if (cur) cur.scrollTop = 0;
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
// 侧栏底部的版本号：点一下直接去「版本更新」页（进去总是重新拉一次状态）。
$('sideVersion')?.addEventListener('click', () => {
  switchPage('update', { force: true });
});
window.addEventListener('hashchange', () => switchPage(location.hash.slice(1)));

// ---------- 侧栏抽屉（窄屏） ----------
// 壳层在 820px 以下把侧栏变成抽屉：顶栏的 ☰ 打开，点遮罩/选页/按 Esc 关闭。
function openSidebar() { document.body.classList.add('side-open'); }
function closeSidebar() { document.body.classList.remove('side-open'); }
$('menuBtn')?.addEventListener('click', openSidebar);
$('sidebarScrim')?.addEventListener('click', closeSidebar);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && document.body.classList.contains('side-open')) closeSidebar();
});

// ---------- 主题（跟随 / 浅色 / 深色） ----------
// 首屏的那一贴由 index.html 的 head 脚本完成，这里只负责切换与持久化。
function applyTheme(mode) {
  const m = mode === 'light' || mode === 'dark' ? mode : 'system';
  localStorage.setItem('rc-theme', m);
  document.documentElement.dataset.theme = m;
  const dark = m === 'dark'
    || (m === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.style.colorScheme = dark ? 'dark' : 'light';
  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', dark ? '#000000' : '#f7f7f9');
  document.querySelectorAll('#themeSwitch button').forEach((b) => {
    b.classList.toggle('on', b.dataset.themeVal === m);
  });
}
document.querySelectorAll('#themeSwitch button').forEach((btn) => {
  btn.addEventListener('click', () => applyTheme(btn.dataset.themeVal));
});
applyTheme(localStorage.getItem('rc-theme') || 'system');
// 「跟随系统」时系统切换深浅，颜色要立刻跟上（只影响令牌，不动存储值）。
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => {
  if ((localStorage.getItem('rc-theme') || 'system') === 'system') {
    applyTheme('system');
  }
});

// ---------- 概览 ----------
// 侧栏底部的版本号入口。只在值变化时写 DOM，loadStatus 每次切页都会跑，
// 没必要每次都重排版。
function renderSideVersion(v) {
  const btn = $('sideVersion');
  const txt = $('sideVersionText');
  if (!btn || !txt) return;
  const ver = (v || '').trim();
  const label = !ver ? '版本未知'
    : /^dev$/i.test(ver) ? '开发版'
    : `v${ver.replace(/^[vV]/, '')}`;
  if (STATE.version === label) return;
  STATE.version = label;
  txt.textContent = label;
  btn.title = ver ? `当前版本 ${ver}，点击查看更新` : '当前版本未知，点击查看更新';
}

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
    // 侧栏底部是版本号入口：点一下跳到「版本更新」页。账号的异常/冷却数量
    // 已经由顶栏的状态胶囊显示，底栏再放一份会一直挂在那里，反而像故障提示。
    renderSideVersion(s.version);
    const navCnt = $('navAccountsCount');
    if (navCnt) navCnt.textContent = a.total ? String(a.total) : '';
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
  provider: 'all', status: 'all', cool: 'all', q: '', sel: new Set(), fold: false,
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
    if (POOL.provider !== 'all' && (a.provider || 'raccoon') !== POOL.provider) return false;
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

// 日日新额度列：积分池是「池 × 窗口」，与小浣熊的单一积分不同。
// 按参考图的做法，把每个窗口压成一行「池名 · 窗口 | 剩余」并把剩余量画成细条；
// 没有拉到额度时给出「未同步」，而不是假装 0。
function renderSNQuotaCell(a) {
  const wins = Array.isArray(a.snWindows) ? a.snWindows.filter((w) => w && w.limit > 0) : [];
  if (!wins.length) {
    return `<div class="quota sn-quota"><span class="q-val dim">未同步额度</span>
      <span class="q-label">${a.snPoolAt ? '上次查询 ' + fmtTime(a.snPoolAt) : '点「查额度」拉取'}</span></div>`;
  }
  const rows = wins.map((w) => {
    const pct = Math.max(2, Math.min(100, Number(w.percent) || 0));
    const low = pct <= 15;
    return `<div class="sn-win">
      <span class="sn-win-name" title="${esc(w.name)}">${esc(w.scope || w.name)} · ${esc(w.label || '')}</span>
      <span class="sn-win-bar"><i class="${low ? 'low' : ''}" style="width:${pct}%"></i></span>
      <span class="sn-win-val ${low ? 'bad' : 'ok'}">${fmtCredits(Math.round(w.remaining))}</span>
    </div>`;
  }).join('');
  return `<div class="quota sn-quota">${rows}
    <span class="q-label">${a.snPoolAt ? '额度更新于 ' + fmtTime(a.snPoolAt) : ''}${a.snPlan ? ' · ' + esc(a.snPlan) : ''}</span>
  </div>`;
}

// 外部渠道额度列：AutoClaw 是纯余额，码道是「剩余/总量」的积分桶（外加免费日额度）。
// 两家都从后端带回一个小的 extQuota 摘要，没同步过就显示「未同步」。
function renderExtQuotaCell(a) {
  const q = a.extQuota;
  if (!q) {
    return `<div class="quota sn-quota"><span class="q-val dim">未同步额度</span>
      <span class="q-label">${a.snPoolAt ? '上次查询 ' + fmtTime(a.snPoolAt) : '点「查额度」拉取'}</span></div>`;
  }
  const codearts = a.provider === 'codearts';
  const total = Number(q.total);
  const remain = Number(codearts ? q.remain : q.total);
  const known = Number.isFinite(total) && total >= 0;
  const pct = known && total > 0 ? Math.max(2, Math.min(100, Math.round((remain / total) * 100))) : 100;
  const low = pct <= 15;
  const label = known
    ? (codearts ? `${fmtCredits(Math.round(remain))} / ${fmtCredits(Math.round(total))}` : `${fmtCredits(Math.round(remain))} 积分`)
    : '未知';
  const daily = codearts && Number(q.dailyLimit) > 0
    ? `<span class="q-label">免费 ${fmtCredits(Math.round(Number(q.dailyLimit) - Number(q.dailyUsed || 0)))} tokens</span>`
    : '';
  return `<div class="quota">
    <div class="q-row"><span class="q-val ${low ? 'bad' : known ? 'ok' : 'dim'}">${label}</span></div>
    <span class="mini-bar ${low ? 'low' : ''}"><i style="width:${pct}%"></i></span>
    ${daily}
    <span class="q-label">${a.snPoolAt ? '更新于 ' + fmtTime(a.snPoolAt) : ''}${q.plan ? ' · ' + esc(q.plan) : ''}</span>
  </div>`;
}

const PROVIDER_META = {
  raccoon: { label: '小浣熊', emoji: '🦝', sub: '商汤小浣熊 · xiaohuanxiong.com' },
  sensenova: { label: '日日新', emoji: '🌊', sub: '商汤日日新 Token Plan · platform.sensenova.cn' },
  autoclaw: { label: 'AutoClaw', emoji: '🦾', sub: '智谱 AutoClaw · autoglm-api.autoglm.ai / zhipuai.cn' },
  codearts: { label: '码道 CodeArts', emoji: '☁️', sub: '华为云码道 · snap-access.myhuaweicloud.com' },
};

function providerOf(a) { return a.provider || 'raccoon'; }

function renderAccounts() {
  // 余额条的参照系：全池已知余额的最大值。每次渲染前算一次，
  // 避免每行都去遍历一遍列表。
  POOL.peakCredits = STATE.accounts.reduce((m, a) => (a.credits > m ? a.credits : m), 0) || 1;
  renderPoolStats();
  const tbody = $('accTable').querySelector('tbody');
  const rows = filteredAccounts();
  const total = STATE.accounts.length;

  // 空状态：没有筛选结果时一定要给一句话，否则表格区只剩一块白底，
  // 看起来像「加载失败」而不是「还没有号」。
  $('poolEmpty').hidden = rows.length > 0;
  $('poolEmpty').textContent = total === 0 ? '还没有账号，去「添加账号」页导入' : '当前筛选下没有账号';
  $('poolFootInfo').textContent = total === 0
    ? '共 0 条'
    : `共 ${total} 条${rows.length === total ? '（全部显示）' : ` · 筛选出 ${rows.length} 条`}`;

  if (!rows.length) {
    tbody.innerHTML = '';
    syncSelUI();
    return;
  }

  // 按供应商分组：两种号是完全不同的上游，混在一张表里会让人误以为
  // 「换号」能跨供应商生效。分组头仿参考图：可折叠、显示该组账号数。
  const groups = {};
  rows.forEach((a) => {
    const p = providerOf(a);
    (groups[p] = groups[p] || []).push(a);
  });
  const order = ['raccoon', 'sensenova', 'autoclaw', 'codearts'].filter((p) => groups[p]);
  // 其它未知供应商追加在后面，避免数据里的新值凭空消失。
  Object.keys(groups).forEach((p) => { if (!order.includes(p)) order.push(p); });

  let idx = 0;
  const html = order.map((p) => {
    const meta = PROVIDER_META[p] || { label: p, emoji: '•', sub: '' };
    const list = groups[p];
    const collapsed = POOL.collapsed && POOL.collapsed[p];
    const head = `<tr class="group-row" data-group="${esc(p)}">
      <td colspan="9">
        <div class="group-head ${collapsed ? 'collapsed' : ''}">
          <span class="gh-caret">${collapsed ? '▸' : '▾'}</span>
          <span class="gh-emoji">${meta.emoji}</span>
          <span class="gh-name">${esc(meta.label)}</span>
          <span class="gh-count">${list.length} 个账号</span>
          <span class="gh-sub">${esc(meta.sub)}</span>
        </div>
      </td>
    </tr>`;
    if (collapsed) return head;
    return head + list.map((a) => { idx += 1; return renderAccountRow(a, idx); }).join('');
  }).join('');

  tbody.innerHTML = html;
  bindRowEvents(tbody);
  bindGroupEvents(tbody);
  syncSelUI();
}

// renderAccountRow 生成单个账号行。序号按当前筛选结果的连续编号。
function renderAccountRow(a, i) {
  const st = a.status || 'online';
  const lowCredit = st === 'exhausted' && !a.coolUntil;
  const stLabel = lowCredit ? '低积分' : (STATUS_LABEL[st] || esc(st));
  const cools = modelCoolsOf(a).filter((m) => m.until > Date.now());
  const exp = expireInfo(a);
  const checked = POOL.sel.has(a.id);
  const sn = providerOf(a) === 'sensenova';

  // 限流列：账号级冷却显示原因 + 剩余时间；只有模型级罚单时显示「N 个模型」。
  // 只要有错误现场（冷却中/需重登/已限流）就做成可点的「错误详情」入口，
  // 单元格本身只放摘要，点开才看到响应头与原始响应体。
  let coolCell;
  if (st === 'cooling' || (st === 'exhausted' && a.coolUntil)) {
    // 账号级失败链会累加命中次数（200108 账号异常），这里带上档位，
    // 否则用户只能看到「冷却」却不知道这是第几次、还要罚多久。
    const ac = a.accountCool && a.accountCool.until > Date.now() ? a.accountCool : null;
    const tier = ac && ac.hits > 0 ? `第 ${ac.hits} 次 · ` : '';
    coolCell = `<div class="coolbox warn clickable" title="点击查看错误详情">
      <span class="cb-main">${esc(a.coolReason || '限流中')}</span>
      <span class="cb-sub">${esc(tier)}${esc(fmtLeft(a.coolUntil || 0))} · 详情</span>
    </div>`;
  } else if (lowCredit) {
    coolCell = `<div class="coolbox bad">
      <span class="cb-main">${esc(a.coolReason || '积分低于阈值')}</span>
      <span class="cb-sub">待余额恢复</span>
    </div>`;
  } else if (st === 'relogin') {
    coolCell = `<div class="coolbox bad clickable" title="点击查看错误详情">
      <span class="cb-main">凭证失效</span><span class="cb-sub">需重新登录 · 详情</span>
    </div>`;
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

  // 余额/额度列按供应商分派：日日新是池×窗口，小浣熊是单一积分。
  const quotaCell = sn ? renderSNQuotaCell(a) : (a.provider === 'autoclaw' || a.provider === 'codearts')
    ? renderExtQuotaCell(a) : renderQuotaCell(a);
  const quotaTitle = sn
    ? (a.snPoolAt ? '额度更新于 ' + fmtTime(a.snPoolAt) : '尚未同步额度')
    : (a.creditsAt ? '余额更新于 ' + fmtTime(a.creditsAt) : '');
  // 日日新的「凭证」列显示控制台账号 + 登录票状态；小浣熊没有这个概念。
  // 外部渠道显示站点与临时凭据到期时刻。
  let subExtra = '';
  if (sn) {
    subExtra = ` · ${a.snTokenValid ? '登录票有效' : '登录票失效'}${a.snUser ? ' · ' + esc(a.snUser) : ''}`;
  } else if (a.provider === 'autoclaw' || a.provider === 'codearts') {
    const site = a.extSite === 'cn' ? '国内站' : '国际站';
    const exp = a.extTokenExp ? ` · 凭据至 ${fmtTime(a.extTokenExp)}` : '';
    subExtra = ` · ${site}${exp}${a.hasExtRefresh ? '' : ' · 无续期材料'}`;
  }

  return `<tr data-id="${esc(a.id)}" data-provider="${esc(providerOf(a))}" class="${checked ? 'sel' : ''}">
    <td class="c-check"><input type="checkbox" class="row-check" ${checked ? 'checked' : ''}></td>
    <td class="c-idx"><span class="idx">#${i}</span></td>
    <td class="c-name">
      <div class="acc-name" title="${esc(a.uid)}">${esc(a.name || shortUID(a.uid))}</div>
      <div class="acc-sub">${esc(shortUID(a.uid))}${a.note ? ' · ' + esc(a.note) : ''}${Number(a.maxConcurrent) > 0 ? ` · 并发≤${Number(a.maxConcurrent)}` : ''}${subExtra}</div>
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
    <td class="c-expire"><span class="${exp.cls}" ${exp.title ? `title="${esc(exp.title)}"` : ''}>${exp.text ? esc(exp.text) : ''}</span></td>
    <td class="c-balance" title="${esc(quotaTitle)}">${quotaCell}</td>
    <td class="c-ops">
      <button class="ghost tiny" data-act="balance">${sn ? '查额度' : '余额'}</button>
      <button class="ghost tiny" data-act="settings">设置</button>
      <button class="ghost tiny more" data-act="more" title="更多操作">…</button>
    </td>
  </tr>`;
}

// bindGroupEvents 处理分组头的折叠。
function bindGroupEvents(tbody) {
  tbody.querySelectorAll('.group-head').forEach((head) => {
    head.addEventListener('click', () => {
      const p = head.closest('tr').dataset.group;
      POOL.collapsed = POOL.collapsed || {};
      POOL.collapsed[p] = !POOL.collapsed[p];
      renderAccounts();
    });
  });
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

    // 两种限流框：有模型级罚单的开限流明细；冷却/需重登的开错误现场。
    const infoBox = tr.querySelector('.coolbox.info');
    if (infoBox) infoBox.addEventListener('click', () => openCoolDetail(infoBox, id));
    const errBox = tr.querySelector('.coolbox.clickable');
    if (errBox) errBox.addEventListener('click', () => openAccountErrors(id));
  });
}

// 顶部筛选按钮上的数字：一直反映全量统计，不随筛选结果变化。
function renderPoolStats() {
  const total = STATE.accounts.length;
  const enabled = STATE.accounts.filter((a) => a.enabled).length;
  const limited = STATE.accounts.filter(isLimited).length;
  const raccoonN = STATE.accounts.filter((a) => providerOf(a) === 'raccoon').length;
  const snN = STATE.accounts.filter((a) => providerOf(a) === 'sensenova').length;
  const acN = STATE.accounts.filter((a) => providerOf(a) === 'autoclaw').length;
  const caN = STATE.accounts.filter((a) => providerOf(a) === 'codearts').length;
  const setB = (sel, vals) => {
    const btns = document.querySelectorAll(sel + ' button');
    btns.forEach((b, i) => {
      const b2 = b.querySelector('b');
      if (b2) b2.textContent = vals[i] ?? 0;
    });
  };
  setB('#pfProvider', [total, raccoonN, snN, acN, caN]);
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
  pfProvider: 'provider',
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

// 错误详情：号池页「限流」列点开的内容。
// 后端从运行事件里取这个号最近几条转发失败，detail 里已经是拼好的上游现场
// （状态码/响应头/原始响应体），这里只负责排版，不做二次解析。
let errDetailAccount = null;

async function openAccountErrors(id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  errDetailAccount = acc || { id };
  $('errDetailBody').innerHTML = '<p class="hint">正在读取错误现场…</p>';
  // 接口地址里是账号的内部 id，直接显示出来对用户没有意义，优先显示名称/UID。
  $('errDetailMeta').textContent = acc ? (acc.name || shortUID(acc.uid)) : shortUID(id);
  openEvModal('errDetailModal');
  try {
    const r = await api(`/api/accounts/${encodeURIComponent(id)}/errors`);
    errDetailAccount = r.account || errDetailAccount;
    renderAccountErrors(r);
  } catch (e) {
    $('errDetailBody').innerHTML = `<p class="err">${esc(e.message)}</p>`;
  }
}

function renderAccountErrors(r) {
  const a = r.account || errDetailAccount || {};
  const events = Array.isArray(r.events) ? r.events : [];
  const cools = modelCoolsOf(a).filter((m) => m.until > Date.now());

  const kv = (k, v) => `<div class="ld-row"><span class="ld-k">${esc(k)}</span><span class="ld-v">${v}</span></div>`;
  const st = a.status || 'online';
  const stLabel = STATUS_LABEL[st] || st;
  const coolLine = a.coolUntil && a.coolUntil > Date.now()
    ? `${esc(a.coolReason || '限流中')} · ${esc(fmtLeft(a.coolUntil))}`
    : (a.coolReason ? esc(a.coolReason) : '-');

  const head = `<div class="ev-detail-head">
      <span class="pill ${esc(st)}"><i class="dot"></i>${esc(stLabel)}</span>
      <span class="ev-detail-msg">${esc(a.name || shortUID(a.uid || ''))}</span>
    </div>
    <div class="ev-detail-time">uid：${esc(a.uid || '-')}</div>
    <div class="pop-body ev-detail-kv">
      ${kv('账号状态', `<span class="pill ${esc(st)}"><i class="dot"></i>${esc(stLabel)}</span>`)}
      ${kv('冷却原因', coolLine)}
      ${a.accountCool && a.accountCool.until > Date.now()
        ? kv('账号级冷却', `第 ${esc(a.accountCool.hits)} 次 · ${esc(fmtLeft(a.accountCool.until))}`)
        : ''}
      ${kv('模型限流', cools.length
        ? cools.map((m) => `${esc(m.model)} · ${esc(fmtLeft(m.until))}`).join('<br>')
        : '<span class="dim">无</span>')}
    </div>`;

  const blocks = events.map((e) => `
    <div class="ev-detail-block">
      <div class="ev-detail-label">
        <span class="tag">${esc(fmtClock(e.ts))}</span>
        <span class="tag">${esc(e.level || '')}</span>
        ${esc(e.message || '')}${e.count > 1 ? ` <span class="hint">· 合并 ${e.count} 次，最近 ${esc(fmtClock(e.lastTs))}</span>` : ''}
      </div>
      <pre class="ld-err ev-pre">${esc(e.detail || '（这条事件没有附加详情）')}</pre>
    </div>`).join('');

  const empty = `<p class="hint">这个号最近没有记录到转发失败事件。
    如果冷却原因是「HTTP 403」，说明失败发生在运行事件表启用之前——等下一次请求失败后再点这里查看。</p>`;

  $('errDetailBody').innerHTML = head + (blocks || empty);
}

$('errDetailClose').addEventListener('click', () => closeEvModal('errDetailModal'));
$('errDetailCopy').addEventListener('click', async () => {
  const text = $('errDetailBody').innerText;
  try {
    await navigator.clipboard.writeText(text);
    $('errDetailCopy').textContent = '已复制';
    setTimeout(() => { $('errDetailCopy').textContent = '复制全部'; }, 1500);
  } catch {
    $('errDetailCopy').textContent = '复制失败';
  }
});
$('errDetailLogs').addEventListener('click', () => {
  // 跳到运行日志，按「这个号 + 失败来源」筛：只按号筛会把签到之类无关事件一起带出来，
  // 用户想看的是这个号到底因为什么被拦。时间范围不设限，早先的失败也能翻到。
  const id = (errDetailAccount && errDetailAccount.id) || '';
  closeEvModal('errDetailModal');
  EV.account = id;
  EV.scopes = ['relay', 'pool'];
  EV.range = 'all';
  syncEvFilterUI();
  switchPage('syslogs', { force: true });
});

function openRowMenu(anchor, id) {
  const acc = STATE.accounts.find((a) => a.id === id);
  if (!acc) return;
  const sn = providerOf(acc) === 'sensenova';
  // 日日新没有「签到」也没有小浣熊那套 refresh_token：「刷新凭据」对它
  // 的实际含义是重登控制台、补建 API Key——保留按钮但改名，避免误操作。
  // 外部渠道有每日签到，「刷新凭据」是续期临时凭据。
  const el = openPop(anchor, `
    <div class="pop-menu">
      <button data-m="balance">${sn ? '查额度' : '查余额'}</button>
      <button data-m="refresh">${sn ? '重登并补建 Key' : '刷新凭据'}</button>
      ${sn ? '' : '<button data-m="checkin">签到</button>'}
      <button data-m="errors">错误详情</button>
      <button data-m="uncool">解除冷却</button>
      <button data-m="toggle">${acc.enabled ? '停用账号' : '启用账号'}</button>
      <div class="sep"></div>
      <button data-m="delete" class="danger">删除账号</button>
    </div>`, { width: 170 });

  el.querySelectorAll('[data-m]').forEach((b) => {
    b.addEventListener('click', () => {
      closePop();
      if (b.dataset.m === 'errors') openAccountErrors(id);
      else accountAction(b.dataset.m, id);
    });
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
      <div class="field"><label>并发上限（0 = 不单独限制）</label><input id="asMaxConcurrent" type="number" step="1" min="0" value="${Number(acc.maxConcurrent) || 0}"></div>
      <p class="hint">uid：${esc(acc.uid)}<br>最近一次使用：${acc.lastUsed ? fmtTime(acc.lastUsed) : '从未'}<br>
        ${providerOf(acc) === 'sensenova'
          ? `控制台账号：${esc(acc.snUser || '未记录')}<br>
             登录票：${acc.snTokenValid ? '有效' : '已失效（转发或查额度时会自动重登）'}<br>
             额度：${acc.snPoolAt ? '更新于 ' + fmtTime(acc.snPoolAt) : '尚未同步'}<br>`
          : (providerOf(acc) === 'autoclaw' || providerOf(acc) === 'codearts')
            ? `站点：${acc.extSite === 'cn' ? '国内站' : '国际站'}<br>
               临时凭据：${acc.extTokenExp ? '至 ' + fmtTime(acc.extTokenExp) : '未知'}${acc.hasExtRefresh ? '（可自动续期）' : '（无续期材料，到期需重新导入）'}<br>
               额度：${acc.snPoolAt ? '更新于 ' + fmtTime(acc.snPoolAt) : '尚未同步'}<br>
               今日签到：${STATE.today && acc.lastCheckinDay === STATE.today ? '已签到' : esc(acc.lastCheckinDay || '未签到')}<br>`
            : `今日签到：${STATE.today && acc.lastCheckinDay === STATE.today ? '已签到' : esc(acc.lastCheckinDay || '未签到')}<br>`}
        并发上限：该号同时最多接几个请求，达到后新请求会落到别的号；0 表示不单独限制。<br>
        优先级：数值大的先被选中（两种选号策略都生效）。想让主力号先用光再切备用号，就给主力号设更高的值。</p>
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
          maxConcurrent: Math.max(0, Math.round(Number(el.querySelector('#asMaxConcurrent').value) || 0)),
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
      const p = providerOf(acc);
      toast(p === 'sensenova' ? `额度已同步 · 主池剩余 ${fmtCredits(r.credits)}`
        : p === 'autoclaw' ? `余额 ${fmtCredits(r.credits)} 积分`
        : p === 'codearts' ? `剩余 ${fmtCredits(r.credits)} 积分`
        : `余额 ${fmtCredits(r.credits)} 积分`);
    } else if (act === 'refresh' && (providerOf(acc) === 'sensenova' || providerOf(acc) === 'autoclaw' || providerOf(acc) === 'codearts')) {
      const r = await api(`/api/accounts/${encodeURIComponent(id)}/refresh`, { method: 'POST' });
      toast(r.message || '已完成');
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
      <button data-b="setMaxConcurrent">设置并发上限（选中）</button>
      <button data-b="setMaxConcurrentAll">设置并发上限（全部）</button>
      <div class="sep"></div>
      <button data-b="delete" class="danger">批量删除</button>
    </div>`, { width: 190 });
  el.querySelectorAll('[data-b]').forEach((b) => {
    b.addEventListener('click', () => {
      closePop();
      const act = b.dataset.b;
      if (act === 'setMaxConcurrent') { promptMaxConcurrent([...POOL.sel]); return; }
      if (act === 'setMaxConcurrentAll') { promptMaxConcurrent([]); return; }
      batchAction(act);
    });
  });
});

// 设置单号并发上限：ids 为空表示全部账号。
async function promptMaxConcurrent(ids) {
  const scope = ids.length ? `${ids.length} 个账号` : '全部账号';
  const raw = prompt(`为${scope}设置并发上限（0 = 不单独限制）：`, '1');
  if (raw === null) return;
  const value = Number(raw);
  if (!Number.isFinite(value) || value < 0) { toast('请填写 0 或正整数', true); return; }
  try {
    const r = await api('/api/accounts/batch', {
      method: 'POST',
      body: JSON.stringify({ action: 'setMaxConcurrent', ids, value: Math.round(value) }),
    });
    toast(`并发上限已设 ${Math.round(value)}：成功 ${r.done || 0}${r.failed ? `，失败 ${r.failed}` : ''}`);
    await Promise.all([loadAccounts(), loadStatus()]);
  } catch (e) { toast(e.message, true); }
}

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

// 有勾选就只导勾选的号，没勾选导全池（小浣熊 + 日日新都在包里）。
// POST 是因为要带 id 清单，拿到 blob 后本地触发下载，文件名用后端定的。
$('exportBtn').addEventListener('click', async () => {
  const ids = [...POOL.sel];
  if (ids.length) $('tableMsg').textContent = `正在导出选中的 ${ids.length} 个账号…`;
  else $('tableMsg').textContent = '正在导出全部账号…';
  try {
    const res = await fetch('/api/accounts/export', {
      method: ids.length ? 'POST' : 'GET',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: ids.length ? JSON.stringify({ ids }) : undefined,
    });
    if (!res.ok) {
      let msg = `导出失败（HTTP ${res.status}）`;
      try { msg = (await res.json()).message || msg; } catch (_) { /* 保持默认 */ }
      throw new Error(msg);
    }
    const blob = await res.blob();
    const cd = res.headers.get('Content-Disposition') || '';
    const m = /filename="([^"]+)"/.exec(cd);
    const name = (m && m[1])
      || `agenthub-credentials-raccoon-${new Date().toISOString().slice(0, 10)}.json`;
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.click();
    URL.revokeObjectURL(url);
    $('tableMsg').textContent = `已导出 ${ids.length || '全部'} 个账号`;
  } catch (e) {
    $('tableMsg').textContent = e.message;
    toast(e.message, true);
  }
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

// 商汤日日新：账号密码导入。登录换票 + 建 Key 在服务端完成，面板只负责提交与回显。
$('snAddBtn').addEventListener('click', async () => {
  const body = {
    username: $('snUsername').value.trim(),
    password: $('snPassword').value,
    apiKey: $('snApiKey').value.trim(),
    name: $('snName').value.trim(),
    note: $('snNote').value.trim(),
  };
  if (!body.username || !body.password) {
    $('snMsg').textContent = '控制台账号和密码都不能为空';
    return;
  }
  $('snMsg').textContent = '登录控制台并创建 API Key…（可能需要几秒）';
  $('snAddBtn').disabled = true;
  try {
    const r = await api('/api/auth/sensenova', { method: 'POST', body: JSON.stringify(body) });
    $('snMsg').textContent = r.message + (r.uid ? `（账号 ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      ['snUsername', 'snPassword', 'snApiKey', 'snName', 'snNote'].forEach((id) => { $(id).value = ''; });
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) {
    $('snMsg').textContent = e.message;
  } finally {
    $('snAddBtn').disabled = false;
  }
});

// AutoClaw：粘贴凭据导入（国际站/国内站均可）。
$('acImportBtn').addEventListener('click', async () => {
  const body = {
    credential: $('acCred').value.trim(),
    site: $('acSite').value,
    name: $('acName').value.trim(),
  };
  if (!body.credential) { $('acMsg').textContent = '凭据不能为空'; return; }
  $('acMsg').textContent = '导入中…';
  try {
    const r = await api('/api/auth/autoclaw', { method: 'POST', body: JSON.stringify(body) });
    $('acMsg').textContent = r.message + (r.uid ? `（账号 ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      $('acCred').value = ''; $('acName').value = '';
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) { $('acMsg').textContent = e.message; }
});

// AutoClaw 国内站：发验证码。
$('acSendCodeBtn').addEventListener('click', async () => {
  const phone = $('acPhone').value.trim();
  if (!phone) { $('acLoginMsg').textContent = '请先填手机号'; return; }
  $('acLoginMsg').textContent = '发送中…';
  try {
    const r = await api('/api/auth/autoclaw/send-code', { method: 'POST', body: JSON.stringify({ phone }) });
    $('acLoginMsg').textContent = r.message || '验证码已发送';
  } catch (e) { $('acLoginMsg').textContent = e.message; }
});

// AutoClaw 国内站：手机验证码登录。
$('acLoginBtn').addEventListener('click', async () => {
  const body = {
    phone: $('acPhone').value.trim(),
    code: $('acCode').value.trim(),
    name: $('acName').value.trim(),
  };
  if (!body.phone || !body.code) { $('acLoginMsg').textContent = '手机号和验证码都要填'; return; }
  $('acLoginMsg').textContent = '登录中…';
  try {
    const r = await api('/api/auth/autoclaw/login', { method: 'POST', body: JSON.stringify(body) });
    $('acLoginMsg').textContent = r.message + (r.uid ? `（账号 ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      ['acPhone', 'acCode', 'acName'].forEach((id) => { $(id).value = ''; });
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) { $('acLoginMsg').textContent = e.message; }
});

// 码道 CodeArts：粘贴凭据 JSON 导入。
$('caImportBtn').addEventListener('click', async () => {
  const body = {
    credential: $('caCred').value.trim(),
    site: $('caSite').value,
    name: $('caName').value.trim(),
  };
  if (!body.credential) { $('caMsg').textContent = '凭据不能为空'; return; }
  $('caMsg').textContent = '导入中…';
  try {
    const r = await api('/api/auth/codearts', { method: 'POST', body: JSON.stringify(body) });
    $('caMsg').textContent = r.message + (r.uid ? `（账号 ${shortUID(r.uid)}）` : '');
    if (r.ok) {
      $('caCred').value = ''; $('caName').value = '';
      await Promise.all([loadAccounts(), loadStatus()]);
    }
  } catch (e) { $('caMsg').textContent = e.message; }
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
    $('setAccountAbnormalCool').value = String(s.accountAbnormalCool !== false);
    $('setCreditStopEnable').value = String(!!s.creditStopEnable);
    $('setCreditStopBelow').value = s.creditStopBelow ?? 0;
    // 号池页的余额条要标红「低于暂停阈值」的号，这里顺手把阈值带过去。
    POOL.creditStopBelow = s.creditStopEnable ? (s.creditStopBelow ?? 0) : 0;
    $('setCreditResumeAbove').value = s.creditResumeAbove ?? 0;
    $('setMaxConcurrent').value = s.maxConcurrent ?? 0;
    $('setQueueWaitSec').value = s.queueWaitSec ?? 600;
    $('setBalanceSyncIntervalMin').value = s.balanceSyncIntervalMin ?? 10;
    $('setBalanceSyncStaleMin').value = s.balanceSyncStaleMin ?? 30;
    $('setClientVersion').value = s.clientVersion || '';
    $('setWebClientVersion').value = s.webClientVersion || '';
    $('setClientOSVersion').value = s.clientOSVersion || '';
    $('setClientPlatform').value = s.clientPlatform || '';
    // 输入框回填原始值（空 = 自动派生）；旁边提示实际发送的 UA，避免用户以为没生效。
    $('setClientUserAgent').value = s.clientUserAgent || '';
    $('setWebUserAgent').value = s.webUserAgent || '';
    $('setClientUserAgent').placeholder = '留空 = ' + (s.effectiveClientUserAgent || '按版本号派生');
    $('setWebUserAgent').placeholder = '留空 = ' + (s.effectiveWebUserAgent || '按版本号派生');
    // 调度设置
    $('setLeaseEnabled').value = String(s.leaseEnabled !== false);
    $('setLeaseConcurrency').value = s.leaseConcurrency ?? 2;
    $('setLeaseTTLSec').value = s.leaseTTLSec ?? 300;
    $('setRetryDelayMS').value = s.retryDelayMS ?? 800;
    $('setBackoffEnabled').value = String(s.backoffEnabled !== false);
    $('setBackoffBaseMS').value = s.backoffBaseMS ?? 2000;
    $('setBackoffMaxMS').value = s.backoffMaxMS ?? 60000;
    $('setAffinityEnabled').value = String(s.affinityEnabled !== false);
    $('setAffinityTTLSec').value = s.affinityTTLSec ?? 86400;
    // 外部渠道基址：空 = 用内置默认。
    $('setAutoClawIntlBase').value = s.autoClawIntlBase || '';
    $('setAutoClawCNBase').value = s.autoClawCNBase || '';
    $('setAutoClawCNManagedBase').value = s.autoClawCNManagedBase || '';
    $('setCodeArtsMainlandBase').value = s.codeArtsMainlandBase || '';
    $('setCodeArtsIntlBase').value = s.codeArtsIntlBase || '';
    // 网关出站改写与 models.dev（同一份 /api/settings 里的字段）
    if (typeof applyGatewayState === 'function') applyGatewayState(s);
  } catch (e) {
    $('settingsMsg').textContent = e.message;
  }
}

// 外部渠道设置单独保存：只提交本卡片字段。
$('saveExternalBtn').addEventListener('click', async () => {
  const body = {
    autoClawIntlBase: $('setAutoClawIntlBase').value.trim(),
    autoClawCNBase: $('setAutoClawCNBase').value.trim(),
    autoClawCNManagedBase: $('setAutoClawCNManagedBase').value.trim(),
    codeArtsMainlandBase: $('setCodeArtsMainlandBase').value.trim(),
    codeArtsIntlBase: $('setCodeArtsIntlBase').value.trim(),
  };
  $('externalMsg').textContent = '保存中…';
  try {
    await api('/api/settings', { method: 'PUT', body: JSON.stringify(body) });
    $('externalMsg').textContent = '已保存（留空的项使用内置默认地址）';
    toast('外部渠道设置已保存');
  } catch (e) {
    $('externalMsg').textContent = e.message;
  }
});

// 调度设置单独保存：只提交本卡片字段。
$('saveDispatchBtn').addEventListener('click', async () => {
  const nums = {
    setLeaseConcurrency: ['租约并发数', 1, 64],
    setLeaseTTLSec: ['租约时长', 30, 3600],
    setRetryDelayMS: ['换号间隔', 0, 60000],
    setBackoffBaseMS: ['退避起点', 100, 600000],
    setBackoffMaxMS: ['退避上限', 100, 3600000],
    setAffinityTTLSec: ['会话绑定时长', 60, 2592000],
  };
  const vals = {};
  for (const [id, [label, min, max]] of Object.entries(nums)) {
    const v = numOrNaN($(id));
    if (!Number.isFinite(v) || v < min || v > max) {
      $('dispatchMsg').textContent = `${label}请填写 ${min}~${max}`;
      return;
    }
    vals[id] = Math.round(v);
  }
  if (vals.setBackoffMaxMS < vals.setBackoffBaseMS) {
    $('dispatchMsg').textContent = '退避上限不能小于起点';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        leaseEnabled: $('setLeaseEnabled').value === 'true',
        leaseConcurrency: vals.setLeaseConcurrency,
        leaseTTLSec: vals.setLeaseTTLSec,
        retryDelayMS: vals.setRetryDelayMS,
        backoffEnabled: $('setBackoffEnabled').value === 'true',
        backoffBaseMS: vals.setBackoffBaseMS,
        backoffMaxMS: vals.setBackoffMaxMS,
        affinityEnabled: $('setAffinityEnabled').value === 'true',
        affinityTTLSec: vals.setAffinityTTLSec,
      }),
    });
    $('dispatchMsg').textContent = '已保存，立即生效';
    await loadSettings();
  } catch (e) {
    $('dispatchMsg').textContent = e.message;
  }
});

// 指纹设置单独保存：只提交指纹字段，避免和别的卡片互相覆盖。
// 四个版本/平台项不能留空（后端也会拒绝）；两个 UA 留空是合法值，表示按版本号派生。
$('saveFingerprintBtn').addEventListener('click', async () => {
  const required = [
    ['setClientVersion', '对话端版本号'],
    ['setWebClientVersion', '管理面版本号'],
    ['setClientOSVersion', '系统版本'],
    ['setClientPlatform', '桌面平台'],
  ];
  for (const [id, label] of required) {
    if (String($(id).value).trim() === '') {
      $('fingerprintMsg').textContent = label + '不能为空';
      return;
    }
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        clientVersion: $('setClientVersion').value.trim(),
        webClientVersion: $('setWebClientVersion').value.trim(),
        clientOSVersion: $('setClientOSVersion').value.trim(),
        clientPlatform: $('setClientPlatform').value.trim(),
        // 留空 = 自动派生；这里不 trim 成 undefined，空串本身就是要保存的值。
        clientUserAgent: $('setClientUserAgent').value.trim(),
        webUserAgent: $('setWebUserAgent').value.trim(),
      }),
    });
    $('fingerprintMsg').textContent = '已保存，立即生效';
    await loadSettings();
  } catch (e) {
    $('fingerprintMsg').textContent = e.message;
  }
});

// 并发设置单独保存：只提交并发相关字段，避免和别的卡片互相覆盖。
$('saveConcurrencyBtn').addEventListener('click', async () => {
  const limit = numOrNaN($('setMaxConcurrent'));
  const wait = numOrNaN($('setQueueWaitSec'));
  if (!Number.isFinite(limit) || limit < 0) {
    $('concurrencyMsg').textContent = '并发上限请填写 0 或正整数（0 = 不限制）';
    return;
  }
  if (!Number.isFinite(wait) || wait < 1 || wait > 3600) {
    $('concurrencyMsg').textContent = '排队时间请填写 1~3600 秒';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        maxConcurrent: Math.round(limit),
        queueWaitSec: Math.round(wait),
      }),
    });
    $('concurrencyMsg').textContent = limit === 0 ? '已保存，当前不限制并发' : '已保存，立即生效';
    await loadSettings();
  } catch (e) {
    $('concurrencyMsg').textContent = e.message;
  }
});

// 余额巡检单独保存：只提交本卡片字段。
$('saveBalanceSyncBtn').addEventListener('click', async () => {
  const interval = numOrNaN($('setBalanceSyncIntervalMin'));
  const stale = numOrNaN($('setBalanceSyncStaleMin'));
  if (!Number.isFinite(interval) || interval < 0 || interval > 1440) {
    $('balanceSyncMsg').textContent = '巡检间隔请填写 0~1440 分钟（0 = 关闭）';
    return;
  }
  if (!Number.isFinite(stale) || stale < 0 || stale > 10080) {
    $('balanceSyncMsg').textContent = '陈旧窗口请填写 0~10080 分钟';
    return;
  }
  if (interval > 0 && stale < interval) {
    $('balanceSyncMsg').textContent = '陈旧窗口不能小于巡检间隔，否则每轮都会重查全部账号';
    return;
  }
  try {
    await api('/api/settings', {
      method: 'PUT',
      body: JSON.stringify({
        balanceSyncIntervalMin: Math.round(interval),
        balanceSyncStaleMin: Math.round(stale),
      }),
    });
    $('balanceSyncMsg').textContent = interval === 0 ? '已保存，已关闭周期巡检' : '已保存，下一轮巡检生效';
    await loadSettings();
  } catch (e) {
    $('balanceSyncMsg').textContent = e.message;
  }
});

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
        accountAbnormalCool: $('setAccountAbnormalCool').value === 'true',
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

// ---------- 网关 · 出站改写与 models.dev ----------
// 脱敏词表编辑器：表格直接编辑，改动先留在 GATEWAY.rules，点「保存」才提交。
// 三态与后端一致：未改 = 不动；空数组 = 恢复内置；数组 = 自定义。
const GATEWAY = {
  rules: [],            // 当前编辑中的词表
  defaults: [],         // 内置词表（恢复用）
  providers: [],        // 已选应用提供商（空 = 全部）
  providerOptions: [],
  limits: { maxRules: 200, maxNameChars: 128, maxPatternChars: 4096, maxReplacementChars: 4096 },
  roles: ['system', 'developer', 'user', 'assistant', 'tool', 'function'],
  custom: false,
  restore: false,
  dirty: false,
};

const ROLE_LABELS = { system: '系统', developer: '开发者', user: '用户', assistant: '助手', tool: '工具', function: '函数' };

function cloneRules(list) {
  return (list || []).map((r) => ({
    id: r.id, name: r.name, enabled: r.enabled !== false,
    matchType: r.matchType === 'regex' ? 'regex' : 'literal',
    pattern: r.pattern || '', replacement: r.replacement || '',
    repeat: r.repeat === true, roles: Array.isArray(r.roles) ? r.roles.slice() : [],
  }));
}

// 前端先做与后端一致的校验，避免每次保存都等一次往返。
function validateRule(rule, index) {
  const lim = GATEWAY.limits;
  const label = `第 ${index + 1} 条`;
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(rule.id)) return `${label}：id 必须是 1–64 位字母、数字、连字符或下划线`;
  if (!rule.name.trim() || [...rule.name.trim()].length > lim.maxNameChars) return `${label}：名称不能为空且最多 ${lim.maxNameChars} 字`;
  if (!rule.pattern.trim() || [...rule.pattern].length > lim.maxPatternChars) return `${label}：匹配内容不能为空且最多 ${lim.maxPatternChars} 字`;
  if ([...rule.replacement].length > lim.maxReplacementChars) return `${label}：替换内容最多 ${lim.maxReplacementChars} 字`;
  if (rule.repeat && (rule.matchType !== 'regex' || rule.replacement !== '')) return `${label}：只有正则删除规则可以启用重复清理`;
  if (rule.matchType === 'regex') {
    try { new RegExp(rule.pattern); } catch (e) { return `${label}：正则无效（${e.message}）`; }
  }
  return '';
}

function sanitizeStateText(on, custom) {
  if (!on) return '已关闭';
  return custom ? '已开启 · 自定义词表' : '已开启 · 内置词表';
}

function renderSanitizeProviders() {
  const wrap = $('sanitizeProviders');
  if (!wrap) return;
  const selected = new Set(GATEWAY.providers);
  wrap.innerHTML = GATEWAY.providerOptions.map((opt) => `
    <label class="chip ${selected.has(opt.id) ? 'on' : ''}" data-provider="${esc(opt.id)}">
      <input type="checkbox" ${selected.has(opt.id) ? 'checked' : ''}>${esc(opt.label || opt.id)}
    </label>`).join('');
  wrap.querySelectorAll('.chip').forEach((chip) => {
    chip.addEventListener('click', (e) => {
      e.preventDefault();
      const id = chip.dataset.provider;
      const set = new Set(GATEWAY.providers);
      if (set.has(id)) set.delete(id); else set.add(id);
      GATEWAY.providers = [...set];
      GATEWAY.dirty = true;
      renderSanitizeProviders();
    });
  });
}

function roleToggles(rule) {
  // 空列表 = 全字段；一旦点了某个角色，就变成「只作用于这些角色的正文」。
  const active = new Set(rule.roles);
  return GATEWAY.roles.map((role) => `
    <span class="role-toggle ${active.has(role) ? 'on' : ''}" data-role="${role}">${ROLE_LABELS[role] || role}</span>`).join('');
}

function renderSanitizeTable() {
  const tbody = document.querySelector('#sanitizeTable tbody');
  if (!tbody) return;
  if (!GATEWAY.rules.length) {
    tbody.innerHTML = '<tr><td colspan="7" class="hint">词表为空。点「新增规则」或「恢复内置词表」。</td></tr>';
  } else {
    tbody.innerHTML = GATEWAY.rules.map((rule, i) => `
      <tr data-index="${i}">
        <td><label class="switch"><input type="checkbox" data-field="enabled" ${rule.enabled ? 'checked' : ''}><span></span></label></td>
        <td>
          <input type="text" data-field="name" value="${esc(rule.name)}" placeholder="名称">
          <input type="text" data-field="id" value="${esc(rule.id)}" placeholder="id" style="margin-top:4px;font-family:var(--font-mono)">
        </td>
        <td>
          <select data-field="matchType">
            <option value="literal" ${rule.matchType === 'literal' ? 'selected' : ''}>字面</option>
            <option value="regex" ${rule.matchType === 'regex' ? 'selected' : ''}>正则</option>
          </select>
          <label class="hint" style="display:flex;gap:5px;align-items:center;margin-top:6px;font-size:10.5px">
            <input type="checkbox" data-field="repeat" ${rule.repeat ? 'checked' : ''}> 重复清理
          </label>
        </td>
        <td><textarea data-field="pattern" rows="2" placeholder="匹配内容">${esc(rule.pattern)}</textarea></td>
        <td><textarea data-field="replacement" rows="2" placeholder="留空 = 删除">${esc(rule.replacement)}</textarea></td>
        <td class="roles-cell">${roleToggles(rule)}</td>
        <td class="row" style="gap:4px"><button class="danger tiny" data-act="del">删</button></td>
      </tr>`).join('');
  }
  $('sanitizeCount').textContent = `${GATEWAY.rules.length} 条`;
  $('sanitizeModeTag').textContent = GATEWAY.restore ? '将恢复内置' : (GATEWAY.dirty ? '未保存' : (GATEWAY.custom ? '自定义' : '内置'));
}

function bindSanitizeTable() {
  const tbody = document.querySelector('#sanitizeTable tbody');
  if (!tbody || tbody.dataset.bound) return;
  tbody.dataset.bound = '1';

  tbody.addEventListener('input', (e) => {
    const tr = e.target.closest('tr[data-index]');
    if (!tr) return;
    const rule = GATEWAY.rules[Number(tr.dataset.index)];
    if (!rule) return;
    const field = e.target.dataset.field;
    if (!field) return;
    if (field === 'enabled' || field === 'repeat') rule[field] = e.target.checked;
    else rule[field] = e.target.value;
    GATEWAY.dirty = true;
    $('sanitizeModeTag').textContent = '未保存';
  });
  tbody.addEventListener('change', (e) => {
    if (e.target.dataset.field === 'matchType') {
      const tr = e.target.closest('tr[data-index]');
      const rule = GATEWAY.rules[Number(tr.dataset.index)];
      if (rule) { rule.matchType = e.target.value; GATEWAY.dirty = true; }
    }
  });
  tbody.addEventListener('click', (e) => {
    const tr = e.target.closest('tr[data-index]');
    if (!tr) return;
    const rule = GATEWAY.rules[Number(tr.dataset.index)];
    if (!rule) return;
    if (e.target.closest('button[data-act=del]')) {
      GATEWAY.rules.splice(Number(tr.dataset.index), 1);
      GATEWAY.dirty = true;
      renderSanitizeTable();
      return;
    }
    const role = e.target.closest('.role-toggle');
    if (role) {
      const set = new Set(rule.roles);
      if (set.has(role.dataset.role)) set.delete(role.dataset.role); else set.add(role.dataset.role);
      rule.roles = GATEWAY.roles.filter((r) => set.has(r));
      GATEWAY.dirty = true;
      renderSanitizeTable();
    }
  });
}

function applySanitizeState(s) {
  GATEWAY.providerOptions = Array.isArray(s.sanitizeProviderOptions) ? s.sanitizeProviderOptions : [];
  GATEWAY.defaults = cloneRules(s.sanitizeDefaults);
  GATEWAY.limits = Object.assign(GATEWAY.limits, s.sanitizeLimits || {});
  GATEWAY.providers = Array.isArray(s.sanitizeProviders) ? s.sanitizeProviders.slice() : [];
  GATEWAY.custom = s.sanitizeCustomRules === true;
  if (!GATEWAY.dirty) {
    GATEWAY.rules = cloneRules(s.sanitizeRules);
    renderSanitizeTable();
  }
  const on = s.sanitizeEnabled === true;
  $('setSanitizeEnabled').checked = on;
  $('sanitizeStateTag').textContent = sanitizeStateText(on, GATEWAY.custom);
  renderSanitizeProviders();
}

// models.dev 状态 → 面板读数。
function applyModelsDevState(s) {
  const on = s.modelsDevEnabled === true;
  $('setModelsDevEnabled').checked = on;
  $('modelsDevStateTag').textContent = on ? '已开启' : '已关闭（现有数据继续用）';
  const src = s.modelsDevSource === 'builtin' ? '随包内嵌快照'
    : s.modelsDevSource === 'cache' ? '上次刷新的缓存'
      : s.modelsDevSource === 'network' ? '联网拉取' : '—';
  $('modelsDevSource').textContent = src;
  $('modelsDevCount').textContent = s.modelsDevCount ? `${Number(s.modelsDevCount).toLocaleString('zh-CN')} 条` : '—';
  $('modelsDevUpdated').textContent = s.modelsDevUpdatedAt ? fmtTime(s.modelsDevUpdatedAt) : '—';
  $('modelsDevLastRefresh').textContent = s.modelsDevLastRefreshAt
    ? `${s.modelsDevLastRefresh || ''} · ${fmtTime(s.modelsDevLastRefreshAt)}`
    : '尚未手动刷新';
}

function applyGatewayState(s) {
  applySanitizeState(s);
  applyModelsDevState(s);
  const role = s.developerRoleCompat !== false;
  $('setDeveloperRoleCompat').checked = role;
  $('roleCompatStateTag').textContent = role ? '已开启' : '已关闭';
}

// 打开设置页时单独拉一次网关状态（与 /api/settings 同源，避免漏项）。
async function loadGatewaySettings() {
  try {
    const r = await api('/api/settings');
    applyGatewayState(r.settings || {});
  } catch (e) {
    $('sanitizeMsg').textContent = e.message;
  }
}

function bindGatewayEvents() {
  if (!$('saveGatewayToggleBtn')) return;

  $('setSanitizeEnabled').addEventListener('change', () => {
    $('sanitizeStateTag').textContent = sanitizeStateText($('setSanitizeEnabled').checked, GATEWAY.custom);
  });
  $('setDeveloperRoleCompat').addEventListener('change', () => {
    $('roleCompatStateTag').textContent = $('setDeveloperRoleCompat').checked ? '已开启' : '已关闭';
  });

  $('sanitizeAddRule').addEventListener('click', () => {
    if (GATEWAY.rules.length >= GATEWAY.limits.maxRules) {
      $('sanitizeMsg').textContent = `词表最多 ${GATEWAY.limits.maxRules} 条`;
      return;
    }
    GATEWAY.rules.push({
      id: 'rule-' + Date.now().toString(36), name: '新规则', enabled: true,
      matchType: 'literal', pattern: '', replacement: '', repeat: false, roles: [],
    });
    GATEWAY.dirty = true;
    renderSanitizeTable();
  });

  $('sanitizeRestore').addEventListener('click', () => {
    if (!confirm('恢复内置词表？当前编辑中的规则会被丢弃。')) return;
    GATEWAY.rules = cloneRules(GATEWAY.defaults);
    GATEWAY.restore = true;
    GATEWAY.dirty = true;
    renderSanitizeTable();
  });

  $('sanitizeExport').addEventListener('click', () => {
    const blob = new Blob([JSON.stringify(GATEWAY.rules, null, 2)], { type: 'application/json' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'raccoon-sanitize-rules.json';
    a.click();
    URL.revokeObjectURL(a.href);
  });

  $('sanitizeImport').addEventListener('change', async (e) => {
    const file = e.target.files && e.target.files[0];
    if (!file) return;
    try {
      const list = JSON.parse(await file.text());
      if (!Array.isArray(list)) throw new Error('文件内容必须是规则数组');
      GATEWAY.rules = cloneRules(list);
      GATEWAY.restore = false;
      GATEWAY.dirty = true;
      renderSanitizeTable();
      $('sanitizeMsg').textContent = '已导入，点「保存」生效';
    } catch (err) {
      $('sanitizeMsg').textContent = '导入失败：' + err.message;
    } finally {
      e.target.value = '';
    }
  });

  bindSanitizeTable();

  // 保存：先本地校验，再整体提交（后端会再做一次完整校验）。
  $('saveGatewayToggleBtn').addEventListener('click', async () => {
    for (let i = 0; i < GATEWAY.rules.length; i++) {
      const err = validateRule(GATEWAY.rules[i], i);
      if (err) { $('sanitizeMsg').textContent = err; return; }
    }
    const ids = new Set();
    for (const rule of GATEWAY.rules) {
      if (ids.has(rule.id)) { $('sanitizeMsg').textContent = `规则 id 重复：${rule.id}`; return; }
      ids.add(rule.id);
    }
    const body = {
      sanitizeEnabled: $('setSanitizeEnabled').checked,
      developerRoleCompat: $('setDeveloperRoleCompat').checked,
      sanitizeProviders: GATEWAY.providers,
      // 恢复内置 = 提交空数组（后端据此回落内置词表）。
      sanitizeRules: GATEWAY.restore ? [] : GATEWAY.rules,
    };
    $('sanitizeMsg').textContent = '保存中…';
    try {
      await api('/api/settings', { method: 'PUT', body: JSON.stringify(body) });
      GATEWAY.dirty = false;
      GATEWAY.restore = false;
      $('sanitizeMsg').textContent = '已保存，下一个请求立即生效';
      toast('网关出站策略已保存');
      await loadGatewaySettings();
    } catch (e) {
      $('sanitizeMsg').textContent = e.message;
    }
  });

  $('saveModelsDevBtn').addEventListener('click', async () => {
    $('modelsDevMsg').textContent = '保存中…';
    try {
      await api('/api/settings', {
        method: 'PUT',
        body: JSON.stringify({ modelsDevEnabled: $('setModelsDevEnabled').checked }),
      });
      $('modelsDevMsg').textContent = '已保存';
      await loadGatewaySettings();
      // 能力补齐状态变了，模型管理页的来源标记也要跟着刷新。
      if (STATE.loaded.has('models')) loadModels();
    } catch (e) {
      $('modelsDevMsg').textContent = e.message;
    }
  });

  $('refreshModelsDevBtn').addEventListener('click', async () => {
    $('modelsDevMsg').textContent = '正在拉取 models.dev…（首次可能几秒）';
    try {
      const r = await api('/api/gateway/modelsdev/refresh', { method: 'POST' });
      $('modelsDevMsg').textContent = (r.ok ? '' : '未刷新：') + (r.message || '');
      await loadGatewaySettings();
      if (r.ok && STATE.loaded.has('models')) loadModels();
    } catch (e) {
      $('modelsDevMsg').textContent = e.message;
    }
  });
}

bindGatewayEvents();

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
  // 499 单列：请求方在完成前断开（App 里点了停止 / 关掉页面 / 客户端超时）。
  // 它既不是上游失败，也不是账号问题，用红色 5xx 样式会误导排障。
  if (code === 499) return `<span class="lpill" title="客户端已断开（用户取消或客户端超时），与上游无关">499 取消</span>`;
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
    // 499（客户端取消）不染失败底色：失败色是给「需要人看一眼」的行用的。
    const code = Number(l.status) || 0;
    const isFail = code >= 400 && code !== 499;
    return `<tr data-id="${esc(l.id)}" class="${isFail ? 'is-fail' : ''}">
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
      ${l.error ? '<button class="ghost tiny" id="ldCopy">复制错误</button>' : ''}
      <button class="ghost tiny" id="ldClose">关闭</button></div>`, { width: 560 });
  document.getElementById('ldClose').addEventListener('click', closePop);
  const copyBtn = document.getElementById('ldCopy');
  if (copyBtn) {
    copyBtn.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(String(l.error || ''));
        toast('错误信息已复制');
      } catch {
        // 非安全上下文（http 访问）没有 clipboard API，退回选中文本让用户手动复制
        const pre = document.querySelector('.pop .ld-err');
        if (pre) {
          const range = document.createRange();
          range.selectNodeContents(pre);
          const sel = window.getSelection();
          sel.removeAllRanges();
          sel.addRange(range);
        }
        toast('已选中错误文本，请手动复制', true);
      }
    });
  }
}

// ---------- 运行日志（结构化事件流 + 文件与清理） ----------
function fmtSize(n) {
  const v = Number(n) || 0;
  if (v < 1024) return v + ' B';
  if (v < 1024 * 1024) return (v / 1024).toFixed(1) + ' KB';
  return (v / 1024 / 1024).toFixed(2) + ' MB';
}

// 事件筛选状态。和调用日志一样，条件全部保存在这里，翻页只改 page。
const EV = {
  level: '', range: 'today', problem: false, scopes: [], account: '', q: '',
  page: 1, pageSize: 50, total: 0, pages: 1, seq: 0, facetsLoaded: false,
  scopeLabels: {}, scopesKnown: [],
};
let EV_ROWS = [];

const LEVEL_LABEL = { info: '信息', warn: '警告', error: '错误' };
const SCOPE_FALLBACK = {
  relay: '转发请求', pool: '号池冷却', checkin: '自动签到', renew: '凭证续期',
  update: '版本更新', logs: '日志管理', account: '账号操作', system: '系统',
};

function evScopeLabel(s) {
  return EV.scopeLabels[s] || SCOPE_FALLBACK[s] || s || '-';
}

function evRangeBounds() {
  const now = new Date();
  const startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  switch (EV.range) {
    case 'today': return { since: startOfToday, until: 0, label: '今天' };
    case 'yesterday': return { since: startOfToday - day, until: startOfToday, label: '昨天' };
    case '7d': return { since: startOfToday - 6 * day, until: 0, label: '近 7 天' };
    case '30d': return { since: startOfToday - 29 * day, until: 0, label: '近 30 天' };
    default: return { since: 0, until: 0, label: '全部时间' };
  }
}

function evLevelCell(level) {
  const cls = level === 'error' ? 'bad' : level === 'warn' ? 'warn' : 'ok';
  return `<span class="lpill ${cls}">${esc(LEVEL_LABEL[level] || level)}</span>`;
}

// 摘要行：主行是事件标题，副行是 detail 的第一行（通常是具体原因）。
function evMessageCell(e) {
  const first = String(e.detail || '').split('\n').find((x) => x.trim()) || '';
  const sub = first.length > 120 ? first.slice(0, 120) + '…' : first;
  const countHint = e.count > 1
    ? `<span class="ev-repeat" title="同一事件在合并窗口内重复出现，已折叠">近 ${e.count} 次</span>`
    : '';
  return `<div class="ev-msg">
    <div class="ev-msg-main" title="${esc(e.message)}"><span class="ev-msg-text">${esc(e.message)}</span>${countHint}</div>
    ${sub ? `<div class="ev-msg-sub" title="${esc(first)}">${esc(sub)}</div>` : ''}
  </div>`;
}

function renderEvRows(rows) {
  const tbody = $('evTable').querySelector('tbody');
  $('evEmpty').hidden = rows.length > 0;
  if (!rows.length) { tbody.innerHTML = ''; return; }
  tbody.innerHTML = rows.map((e) => {
    const uid = e.accountUID || e.accountId || '';
    const acc = e.accountName
      ? `<div class="log-acc"><div class="acc-name" title="${esc(uid)}">${esc(e.accountName)}</div><div class="acc-sub">${esc(shortUID(uid))}</div></div>`
      : (uid ? `<div class="log-acc"><div class="acc-name">${esc(shortUID(uid))}</div></div>` : '<span class="dim">-</span>');
    return `<tr data-id="${esc(e.id)}" class="ev-row ${e.level}">
      <td class="ev-c-time"><div class="log-time">${esc(fmtClock(e.ts))}</div></td>
      <td class="ev-c-level">${evLevelCell(e.level)}</td>
      <td class="ev-c-scope"><span class="tag">${esc(evScopeLabel(e.scope))}</span></td>
      <td class="ev-c-account">${acc}</td>
      <td class="ev-c-msg">${evMessageCell(e)}</td>
      <td class="ev-c-count"><span class="ev-count ${e.count > 1 ? 'hot' : ''}">${e.count > 1 ? '×' + e.count : '—'}</span></td>
      <td class="ev-c-detail"><button class="ghost tiny ev-detail" data-id="${esc(e.id)}">详情</button></td>
    </tr>`;
  }).join('');
}

function renderEvFoot(sum) {
  const pages = Math.max(1, EV.pages);
  const b = evRangeBounds();
  EV.total = (sum && sum.total) || 0;
  $('evCountAll').textContent = String(EV.total);
  $('evCountInfo').textContent = String(Math.max(0, EV.total - ((sum && sum.failed) || 0) - ((sum && sum.warning) || 0)));
  $('evCountWarn').textContent = String((sum && sum.warning) || 0);
  $('evCountError').textContent = String((sum && sum.failed) || 0);
  $('evHeaderCount').innerHTML = `${EV.total} 条` +
    (sum && sum.failed ? ` · <span class="bad">${sum.failed} 错误</span>` : '') +
    (sum && sum.warning ? ` · <span class="warn">${sum.warning} 警告</span>` : '');
  $('evRangeInfo').innerHTML = `${esc(b.label)} · 共 ${EV.total} 条`;
  $('evPageInfo').textContent = `第 ${EV.page} / ${pages} 页`;
  $('evPrev').disabled = EV.page <= 1;
  $('evNext').disabled = EV.page >= pages;
  $('evJump').max = String(pages);
  if (document.activeElement !== $('evJump')) $('evJump').value = String(EV.page);
}

function evQuery() {
  const b = evRangeBounds();
  const p = new URLSearchParams();
  if (b.since) p.set('since', b.since);
  if (b.until) p.set('until', b.until);
  if (EV.level) p.set('level', EV.level);
  if (EV.problem) p.set('problem', '1');
  if (EV.scopes.length) p.set('scopes', EV.scopes.join(','));
  if (EV.account) p.set('account', EV.account);
  if (EV.q.trim()) p.set('q', EV.q.trim());
  p.set('page', EV.page);
  p.set('pageSize', EV.pageSize);
  return p;
}

async function loadEvents() {
  const seq = ++EV.seq;
  $('syslogMsg').textContent = '';
  try {
    const r = await api('/api/logs/events?' + evQuery().toString());
    if (seq !== EV.seq) return; // 快速翻页/改筛选时丢弃过期响应
    EV.total = (r.summary && r.summary.total) || 0;
    EV.pages = r.pages || 1;
    if (EV.page > EV.pages) { EV.page = EV.pages; return loadEvents(); }
    EV.page = r.page || EV.page;
    EV_ROWS = r.events || [];
    renderEvRows(EV_ROWS);
    renderEvFoot(r.summary);
  } catch (e) {
    if (seq === EV.seq) $('syslogMsg').textContent = e.message;
  }
}

// 来源下拉依赖历史事件里出现过的值，缓存一次即可。
async function ensureEvFacets() {
  try {
    const f = await api('/api/logs/events/facets');
    EV.scopeLabels = f.labels || SCOPE_FALLBACK;
    EV.scopesKnown = f.scopes || [];
    EV.facetsLoaded = true;
    syncEvFilterUI();
  } catch { /* 拉不到就用内置字典 */ }
}

function applyEvFilter() {
  EV.page = 1;
  loadEvents();
}

function gotoEvPage(p) {
  EV.page = Math.max(1, Math.min(EV.pages || 1, p));
  loadEvents();
}

function syncEvFilterUI() {
  document.querySelectorAll('#evLevel button').forEach((b) => b.classList.toggle('on', b.dataset.v === EV.level));
  document.querySelectorAll('#evRange button').forEach((b) => b.classList.toggle('on', b.dataset.v === EV.range));
  $('evProblem').checked = EV.problem;
  $('evSearch').value = EV.q;
  $('evFilterCount').textContent = EV.scopes.length ? String(EV.scopes.length) : '';

  // 当前生效的筛选项统一做成可点掉的标签条：来源和账号筛选都藏在弹层里，
  // 不显示出来用户会以为列表「怎么只剩下几条」。点 × 即解除该项。
  const chips = EV.scopes.map((s) =>
    `<button class="ev-chip" data-kind="scope" data-v="${esc(s)}">${esc(evScopeLabel(s))}<i>×</i></button>`);
  if (EV.account) {
    const acc = (STATE.accounts || []).find((a) => a.id === EV.account);
    const label = acc ? (acc.name || shortUID(acc.uid)) : shortUID(EV.account);
    chips.push(`<button class="ev-chip ev-chip-acc" data-kind="account" data-v="${esc(EV.account)}">账号：${esc(label)}<i>×</i></button>`);
  }
  const bar = $('evChipBar');
  if (!chips.length) { bar.hidden = true; bar.innerHTML = ''; return; }
  bar.hidden = false;
  bar.innerHTML = chips.join('');
}

document.querySelectorAll('#evLevel button').forEach((b) => {
  b.addEventListener('click', () => { EV.level = b.dataset.v; EV.page = 1; syncEvFilterUI(); loadEvents(); });
});
document.querySelectorAll('#evRange button').forEach((b) => {
  b.addEventListener('click', () => { EV.range = b.dataset.v; syncEvFilterUI(); applyEvFilter(); });
});
$('evProblem').addEventListener('change', () => { EV.problem = $('evProblem').checked; EV.page = 1; loadEvents(); });
$('evPageSize').addEventListener('change', () => { EV.pageSize = Number($('evPageSize').value) || 50; applyEvFilter(); });
$('evPrev').addEventListener('click', () => gotoEvPage(EV.page - 1));
$('evNext').addEventListener('click', () => gotoEvPage(EV.page + 1));
$('evJump').addEventListener('change', () => gotoEvPage(Number($('evJump').value) || 1));

$('evChipBar').addEventListener('click', (e) => {
  const chip = e.target.closest('.ev-chip');
  if (!chip) return;
  if (chip.dataset.kind === 'account') {
    // 账号筛选存的是 id，而输入框里可以填 id、UID 或名称。清掉框里的值，
    // 否则下次点「筛选」会把刚点掉的账号又加回来。
    EV.account = '';
    if ($('evAccountInput')) $('evAccountInput').value = '';
  } else {
    EV.scopes = EV.scopes.filter((s) => s !== chip.dataset.v);
  }
  syncEvFilterUI();
  applyEvFilter();
});

let evSearchTimer = null;
$('evSearch').addEventListener('input', () => {
  clearTimeout(evSearchTimer);
  const v = $('evSearch').value;
  evSearchTimer = setTimeout(() => { EV.q = v; EV.page = 1; loadEvents(); }, 300);
});
$('evSearch').addEventListener('keydown', (e) => {
  if (e.key !== 'Enter') return;
  clearTimeout(evSearchTimer);
  EV.q = $('evSearch').value;
  EV.page = 1;
  loadEvents();
});

$('evReset').addEventListener('click', () => {
  Object.assign(EV, { level: '', range: 'today', problem: false, scopes: [], account: '', q: '' });
  syncEvFilterUI();
  applyEvFilter();
});

// 来源多选 + 账号筛选放在弹层里：平时筛选条已经很满，展开会把行挤到两行。
$('evFilterBtn').addEventListener('click', (e) => {
  const scopes = EV.facetsLoaded && EV.scopesKnown.length ? EV.scopesKnown : Object.keys(SCOPE_FALLBACK);
  $('evModalTitle').textContent = '筛选运行日志';
  $('evModalBody').innerHTML = `
    <div class="ev-modal-sec">
      <div class="ev-modal-label">来源（可多选）</div>
      <div class="ev-modal-checks" id="evScopeChecks">
        ${scopes.map((s) => `<label class="ev-check">
          <input type="checkbox" value="${esc(s)}" ${EV.scopes.includes(s) ? 'checked' : ''}>
          <span>${esc(evScopeLabel(s))}</span>
        </label>`).join('')}
      </div>
    </div>
    <div class="ev-modal-sec">
      <div class="ev-modal-label">账号</div>
      <input id="evAccountInput" placeholder="按账号 UID 或名称包含…" value="${esc(EV.account)}" list="evAccountList">
      <datalist id="evAccountList">${(STATE.accounts || []).map((a) =>
        `<option value="${esc(a.id)}">${esc(a.name || shortUID(a.uid))}</option>`).join('')}</datalist>
    </div>`;
  openEvModal('evModal');
});

$('evModalApply').addEventListener('click', () => {
  EV.scopes = Array.from($('evScopeChecks').querySelectorAll('input:checked')).map((x) => x.value);
  const acc = $('evAccountInput');
  EV.account = acc ? acc.value.trim() : EV.account;
  closeEvModal('evModal');
  syncEvFilterUI();
  applyEvFilter();
});
$('evModalReset').addEventListener('click', () => {
  $('evScopeChecks').querySelectorAll('input').forEach((x) => { x.checked = false; });
  if ($('evAccountInput')) $('evAccountInput').value = '';
  // 重置要真的清掉生效中的筛选，否则点完「重置」列表还是按旧账号/来源筛着。
  EV.account = '';
  EV.scopes = [];
  syncEvFilterUI();
  applyEvFilter();
});
$('evModalClose').addEventListener('click', () => closeEvModal('evModal'));

function openEvModal(id) {
  $(id).hidden = false;
  document.body.classList.add('modal-open');
}
function closeEvModal(id) {
  $(id).hidden = true;
  if (document.querySelectorAll('.modal-scrim:not([hidden])').length === 0) {
    document.body.classList.remove('modal-open');
  }
}
document.querySelectorAll('.modal-scrim').forEach((scrim) => {
  scrim.addEventListener('click', (e) => { if (e.target === scrim) closeEvModal(scrim.id); });
});
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  document.querySelectorAll('.modal-scrim:not([hidden])').forEach((s) => closeEvModal(s.id));
});

// 详情：两个视图 —— 人读的字段视图，和可复制的原始 JSON（等同于事件落库的样子）。
let evDetailRow = null;
$('evTable').addEventListener('click', (e) => {
  const btn = e.target.closest('.ev-detail');
  if (!btn) return;
  const row = EV_ROWS.find((x) => String(x.id) === String(btn.dataset.id));
  if (!row) return;
  evDetailRow = row;
  renderEvDetail('fields');
  openEvModal('evDetailModal');
});

function renderEvDetail(tab) {
  const e = evDetailRow;
  if (!e) return;
  const kv = (k, v) => `<div class="ld-row"><span class="ld-k">${esc(k)}</span><span class="ld-v">${v}</span></div>`;
  const uid = e.accountUID || e.accountId || '';
  const meta = `#${e.id} · ${e.count > 1 ? `合并 ${e.count} 次 · ` : ''}最近 ${fmtClock(e.lastTs || e.ts)}`;
  $('evDetailMeta').textContent = meta;

  const raw = JSON.stringify({
    id: e.id, ts: e.ts, lastTs: e.lastTs, level: e.level, scope: e.scope,
    component: e.component, accountId: e.accountId, accountName: e.accountName,
    accountUid: e.accountUid, message: e.message, detail: e.detail, count: e.count,
  }, null, 2);

  const tabs = `<div class="ev-dtabs">
    <button class="${tab === 'fields' ? 'on' : ''}" data-tab="fields">字段视图</button>
    <button class="${tab === 'raw' ? 'on' : ''}" data-tab="raw">原始记录</button>
  </div>`;

  const fields = `<div class="ev-detail-head">
      ${evLevelCell(e.level)}
      <span class="ev-detail-msg">${esc(e.message)}</span>
    </div>
    <div class="ev-detail-time">${esc(fmtClock(e.ts))}${e.count > 1 ? ` · 合并 ${e.count} 次，最近 ${esc(fmtClock(e.lastTs))}` : ''}</div>
    <div class="pop-body ev-detail-kv">
      ${kv('来源', `<span class="tag">${esc(evScopeLabel(e.scope))}</span> <span class="hint">${esc(e.scope)}</span>`)}
      ${kv('组件', esc(e.component || '-'))}
      ${kv('账号', e.accountName ? `${esc(e.accountName)} <span class="hint">${esc(uid)}</span>` : (uid ? esc(uid) : '-'))}
      ${kv('事件', esc(e.message))}
    </div>
    ${e.detail ? `<div class="ev-detail-block"><div class="ev-detail-label">详情</div><pre class="ld-err ev-pre">${esc(e.detail)}</pre></div>` : ''}`;

  const rawView = `<div class="ev-detail-block"><pre class="ld-err ev-pre raw">${esc(raw)}</pre></div>`;

  $('evDetailBody').innerHTML = tabs + (tab === 'fields' ? fields : rawView);
  $('evDetailBody').querySelectorAll('.ev-dtabs button').forEach((b) => {
    b.addEventListener('click', () => renderEvDetail(b.dataset.tab));
  });
}

$('evDetailClose').addEventListener('click', () => closeEvModal('evDetailModal'));
$('evDetailCopy').addEventListener('click', async () => {
  const e = evDetailRow;
  if (!e) return;
  const text = `[${fmtClock(e.ts)}] [${e.level.toUpperCase()}] [${e.scope}/${e.component}] ${e.message}\n${e.detail || ''}`;
  try {
    await navigator.clipboard.writeText(text);
    $('evDetailCopy').textContent = '已复制';
    setTimeout(() => { $('evDetailCopy').textContent = '复制详情'; }, 1500);
  } catch {
    $('evDetailCopy').textContent = '复制失败';
  }
});

// 实时刷新：和调用日志同一套策略，切走或标签隐藏就停，请求不叠加。
const EV_AUTO = { on: false, sec: 10, timer: null, inflight: false };

function evAutoTick() {
  if (!EV_AUTO.on || EV_AUTO.inflight || STATE.page !== 'syslogs' || document.hidden) return;
  EV_AUTO.inflight = true;
  Promise.resolve(loadEvents()).finally(() => { EV_AUTO.inflight = false; });
}

function syncEvAuto() {
  clearInterval(EV_AUTO.timer);
  EV_AUTO.timer = null;
  if (EV_AUTO.on) EV_AUTO.timer = setInterval(evAutoTick, EV_AUTO.sec * 1000);
  $('evAutoDot').classList.toggle('on', EV_AUTO.on);
  localStorage.setItem('evAutoRefresh', JSON.stringify({ on: EV_AUTO.on, sec: EV_AUTO.sec }));
  if (EV_AUTO.on) evAutoTick();
}

$('evAuto').addEventListener('change', () => { EV_AUTO.on = $('evAuto').checked; syncEvAuto(); });
$('evAutoSec').addEventListener('change', () => {
  EV_AUTO.sec = Number($('evAutoSec').value) || 10;
  syncEvAuto();
});

// 切回浏览器标签时补一次，不让页面停在切走前的那一刻。
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && EV_AUTO.on) evAutoTick();
});

// 恢复上次的实时刷新设置（默认关闭）。
try {
  const saved = JSON.parse(localStorage.getItem('evAutoRefresh') || '{}');
  if (saved.on) {
    EV_AUTO.on = true;
    EV_AUTO.sec = Number(saved.sec) || 10;
    $('evAuto').checked = true;
    $('evAutoSec').value = String(EV_AUTO.sec);
  }
} catch { /* 存储被清或格式坏了就用默认值 */ }
syncEvAuto();

// ---------- 日志存储与清理 ----------
async function loadSysLogs() {
  // 页面加载器同时负责事件流和存储概览：事件流是主视图，概览失败不影响它。
  await Promise.all([ensureEvFacets().then(() => loadEvents()), loadSysStatus()]);
}

async function loadSysStatus() {
  try {
    const r = await api('/api/logs/manage');
    if (!r.ok) { $('logStats').innerHTML = `<div class="stat"><div class="k">状态</div><div class="v">${esc(r.message || '日志管理未启用')}</div></div>`; return; }
    const s = r.status || {};
    const c = s.config || {};

    $('logStats').innerHTML = [
      ['日志目录', c.logDir || '-'],
      ['文件日志', c.logFileEnable ? '开启' : '关闭'],
      ['日志文件', `${s.fileCount || 0} 个 · ${fmtSize(s.fileBytes)}`],
      ['运行事件', s.eventCount >= 0 ? `${s.eventCount || 0} 条` : '未知'],
      ['调用流水', s.usageCount >= 0 ? `${s.usageCount || 0} 条` : '未知'],
      ['签到记录', s.checkinCount >= 0 ? `${s.checkinCount || 0} 条` : '未知'],
      ['自动清理', c.logCleanEnable ? `开启 · 每 ${c.logCleanIntervalMin} 分钟` : '关闭'],
      ['保留策略', `文件 ${c.logFileRetainDays || 0} 天 / 库内 ${c.logDBRetainDays || 0} 天`],
    ].map(([k, v]) => `<div class="stat"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join('');

    const lc = s.lastCleanResult || {};
    if (s.lastClean) {
      const parts = [`上次清理 ${fmtTime(s.lastClean)}`];
      parts.push(`删除文件 ${lc.removedFiles || 0} 个（${fmtSize(lc.freedBytes)}）`);
      parts.push(`事件 ${lc.removedEvents || 0} 条`);
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

$('refreshSyslogsBtn').addEventListener('click', () => {
  $('syslogMsg').textContent = '';
  EV.facetsLoaded = false;
  loadSysLogs();
});
$('refreshSysStatusBtn').addEventListener('click', () => { $('syslogMsg').textContent = ''; loadSysStatus(); });

async function doCleanLogs() {
  if (!confirm('立即按当前策略清理过期日志与流水？')) return;
  $('syslogMsg').textContent = '清理中…';
  try {
    const r = await api('/api/logs/clean', { method: 'POST' });
    const x = r.result || {};
    $('syslogMsg').textContent =
      `清理完成：文件 ${x.removedFiles || 0} 个（${fmtSize(x.freedBytes)}），` +
      `运行事件 ${x.removedEvents || 0} 条，调用流水 ${x.removedUsage || 0} 条，` +
      `签到记录 ${x.removedCheckin || 0} 条` + (x.error ? `；错误：${x.error}` : '');
    await Promise.all([loadEvents(), loadSysStatus()]);
  } catch (e) {
    $('syslogMsg').textContent = e.message;
  }
}
$('cleanLogsBtn').addEventListener('click', doCleanLogs);
$('cleanLogsBtn2').addEventListener('click', doCleanLogs);

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
    ['下载来源', esc(s.source || '-')],
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
  if (s.source && s.source !== 'api.github.com') {
    hint += ' 直连 GitHub 失败，已自动改用镜像源下载。';
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
// 登录前后是整个壳层的两副形态：未登录收起侧栏与顶栏（body.logged-out），
// 登录后恢复。登录卡片本身永远在 DOM 里，靠 body 类切换显示，避免首屏闪一下。
function showLogin() {
  $('loginCard').hidden = false;
  $('app').hidden = true;
  $('logoutBtn').hidden = true;
  document.body.classList.add('logged-out');
  // 重新登录后旧会话的数据已经作废，清掉「已加载」标记让它重新拉。
  STATE.loaded.clear();
}

function showPanel() {
  STATE.loggedIn = true;
  $('loginCard').hidden = true;
  $('app').hidden = false;
  $('logoutBtn').hidden = false;
  document.body.classList.remove('logged-out');
  switchPage(location.hash.slice(1));
}

// ---------- 签到中心 ----------
// 页面分三块：顶部概况与定时计划、账号签到状态、签到记录。
// 账号与记录两次请求分开拉，刷新记录时不必重画账号行。
const CK = {
  data: null,
  accFilter: 'all', accQ: '',
  range: 'today', result: '', source: '', account: '', q: '',
  page: 1, pageSize: 50, total: 0, pages: 1, seq: 0,
  auto: false, autoSec: 10, autoTimer: null, inflight: false,
};

function ckRangeBounds() {
  const now = new Date();
  const start = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86400000;
  switch (CK.range) {
    case 'today': return { since: start, until: 0, label: '今天' };
    case 'yesterday': return { since: start - day, until: start, label: '昨天' };
    case '7d': return { since: start - 6 * day, until: 0, label: '近 7 天' };
    case '30d': return { since: start - 29 * day, until: 0, label: '近 30 天' };
    default: return { since: 0, until: 0, label: '全部时间' };
  }
}

const CK_SOURCE_LABEL = { auto: '定时', manual: '手动', onboard: '新号', '': '-' };

async function loadCheckinCenter() {
  try {
    const r = await api('/api/checkin/center');
    CK.data = r;
    renderCheckinOverview(r);
    renderCheckinAccounts();
    await loadCheckinRecords();
  } catch (e) {
    $('ckAccCount').textContent = e.message;
  }
}

function renderCheckinOverview(r) {
  const s = r.summary || {};
  const total = s.enabled || 0;
  const done = s.done || 0;
  const pct = total ? Math.round((done / total) * 100) : 0;
  $('ckDone').innerHTML = `${done}<small>/${total}</small>`;
  $('ckBar').style.width = pct + '%';
  $('ckDoneSub').textContent = total ? `完成 ${pct}%` : '还没有启用的账号';
  $('ckPending').textContent = s.pending || 0;
  $('ckPendingSub').textContent = s.disabled ? `另有 ${s.disabled} 个已停用` : '等待定时任务';
  $('ckFailed').textContent = s.failed || 0;
  $('ckTotal').textContent = r.totalRecords >= 0 ? fmtNum(r.totalRecords) : '未知';
  $('ckTotalSub').textContent = r.totalCredit >= 0 ? `累计 ${fmtCredits(r.totalCredit)} 积分` : '全部签到流水';

  const sc = r.schedule || {};
  const on = !!sc.enabled;
  $('ckSchedDot').classList.toggle('on', on);
  $('ckSchedState').textContent = on ? '已开启' : '已关闭';
  const hh = String(sc.hour ?? 0).padStart(2, '0');
  const mm = String(sc.minute ?? 0).padStart(2, '0');
  $('ckSchedDesc').textContent = on
    ? `每天 ${hh}:${mm} 起执行，每个号叠加 0~${sc.jitterMin ?? 0} 分钟确定性抖动；签到口径与号池页一致。`
    : '自动签到已关闭，可在这里手动补签，或去设置页开启定时任务。';
  $('ckNextRun').textContent = on && sc.nextRun ? fmtTime(sc.nextRun) : '-';
  $('ckNextSub').textContent = on
    ? (sc.running ? '正在执行' : (sc.lastTick ? `上次 ${fmtClock(sc.lastTick)}` : '尚未执行'))
    : '未启用';

  // 账号筛选下拉随账号列表刷新
  const sel = $('ckAccount');
  if (sel) {
    const cur = sel.value;
    sel.innerHTML = '<option value="">全部账号</option>' + (r.accounts || []).map((a) =>
      `<option value="${esc(a.id)}">${esc(a.name || shortUID(a.uid))}</option>`).join('');
    sel.value = cur;
  }
}

function ckAccountState(a) {
  if (a.noCheckin) return { cls: 'dim', text: '无签到' };
  if (!a.enabled) return { cls: 'dim', text: '已停用' };
  if (a.signedToday) return { cls: 'ok', text: '今日已签' };
  if (a.hasToday && !a.todayOk) return { cls: 'bad', text: '今日失败' };
  return { cls: 'warn', text: '待签到' };
}

function ckFilteredAccounts() {
  const list = (CK.data && CK.data.accounts) || [];
  const q = CK.accQ.trim().toLowerCase();
  return list.filter((a) => {
    if (CK.accFilter === 'done' && !a.signedToday) return false;
    if (CK.accFilter === 'pending' && (a.signedToday || (a.hasToday && !a.todayOk))) return false;
    if (CK.accFilter === 'failed' && !(a.hasToday && !a.todayOk)) return false;
    if (!q) return true;
    return [a.name, a.uid, a.note].some((v) => String(v || '').toLowerCase().includes(q));
  });
}

function renderCheckinAccounts() {
  const all = (CK.data && CK.data.accounts) || [];
  $('ckAccAll').textContent = all.length;
  $('ckAccDone').textContent = all.filter((a) => a.signedToday).length;
  $('ckAccPending').textContent = all.filter((a) => a.enabled && !a.signedToday && !(a.hasToday && !a.todayOk)).length;
  $('ckAccFailed').textContent = all.filter((a) => a.hasToday && !a.todayOk).length;

  const list = ckFilteredAccounts();
  $('ckAccCount').textContent = `${list.length} 个`;
  $('ckAccEmpty').hidden = list.length > 0;
  const rows = list.map((a) => {
    const st = ckAccountState(a);
    const today = a.hasToday
      ? (a.todayOk ? (a.todayAlready ? '今日已领' : '领取成功') : '失败')
      : '-';
    const msg = a.hasToday ? a.todayMessage : '';
    return `<tr data-id="${esc(a.id)}">
      <td class="lc-account">
        <div class="acc-name">${esc(a.name || shortUID(a.uid))}</div>
        <div class="acc-sub">${esc(shortUID(a.uid))}${a.note ? ' · ' + esc(a.note) : ''}</div>
      </td>
      <td><span class="ck-state ${st.cls}">${esc(st.text)}</span></td>
      <td title="${esc(msg || '')}">${esc(today)}</td>
      <td class="lc-time">${a.todayAt ? esc(fmtTime(a.todayAt)) : '-'}</td>
      <td>${esc(CK_SOURCE_LABEL[a.todaySource] || a.todaySource || '-')}</td>
      <td class="lc-detail">
        ${a.enabled && !a.noCheckin ? `<button class="ghost tiny" data-act="checkin">补签</button>` : ''}
      </td>
    </tr>`;
  }).join('');
  $('ckAccTable').querySelector('tbody').innerHTML = rows;
}

function ckQuery() {
  const b = ckRangeBounds();
  const p = new URLSearchParams({
    page: String(CK.page), pageSize: String(CK.pageSize),
    since: String(b.since), until: String(b.until),
  });
  if (CK.result) p.set('result', CK.result);
  if (CK.source) p.set('source', CK.source);
  if (CK.account) p.set('account', CK.account);
  if (CK.q) p.set('q', CK.q);
  return p.toString();
}

async function loadCheckinRecords() {
  const seq = ++CK.seq;
  try {
    const r = await api('/api/checkin/history?' + ckQuery());
    if (seq !== CK.seq) return; // 慢请求后到就丢弃，避免旧结果覆盖新筛选
    CK.total = r.summary ? r.summary.total : 0;
    CK.pages = r.pages || 1;
    renderCheckinRecords(r);
  } catch (e) {
    $('ckRecMsg').textContent = e.message;
  }
}

function ckRecordResult(r) {
  if (!r.ok) return { cls: 'bad', text: '失败' };
  if (r.already) return { cls: 'warn', text: '今日已领' };
  return { cls: 'ok', text: r.credit > 0 ? `+${r.credit}` : '成功' };
}

function renderCheckinRecords(r) {
  const rows = (r.records || []).map((x) => {
    const res = ckRecordResult(x);
    return `<tr>
      <td class="lc-time">${esc(fmtClock(x.at))}</td>
      <td class="lc-account"><div class="acc-name">${esc(x.accountName || shortUID(x.accountUid))}</div></td>
      <td>${esc(x.day || '-')}</td>
      <td><span class="ck-state ${res.cls}">${esc(res.text)}</span></td>
      <td>${esc(CK_SOURCE_LABEL[x.source] || x.source || '-')}</td>
      <td class="lc-error">${esc(x.message || '')}</td>
    </tr>`;
  }).join('');
  $('ckRecTable').querySelector('tbody').innerHTML = rows;
  $('ckRecEmpty').hidden = (r.records || []).length > 0;
  const b = ckRangeBounds();
  $('ckRecRangeInfo').textContent = `${b.label} · 共 ${CK.total} 条`;
  $('ckRecPageInfo').textContent = `第 ${CK.page} / ${Math.max(1, CK.pages)} 页`;
  $('ckRecCount').textContent = `共 ${CK.total} 条`;
  $('ckPrev').disabled = CK.page <= 1;
  $('ckNext').disabled = CK.page >= CK.pages;
  $('ckRecMsg').textContent = '';
}

// 自动刷新只在本页且标签可见时跑，切走就停，避免后台白烧请求。
function ckAutoTick() {
  if (!CK.auto || CK.inflight || STATE.page !== 'checkin' || document.hidden) return;
  CK.inflight = true;
  Promise.resolve(loadCheckinCenter()).finally(() => { CK.inflight = false; });
}

function syncCkAuto() {
  clearInterval(CK.autoTimer);
  CK.autoTimer = null;
  if (CK.auto) CK.autoTimer = setInterval(ckAutoTick, CK.autoSec * 1000);
  $('ckAutoDot').classList.toggle('on', CK.auto);
  localStorage.setItem('checkinAutoRefresh', JSON.stringify({ on: CK.auto, sec: CK.autoSec }));
  if (CK.auto) ckAutoTick();
}

// 分段按钮（账号筛选 / 时间范围）共用：点一下切 class 并写回 CK。
function bindSeg(id, key, after) {
  const box = $(id);
  if (!box) return;
  box.addEventListener('click', (e) => {
    const b = e.target.closest('button');
    if (!b) return;
    box.querySelectorAll('button').forEach((x) => x.classList.toggle('on', x === b));
    CK[key] = b.dataset.v;
    after();
  });
}
bindSeg('ckAccFilter', 'accFilter', renderCheckinAccounts);
bindSeg('ckRange', 'range', () => { CK.page = 1; loadCheckinRecords(); });

$('ckAccSearch').addEventListener('input', (e) => { CK.accQ = e.target.value; renderCheckinAccounts(); });
$('ckResult').addEventListener('change', (e) => { CK.result = e.target.value; CK.page = 1; loadCheckinRecords(); });
$('ckSource').addEventListener('change', (e) => { CK.source = e.target.value; CK.page = 1; loadCheckinRecords(); });
$('ckAccount').addEventListener('change', (e) => { CK.account = e.target.value; CK.page = 1; loadCheckinRecords(); });
$('ckRecSearch').addEventListener('input', (e) => {
  clearTimeout(CK.qTimer);
  const v = e.target.value;
  CK.qTimer = setTimeout(() => { CK.q = v; CK.page = 1; loadCheckinRecords(); }, 300);
});
$('ckRecReset').addEventListener('click', () => {
  CK.result = ''; CK.source = ''; CK.account = ''; CK.q = ''; CK.page = 1;
  $('ckResult').value = ''; $('ckSource').value = ''; $('ckAccount').value = ''; $('ckRecSearch').value = '';
  loadCheckinRecords();
});
$('ckPageSize').addEventListener('change', (e) => {
  CK.pageSize = Number(e.target.value) || 50;
  CK.page = 1;
  loadCheckinRecords();
});
$('ckPrev').addEventListener('click', () => {
  if (CK.page <= 1) return;
  CK.page--;
  loadCheckinRecords();
});
$('ckNext').addEventListener('click', () => {
  if (CK.page >= CK.pages) return;
  CK.page++;
  loadCheckinRecords();
});
$('ckAuto').addEventListener('change', () => { CK.auto = $('ckAuto').checked; syncCkAuto(); });
$('ckAutoSec').addEventListener('change', () => {
  CK.autoSec = Number($('ckAutoSec').value) || 10;
  syncCkAuto();
});
document.addEventListener('visibilitychange', () => { if (!document.hidden && CK.auto) ckAutoTick(); });

try {
  const saved = JSON.parse(localStorage.getItem('checkinAutoRefresh') || '{}');
  if (saved.on) {
    CK.auto = true;
    CK.autoSec = Number(saved.sec) || 10;
    $('ckAuto').checked = true;
    $('ckAutoSec').value = String(CK.autoSec);
  }
} catch { /* 存储坏了就用默认值 */ }
syncCkAuto();

$('ckRunAllBtn').addEventListener('click', async () => {
  $('ckRunAllBtn').disabled = true;
  try {
    await api('/api/checkin/all', { method: 'POST', body: '{}' });
    toast('已开始签到全部账号');
    STATE.loaded.delete('checkin');
    await loadCheckinCenter();
  } catch (e) {
    toast(e.message, true);
  } finally {
    $('ckRunAllBtn').disabled = false;
  }
});
$('ckSchedBtn').addEventListener('click', () => switchPage('settings'));
$('ckRefreshBtn').addEventListener('click', () => loadCheckinCenter());
$('ckRecRefreshBtn').addEventListener('click', () => loadCheckinRecords());

// 行内「补签」用事件委托：重画账号行后不需要重新绑定
$('ckAccTable').addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-act="checkin"]');
  if (!btn) return;
  const tr = btn.closest('tr');
  const id = tr && tr.dataset.id;
  if (!id) return;
  btn.disabled = true;
  try {
    await api(`/api/accounts/${encodeURIComponent(id)}/checkin`, { method: 'POST', body: '{}' });
    await loadCheckinCenter();
  } catch (err) {
    toast(err.message, true);
    btn.disabled = false;
  }
});


// 已登录（cookie 有效）则直接进面板
(async () => {
  try {
    await api('/api/status');
    showPanel();
  } catch {
    showLogin();
  }
})();

// ---------- 报表 ----------
// 读后端 /api/report：日聚合 + 模型/协议分解 + 账号明细 + 耗时分布。
// 图表统一用内联 SVG 画（项目不引第三方图表库），风格与面板其它页保持一致。
const REPORT = {
  range: '7', start: '', end: '',
  dim: 'models', metric: 'tokens', entity: '__all__', accSort: 'tokens',
  data: null, loading: false,
};

// 饼图/堆叠柱/图例共用一套调色板，同一模型在所有图里颜色一致。
// 取值一律来自 CSS 令牌（--chart-1…12），主题切换时图表自动跟着换色；
// 令牌取不到时再退回一组写死的蓝紫色（老浏览器不支持 oklch 的情况）。
const RP_COLOR_VARS = Array.from({ length: 12 }, (_, i) => `var(--chart-${i + 1})`);
const RP_COLORS_FALLBACK = [
  '#4ea1ff', '#2fc7c7', '#9b8cff', '#e579c8', '#3fb96a',
  '#d8a13a', '#e0745a', '#56c2c2', '#b18cf0', '#6f9ae8',
  '#7ec46a', '#d477a8',
];
function rpColors() {
  const cs = getComputedStyle(document.documentElement);
  const probe = cs.getPropertyValue('--chart-1').trim();
  return probe ? RP_COLOR_VARS : RP_COLORS_FALLBACK;
}
const RP_COLORS = rpColors();
const RP_DIM_TITLE = { models: '模型', apis: '协议', accounts: '账号' };

function rpFmtInt(n) {
  const v = Number(n) || 0;
  if (v >= 1e8) return (v / 1e8).toFixed(2).replace(/\.?0+$/, '') + ' 亿';
  if (v >= 1e4) return (v / 1e4).toFixed(1).replace(/\.0$/, '') + ' 万';
  return String(Math.round(v));
}
function rpFullInt(n) { return (Number(n) || 0).toLocaleString('zh-CN'); }
function rpFmtMs(ms) {
  const v = Number(ms) || 0;
  return v >= 1000 ? (v / 1000).toFixed(2) + 's' : Math.round(v) + 'ms';
}
function rpPct(v, digits = 1) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return '—';
  return Number(v).toFixed(digits) + '%';
}
function rpDayLabel(key) {
  const [y, m, d] = String(key || '').split('-').map(Number);
  if (!y || !m || !d) return String(key || '');
  const wd = ['日', '一', '二', '三', '四', '五', '六'][new Date(y, m - 1, d).getDay()];
  return `${m}月${d}日 周${wd}`;
}
function rpShortDay(key) {
  const [, m, d] = String(key || '').split('-').map(Number);
  return m && d ? `${m}/${d}` : String(key || '');
}

// 好看一点的纵轴上限：把峰值四等分后取整到 1/1.5/2/2.5/3/4/5/6/8/10 这类步长。
const RP_STEPS = [1, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10];
function rpAxisMax(peak) {
  if (!(peak > 0)) return 1;
  const raw = peak / 4;
  const pow = 10 ** Math.floor(Math.log10(raw));
  const norm = raw / pow;
  const step = RP_STEPS.find((x) => x >= norm - 1e-9) ?? 10;
  return step * pow * 4;
}

async function loadReport() {
  if (REPORT.loading) return;
  REPORT.loading = true;
  $('rpMsg').textContent = '';
  try {
    const q = new URLSearchParams({ range: REPORT.range });
    if (REPORT.range === 'custom') {
      if (REPORT.start) q.set('start', REPORT.start);
      if (REPORT.end) q.set('end', REPORT.end);
    }
    const r = await api('/api/report?' + q.toString());
    REPORT.data = (r && r.summary) || null;
    if (REPORT.data) {
      REPORT.start = REPORT.data.startDate || '';
      REPORT.end = REPORT.data.endDate || '';
    }
    renderReport();
  } catch (e) {
    $('rpMsg').textContent = e.message;
    if (e.status === 401) showLogin();
  } finally {
    REPORT.loading = false;
  }
}

function renderReport() {
  const s = REPORT.data;
  const clear = (id, text = '暂无数据') => { const el = $(id); if (el) el.innerHTML = `<div class="empty">${esc(text)}</div>`; };
  if (!s) {
    ['rpKpis', 'rpLatency', 'rpModels', 'rpAccounts', 'rpApis',
      'rpModelsDonut', 'rpApisDonut', 'rpTrend', 'rpHeat', 'rpTrendLegend'].forEach((id) => clear(id, '暂无数据'));
    $('rpAccTable').innerHTML = '';
    $('rpApiTable').innerHTML = '';
    return;
  }
  const o = s.overview || {};
  const lat = s.latency || {};
  $('rpRangeLabel').textContent = `${s.label || ''} · ${s.startDate || ''} ~ ${s.endDate || ''}`;
  $('rpOverviewSub').textContent = `${s.startDate || ''} ~ ${s.endDate || ''} · 共 ${(s.trend || []).length} 天`;
  $('rpTrendRange').textContent = s.label || '';

  // ── 概览 KPI ──
  // 成功率在 0 请求时没有意义（0/0），显示破折号而不是 0%。
  // 8 个读数在 4 列网格里正好两行，第三行再放「延迟五连」。
  const successRate = o.requests ? rpPct(o.successRate) : '—';
  const top = s.models && s.models.length ? s.models[0] : null;
  const kpis = [
    ['总 Token', rpFmtInt(o.tokens), `输入 ${rpFmtInt(o.promptTokens)} · 输出 ${rpFmtInt(o.completionTokens)}`, 'accent'],
    ['成功请求数', rpFmtInt(o.successful), `成功率 ${successRate}`, 'ok'],
    ['总请求数', rpFmtInt(o.requests), s.label || '', ''],
    ['Top 模型', top ? top.model : '—',
      top ? `${rpFmtInt(top.tokens)} token · 占 ${rpPct(top.share)}` : '所选范围内还没有模型用量', ''],
    ['平均耗时', rpFmtMs(lat.avgMs), 'P95 ' + rpFmtMs(lat.p95Ms), ''],
    ['输出速度', (lat.tokenPerSec || 0) + ' tok/s', '峰值 ' + rpFmtMs(lat.maxMs), ''],
    ['活跃天数', String(o.activeDays || 0), `区间共 ${(s.trend || []).length} 天`, ''],
    ['当前连续天数', String(o.streak || 0), '含今天在内往前数', ''],
  ];
  $('rpKpis').innerHTML = kpis.map(([k, v, sub, cls]) =>
    `<div class="rp-kpi ${cls}"><div class="k">${esc(k)}</div>` +
    `<div class="v" title="${esc(v)}">${esc(v)}</div>` +
    `<div class="sub">${esc(sub)}</div></div>`).join('');

  // ── 耗时与吞吐 ──
  $('rpLatency').innerHTML = [
    ['平均耗时', rpFmtMs(lat.avgMs), ''],
    ['P50', rpFmtMs(lat.p50Ms), ''],
    ['P95', rpFmtMs(lat.p95Ms), 'warn'],
    ['峰值耗时', rpFmtMs(lat.maxMs), ''],
    ['平均输出速度', (lat.tokenPerSec || 0) + ' tok/s', 'ok'],
  ].map(([k, v, cls]) =>
    `<div class="rp-kpi ${cls}"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div></div>`).join('');

  // 报表页的 8 个 KPI 在 4 列网格里是两整行，容器类名跟着改一下。
  const kpiBox = $('rpKpis');
  if (kpiBox) kpiBox.classList.add('rp-kpis-8');

  // ── Top 排行（账号 / 协议）──
  renderRpRank('rpAccounts', 'rpAccountsTotal', (s.accounts || []).map((a) => ({
    name: a.name || shortUID(a.uid || a.id) || a.id || '未记录账号',
    value: a.tokens, requests: a.requests, share: a.share,
  })));
  renderRpRank('rpApis', 'rpApisTotal', (s.apis || []).map((a) => ({
    name: a.api || '未知协议', value: a.tokens, requests: a.requests, share: a.share,
  })));

  // ── 环形图 ──
  renderRpDonut('rpModelsDonut', (s.models || []).map((m) => ({
    name: m.model || '未知模型', tokens: m.tokens, requests: m.requests,
  })));
  renderRpDonut('rpApisDonut', (s.apis || []).map((a) => ({
    name: a.api || '未知协议', tokens: a.tokens, requests: a.requests,
  })));

  // ── 趋势 ──
  renderRpTrend();

  // ── 热力图 ──
  renderRpHeat(s.heatmap || []);

  // ── 明细表 ──
  renderRpAccTable(s.accounts || []);
  renderRpApiTable(s.apis || []);
}

// 每个维度下建模名称 → 颜色，保证环形图、排行、堆叠柱同一模型同色。
function rpColorMap(names) {
  const map = new Map();
  for (const name of names) {
    if (!name || map.has(name)) continue;
    map.set(name, RP_COLORS[map.size % RP_COLORS.length]);
  }
  return map;
}

function renderRpRank(id, totalId, rows) {
  const el = $(id);
  const totalEl = $(totalId);
  const list = rows.filter((r) => Number(r.value) > 0)
    .sort((a, b) => Number(b.value) - Number(a.value));
  if (totalEl) {
    totalEl.textContent = `按 Token · 合计 ${rpFmtInt(list.reduce((s, r) => s + (Number(r.value) || 0), 0))}`;
  }
  if (!list.length) {
    el.innerHTML = '<div class="empty">所选范围内还没有用量</div>';
    return;
  }
  const total = list.reduce((s, r) => s + (Number(r.value) || 0), 0) || 1;
  el.innerHTML = list.slice(0, 30).map((r) => {
    const pct = (Number(r.value) / total) * 100;
    return `<div class="rp-rank-row" title="${esc(r.name)} · ${rpFullInt(r.value)} token · ${rpPct(pct)}">` +
      `<span class="name">${esc(r.name)}</span>` +
      `<span class="meter"><i style="width:${Math.max(pct, 1.5).toFixed(1)}%"></i></span>` +
      `<span class="num">${esc(rpFmtInt(r.value))}</span>` +
      `<span class="pct">${esc(rpPct(pct))}</span></div>`;
  }).join('');
}

// renderRpDonut 用纯 SVG 画环形图：中心显示总量，右侧图例逐项列出名字、Token、占比。
function renderRpDonut(id, rows) {
  const el = $(id);
  const list = rows.map((r) => ({ ...r, tokens: Number(r.tokens) || 0 }))
    .filter((r) => r.tokens > 0)
    .sort((a, b) => b.tokens - a.tokens);
  if (!list.length) {
    el.innerHTML = '<div class="empty">所选范围内还没有用量</div>';
    return;
  }
  const colors = rpColorMap(list.map((r) => r.name));
  const total = list.reduce((s, r) => s + r.tokens, 0);
  const R = 52, r = 33, cx = 66, cy = 66, C = 2 * Math.PI * r;
  let offset = 0;
  let arcs = '';
  list.forEach((item) => {
    const frac = item.tokens / total;
    const len = frac * C;
    const color = colors.get(item.name) || RP_COLORS[0];
    arcs += `<circle cx="${cx}" cy="${cy}" r="${r}" fill="none" stroke="${color}" stroke-width="${R - r}"` +
      ` stroke-dasharray="${len.toFixed(2)} ${(C - len).toFixed(2)}"` +
      ` stroke-dashoffset="${(-offset).toFixed(2)}" transform="rotate(-90 ${cx} ${cy})">` +
      `<title>${esc(item.name)} · ${rpFullInt(item.tokens)} token · ${rpPct(frac * 100)}</title></circle>`;
    offset += len;
  });
  const legend = list.map((item) => {
    const color = colors.get(item.name);
    const pct = (item.tokens / total) * 100;
    return `<div class="rp-donut-legend-row" title="${esc(item.name)}">` +
      `<span class="dot" style="background:${color}"></span>` +
      `<span class="lname">${esc(item.name)}</span>` +
      `<span class="ltok">${esc(rpFmtInt(item.tokens))}</span>` +
      `<span class="lpct">${esc(rpPct(pct))}</span></div>`;
  }).join('');
  el.innerHTML = `<div class="rp-donut-layout">` +
    `<div class="rp-donut-wrap"><svg viewBox="0 0 ${cx * 2} ${cy * 2}" role="img">${arcs}</svg>` +
    `<div class="rp-donut-center"><div class="v">${esc(rpFmtInt(total))}</div><div class="k">总 Token</div></div></div>` +
    `<div class="rp-donut-legend">${legend}</div></div>`;
}

// 趋势图：按维度取逐日分解。维度为「全部实体」时叠成堆叠柱（每段一个模型/协议/账号），
// 选定单一实体时只画一根纯柱；指标可切 Token / 请求数。
function rpTrendSeries() {
  const s = REPORT.data;
  if (!s) return { rows: [], names: [] };
  const dim = REPORT.dim;
  const dayRows = s.trend || [];
  const slices = dim === 'models' ? (s.trendModels || [])
    : dim === 'apis' ? (s.trendApis || [])
      : (s.trendAccounts || []);
  const metric = REPORT.metric;

  // 下拉候选必须取自「完整 slices」：若先按当前选中过滤，选完某一项后
  // 下拉就只剩它自己，再也切不回别的项。
  const totals = new Map();
  for (const item of slices) {
    if (!item.key) continue;
    const v = metric === 'requests' ? Number(item.requests) || 0 : Number(item.tokens) || 0;
    totals.set(item.key, (totals.get(item.key) || 0) + v);
  }
  // 颜色按「区间总用量」排序后分配，与环形图/排行的配色保持一致——
  // 否则同一模型在环形图里是蓝色、在趋势图里却是绿色，读者要多一次对照。
  const names = [...totals.entries()]
    .sort((a, b) => b[1] - a[1] || String(a[0]).localeCompare(String(b[0])))
    .map(([name]) => name);

  const byDay = new Map();
  for (const item of slices) {
    if (REPORT.entity !== '__all__' && item.key !== REPORT.entity) continue;
    if (!byDay.has(item.day)) byDay.set(item.day, []);
    const v = metric === 'requests' ? Number(item.requests) || 0 : Number(item.tokens) || 0;
    if (v > 0) byDay.get(item.day).push({ name: item.key || '未知', tokens: v });
  }
  const rows = dayRows.map((d) => {
    const segs = (byDay.get(d.day) || []).sort((a, b) => b.tokens - a.tokens);
    const segTotal = segs.reduce((sum, x) => sum + x.tokens, 0);
    // 「全部」直接用当日大盘合计，避免分解表缺项时柱子比真实总量矮；
    // 选定单一实体时按该实体自己的逐日值画。
    const total = REPORT.entity === '__all__'
      ? (metric === 'requests' ? Number(d.requests) || 0 : Number(d.tokens) || 0)
      : segTotal;
    return { day: d.day, total, segments: segs, requests: Number(d.requests) || 0 };
  });
  return { rows, names };
}

function renderRpTrend() {
  const el = $('rpTrend');
  if (!el) return;
  const { rows, names } = rpTrendSeries();
  // 颜色按「区间总用量」排序后分配，与环形图/排行保持一致；下拉候选也按同一顺序。
  const colors = rpColorMap(names);
  $('rpTrendTitle').textContent = REPORT.metric === 'tokens' ? `按天 Token 趋势 · ${RP_DIM_TITLE[REPORT.dim] || ''}`
    : `按天请求趋势 · ${RP_DIM_TITLE[REPORT.dim] || ''}`;
  renderRpEntityOptions(names);

  const maxTotal = Math.max(1, ...rows.map((r) => r.total));
  if (!rows.length || !rows.some((r) => r.total > 0)) {
    el.innerHTML = '<div class="empty">所选范围内暂无请求</div>';
    $('rpTrendLegend').innerHTML = '';
    return;
  }
  const W = Math.max(320, el.clientWidth || 720);
  const H = 230;
  const padL = 52, padR = 12, padT = 14, padB = 28;
  const plotW = W - padL - padR;
  const plotH = H - padT - padB;
  const n = rows.length;
  const gap = n > 1 ? Math.min(6, (plotW / n) * 0.25) : 0;
  const bw = Math.max(3, (plotW - gap * (n - 1)) / n);
  const axisMax = rpAxisMax(maxTotal);

  const fmtAxis = (v) => rpFmtInt(v);
  let svg = '';
  for (let i = 0; i <= 4; i++) {
    const y = padT + plotH - (plotH * i) / 4;
    const val = Math.round((axisMax * i) / 4);
    svg += `<line class="gridline" x1="${padL}" y1="${y}" x2="${W - padR}" y2="${y}"/>`;
    svg += `<text class="axis-text" x="${padL - 6}" y="${y + 3}" text-anchor="end">${esc(fmtAxis(val))}</text>`;
  }
  const labelEvery = Math.max(1, Math.ceil(n / 12));
  rows.forEach((row, i) => {
    const x = padL + i * (bw + gap);
    let base = padT + plotH;
    const segs = row.segments.length ? row.segments : [{ name: '总量', tokens: row.total }];
    const usedNames = row.segments.length ? colors : new Map([['总量', RP_COLORS[0]]]);
    for (const seg of segs) {
      if (seg.tokens <= 0) continue;
      const h = (seg.tokens / axisMax) * plotH;
      base -= h;
      const color = usedNames.get(seg.name) || RP_COLORS[0];
      const tip = `${rpDayLabel(row.day)} · ${seg.name}: ${rpFullInt(seg.tokens)}` +
        (REPORT.metric === 'tokens' ? ' token' : ' 次') +
        ` · 当日合计 ${rpFullInt(row.total)} · ${row.requests} 次请求`;
      svg += `<rect class="seg" x="${x.toFixed(1)}" y="${base.toFixed(1)}" width="${bw.toFixed(1)}"` +
        ` height="${h.toFixed(1)}" fill="${color}"><title>${esc(tip)}</title></rect>`;
    }
    if (i % labelEvery === 0 || i === n - 1) {
      svg += `<text class="axis-text" x="${(x + bw / 2).toFixed(1)}" y="${H - 8}"` +
        ` text-anchor="middle">${esc(rpShortDay(row.day))}</text>`;
    }
  });
  el.innerHTML = `<svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img">${svg}</svg>`;

  // 图例：只在「全部实体」且按模型/协议时展示各段颜色
  const legendEl = $('rpTrendLegend');
  if (REPORT.entity === '__all__' && names.length) {
    legendEl.innerHTML = names.slice(0, 16).map((name) =>
      `<span class="rp-legend-item" title="${esc(name)}">` +
      `<i style="background:${colors.get(name)}"></i>${esc(name)}</span>`).join('');
  } else {
    legendEl.innerHTML = '';
  }
}

// renderRpEntityOptions 的候选来自完整 slices（不受当前选中过滤影响），
// 这样选中某个实体后下拉不会只剩它自己、无法切回别的项。
function renderRpEntityOptions(names) {
  const sel = $('rpEntity');
  if (!sel) return;
  const current = REPORT.entity;
  const opts = ['<option value="__all__">全部</option>']
    .concat(names.filter(Boolean).map((name) =>
      `<option value="${esc(name)}">${esc(name)}</option>`));
  sel.innerHTML = opts.join('');
  sel.value = names.includes(current) ? current : '__all__';
  REPORT.entity = sel.value;
}

// 热力图：固定一年。颜色按当天 Token 用量分档；档位阈值随本窗口峰值自动定，
// 图例里直接写出每档对应的 Token 区间，比「浅/深」更有信息量。
function rpHeatThresholds(peak) {
  const top = Number(peak) || 0;
  if (top <= 0) return [0, 1, 2, 3, Infinity];
  const step = Math.max(1, Math.floor(top / 4));
  return [0, step, step * 2, step * 3, Infinity];
}
function rpHeatLevelText(level, thresholds) {
  if (level === 0) return '0';
  const lower = thresholds[level - 1];
  const upper = thresholds[level];
  if (!Number.isFinite(upper)) return `>${rpFmtInt(lower)}`;
  return lower === 0 ? `≤${rpFmtInt(upper)}` : `${rpFmtInt(lower)}–${rpFmtInt(upper)}`;
}

function renderRpHeat(cells) {
  const el = $('rpHeat');
  const byDay = {};
  (cells || []).forEach((c) => { byDay[c.day] = c; });
  const today = new Date();
  const end = new Date(today.getFullYear(), today.getMonth(), today.getDate());
  const start = new Date(end);
  start.setDate(start.getDate() - 364);
  // 从 start 所在周的周一算起，保证列对齐星期
  const offset = (start.getDay() + 6) % 7; // 周一 = 0
  start.setDate(start.getDate() - offset);
  end.setDate(end.getDate() + (6 - ((end.getDay() + 6) % 7)));

  const pad = (k) => String(k).padStart(2, '0');
  const keyOf = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const days = [];
  for (const d = new Date(start); d <= end; d.setDate(d.getDate() + 1)) days.push(keyOf(d));

  const peak = days.reduce((mx, key) => Math.max(mx, Number((byDay[key] || {}).tokens) || 0), 0);
  const thresholds = rpHeatThresholds(peak);
  const levelOf = (tokens) => thresholds.findIndex((limit) => Number(tokens) <= limit);

  const grid = days.map((key) => {
    const cell = byDay[key] || {};
    const tokens = Number(cell.tokens) || 0;
    const requests = Number(cell.requests) || 0;
    const tip = requests
      ? `${rpDayLabel(key)} · ${rpFullInt(tokens)} token · ${requests} 次请求`
      : `${rpDayLabel(key)} · 无请求`;
    return `<div class="cell l${levelOf(tokens)}" title="${esc(tip)}"></div>`;
  }).join('');

  const legend = [0, 1, 2, 3, 4].map((lv) =>
    `<span class="rp-legend-item"><i class="cell l${lv}"></i>${esc(rpHeatLevelText(lv, thresholds))}</span>`).join('');
  el.innerHTML = `<div class="heat-grid">${grid}</div>` +
    `<div class="heat-legend"><span class="hint">Token</span>${legend}` +
    `<span class="spacer"></span><span class="hint">方格为本地自然日，无请求显示为空槽</span></div>`;
}

function renderRpAccTable(rows) {
  const list = [...rows].sort((a, b) => {
    const key = REPORT.accSort === 'requests' ? 'requests' : 'tokens';
    return (Number(b[key]) || 0) - (Number(a[key]) || 0) || String(a.name || a.id).localeCompare(String(b.name || b.id));
  });
  const tbody = $('rpAccTable');
  if (!list.length) {
    tbody.innerHTML = '<tr><td colspan="7"><div class="empty">所选范围内暂无请求</div></td></tr>';
    return;
  }
  tbody.innerHTML = list.map((a) => {
    const name = a.name || shortUID(a.uid || a.id) || '未记录账号';
    const uid = a.uid || a.id || '';
    // 名称本身已够区分时不再重复一行 uid；只有回落成 uid 显示时才补账号 id。
    const sub = a.name && uid ? shortUID(uid) : '';
    return `<tr>` +
      `<td><span class="rp-cell-name" title="${esc(name)}">${esc(name)}</span>` +
      (sub ? `<span class="rp-cell-sub">${esc(sub)}</span>` : '') + `</td>` +
      `<td class="num">${esc(rpFullInt(a.requests))}</td>` +
      `<td class="num"><span class="ok">${esc(rpFullInt(a.success))}</span> / <span class="bad">${esc(rpFullInt(a.failures))}</span></td>` +
      `<td class="num">${esc(rpPct(a.successRate))}</td>` +
      `<td class="num" title="${esc(rpFullInt(a.tokens))}">${esc(rpFmtInt(a.tokens))}</td>` +
      `<td class="num">${esc(rpPct(a.share))}</td>` +
      `<td class="num">${esc(rpFmtInt(a.promptTokens))} / ${esc(rpFmtInt(a.completionTokens))}</td>` +
      `</tr>`;
  }).join('');
}

function renderRpApiTable(rows) {
  const list = [...rows].sort((a, b) => (Number(b.tokens) || 0) - (Number(a.tokens) || 0));
  const tbody = $('rpApiTable');
  if (!list.length) {
    tbody.innerHTML = '<tr><td colspan="7"><div class="empty">所选范围内暂无请求</div></td></tr>';
    return;
  }
  tbody.innerHTML = list.map((a) => `<tr>` +
    `<td><span class="rp-cell-name">${esc(a.api || '未知协议')}</span></td>` +
    `<td class="num">${esc(rpFullInt(a.requests))}</td>` +
    `<td class="num"><span class="ok">${esc(rpFullInt(a.success))}</span> / <span class="bad">${esc(rpFullInt(a.failures))}</span></td>` +
    `<td class="num">${esc(rpPct(a.successRate))}</td>` +
    `<td class="num" title="${esc(rpFullInt(a.tokens))}">${esc(rpFmtInt(a.tokens))}</td>` +
    `<td class="num">${esc(rpPct(a.share))}</td>` +
    `<td class="num">${esc(rpFmtInt(a.promptTokens))} / ${esc(rpFmtInt(a.completionTokens))}</td>` +
    `</tr>`).join('');
}

// ---------- 报表交互 ----------
function rpSetSeg(id, value, attr = 'v') {
  document.querySelectorAll(`#${id} button`).forEach((b) => b.classList.toggle('on', b.dataset[attr] === value));
}

document.querySelectorAll('#rpRange button').forEach((btn) => {
  btn.addEventListener('click', () => {
    REPORT.range = btn.dataset.v;
    rpSetSeg('rpRange', REPORT.range);
    const custom = REPORT.range === 'custom';
    const box = $('rpCustomRange');
    if (box) box.hidden = !custom;
    if (custom) {
      const d = new Date();
      const iso = (x) => `${x.getFullYear()}-${String(x.getMonth() + 1).padStart(2, '0')}-${String(x.getDate()).padStart(2, '0')}`;
      if (!REPORT.start) REPORT.start = iso(new Date(d.getTime() - 6 * 86400000));
      if (!REPORT.end) REPORT.end = iso(d);
      if ($('rpStartDate')) $('rpStartDate').value = REPORT.start;
      if ($('rpEndDate')) $('rpEndDate').value = REPORT.end;
    }
    loadReport();
  });
});
[['rpStartDate', 'start'], ['rpEndDate', 'end']].forEach(([id, key]) => {
  const el = $(id);
  if (el) el.addEventListener('change', () => { REPORT[key] = el.value; if (REPORT.range === 'custom') loadReport(); });
});
document.querySelectorAll('#rpDim button').forEach((btn) => {
  btn.addEventListener('click', () => {
    REPORT.dim = btn.dataset.v;
    REPORT.entity = '__all__';
    rpSetSeg('rpDim', REPORT.dim);
    renderRpTrend();
  });
});
document.querySelectorAll('#rpMetric button').forEach((btn) => {
  btn.addEventListener('click', () => {
    REPORT.metric = btn.dataset.v;
    rpSetSeg('rpMetric', REPORT.metric);
    renderRpTrend();
  });
});
document.querySelectorAll('#rpAccSort button').forEach((btn) => {
  btn.addEventListener('click', () => {
    REPORT.accSort = btn.dataset.v;
    rpSetSeg('rpAccSort', REPORT.accSort);
    if (REPORT.data) renderRpAccTable(REPORT.data.accounts || []);
  });
});
const rpEntitySel = $('rpEntity');
if (rpEntitySel) {
  rpEntitySel.addEventListener('change', () => { REPORT.entity = rpEntitySel.value; renderRpTrend(); });
}
const rpRefreshBtn = $('rpRefreshBtn');
if (rpRefreshBtn) rpRefreshBtn.addEventListener('click', () => loadReport());

// 窗口变化时重画趋势图（SVG 按容器宽度计算柱宽）
let rpResizeTimer = null;
window.addEventListener('resize', () => {
  if (STATE.page !== 'report' || !REPORT.data) return;
  clearTimeout(rpResizeTimer);
  rpResizeTimer = setTimeout(() => renderRpTrend(), 150);
});
