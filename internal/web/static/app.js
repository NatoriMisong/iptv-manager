'use strict';
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];
let state = null;
let csrf = '';
let toastTimer;
let busy = false;
let bulkSubmitting = false;
let builtinSubmitting = false;
let builtinSources = [];
function toast(message, failed = false) {
  const el = $('#toast');
  el.textContent = message;
  el.classList.toggle('failed', failed);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 5000);
}
function showLogin() {
  state = null;
  csrf = '';
  builtinSources = [];
  $('#app').hidden = true;
  $('#login-screen').hidden = false;
  $$('dialog[open]').forEach(d => d.close());
  $('#login-password').focus();
}
async function api(path, { method = 'GET', body } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(`/api${path}`, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin', cache: 'no-store' });
  const result = await response.json();
  if (!response.ok) {
    if (response.status === 401 && path !== '/login') showLogin();
    throw new Error(result.error || `请求失败（${response.status}）`);
  }
  return result;
}
async function loadState({ background = false } = {}) {
  const next = await api('/state');
  state = next;
  builtinSources = next.builtin_sources || [];
  csrf = next.csrf;
  $('#login-screen').hidden = true;
  $('#app').hidden = false;
  renderChannels();
  renderStats();
  renderSubscriptions();
  renderBuiltinPage();
  renderProxies();
  $('#version').textContent = next.version ? `v${next.version.replace(/^v/, '')}` : '';
  if (!background) fillSettings();
}
function renderStats() {
  const channels = state.channels || [];
  $('#total-count').textContent = channels.length;
  $('#table-count').textContent = channels.length;
  $('#enabled-count').textContent = `${channels.filter(c => c.enabled && !c.source_missing).length} 个启用 · ${channels.filter(c => !c.enabled || c.source_missing).length} 个停用 / 失效`;
  const gb = Math.max(0, state.traffic?.bytes || 0) / 1e9;
  $('#traffic-value').textContent = gb.toFixed(2);
  $('#budget-value').textContent = state.settings.monthly_budget_gb;
  const percent = state.settings.monthly_budget_gb > 0 ? gb / state.settings.monthly_budget_gb * 100 : 0;
  $('#budget-progress').value = Math.min(100, percent);
  $('#budget-note').textContent = percent >= 100 ? '已达到预算，请留意服务商流量额度' : `已使用 ${percent.toFixed(1)}% · ${state.traffic?.month || ''} UTC`;
}
function node(tag, className, text) {
  const el = document.createElement(tag);
  if (className) el.className = className;
  if (text !== undefined) el.textContent = text;
  return el;
}
function action(text, title, fn, className = 'quiet') {
  const button = node('button', className, text);
  button.type = 'button';
  button.title = title;
  button.setAttribute('aria-label', title);
  button.addEventListener('click', () => perform(fn));
  return button;
}
async function perform(fn) {
  if (busy) return;
  busy = true;
  try { await fn(); } catch (error) { toast(error.message, true); } finally { busy = false; }
}
function renderChannels() {
  const container = $('#channel-rows');
  container.replaceChildren();
  const channels = state.channels || [];
  $('#empty-state').hidden = channels.length !== 0;
  $('.table-wrap').hidden = channels.length === 0;
  channels.forEach((ch, index) => {
    const row = node('tr');
    const ordering = node('td');
    const orderButtons = node('div', 'order-buttons');
    const up = action('↑', `上移 ${ch.name}`, () => reorder(index, -1));
    const down = action('↓', `下移 ${ch.name}`, () => reorder(index, 1));
    up.disabled = index === 0;
    down.disabled = index === channels.length - 1;
    orderButtons.append(up, down);
    ordering.append(orderButtons);
    const channel = node('td');
    const cell = node('div', 'channel-cell');
    const logo = node('div', 'channel-logo', [...ch.name][0] || '▶');
    if (ch.logo) {
      const img = new Image();
      img.alt = '';
      img.loading = 'lazy';
      img.referrerPolicy = 'no-referrer';
      img.addEventListener('error', () => img.remove());
      img.src = ch.logo;
      logo.replaceChildren(img);
    }
    const identity = node('div');
    const name = node('div', 'channel-name', ch.name);
    name.title = ch.name;
    const sourceName = ch.subscription_id ? `M3U · ${state.subscriptions?.find(s => s.id === ch.subscription_id)?.name || '订阅'}` : ch.source_type === 'builtin' ? (builtinSources.find(source => source.id === ch.provider_id)?.name || '内置直播') : ch.source_type === 'stream' ? '通用直播' : 'YouTube';
    identity.append(name, node('div', 'channel-meta', `${ch.group || '未分组'} · ${sourceName}`));
    cell.append(logo, identity);
    channel.append(cell);
    const mode = node('td');
    mode.append(node('span', 'badge', ch.mode === 'direct' ? '客户端直连' : '服务器中继'));
    mode.append(node('div', 'channel-meta', `代理：${proxyLabel(channelProxyRef(ch))}`));
    const originalQuality = ch.source_type !== 'youtube';
    const quality = node('td', '', originalQuality ? '原始画质' : `${ch.quality || state.settings.default_quality}p`);
    if (!ch.quality && !originalQuality) quality.append(node('div', 'channel-meta', '继承全局'));
    const source = node('td');
    const status = state.statuses?.[ch.id];
    const kind = ch.source_missing ? 'missing' : !ch.enabled ? 'disabled' : status?.state || 'unknown';
    const labels = { unknown: '未检测', resolving: '解析中', ready: '来源就绪', offline: '未开播', error: '获取失败', disabled: '已停用', missing: '订阅已无此源' };
    const badge = node('span', `badge status-${Object.hasOwn(labels, kind) ? kind : 'unknown'}`);
    badge.append(node('span', 'status-dot'), document.createTextNode(labels[kind] || '未检测'));
    source.append(badge);
    if (status?.message && ch.enabled) {
      const note = node('div', 'status-note', status.message);
      note.title = status.message;
      source.append(note);
    }
    const enabled = node('td');
    const toggle = action('', `${ch.enabled ? '停用' : '启用'} ${ch.name}`, async () => {
      await api(`/channels/${encodeURIComponent(ch.id)}`, { method: 'PUT', body: { ...ch, enabled: !ch.enabled } });
      await loadState({ background: true });
    }, 'toggle');
    toggle.setAttribute('role', 'switch');
    toggle.setAttribute('aria-checked', String(ch.enabled));
    enabled.append(toggle);
    const actions = node('td');
    const buttons = node('div', 'row-actions');
    buttons.append(action('编辑', `编辑 ${ch.name}`, () => editChannel(ch)), action('刷新来源', `清除 ${ch.name} 来源缓存`, async () => {
      const result = await api(`/channels/${encodeURIComponent(ch.id)}/refresh`, { method: 'POST' });
      await loadState({ background: true });
      toast(result.message);
    }));
    if (!ch.subscription_id) buttons.append(action('删除', `删除 ${ch.name}`, async () => {
      if (!confirm(`删除频道「${ch.name}」？此频道的固定播放地址将失效。`)) return;
      await api(`/channels/${encodeURIComponent(ch.id)}`, { method: 'DELETE' });
      await loadState({ background: true });
      toast('频道已删除');
    }, 'quiet danger'));
    actions.append(buttons);
    row.append(ordering, channel, mode, quality, source, enabled, actions);
    container.append(row);
  });
}
async function reorder(index, step) {
  const ids = state.channels.map(c => c.id);
  [ids[index], ids[index + step]] = [ids[index + step], ids[index]];
  await api('/reorder', { method: 'POST', body: { ids } });
  await loadState({ background: true });
}
function editChannel(channel) {
  const form = $('#channel-form');
  form.reset();
  $('#channel-error').textContent = '';
  $('#channel-dialog-title').textContent = channel ? '编辑频道' : '添加频道';
  const data = channel || { id: '', source_type: 'stream', name: '', url: '', group: '', logo: '', enabled: true, mode: 'relay', quality: 0, proxy: 'direct', sort_order: state.channels?.length || 0 };
  Object.entries(data).forEach(([key, value]) => { if (form.elements.namedItem(key)) form.elements.namedItem(key).value = String(value); });
  fillProxySelect($('#channel-proxy'), data.proxy);
  const managed = !!channel?.subscription_id;
  const builtin = channel?.source_type === 'builtin';
  $('#channel-proxy-field').hidden = builtin;
  $('#channel-proxy-note').hidden = !builtin;
  ['name', 'url', 'group', 'logo'].forEach(key => { form.elements.namedItem(key).readOnly = managed; });
  $('#channel-url').readOnly = managed || builtin;
  $('#channel-type').disabled = managed || builtin;
  $('#channel-type').hidden = builtin;
  $('#channel-type-label').hidden = builtin;
  $('#channel-provider-info').hidden = !builtin;
  $('#channel-provider-info').textContent = builtin ? `内置直播源：${builtinSources.find(source => source.id === channel.provider_id)?.name || channel.provider_id}（来源固定）` : '';
  $('#channel-builtin-option').hidden = !!channel;
  $('#channel-builtin-option').disabled = !!channel;
  $('#channel-managed').hidden = !managed;
  updateSourceFields('channel');
  $('details', form).open = false;
  $('#channel-dialog').showModal();
}
function updateSourceFields(prefix) {
  const stream = $(`#${prefix}-type`).value === 'stream';
  const builtin = $(`#${prefix}-type`).value === 'builtin';
  $(`#${prefix}-quality-field`).hidden = stream || builtin;
  if (stream || builtin) $(`#${prefix}-quality`).value = '0';
  if (prefix === 'channel') {
    $('#channel-url').placeholder = stream ? 'https://example.com/live.m3u8' : 'https://www.youtube.com/watch?v=…';
    $('#channel-source-help').textContent = stream ? '支持公网 HTTP/HTTPS HLS 和媒体直播地址。直连由播放器连接原始来源，中继由服务器转发。' : '通过 yt-dlp 按需解析 YouTube 直播；支持 watch?v=… 和 youtu.be/… 链接。';
    if (builtin) {
      const channel = state.channels.find(ch => ch.id === $('#channel-form').elements.namedItem('id').value);
      $('#channel-source-help').textContent = builtinSources.find(source => source.id === channel?.provider_id)?.playback_help || '内置来源由对应模块处理，可调整播放方式、代理和启停状态。';
    }
  } else {
    $('#bulk-text').placeholder = stream ? '新闻频道,https://example.com/live.m3u8\nhttps://example.com/channel.ts' : '中天新闻,https://www.youtube.com/watch?v=vr3XyVCR4T0\nhttps://youtu.be/V1p33hqPrUk';
  }
}
['channel', 'bulk'].forEach(prefix => $(`#${prefix}-type`).addEventListener('change', () => {
  if (prefix === 'channel' && $('#channel-type').value === 'builtin') {
    $('#channel-type').value = 'stream';
    updateSourceFields('channel');
    perform(openBuiltinSources);
    return;
  }
  updateSourceFields(prefix);
}));
async function openBuiltinSources(sourceId) {
  if (!builtinSources.length) {
    const catalog = await api('/builtin-sources');
    builtinSources = catalog.sources;
  }
  $('#builtin-form').reset();
  const select = $('#builtin-source');
  select.replaceChildren(...builtinSources.map(source => {
    const option = node('option', '', source.name);
    option.value = source.id;
    return option;
  }));
  if (typeof sourceId === 'string') select.value = sourceId;
  renderBuiltinSources();
  $('#channel-dialog').close();
  $('#builtin-dialog').showModal();
}
function renderBuiltinSources() {
  const source = builtinSources.find(item => item.id === $('#builtin-source').value);
  const list = $('#builtin-channels');
  list.replaceChildren();
  $('#builtin-error').textContent = '';
  $('#builtin-result').hidden = true;
  $('#builtin-description').textContent = source.description;
  $('#builtin-help').textContent = source.playback_help || '';
  $('#builtin-website').href = source.website;
  $('#builtin-group').value = source.name;
  $('#builtin-mode').value = source.default_mode === 'direct' ? 'direct' : 'relay';
  source.channels.forEach(channel => {
    const row = node('div', 'builtin-channel');
    const label = node('label', 'builtin-check');
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.value = channel.id;
    checkbox.checked = !!channel.selected;
    checkbox.addEventListener('change', updateBuiltinCount);
    const detail = node('span');
    detail.append(node('strong', '', channel.name));
    if (channel.note) detail.append(node('span', 'muted tiny', channel.note));
    label.append(checkbox, detail);
    const link = node('a', 'builtin-url', source.link_label || '来源地址 ↗');
    link.href = channel.url;
    link.title = channel.url;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
    row.append(label, link);
    list.append(row);
  });
  updateBuiltinCount();
}
function updateBuiltinCount() {
  const channels = $$('#builtin-channels input');
  const count = channels.filter(channel => channel.checked).length;
  $('#builtin-count').textContent = `已选择 ${count} / ${channels.length} 个频道`;
  $('#builtin-submit').disabled = builtinSubmitting || count === 0;
}
$('#builtin-source').addEventListener('change', renderBuiltinSources);
[['#builtin-select-all', true], ['#builtin-select-none', false]].forEach(([selector, checked]) => {
  $(selector).addEventListener('click', () => {
    $$('#builtin-channels input').forEach(channel => { channel.checked = checked; });
    updateBuiltinCount();
  });
});
$('#builtin-dialog').addEventListener('cancel', event => { if (builtinSubmitting) event.preventDefault(); });
$('#builtin-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (builtinSubmitting || busy) return;
  const source = builtinSources.find(item => item.id === $('#builtin-source').value);
  const selected = new Set($$('#builtin-channels input:checked').map(input => input.value));
  const channels = source.channels.filter(channel => selected.has(channel.id));
  if (!channels.length) return;
  const body = { source_type: 'builtin', provider_id: source.id, channel_ids: channels.map(channel => channel.id), group: $('#builtin-group').value.trim(), mode: $('#builtin-mode').value, quality: 0 };
  const controls = $$('input, select, button', event.currentTarget);
  builtinSubmitting = true;
  busy = true;
  controls.forEach(control => { control.disabled = true; });
  $('#builtin-submit').textContent = '正在添加…';
  $('#builtin-error').textContent = '';
  $('#builtin-result').hidden = true;
  try {
    const result = await api('/channels/bulk', { method: 'POST', body });
    $('#builtin-result').textContent = `已添加 ${result.added} 个 · 跳过 ${result.skipped} 个 · 失败 ${result.failed} 个`;
    $('#builtin-result').hidden = false;
    $('#builtin-error').textContent = result.results.filter(item => item.status === 'failed').map(item => `${item.name || '频道'}：${item.message}`).join('；');
    try { await loadState({ background: true }); }
    catch (error) { $('#builtin-error').textContent += ` 频道列表刷新失败：${error.message}`; }
  } catch (error) { $('#builtin-error').textContent = error.message; }
  finally {
    controls.forEach(control => { control.disabled = false; });
    $('#builtin-submit').textContent = '添加所选频道';
    builtinSubmitting = false;
    busy = false;
    updateBuiltinCount();
  }
});
function editSubscription(sub) {
  const form = $('#subscription-form');
  form.reset();
  const data = sub || { id: '', name: '', url: '', proxy: 'direct', interval_minutes: 60, enabled: true };
  Object.entries(data).forEach(([key, value]) => { if (form.elements.namedItem(key)) form.elements.namedItem(key).value = String(value); });
  fillProxySelect($('#subscription-proxy'), data.proxy);
  $('#subscription-error').textContent = '';
  $('#subscription-dialog-title').textContent = sub ? '编辑 M3U 订阅' : '添加 M3U 订阅';
  $('#subscription-dialog').showModal();
}
function renderSubscriptions() {
  const list = $('#subscription-list');
  list.replaceChildren();
  const subs = state.subscriptions || [];
  $('#subscriptions-empty').hidden = subs.length !== 0;
  const time = value => !value || value.startsWith('0001-') ? '尚未同步' : new Date(value).toLocaleString();
  subs.forEach(sub => {
    const card = node('article', 'panel settings-card');
    const channels = state.channels.filter(ch => ch.subscription_id === sub.id);
    card.append(node('h2', '', sub.name));
    card.append(node('p', 'muted', `${new URL(sub.url).host} · ${channels.filter(ch => !ch.source_missing).length} 个来源有效 · ${channels.filter(ch => ch.source_missing).length} 个失效`));
    card.append(node('p', 'field-help', `${sub.enabled ? `每 ${sub.interval_minutes} 分钟自动更新` : '自动更新已暂停'} · 最近成功：${time(sub.last_sync)}`));
    if (sub.last_error) card.append(node('p', 'error', `同步失败：${sub.last_error}`));
    if (sub.skipped) card.append(node('p', 'field-help', `最近一次同步跳过 ${sub.skipped} 个重复、不支持或无效的条目`));
    const buttons = node('div', 'subscription-buttons');
    buttons.append(action('编辑订阅', `编辑订阅 ${sub.name}`, () => editSubscription(sub)), action('立即同步', `立即同步 ${sub.name}`, async () => {
      buttons.querySelectorAll('button').forEach(b => { b.disabled = true; });
      try { await api(`/subscriptions/${encodeURIComponent(sub.id)}/sync`, { method: 'POST' }); toast('订阅同步完成'); }
      finally { await loadState({ background: true }); buttons.querySelectorAll('button').forEach(b => { b.disabled = false; }); }
    }), action('删除订阅', `删除订阅 ${sub.name}`, async () => {
      if (!confirm(`删除订阅「${sub.name}」及其 ${channels.length} 个频道？对应的播放地址将失效。`)) return;
      await api(`/subscriptions/${encodeURIComponent(sub.id)}`, { method: 'DELETE' });
      await loadState({ background: true }); toast('订阅及关联频道已删除');
    }, 'quiet danger'));
    card.append(buttons); list.append(card);
  });
}
function fillSettings() {
  const form = $('#settings-form');
  Object.entries(state.settings).forEach(([key, value]) => { if (form.elements.namedItem(key)) form.elements.namedItem(key).value = String(value); });
  if (!state.settings.base_url) form.elements.namedItem('base_url').value = location.origin;
}
function updateBulkCount() {
  const input = $('#bulk-text');
  const count = input.value.split('\n').filter(line => line.trim()).length;
  $('#bulk-count').textContent = `${count} / 100`;
  input.setCustomValidity(count > 100 ? '每批最多添加 100 行频道，请分批提交' : '');
}
function renderBulkResult(result) {
  const labels = { added: '已添加', skipped: '已跳过', failed: '失败' };
  const list = $('#bulk-result-list');
  list.replaceChildren();
  result.results.forEach(item => {
    const row = node('li');
    const detail = node('div');
    detail.append(node('strong', '', `第 ${item.line} 行${item.name ? ` · ${item.name}` : ''}`));
    detail.append(node('p', 'muted tiny', item.message));
    if (item.url) detail.append(node('p', 'muted tiny', item.url));
    row.append(node('span', `badge bulk-${item.status}`, labels[item.status] || '未知'), detail);
    list.append(row);
  });
  $('#bulk-summary').textContent = `已添加 ${result.added} 个 · 跳过 ${result.skipped} 个 · 失败 ${result.failed} 个`;
  $('#bulk-results').hidden = false;
  $('#bulk-summary').focus();
  $('#bulk-submit').scrollIntoView({ block: 'nearest' });
}
function subscription() {
  const base = (state.settings.base_url || location.origin).replace(/\/+$/, '');
  const url = new URL(`${base}/playlist.m3u`);
  url.searchParams.set('token', state.settings.playback_token);
  return url.toString();
}
async function copySubscription() {
  if (!state.settings.base_url) {
    $('[data-view="settings"]').click();
    $('#base-url').focus();
    toast('请先保存播放器可访问的公开地址，再复制订阅', true);
    return;
  }
  const value = subscription();
  try {
    if (!navigator.clipboard) throw new Error('manual');
    await navigator.clipboard.writeText(value);
    toast('订阅地址已复制，可在 VLC 中打开网络串流');
  } catch {
    $('#copy-value').value = value;
    $('#copy-dialog').showModal();
    $('#copy-value').select();
  }
}
$('#login-form').addEventListener('submit', async event => {
  event.preventDefault();
  const button = $('button', event.currentTarget);
  button.disabled = true;
  $('#login-error').textContent = '';
  try {
    const result = await api('/login', { method: 'POST', body: { password: $('#login-password').value } });
    csrf = result.csrf;
    $('#login-password').value = '';
    await loadState();
  } catch (error) { $('#login-error').textContent = error.message; }
  finally { button.disabled = false; }
});
$('#logout').addEventListener('click', () => perform(async () => { await api('/logout', { method: 'POST' }); showLogin(); }));
$('#reload').addEventListener('click', () => perform(async () => { await loadState({ background: true }); toast('状态已更新'); }));
$('#add-channel').addEventListener('click', () => editChannel());
$('#empty-add').addEventListener('click', () => editChannel());
$('#bulk-add').addEventListener('click', () => {
  $('#bulk-form').reset();
  fillProxySelect($('#bulk-proxy'), 'direct');
  $('#bulk-error').textContent = '';
  $('#bulk-results').hidden = true;
  $('#bulk-result-list').replaceChildren();
  updateBulkCount();
  updateSourceFields('bulk');
  $('#bulk-dialog').showModal();
  $('#bulk-text').focus();
});
$('#bulk-text').addEventListener('input', updateBulkCount);
$('#bulk-dialog').addEventListener('cancel', event => { if (bulkSubmitting) event.preventDefault(); });
$('#bulk-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (bulkSubmitting || busy) return;
  const form = event.currentTarget;
  const data = Object.fromEntries(new FormData(form));
  data.quality = Number(data.quality);
  const controls = $$('input, textarea, select, button', form);
  bulkSubmitting = true;
  busy = true;
  controls.forEach(control => { control.disabled = true; });
  $('#bulk-submit').textContent = '正在添加…';
  $('#bulk-error').textContent = '';
  $('#bulk-results').hidden = true;
  try {
    const result = await api('/channels/bulk', { method: 'POST', body: data });
    renderBulkResult(result);
    try { await loadState({ background: true }); }
    catch (error) { $('#bulk-error').textContent = `添加结果已返回，但频道列表刷新失败：${error.message}`; }
  } catch (error) { $('#bulk-error').textContent = error.message; }
  finally {
    controls.forEach(control => { control.disabled = false; });
    $('#bulk-submit').textContent = '添加频道';
    bulkSubmitting = false;
    busy = false;
  }
});
$$('[data-view]').forEach(button => button.addEventListener('click', () => {
  $$('[data-view]').forEach(b => b.classList.toggle('active', b === button));
  ['channels', 'subscriptions', 'builtin', 'proxies', 'settings'].forEach(view => { $(`#view-${view}`).hidden = button.dataset.view !== view; });
}));
$$('.close-dialog').forEach(button => button.addEventListener('click', () => button.closest('dialog').close()));
$('#copy-subscription').addEventListener('click', () => copySubscription());
$('#channel-form').addEventListener('submit', async event => {
  event.preventDefault();
  let data = Object.fromEntries(new FormData(event.currentTarget));
  data = { ...state.channels.find(ch => ch.id === data.id), ...data };
  data.enabled = data.enabled === 'true';
  data.quality = Number(data.quality);
  data.sort_order = Number(data.sort_order);
  data.proxy = data.proxy || 'direct';
  const button = $('button[type="submit"]', event.currentTarget);
  button.disabled = true;
  $('#channel-error').textContent = '';
  try {
    await api(data.id ? `/channels/${encodeURIComponent(data.id)}` : '/channels', { method: data.id ? 'PUT' : 'POST', body: data });
    $('#channel-dialog').close();
    await loadState({ background: true });
    toast('频道已保存');
  } catch (error) { $('#channel-error').textContent = error.message; }
  finally { button.disabled = false; }
});
$('#add-subscription').addEventListener('click', () => editSubscription());
$('#subscription-form').addEventListener('submit', async event => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.interval_minutes = Number(data.interval_minutes);
  data.enabled = data.enabled === 'true';
  data.proxy = data.proxy || 'direct';
  const button = $('button[type="submit"]', event.currentTarget);
  button.disabled = true; $('#subscription-error').textContent = '';
  try {
    const saved = await api(data.id ? `/subscriptions/${encodeURIComponent(data.id)}` : '/subscriptions', {method: data.id ? 'PUT' : 'POST', body: data});
    $('#subscription-dialog').close();
    await loadState({background:true});
    toast('订阅已保存，正在获取频道列表…');
    await perform(async () => {
      try { await api(`/subscriptions/${encodeURIComponent(saved.id)}/sync`, {method:'POST'}); toast('订阅同步完成'); }
      finally { await loadState({background:true}); }
    });
  } catch (error) { if ($('#subscription-dialog').open) $('#subscription-error').textContent = error.message; else toast(error.message,true); }
  finally { button.disabled = false; }
});
$('#settings-form').addEventListener('submit', async event => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.default_quality = Number(data.default_quality);
  data.monthly_budget_gb = Number(data.monthly_budget_gb);
  const button = $('button[type="submit"]', event.currentTarget);
  button.disabled = true;
  $('#settings-error').textContent = '';
  try { await api('/settings', { method: 'PUT', body: data }); await loadState(); toast('设置已保存'); }
  catch (error) { $('#settings-error').textContent = error.message; }
  finally { button.disabled = false; }
});
$('#change-password').addEventListener('click', () => { $('#password-form').reset(); $('#password-error').textContent = ''; $('#password-dialog').showModal(); });
$('#password-form').addEventListener('submit', async event => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  const button = $('button[type="submit"]', event.currentTarget);
  button.disabled = true;
  $('#password-error').textContent = '';
  try { await api('/password', { method: 'POST', body: data }); event.target.reset(); showLogin(); toast('密码已更新，请使用新密码登录'); }
  catch (error) { $('#password-error').textContent = error.message; }
  finally { button.disabled = false; }
});
$('#rotate-token').addEventListener('click', () => perform(async () => {
  if (!confirm('重置播放令牌会使所有旧订阅和播放地址失效。确认后，需要重新复制订阅到设备。继续？')) return;
  await api('/token', { method: 'POST' });
  await loadState();
  toast('播放令牌已重置，请重新复制订阅地址');
}));
$('#backup').addEventListener('click', () => perform(async () => {
  if (!confirm('导出的配置包含播放令牌及已填写的代理凭据，请保存在可信位置。继续导出？')) return;
  const data = await api('/backup');
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = `iptv-manager-${new Date().toISOString().slice(0, 10)}.json`;
  document.body.append(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}));
$('#restore').addEventListener('click', () => $('#restore-file').click());
$('#restore-file').addEventListener('change', event => perform(async () => {
  const file = event.target.files[0];
  event.target.value = '';
  if (!file) return;
  if (file.size > 32 * 1024 * 1024) throw new Error('备份文件不能超过 32 MB');
  if (!confirm('导入会替换当前频道、M3U 订阅、设置和播放令牌（不修改管理密码）。请确认文件来自可信来源。继续？')) return;
  let data;
  try { data = JSON.parse(await file.text()); } catch { throw new Error('文件不是有效的 JSON 备份'); }
  await api('/restore', { method: 'POST', body: data });
  await loadState();
  toast('配置已导入');
}));
loadState().catch(error => { if ($('#login-screen').hidden) { showLogin(); $('#login-error').textContent = error.message; } });
setInterval(() => {
  if (!state || document.hidden || $$('dialog[open]').length || busy) return;
  loadState({ background: true }).catch(() => {});
}, 20000);

const proxyTests = new Map();
function proxyAddress(p) {
  const host = p.host.includes(':') ? `[${p.host}]` : p.host;
  return `${p.scheme}://${host}:${p.port}`;
}
function proxyLabel(ref) {
  if (!ref || ref === 'direct') return '直连';
  return (state.settings.proxies || []).find(p => p.id === ref)?.name || '已删除的代理';
}
function channelProxyRef(ch) {
  if (ch.source_type === 'builtin') return state.settings.provider_proxies?.[ch.provider_id] || 'direct';
  return ch.proxy || 'direct';
}
function fillProxySelect(select, value) {
  const direct = node('option', '', '直连（不使用代理）');
  direct.value = 'direct';
  select.replaceChildren(direct, ...(state.settings.proxies || []).map(p => {
    const option = node('option', '', `${p.name} · ${proxyAddress(p)}`);
    option.value = p.id;
    return option;
  }));
  select.value = value && [...select.options].some(option => option.value === value) ? value : 'direct';
}
function proxyUsage(ref) {
  const channels = (state.channels || []).filter(ch => ch.source_type !== 'builtin' && (ch.proxy || 'direct') === ref).length;
  const subscriptions = (state.subscriptions || []).filter(sub => (sub.proxy || 'direct') === ref).length;
  const providers = Object.values(state.settings.provider_proxies || {}).filter(value => (value || 'direct') === ref).length;
  return `${channels} 个频道 · ${subscriptions} 个订阅 · ${providers} 个内置直播源`;
}
function renderBuiltinPage() {
  const list = $('#builtin-list');
  list.replaceChildren();
  builtinSources.forEach(source => {
    const card = node('article', 'panel settings-card');
    const head = node('div', 'card-head');
    const title = node('div');
    title.append(node('h2', '', source.name), node('p', 'muted', source.description || ''));
    const added = (state.channels || []).filter(ch => ch.source_type === 'builtin' && ch.provider_id === source.id).length;
    head.append(title, node('span', 'badge', `已添加 ${added} / ${source.channels.length} 个频道`));
    card.append(head);
    if (source.playback_help) card.append(node('p', 'field-help', source.playback_help));
    const row = node('div', 'builtin-proxy');
    const label = node('label', '', '出站代理');
    label.htmlFor = `provider-proxy-${source.id}`;
    const select = document.createElement('select');
    select.id = label.htmlFor;
    fillProxySelect(select, state.settings.provider_proxies?.[source.id] || 'direct');
    select.addEventListener('change', () => perform(async () => {
      select.disabled = true;
      try {
        await api(`/providers/${encodeURIComponent(source.id)}/proxy`, { method: 'PUT', body: { proxy: select.value } });
        await loadState({ background: true });
        toast(`${source.name} 的代理已更新，下次播放时生效`);
      } finally { select.disabled = false; }
    }));
    row.append(label, select, node('p', 'muted tiny', '该来源的所有频道共用此代理：解析播放地址、获取清单和中继分片都经过它。'));
    card.append(row);
    const buttons = node('div', 'subscription-buttons');
    buttons.append(action('添加频道', `添加 ${source.name} 频道`, () => openBuiltinSources(source.id), 'outline'));
    if (source.website) {
      const link = node('a', 'builtin-url', source.link_label ? '官方网站 ↗' : '官方网站 ↗');
      link.href = source.website;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      buttons.append(link);
    }
    card.append(buttons);
    list.append(card);
  });
}
function renderProxies() {
  const list = $('#proxy-list');
  list.replaceChildren();
  const direct = node('article', 'panel settings-card');
  const directHead = node('div', 'card-head');
  const directTitle = node('div');
  directTitle.append(node('h2', '', '直连'), node('p', 'muted', '不使用代理，服务器直接访问直播来源。新频道和新订阅默认直连。'));
  directHead.append(directTitle, node('span', 'badge', '固定选项'));
  direct.append(directHead, node('p', 'field-help', `使用中：${proxyUsage('direct')}`));
  list.append(direct);
  const proxies = state.settings.proxies || [];
  proxies.forEach(p => {
    const card = node('article', 'panel settings-card');
    const head = node('div', 'card-head');
    const title = node('div');
    title.append(node('h2', '', p.name), node('p', 'proxy-address', proxyAddress(p) + (p.username ? ` · 用户名 ${p.username}` : '')));
    head.append(title, node('span', 'badge', p.scheme.toUpperCase()));
    card.append(head, node('p', 'field-help', `使用中：${proxyUsage(p.id)}`));
    const outcome = proxyTests.get(p.id);
    const result = node('p', `proxy-test field-help ${outcome?.status || ''}`.trim(), outcome?.text || '');
    result.setAttribute('role', 'status');
    card.append(result);
    const buttons = node('div', 'subscription-buttons');
    const test = node('button', 'outline', outcome?.status === 'running' ? '测试中…' : '测试');
    test.type = 'button';
    test.disabled = outcome?.status === 'running';
    test.addEventListener('click', () => testProxy(p));
    buttons.append(test, action('编辑', `编辑代理 ${p.name}`, () => editProxy(p)), action('删除', `删除代理 ${p.name}`, async () => {
      if (!confirm(`删除代理「${p.name}」？正在使用它的频道、订阅或内置直播源会阻止删除。`)) return;
      await api(`/proxies/${encodeURIComponent(p.id)}`, { method: 'DELETE' });
      proxyTests.delete(p.id);
      await loadState({ background: true });
      toast('代理已删除');
    }, 'quiet danger'));
    card.append(buttons);
    list.append(card);
  });
  if (!proxies.length) list.append(node('p', 'muted tiny', '尚未添加代理。点击右上角“添加代理”，然后在频道、订阅或内置直播源中选择它。'));
}
async function testProxy(p) {
  if (proxyTests.get(p.id)?.status === 'running') return;
  proxyTests.set(p.id, { status: 'running', text: '正在通过代理访问 ipip.info…' });
  renderProxies();
  try {
    const outcome = await api(`/proxies/${encodeURIComponent(p.id)}/test`, { method: 'POST' });
    proxyTests.set(p.id, { status: 'ok', text: `出口 IP ${outcome.ip} · ${outcome.elapsed_ms} ms` });
  } catch (error) {
    proxyTests.set(p.id, { status: 'failed', text: `测试失败：${error.message}` });
  }
  renderProxies();
}
function editProxy(p) {
  const form = $('#proxy-form');
  form.reset();
  $('#proxy-error').textContent = '';
  $('#proxy-dialog-title').textContent = p ? '编辑代理' : '添加代理';
  const data = p || { id: '', name: '', scheme: 'socks5', host: '', port: 1080, username: '', password: '' };
  Object.entries(data).forEach(([key, value]) => { if (form.elements.namedItem(key)) form.elements.namedItem(key).value = String(value); });
  $('#proxy-dialog').showModal();
  $('#proxy-name').focus();
}
$('#add-proxy').addEventListener('click', () => editProxy());
$('#proxy-form').addEventListener('submit', async event => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.currentTarget));
  data.port = Number(data.port);
  const button = $('button[type="submit"]', event.currentTarget);
  button.disabled = true;
  $('#proxy-error').textContent = '';
  try {
    await api(data.id ? `/proxies/${encodeURIComponent(data.id)}` : '/proxies', { method: data.id ? 'PUT' : 'POST', body: data });
    $('#proxy-dialog').close();
    proxyTests.delete(data.id);
    await loadState({ background: true });
    toast(data.id ? '代理已更新，使用它的频道将重新连接' : '代理已保存，可在频道、订阅或内置直播源中选择');
  } catch (error) { $('#proxy-error').textContent = error.message; }
  finally { button.disabled = false; }
});
