/* 模型管理页：模型清单 / 启停 / 别名映射 / 自定义模型 / 能力位覆盖 / 连通性测试。
 *
 * 数据形状与后端 /api/models/manage 一致：
 *   view.models[]   —— 每个上游模型一行，自带 aliases 与生效能力位
 *   view.mappings[] —— 全部映射（含自映射默认绑定；dangling = 上游已没有这个模型）
 *   view.providers[]—— 左侧分组
 *
 * 所有写接口都回整份 view，前端就地重绘，不再单独拉一次。 */

const MODEL_STATE = { view: null, busy: false };

const MODEL_CAP_LABELS = {
  maxInputTokens: '上下文',
  maxOutputTokens: '最大输出',
  supportsToolCall: '工具调用',
  supportsImages: '图片输入',
  supportsVideo: '视频输入',
  supportsReasoning: '思考',
};

const REASONING_LEVELS = ['off', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];

async function loadModels() {
  try {
    const data = await api('/api/models/manage');
    MODEL_STATE.view = data.view;
    renderModels();
  } catch (e) {
    const tbody = document.querySelector('#modelTable tbody');
    if (tbody) tbody.innerHTML = `<tr><td colspan="9" class="err">加载失败：${esc(e.message)}</td></tr>`;
  }
}

function modelView() { return MODEL_STATE.view || { models: [], mappings: [], providers: [] }; }

function fmtModelTokens(n) {
  if (n == null) return '—';
  if (n >= 1000000) return (n / 1000000).toFixed(n % 1000000 ? 1 : 0) + 'M';
  if (n >= 1000) return (n / 1000).toFixed(n % 1000 ? 1 : 0) + 'K';
  return String(n);
}

function capBadges(m) {
  const out = [];
  if (m.supportsToolCall) out.push('工具');
  if (m.supportsImages) out.push('图片');
  if (m.supportsVideo) out.push('视频');
  if (m.supportsReasoning) out.push('思考');
  return out.length ? out.map((x) => `<span class="tag">${x}</span>`).join(' ') : '—';
}

function modelFiltered() {
  const view = modelView();
  const q = $('modelSearch').value.trim().toLowerCase();
  const provider = $('modelProvider').value;
  const state = $('modelState').value;
  return view.models.filter((m) => {
    if (provider && m.provider !== provider) return false;
    if (state === 'on' && !m.enabled) return false;
    if (state === 'off' && m.enabled) return false;
    if (state === 'custom' && !m.isCustom) return false;
    if (!q) return true;
    return (m.id + ' ' + (m.name || '') + ' ' + (m.aliases || []).join(' ')).toLowerCase().includes(q);
  });
}

function renderModels() {
  const view = modelView();

  // 供应商下拉：保留当前选项，避免刷新后跳回「全部」。
  const sel = $('modelProvider');
  const keep = sel.value;
  const providers = view.providers || [];
  sel.innerHTML = '<option value="">全部供应商</option>' +
    providers.map((p) => `<option value="${esc(p.provider)}">${esc(p.providerLabel || p.provider)}（${p.count}）</option>`).join('');
  sel.value = keep;

  const rows = modelFiltered();
  const tbody = document.querySelector('#modelTable tbody');
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="9" class="hint">没有匹配的模型</td></tr>';
  } else {
    tbody.innerHTML = rows.map((m) => `
      <tr data-provider="${esc(m.provider)}" data-id="${esc(m.id)}">
        <td><b>${esc(m.id)}</b>${m.name && m.name !== m.id ? `<br><span class="tag">${esc(m.name)}</span>` : ''}</td>
        <td>${esc(m.providerLabel || m.provider)}</td>
        <td>${m.isCustom ? '<span class="tag">自定义</span>' : esc(m.source === 'remote' ? '实时' : '内置')}</td>
        <td>${fmtModelTokens(m.capabilities.maxInputTokens)}</td>
        <td>${fmtModelTokens(m.capabilities.maxOutputTokens)}</td>
        <td>${capBadges(m)}</td>
        <td>${(m.aliases && m.aliases.length)
          ? m.aliases.map((a) => `<span class="pill">${esc(a)}</span>`).join(' ')
          : '<span class="hint">—</span>'}</td>
        <td><label class="switch"><input type="checkbox" data-act="state" ${m.enabled ? 'checked' : ''}><span></span></label></td>
        <td class="row" style="gap:4px">
          <button class="ghost tiny" data-act="alias">＋别名</button>
          <button class="ghost tiny" data-act="caps">能力</button>
          <button class="ghost tiny" data-act="test">测试</button>
          ${m.isCustom ? '<button class="danger tiny" data-act="rmcustom">删</button>' : ''}
        </td>
      </tr>`).join('');
  }
  renderMappings();
}

function renderMappings() {
  const view = modelView();
  const tbody = document.querySelector('#mappingTable tbody');
  if (!tbody) return;
  const rows = view.mappings || [];
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="6" class="hint">还没有映射。点右上角「新建映射」。</td></tr>';
    return;
  }
  tbody.innerHTML = rows.map((m) => `
    <tr data-provider="${esc(m.provider)}" data-alias="${esc(m.alias)}" data-target="${esc(m.target)}">
      <td><b>${esc(m.alias)}</b>${m.isDefault ? ' <span class="tag">默认</span>' : ''}</td>
      <td>${esc(m.target)}</td>
      <td>${esc(m.provider)}</td>
      <td>${m.reasoning ? `<span class="tag">${esc(m.reasoning)}</span>` : '<span class="hint">—</span>'}</td>
      <td>${m.dangling ? '<span class="pill lowcredit">未挂载</span>' : (m.enabled ? '<span class="pill">启用</span>' : '<span class="pill cooling">已关闭</span>')}</td>
      <td class="row" style="gap:4px">
        <button class="ghost tiny" data-act="editmap">${m.isDefault ? '等级' : '编辑'}</button>
        ${m.isDefault ? '' : '<button class="danger tiny" data-act="rmmap">删</button>'}
      </td>
    </tr>`).join('');
}

/* ---------------------------------------------------------------- 写操作 */

async function modelPost(path, body) {
  if (MODEL_STATE.busy) return null;
  MODEL_STATE.busy = true;
  try {
    const data = await api(path, { method: 'POST', body: JSON.stringify(body) });
    if (data && data.view) {
      MODEL_STATE.view = data.view;
      renderModels();
    }
    return data;
  } catch (e) {
    toastModelError(e.message);
    return null;
  } finally {
    MODEL_STATE.busy = false;
  }
}

function toastModelError(msg) {
  const el = $('modelDialogErr');
  if (el && !$('modelDialog').hidden) { el.textContent = msg; return; }
  // 不在弹层里时用最轻量的提示：顶部状态条不归这里管，改用 alert 式的行内提示。
  window.alert(msg);
}

/* ---------------------------------------------------------------- 弹层 */

function openModelDialog(title, html) {
  $('modelDialogTitle').textContent = title;
  $('modelDialogForm').innerHTML = html;
  $('modelDialogErr').textContent = '';
  $('modelDialog').hidden = false;
  document.body.classList.add('modal-open');
}

function closeModelDialog() {
  $('modelDialog').hidden = true;
  document.body.classList.remove('modal-open');
}

function providerOptions(selected) {
  const view = modelView();
  return (view.providers || []).map((p) =>
    `<option value="${esc(p.provider)}" ${p.provider === selected ? 'selected' : ''}>${esc(p.providerLabel || p.provider)}</option>`).join('');
}

function modelOptions(provider) {
  const view = modelView();
  return view.models.filter((m) => m.provider === provider)
    .map((m) => `<option value="${esc(m.id)}">${esc(m.id)}</option>`).join('');
}

function openAliasDialog(provider, target) {
  openModelDialog('新建别名', `
    <div class="row" style="flex-direction:column;align-items:stretch;gap:10px">
      <label>对外名（下游请求时用）<input id="aliasName" placeholder="例如 my-model"></label>
      <label>上游模型
        <select id="aliasTarget">${modelOptions(provider)}</select>
      </label>
      <label>思考等级（可选）
        <select id="aliasLevel">
          <option value="">不覆盖</option>
          ${REASONING_LEVELS.map((l) => `<option value="${l}">${l}</option>`).join('')}
        </select>
      </label>
      <div class="row"><button class="primary" id="aliasSave">保存</button></div>
    </div>`);
  $('aliasTarget').value = target;
  $('aliasSave').onclick = async () => {
    const alias = $('aliasName').value.trim();
    const tgt = $('aliasTarget').value;
    const level = $('aliasLevel').value;
    if (!alias) { $('modelDialogErr').textContent = '请填写对外名'; return; }
    const body = { provider, alias, target: tgt, create: true };
    if (level) body.reasoning = level;
    const ok = await modelPost('/api/models/mappings', body);
    if (ok) closeModelDialog();
  };
}

function openMappingEditDialog(row) {
  const level = row.reasoning || '';
  openModelDialog(row.isDefault ? '设置思考等级' : '编辑映射', `
    <div class="row" style="flex-direction:column;align-items:stretch;gap:10px">
      <label>对外名<input value="${esc(row.alias)}" disabled></label>
      <label>上游模型<input value="${esc(row.target)}" disabled></label>
      <label>思考等级
        <select id="mapLevel">
          <option value="">不覆盖</option>
          ${REASONING_LEVELS.map((l) => `<option value="${l}" ${l === level ? 'selected' : ''}>${l}</option>`).join('')}
        </select>
      </label>
      <label class="row" style="gap:8px"><input type="checkbox" id="mapEnabled" ${row.enabled ? 'checked' : ''}> 启用这条映射</label>
      <div class="row"><button class="primary" id="mapSave">保存</button></div>
    </div>`);
  $('mapSave').onclick = async () => {
    const body = {
      provider: row.provider, alias: row.alias, target: row.target,
      reasoning: $('mapLevel').value,
      enabled: $('mapEnabled').checked,
      create: false,
    };
    const ok = await modelPost('/api/models/mappings', body);
    if (ok) closeModelDialog();
  };
}

function openCapsDialog(row) {
  const caps = row.capabilities || {};
  const field = (key) => {
    const v = caps[key];
    if (key === 'maxInputTokens' || key === 'maxOutputTokens') {
      return `<input id="cap-${key}" type="number" min="1" placeholder="未声明"
        value="${v == null ? '' : v}">`;
    }
    return `<select id="cap-${key}">
      <option value="" ${v == null ? 'selected' : ''}>未声明</option>
      <option value="true" ${v === true ? 'selected' : ''}>支持</option>
      <option value="false" ${v === false ? 'selected' : ''}>不支持</option>
    </select>`;
  };
  openModelDialog('能力位覆盖 · ' + row.id, `
    <p class="hint">留空 = 不覆盖（用上游声明）。改过之后会立即影响对下游的声明。</p>
    <div class="row" style="flex-direction:column;align-items:stretch;gap:10px">
      ${Object.keys(MODEL_CAP_LABELS).map((k) =>
        `<label>${MODEL_CAP_LABELS[k]}${field(k)}</label>`).join('')}
      <div class="row">
        <button class="primary" id="capsSave">保存</button>
        <button class="ghost" id="capsClear">清除全部覆盖</button>
      </div>
    </div>`);
  $('capsSave').onclick = async () => {
    const values = {};
    for (const k of Object.keys(MODEL_CAP_LABELS)) {
      const el = $('cap-' + k);
      if (!el) continue;
      const raw = el.value.trim();
      if (raw === '') { values[k] = null; continue; }
      if (raw === 'true' || raw === 'false') { values[k] = raw === 'true'; continue; }
      const n = Number(raw);
      if (!Number.isFinite(n) || n < 1) { $('modelDialogErr').textContent = MODEL_CAP_LABELS[k] + ' 必须是正整数'; return; }
      values[k] = n;
    }
    const ok = await modelPost('/api/models/capabilities', { provider: row.provider, id: row.id, capabilities: values });
    if (ok) closeModelDialog();
  };
  $('capsClear').onclick = async () => {
    const ok = await modelPost('/api/models/capabilities', { provider: row.provider, id: row.id, capabilities: null });
    if (ok) closeModelDialog();
  };
}

async function openTestDialog(row) {
  openModelDialog('连通性测试 · ' + row.id, '<p class="hint">正在向上游请求…</p>');
  const data = await modelPost('/api/models/test', { provider: row.provider, id: row.id });
  if (!data) return;
  const cls = data.ok ? 'ok' : 'err';
  $('modelDialogForm').innerHTML = `<p class="${cls}">${esc(data.message || '')}</p>
    <p class="hint">耗时 ${data.elapsedMs || 0} ms</p>`;
}

function openCustomDialog() {
  openModelDialog('登记自定义模型', `
    <p class="hint">上游清单里没有、但实际可用的模型 id。常用于平台提前放量、目录还没更新时。</p>
    <div class="row" style="flex-direction:column;align-items:stretch;gap:10px">
      <label>提供商<select id="customProvider">${providerOptions('')}</select></label>
      <label>模型 id<input id="customID" placeholder="上游的模型 id"></label>
      <div class="row"><button class="primary" id="customSave">登记</button></div>
    </div>`);
  $('customSave').onclick = async () => {
    const provider = $('customProvider').value;
    const id = $('customID').value.trim();
    if (!id) { $('modelDialogErr').textContent = '请填写模型 id'; return; }
    const ok = await modelPost('/api/models/custom', { provider, id });
    if (ok) closeModelDialog();
  };
}

/* ---------------------------------------------------------------- 事件 */

document.addEventListener('DOMContentLoaded', () => {
  const search = $('modelSearch');
  if (search) {
    search.addEventListener('input', renderModels);
    $('modelProvider').addEventListener('change', renderModels);
    $('modelState').addEventListener('change', renderModels);
    $('modelRefreshBtn').addEventListener('click', loadModels);
    $('modelAddCustomBtn').addEventListener('click', openCustomDialog);
    $('modelDialogClose').addEventListener('click', closeModelDialog);
    $('modelDialog').addEventListener('click', (e) => {
      if (e.target === $('modelDialog')) closeModelDialog();
    });
  }

  document.querySelector('#modelTable tbody')?.addEventListener('change', async (e) => {
    const cb = e.target.closest('input[data-act=state]');
    if (!cb) return;
    const tr = cb.closest('tr');
    const ok = await modelPost('/api/models/state', {
      provider: tr.dataset.provider, id: tr.dataset.id, enabled: cb.checked,
    });
    if (!ok) cb.checked = !cb.checked; // 失败时回滚开关
  });

  document.querySelector('#modelTable tbody')?.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const tr = btn.closest('tr');
    const provider = tr.dataset.provider;
    const id = tr.dataset.id;
    const row = modelView().models.find((m) => m.provider === provider && m.id === id);
    switch (btn.dataset.act) {
      case 'alias': openAliasDialog(provider, id); break;
      case 'caps': if (row) openCapsDialog(row); break;
      case 'test': if (row) openTestDialog(row); break;
      case 'rmcustom':
        if (row && confirm(`移除自定义模型登记 ${id}？`)) {
          modelPost('/api/models/custom/remove', { provider, id });
        }
        break;
    }
  });

  document.querySelector('#mappingTable tbody')?.addEventListener('click', (e) => {
    const btn = e.target.closest('button[data-act]');
    if (!btn) return;
    const tr = btn.closest('tr');
    const provider = tr.dataset.provider;
    const alias = tr.dataset.alias;
    const target = tr.dataset.target;
    const row = (modelView().mappings || []).find((m) =>
      m.provider === provider && m.alias === alias && m.target === target);
    if (!row) return;
    if (btn.dataset.act === 'editmap') openMappingEditDialog(row);
    if (btn.dataset.act === 'rmmap') {
      if (confirm(`删除映射 ${alias} → ${target}？`)) {
        modelPost('/api/models/mappings/remove', { provider, alias, target });
      }
    }
  });
});
