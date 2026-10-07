'use strict';
const $ = id => document.getElementById(id);
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
  const response = await fetch('/api/v1' + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: 'same-origin' });
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
function signedOut() { clearTimeout(authExpiry); window.mvpSession?.close(); identity = null; $('login-panel').hidden = false; $('workspace').hidden = true; $('session-view').hidden = true; $('account').hidden = true; }
async function refreshIdentity() {
  identity = await api('/me'); scheduleExpiry();
  $('login-panel').hidden = true; $('account').hidden = false; $('login-name').textContent = identity.user.login;
  $('admin-tab').hidden = !identity.user.admin;
  if (!identity.user.admin) switchTab(false);
  const match = location.pathname.match(/^\/sessions\/([a-f0-9]+)$/);
  if (match) {
    $('workspace').hidden = true; $('session-view').hidden = false;
    const s = await api('/sessions/' + match[1]); $('session-title').textContent = appName(s.application_id); $('session-state').textContent = 'Состояние: ' + stateName(s.state);
    if (!['closed', 'failed', 'stopping'].includes(s.state)) window.mvpSession.start(s.id, identity.csrf_token);
    $('stop-app').onclick = () => act(async () => { await api('/sessions/' + s.id, 'DELETE'); window.mvpSession.close(); location.assign('/'); }, $('stop-app'));
  } else { $('workspace').hidden = false; $('session-view').hidden = true; await refreshCatalog(); }
}
function appName(id) { return ({falkon:'Falkon',firefox:'Firefox',terminal:'Терминал'})[id] || id; }
function stateName(state) { return ({ preparing: 'Подготовка', ready: 'Готово', connecting: 'Подключение', active: 'Работает', reconnecting: 'Восстановление связи', stopping: 'Завершение', closed: 'Завершён', failed: 'Ошибка запуска' })[state] || state; }
function sessionRow(s, admin = false) {
  const row = el('div', undefined, 'row'), title = el('div'); title.append(el('strong', appName(s.application_id)));
  title.append(el('small', stateName(s.state) + (admin ? ' · пользователь #' + s.user_id : ''))); row.append(title);
  const actions = el('div', undefined, 'actions');
  if (!admin) { const a = el('a', 'Открыть', 'button secondary'); a.href = '/sessions/' + encodeURIComponent(s.id); a.target = '_blank'; a.rel = 'noopener'; actions.append(a); }
  const stop = el('button', 'Завершить', 'secondary'); stop.addEventListener('click', () => act(async () => { await api((admin ? '/admin' : '') + '/sessions/' + s.id, 'DELETE'); await (admin ? refreshAdmin() : refreshCatalog()); }, stop)); actions.append(stop); row.append(actions); return row;
}
async function refreshCatalog() {
  const [apps, sessions] = await Promise.all([api('/applications'), api('/sessions')]);
  $('runtime-note').hidden = identity.runtime_available; $('applications').replaceChildren();
  if (!apps.length) $('applications').append(el('p', 'Пока нет доступных приложений. Обратитесь к администратору.', 'empty'));
  for (const app of apps) {
    const card = el('article', undefined, 'app-card'); card.dataset.app = app.id; card.append(el('div', app.id !== 'terminal' ? '◎' : '>_', 'app-icon'), el('h2', app.name), el('p', app.id !== 'terminal' ? 'Работа с внутренними веб-сервисами в удалённом браузере.' : 'Командная строка в отдельном серверном окружении.'));
    card.append(el('span', [app.can_control ? 'Управление' : 'Просмотр', ...(app.can_print ? ['Печать'] : [])].join(' · '), 'pill'));
    const start = el('button', 'Открыть приложение ↗');
    start.addEventListener('click', () => act(async () => {
      // Open synchronously on the click so popup blockers do not discard the tab.
      const tab = identity.runtime_available ? window.open('about:blank', '_blank') : null;
      if (tab) tab.opener = null;
      try { const s = await api('/sessions', 'POST', { application_id: app.id }); if (tab) tab.location.href = '/sessions/' + s.id; else location.assign('/sessions/' + s.id); await refreshCatalog(); }
      catch (e) { if (tab) tab.close(); throw e; }
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
}
function editUser(user) {
  const f = $('user-form'); f.elements.id.value = user.id; f.elements.login.value = user.login; f.elements.password.value = ''; f.elements.enabled.checked = user.enabled; renderRoles(user.role_ids); $('user-form-title').textContent = 'Редактирование пользователя'; f.scrollIntoView({ behavior: 'smooth', block: 'center' });
}
function resetUser() { $('user-form').reset(); $('user-form').elements.id.value = '0'; $('user-form-title').textContent = 'Новый пользователь'; renderRoles([]); }
function loadGrant() {
  const f = $('grant-form'); const grant = adminData.grants.find(g => g.role_id === Number(f.elements.role_id.value) && g.application_id === f.elements.application_id.value);
  for (const k of ['can_launch', 'can_control', 'can_print']) f.elements[k].checked = Boolean(grant?.[k]);
}
async function refreshAdmin() {
  adminData = await api('/admin');
  const selectedRoles = [...$('user-form').querySelectorAll('input[name="role"]:checked')].map(b => Number(b.value));
  const selectedGrantRole = $('grant-role').value, selectedGrantApp = $('grant-app').value;
  $('users-list').replaceChildren();
  for (const user of adminData.users) { const row = el('div', undefined, 'row'), text = el('div'); text.append(el('strong', user.login), el('small', (user.enabled ? 'Активен' : 'Отключён') + (user.admin ? ' · администратор' : ''))); const button = el('button', 'Изменить', 'secondary'); button.onclick = () => editUser(user); row.append(text, button); $('users-list').append(row); }
  $('roles-list').replaceChildren(...adminData.roles.map(r => el('p', r.name + (r.admin ? ' · администрирование' : ''))));
  $('grant-role').replaceChildren(...adminData.roles.map(r => option(r.id, r.name))); $('grant-app').replaceChildren(...adminData.applications.map(a => option(a.id, a.name)));
  if (adminData.roles.some(r => String(r.id) === selectedGrantRole)) $('grant-role').value = selectedGrantRole;
  if (adminData.applications.some(a => a.id === selectedGrantApp)) $('grant-app').value = selectedGrantApp;
  renderRoles(selectedRoles); loadGrant();
  $('publications').replaceChildren();
  for (const a of adminData.applications) { const row = el('div', undefined, 'row'); row.append(el('strong', a.name)); const b = el('button', a.enabled ? 'Снять с публикации' : 'Опубликовать', 'secondary'); b.onclick = () => act(async () => { await api('/admin/applications/' + a.id, 'PUT', { enabled: !a.enabled }); await refreshAdmin(); }, b); row.append(b); $('publications').append(row); }
  $('admin-sessions').replaceChildren(...adminData.sessions.map(s => sessionRow(s, true))); if (!adminData.sessions.length) $('admin-sessions').append(el('p', 'Нет активных сеансов.', 'empty'));
}
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
$('role-form').addEventListener('submit', e => { e.preventDefault(); const f = e.currentTarget; act(async () => { await api('/admin/roles', 'PUT', { id: 0, name: f.elements.name.value, admin: f.elements.admin.checked }); f.reset(); await refreshAdmin(); notice('Роль создана.'); }, e.submitter); });
$('grant-form').addEventListener('submit', e => { e.preventDefault(); const f = e.currentTarget; act(async () => { await api('/admin/grants', 'PUT', { role_id: Number(f.elements.role_id.value), application_id: f.elements.application_id.value, can_launch: f.elements.can_launch.checked, can_control: f.elements.can_control.checked, can_print: f.elements.can_print.checked }); await refreshAdmin(); notice('Разрешения сохранены.'); }, e.submitter); });
$('grant-role').onchange = loadGrant; $('grant-app').onchange = loadGrant;
refreshIdentity().catch(e => { signedOut(); if (e.status !== 401) notice(e.message, true); });
