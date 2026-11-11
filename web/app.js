'use strict';
const $ = id => document.getElementById(id);
const applicationNames = new Map();
let identity = null, adminData = null, authExpiry, authDeadline = 0;
function expiredLogin() { signedOut(); notice('Срок входа истёк. Введите логин и пароль снова.'); }
window.addEventListener('mvp-auth-expired', expiredLogin);
document.addEventListener('visibilitychange', () => { if (!document.hidden && identity && authDeadline && Date.now() >= authDeadline) expiredLogin(); });
function scheduleExpiry() {
  clearTimeout(authExpiry);
  const remaining = (identity.expires_at - identity.server_time) * 1000;
  if (Number.isFinite(remaining)) { authDeadline = Date.now() + remaining; authExpiry = setTimeout(expiredLogin, Math.max(0, remaining)); }
}

function el(tag, text, className) { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (className) e.className = className; return e; }
function notice(text, error = false) { $('message').textContent = text; $('message').className = error ? 'error' : ''; $('message').hidden = !text; }
async function api(path, method = 'GET', body) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (identity) headers['X-CSRF-Token'] = identity.csrf_token;
  let response;
  for (let attempt = 0; ; attempt++) {
    try {
      response = await fetch('/api/v1' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin' });
      break;
    } catch (error) {
      // Reads are safe to retry when Docker changes the local network.
      if (method !== 'GET' || !(error instanceof TypeError) || attempt >= 2) throw error;
      await new Promise(resolve => setTimeout(resolve, 500));
    }
  }
  const data = await response.json();
  if (!response.ok) { const error = new Error(data.error?.message || 'Ошибка запроса'); error.status = response.status; throw error; }
  return data;
}
async function act(action, button) {
  if (button) button.disabled = true;
  try { notice(''); await action(); } catch (e) {
    if (e.status === 401) signedOut();
    if (e.status === 403 && identity) { try { await refreshIdentity(); } catch (refreshError) { if (refreshError.status === 401) signedOut(); } }
    notice(e.message, true);
  }
  finally { if (button) button.disabled = false; }
}
const sessionFrames = new Map();
let currentSessionID = '', navigationVersion = 0;
const streamView = () => new URLSearchParams(location.search).get('embedded') === '1';
function renderSessionTabs(sessions) {
  $('session-tabs').replaceChildren();
  for (const s of sessions) {
    const button = el('button', appName(s.application_id), 'secondary session-tab');
    button.type = 'button'; button.dataset.session = s.id;
    button.setAttribute('aria-pressed', String(s.id === currentSessionID));
    button.onclick = () => act(() => navigateWorkspace('/sessions/' + s.id));
    $('session-tabs').append(button);
  }
  for (const [id, frame] of sessionFrames) {
    if (!sessions.some(s => s.id === id)) { frame.remove(); sessionFrames.delete(id); }
  }
}
async function showDesktop(id, version) {
  const [apps, sessions] = await Promise.all([api('/applications'), api('/sessions')]);
  if (version !== navigationVersion || !identity) return;
  apps.forEach(a => applicationNames.set(a.id, a.name));
  const active = sessions.filter(s => !['closed', 'failed', 'stopping'].includes(s.state));
  if (!active.some(s => s.id === id)) {
    await navigateWorkspace('/');
    notice('Приложение завершено или недоступно. Выберите приложение для запуска.');
    return;
  }
  currentSessionID = id;
  renderSessionTabs(active);
  if (!sessionFrames.has(id)) {
    const frame = document.createElement('iframe');
    frame.src = '/sessions/' + id + '?embedded=1';
    frame.title = appName(active.find(s => s.id === id).application_id);
    frame.allow = 'autoplay; fullscreen'; frame.allowFullscreen = true;
    sessionFrames.set(id, frame); $('session-frames').append(frame);
  }
  for (const [key, frame] of sessionFrames) {
    frame.hidden = key !== id;
    if (key !== id) frame.contentWindow?.postMessage({type: 'mvp-background'}, location.origin);
  }
  $('workspace').hidden = true; $('session-view').hidden = true; $('desktop-view').hidden = false;
  document.body.classList.add('desktop-mode');
}
async function navigateWorkspace(path) {
  if (path === '/') switchTab(false);
  if (location.pathname !== path || location.search) history.pushState({}, '', path);
  await refreshIdentity();
}
window.addEventListener('popstate', () => act(refreshIdentity));
window.addEventListener('message', event => {
  if (event.origin !== location.origin || ![...sessionFrames.values()].some(f => f.contentWindow === event.source)) return;
  if (event.data?.type === 'mvp-session-ended') act(async () => {
    const sessions = await api('/sessions');
    const active = sessions.filter(s => !['closed','failed','stopping'].includes(s.state));
    renderSessionTabs(active);
    if (currentSessionID && !active.some(s => s.id === currentSessionID)) await navigateWorkspace(active.length ? '/sessions/' + active[0].id : '/');
  });
  if (event.data?.type === 'mvp-catalog') act(() => navigateWorkspace('/'));
});
$('desktop-catalog').onclick = () => act(() => navigateWorkspace('/'));
document.querySelectorAll('.brand, .rail-link').forEach(link => link.onclick = event => {
  if (streamView()) return;
  event.preventDefault(); act(() => navigateWorkspace('/'));
});
function signedOut() { navigationVersion++; currentSessionID = ''; clearTimeout(authExpiry); for (const frame of sessionFrames.values()) frame.remove(); sessionFrames.clear(); $('desktop-view').hidden = true; document.body.classList.remove('desktop-mode'); window.mvpSession?.close(); identity = null; $('login-panel').hidden = false; $('workspace').hidden = true; $('session-view').hidden = true; $('account').hidden = true; }
async function refreshIdentity() {
  const version = ++navigationVersion;
  const loaded = await api('/me'); if (version !== navigationVersion) return;
  identity = loaded; scheduleExpiry();
  $('login-panel').hidden = true; $('account').hidden = false; $('login-name').textContent = identity.user.login;
  $('admin-tab').hidden = !identity.user.admin;
  if (!identity.user.admin) switchTab(false);
  const match = location.pathname.match(/^\/sessions\/([a-f0-9]+)$/);
  document.body.classList.toggle('stream-mode', streamView());
  if (match && !streamView()) { await showDesktop(match[1], version); return; }
  $('desktop-view').hidden = true; document.body.classList.remove('desktop-mode');
  if (match) {
    $('workspace').hidden = true; $('session-view').hidden = false;
    const apps = await api('/applications'); apps.forEach(a => applicationNames.set(a.id, a.name)); const s = await api('/sessions/' + match[1]); $('session-title').textContent = appName(s.application_id); $('session-state').textContent = 'Состояние: ' + stateName(s.state);
    if (!['closed', 'failed', 'stopping'].includes(s.state)) window.mvpSession.start(s.id, identity.csrf_token);
    $('stop-app').onclick = () => act(async () => { await api('/sessions/' + s.id, 'DELETE'); window.mvpSession.close(); if (window.parent !== window) parent.postMessage({type:'mvp-session-ended'}, location.origin); else location.assign('/'); }, $('stop-app'));
  } else {
    currentSessionID = '';
    for (const frame of sessionFrames.values()) frame.contentWindow?.postMessage({type:'mvp-background'}, location.origin);
    $('workspace').hidden = false; $('session-view').hidden = true; await refreshCatalog(); }
}
function appName(id) { return applicationNames.get(id) || ({falkon:'Falkon',firefox:'Firefox',terminal:'Терминал'})[id] || id; }
function stateName(state) { return ({ preparing: 'Подготовка', ready: 'Готово', connecting: 'Подключение', active: 'Работает', reconnecting: 'Восстановление связи', stopping: 'Завершение', closed: 'Завершён', failed: 'Ошибка запуска' })[state] || state; }
function sessionRow(s, admin = false) {
  const row = el('div', undefined, 'row'), title = el('div'); title.append(el('strong', appName(s.application_id)));
  title.append(el('small', stateName(s.state) + (admin ? ' · пользователь #' + s.user_id : ''))); row.append(title);
  const actions = el('div', undefined, 'actions');
  if (!admin) { const a = el('a', 'Открыть', 'button secondary'); a.href = '/sessions/' + encodeURIComponent(s.id); a.onclick = event => { event.preventDefault(); act(() => navigateWorkspace(a.pathname)); }; actions.append(a); }
  const stop = el('button', 'Завершить', 'secondary'); stop.addEventListener('click', () => act(async () => { await api((admin ? '/admin' : '') + '/sessions/' + s.id, 'DELETE'); await (admin ? refreshAdmin() : refreshCatalog()); }, stop)); actions.append(stop); row.append(actions); return row;
}
// A Docker network change can abort Chromium's in-flight fetch on a local host.
// Never retry the POST: recover only a unique new session, excluding existing ones.
async function launchApplication(appID) {
  const before = new Set((await api('/sessions')).map(s => s.id));
  try { return await api('/sessions', 'POST', { application_id: appID }); }
  catch (error) {
    if (!(error instanceof TypeError)) throw error;
    notice('Сеть изменилась во время запуска. Ожидаем приложение…');
    const deadline = Date.now() + 35000;
    while (Date.now() < deadline) {
      await new Promise(resolve => setTimeout(resolve, 750));
      let sessions;
      try { sessions = await api('/sessions'); }
      catch (e) { if (e instanceof TypeError) continue; throw e; }
      const found = sessions.filter(s => s.application_id === appID && !before.has(s.id));
      if (found.length > 1) throw new Error('Найдено несколько новых сеансов. Выберите нужный в разделе «Активные сеансы».');
      if (found.length === 1 && ['ready', 'connecting', 'active', 'reconnecting'].includes(found[0].state)) {
        notice(''); return found[0];
      }
    }
    throw new Error('Не удалось подтвердить запуск после изменения сети. Проверьте «Активные сеансы» перед повторным запуском.');
  }
}
async function refreshCatalog() {
  const [apps, sessions] = await Promise.all([api('/applications'), api('/sessions')]);
  apps.forEach(a => applicationNames.set(a.id, a.name));
  $('runtime-note').hidden = identity.runtime_available; $('applications').replaceChildren();
  if (!apps.length) $('applications').append(el('p', 'Пока нет доступных приложений. Обратитесь к администратору.', 'empty'));
  for (const app of apps) {
    const card = el('article', undefined, 'app-card'); card.dataset.app = app.id; card.append(el('div', app.template !== 'terminal' ? '◎' : '>_', 'app-icon'), el('h2', app.name), el('p', app.start_url || (app.template !== 'terminal' ? 'Работа с внутренними веб-сервисами в удалённом браузере.' : 'Командная строка в отдельном серверном окружении.')));
    card.append(el('span', [app.can_control ? 'Управление' : 'Просмотр', ...(app.can_print ? ['Печать'] : [])].join(' · '), 'pill'));
    const start = el('button', 'Открыть приложение');
    start.addEventListener('click', () => act(async () => {
      notice('Запускаем ' + app.name + '. Подождите…');
      const launched = await launchApplication(app.id);
      notice('');
      await navigateWorkspace('/sessions/' + launched.id);
    }, start)); card.append(start); $('applications').append(card);
  }
  $('session-count').textContent = sessions.length + ' / 2'; $('sessions').replaceChildren(...sessions.map(s => sessionRow(s)));
  if (!sessions.length) $('sessions').append(el('p', 'Здесь появятся запущенные приложения.', 'empty'));
}
function switchTab(admin) {
  $('catalog-view').hidden = admin; $('admin-view').hidden = !admin;
  $('catalog-tab').classList.toggle('active', !admin); $('admin-tab').classList.toggle('active', admin);
}
function option(value, text) { const o = el('option', text); o.value = value; return o; }
function renderRoles(selected = []) {
  $('user-roles').replaceChildren();
  for (const role of adminData.roles) { const label = el('label', undefined, 'check'), box = document.createElement('input'); box.type = 'checkbox'; box.name = 'role'; box.value = role.id; box.checked = selected.includes(role.id); label.append(box, document.createTextNode(role.name)); $('user-roles').append(label); }
  filterUserRoles();
}
function filterUserRoles() {
  const query = $('user-role-search').value.trim().toLocaleLowerCase();
  for (const label of $('user-roles').children) label.hidden = !label.textContent.toLocaleLowerCase().includes(query);
  $('user-role-count').textContent = '(' + $('user-roles').querySelectorAll('input:checked').length + ' выбрано)';
}
$('user-role-search').oninput = filterUserRoles;
$('user-roles').onchange = filterUserRoles;
function matchesSearch(value, query) { return value.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()); }
function fillSelect(id, items, label) {
  const select = $(id), previous = select.value;
  select.replaceChildren(...items.map(item => option(item.id, label(item))));
  select.disabled = !items.length;
  if (items.some(item => String(item.id) === previous)) select.value = previous;
  if (!items.length) select.append(option('', 'Ничего не найдено'));
}
function filteredSelection(prefix, items, label, searchable, state) {
  const query = $(prefix + '-search').value, status = $(prefix + '-status').value;
  const filtered = items.filter(item => matchesSearch(searchable(item), query) && (!status || String(state(item)) === status));
  fillSelect(prefix + '-select', filtered, label);
  $(prefix + '-count').textContent = 'Найдено: ' + filtered.length + ' из ' + items.length;
  return filtered.filter(item => String(item.id) === $(prefix + '-select').value);
}
function renderGrantOptions() {
  fillSelect('grant-role', adminData.roles.filter(r => matchesSearch(r.name, $('grant-role-search').value)), r => r.name);
  fillSelect('grant-app', adminData.applications.filter(a => matchesSearch(a.name + ' ' + a.id, $('grant-app-search').value)), a => a.name);
  loadGrant();
}
function editUser(user) {
  const f = $('user-form'); f.elements.id.value = user.id; f.elements.login.value = user.login; f.elements.password.value = ''; f.elements.enabled.checked = user.enabled; renderRoles(user.role_ids); $('user-form-title').textContent = 'Редактирование пользователя'; f.scrollIntoView({ behavior: 'smooth', block: 'center' });
}
function resetUser() { $('user-form').reset(); $('user-form').elements.id.value = '0'; $('user-form-title').textContent = 'Новый пользователь'; renderRoles([]); }
function loadGrant() {
  const f = $('grant-form'); f.querySelector('button[type=submit]').disabled = !$('grant-role').value || !$('grant-app').value; const grant = adminData.grants.find(g => g.role_id === Number(f.elements.role_id.value) && g.application_id === f.elements.application_id.value);
  for (const k of ['can_launch', 'can_control', 'can_print']) f.elements[k].checked = Boolean(grant?.[k]);
}
async function refreshAdmin() {
  adminData = await api('/admin'); adminData.applications.forEach(a => applicationNames.set(a.id, a.name));
  const selectedRoles = [...$('user-form').querySelectorAll('input[name="role"]:checked')].map(b => Number(b.value));
  renderAdminLists(); renderGrantOptions(); renderRoles(selectedRoles);
  $('admin-sessions').replaceChildren(...adminData.sessions.map(s => sessionRow(s, true))); if (!adminData.sessions.length) $('admin-sessions').append(el('p', 'Нет активных сеансов.', 'empty'));
}
function renderAdminLists() {
  $('users-list').replaceChildren();
  for (const user of filteredSelection('users', adminData.users, u => u.login, u => u.login + ' ' + adminData.roles.filter(r => (u.role_ids || []).includes(r.id)).map(r => r.name).join(' '), u => u.enabled)) { const row = el('div', undefined, 'row'), text = el('div'); text.append(el('strong', user.login), el('small', (user.enabled ? 'Активен' : 'Отключён') + (user.admin ? ' · администратор' : ''))); const button = el('button', 'Изменить', 'secondary'); button.onclick = () => editUser(user); row.append(text, button); $('users-list').append(row); }
  const roles = filteredSelection('roles', adminData.roles, r => r.name, r => r.name, r => r.admin);
  $('roles-list').replaceChildren(...roles.map(r => el('p', r.name + (r.admin ? ' · администрирование' : ' · обычная роль'))));
  $('publications').replaceChildren();
  for (const a of filteredSelection('publications', adminData.applications, a => a.name, a => [a.name, a.id, a.start_url || '', a.template].join(' '), a => a.enabled)) { const row = el('div', undefined, 'row'); row.append(el('strong', a.name)); const b = el('button', a.enabled ? 'Снять с публикации' : 'Опубликовать', 'secondary'); b.onclick = () => act(async () => { await api('/admin/applications/' + a.id, 'PUT', { enabled: !a.enabled }); await refreshAdmin(); }, b); row.append(b); if (a.access) {const edit = el('button', 'Настройки', 'secondary'); edit.onclick = () => editPublication(a); row.append(edit);} else {row.append(el('small', 'Общее приложение · без правил публикации сайта'));} $('publications').append(row); }
}
for (const prefix of ['users', 'roles', 'publications']) {
  $(prefix + '-search').oninput = renderAdminLists;
  $(prefix + '-status').onchange = renderAdminLists;
  $(prefix + '-select').onchange = renderAdminLists;
}
$('grant-role-search').oninput = renderGrantOptions;
$('grant-app-search').oninput = renderGrantOptions;
$('login-form').addEventListener('submit', e => { e.preventDefault(); act(async () => { const form = e.currentTarget; identity = await api('/login', 'POST', { login: form.elements.login.value, password: form.elements.password.value }); form.elements.password.value = ''; await refreshIdentity(); }, e.submitter); });
$('logout').onclick = () => act(async () => { await api('/logout', 'POST'); signedOut(); }, $('logout'));
$('refresh').onclick = () => act(refreshIdentity, $('refresh'));
$('catalog-tab').onclick = () => act(async () => { switchTab(false); await refreshCatalog(); });
$('admin-tab').onclick = () => act(async () => { switchTab(true); await refreshAdmin(); });
$('reset-user').onclick = resetUser;
$('user-form').addEventListener('submit', e => {
  e.preventDefault(); const f = e.currentTarget;
  act(async () => {
    const selfLogout = Number(f.elements.id.value) === identity.user.id && (f.elements.password.value !== '' || !f.elements.enabled.checked);
    await api('/admin/users', 'PUT', { id: Number(f.elements.id.value), login: f.elements.login.value, password: f.elements.password.value, enabled: f.elements.enabled.checked, role_ids: [...f.querySelectorAll('input[name="role"]:checked')].map(b => Number(b.value)) });
    f.elements.password.value = '';
    if (selfLogout) { signedOut(); notice('Изменения сохранены. Войдите снова с актуальными учётными данными.'); return; }
    await refreshIdentity(); if (identity?.user.admin) { await refreshAdmin(); resetUser(); } notice('Пользователь сохранён.');
  }, e.submitter);
});
$('role-form').addEventListener('submit', e => { e.preventDefault(); const f = e.currentTarget; act(async () => { await api('/admin/roles', 'PUT', { id: 0, name: f.elements.name.value, admin: f.elements.admin.checked }); f.elements.name.value = ''; f.elements.admin.checked = false; await refreshAdmin(); notice('Роль создана.'); }, e.submitter); });
$('grant-form').addEventListener('submit', e => { e.preventDefault(); const f = e.currentTarget; act(async () => { await api('/admin/grants', 'PUT', { role_id: Number(f.elements.role_id.value), application_id: f.elements.application_id.value, can_launch: f.elements.can_launch.checked, can_control: f.elements.can_control.checked, can_print: f.elements.can_print.checked }); await refreshAdmin(); notice('Разрешения сохранены.'); }, e.submitter); });
$('grant-role').onchange = loadGrant; $('grant-app').onchange = loadGrant;
refreshIdentity().catch(e => { signedOut(); if (e.status !== 401) notice(e.message, true); });

function resetPublication() {
 const f=$('publication-form'); f.reset(); f.elements.id.readOnly=false;
 $('publication-form-title').textContent='Новая публикация сайта';
}
function editPublication(a) {
 const f=$('publication-form');
 for (const key of ['id','name','template','start_url']) f.elements[key].value=a[key];
 f.elements.enabled.checked=a.enabled; f.elements.id.readOnly=true;
 for (const key of ['domains','cidrs','ports']) f.elements[key].value=(a.access[key] || []).join(', ');
 $('publication-form-title').textContent='Настройки публикации'; f.scrollIntoView({behavior:'smooth',block:'center'});
}
$('reset-publication').onclick=resetPublication;
$('publication-form').addEventListener('submit',e=>{
 e.preventDefault(); const f=e.currentTarget;
 act(async()=>{
  const list=name=>f.elements[name].value.split(',').map(v=>v.trim()).filter(Boolean);
  const domains=list('domains').map(v=>v.toLowerCase());
  const cidrs=list('cidrs').map(v=>v.includes('/')?v:v+(v.includes(':')?'/128':'/32'));
  const ports=list('ports').map(Number);
  if (ports.some(v=>!Number.isInteger(v)||v<1||v>65535)) throw new Error('Порты должны быть целыми числами от 1 до 65535.');
  await api('/admin/publications/'+encodeURIComponent(f.elements.id.value),'PUT',{
   name:f.elements.name.value,template:f.elements.template.value,start_url:f.elements.start_url.value,
   enabled:f.elements.enabled.checked,access:{domains,cidrs,ports}
  });
  await refreshAdmin(); notice('Публикация сохранена. Назначьте доступ в разделе «Разрешения роли».');
 },e.submitter);
});

$('stream-catalog').onclick = event => {
  if (window.parent !== window) { event.preventDefault(); parent.postMessage({type:'mvp-catalog'}, location.origin); }
};
