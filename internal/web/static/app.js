// Browser client for Wintermute.
//
// Four views over one API: the chat that has always been here, plus the tasks,
// CRM and company modules that moved across from the RCSA application. The
// browser declares no client-side tools, so a turn from here either completes
// or reports an error — file operations belong to the desktop harness.
'use strict';

const $ = (id) => document.getElementById(id);
const state = { token: null, sessionId: null, sending: false, view: 'chat' };

function api(path, options = {}) {
  const headers = Object.assign(
    { 'Content-Type': 'application/json', Authorization: `Bearer ${state.token}` },
    options.headers || {},
  );
  return fetch(path, Object.assign({}, options, { headers })).then(async (res) => {
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `${res.status} ${res.statusText}`);
    return body;
  });
}

/* ---------- small helpers ---------- */

// Every value that reaches the DOM goes through el() or textContent. There is
// no innerHTML path taking server data: a client name is user input, and the
// CRM is full of them.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
    else if (v === true) node.setAttribute(k, '');
    else node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c);
  }
  return node;
}

let toastTimer = null;
function toast(message, isError = false) {
  const t = $('toast');
  t.textContent = message;
  t.className = isError ? 'toast err' : 'toast';
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, isError ? 6000 : 2500);
}

const money = (n) => (Number(n) || 0).toLocaleString(undefined, {
  minimumFractionDigits: 2, maximumFractionDigits: 2,
});
const hours = (n) => (Number(n) || 0).toLocaleString(undefined, { maximumFractionDigits: 2 });

/* ---------- auth gate ---------- */

$('gate-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const err = $('gate-error');
  err.hidden = true;
  state.token = $('token').value.trim();
  try {
    const me = await api('/api/v1/me');
    localStorage.setItem('wintermute_token', state.token);
    // Mirror the token into a cookie so plain navigations authenticate too.
    document.cookie = `wintermute_token=${state.token}; path=/; SameSite=Strict; max-age=31536000`;
    start(me);
  } catch (e2) {
    state.token = null;
    err.textContent = e2.message;
    err.hidden = false;
  }
});

$('logout').addEventListener('click', () => {
  localStorage.removeItem('wintermute_token');
  document.cookie = 'wintermute_token=; path=/; max-age=0';
  location.reload();
});

function start(me) {
  $('gate').hidden = true;
  $('app').hidden = false;
  $('model').textContent = me.default_backend ? `${me.default_backend}` : '';
  loadSessions().catch(showError);
}

/* ---------- view switching ---------- */

// Views load on first activation rather than at boot: opening the chat should
// not cost four extra round trips for panes nobody looked at.
const loaded = new Set();
const loaders = {
  tasks: () => Promise.all([loadLists(), renderTasks()]),
  crm: () => renderCRM(),
  company: () => loadCompany(),
};

function switchView(name) {
  state.view = name;
  for (const btn of document.querySelectorAll('.view-btn')) {
    btn.classList.toggle('active', btn.dataset.view === name);
  }
  for (const section of document.querySelectorAll('.view')) {
    section.classList.toggle('active', section.dataset.view === name);
  }
  closeSidebar();
  if (!loaded.has(name) && loaders[name]) {
    loaded.add(name);
    loaders[name]().catch((err) => { loaded.delete(name); showError(err); });
  }
}

for (const btn of document.querySelectorAll('.view-btn')) {
  btn.addEventListener('click', () => switchView(btn.dataset.view));
}
$('menu').addEventListener('click', () => {
  const bar = document.querySelector('.view.active .sidebar');
  if (bar) bar.classList.toggle('open');
});
function closeSidebar() {
  for (const bar of document.querySelectorAll('.sidebar')) bar.classList.remove('open');
}

/* ---------- editor dialog ----------
   One dialog for every record type. Each caller passes a field spec and gets
   back the edited values; validation stays on the server, which is the only
   place that can enforce it anyway. */

function openEditor(title, fields, onSave) {
  const form = $('editor-form');
  const box = $('editor-fields');
  const err = $('editor-error');
  err.hidden = true;
  $('editor-title').textContent = title;
  box.innerHTML = '';

  for (const f of fields) {
    let input;
    if (f.type === 'textarea') {
      input = el('textarea', { id: `f-${f.name}` });
      input.value = f.value ?? '';
    } else if (f.type === 'select') {
      input = el('select', { id: `f-${f.name}` },
        f.options.map((o) => {
          const opt = el('option', { value: o.value ?? o, text: o.label ?? o });
          if ((o.value ?? o) === f.value) opt.selected = true;
          return opt;
        }));
    } else if (f.type === 'checkbox') {
      input = el('input', { type: 'checkbox', id: `f-${f.name}` });
      input.checked = Boolean(f.value);
    } else {
      input = el('input', { type: f.type || 'text', id: `f-${f.name}` });
      if (f.step) input.step = f.step;
      input.value = f.value ?? '';
    }
    box.append(el('div', { class: 'field' }, el('label', { for: `f-${f.name}`, text: f.label }), input));
  }

  const dialog = $('editor');
  const submit = async (e) => {
    e.preventDefault();
    const values = {};
    for (const f of fields) {
      const input = $(`f-${f.name}`);
      if (f.type === 'checkbox') values[f.name] = input.checked;
      else if (f.type === 'number') values[f.name] = Number(input.value || 0);
      else values[f.name] = input.value;
    }
    $('editor-save').disabled = true;
    try {
      await onSave(values);
      cleanup();
      dialog.close();
    } catch (e2) {
      err.textContent = e2.message;
      err.hidden = false;
    } finally {
      $('editor-save').disabled = false;
    }
  };
  const cancel = () => { cleanup(); dialog.close(); };
  function cleanup() {
    form.removeEventListener('submit', submit);
    $('editor-cancel').removeEventListener('click', cancel);
  }

  form.addEventListener('submit', submit);
  $('editor-cancel').addEventListener('click', cancel);
  dialog.showModal();
}

async function confirmDelete(what, fn) {
  if (!window.confirm(`Delete ${what}? This cannot be undone.`)) return;
  try {
    await fn();
    toast('Deleted.');
  } catch (err) {
    showError(err);
  }
}

/* ================= CHAT ================= */

async function loadSessions() {
  const { sessions } = await api('/api/v1/sessions');
  const list = $('sessions');
  list.innerHTML = '';
  for (const s of sessions) {
    const li = el('li', {
      text: s.title || 'Untitled',
      title: s.title || 'Untitled',
      class: s.id === state.sessionId ? 'active' : '',
      onclick: () => openSession(s.id).catch(showError),
    });
    list.append(li);
  }
}

async function newSession() {
  const sess = await api('/api/v1/sessions', { method: 'POST', body: JSON.stringify({ title: '' }) });
  state.sessionId = sess.id;
  $('messages').replaceChildren(
    el('div', { class: 'empty muted', text: 'Ask about your media library, your tasks, or anything else.' }));
  await loadSessions();
  return sess.id;
}

async function openSession(id) {
  state.sessionId = id;
  const { messages } = await api(`/api/v1/sessions/${id}/messages`);
  $('messages').innerHTML = '';
  for (const m of messages || []) appendMessage(m);
  await loadSessions();
  closeSidebar();
}

$('new-session').addEventListener('click', () => newSession().catch(showError));

function appendMessage(m) {
  const box = $('messages');
  const empty = box.querySelector('.empty');
  if (empty) empty.remove();

  let text = m.content || '';
  if (m.role === 'assistant' && m.tool_calls && m.tool_calls.length) {
    const names = m.tool_calls.map((c) => c.name).join(', ');
    text = text ? `${text}\n\n→ requested: ${names}` : `→ requested: ${names}`;
  }
  if (!text) return;

  box.append(el('div', { class: `msg ${m.role}${m.is_error ? ' error' : ''}` },
    el('div', { class: 'role', text: m.role }),
    el('div', { class: 'bubble', text })));
  box.scrollTop = box.scrollHeight;
}

function showError(err) {
  if (state.view === 'chat') appendMessage({ role: 'tool', content: err.message, is_error: true });
  else toast(err.message, true);
}

const input = $('input');
input.addEventListener('input', () => {
  input.style.height = 'auto';
  input.style.height = `${Math.min(input.scrollHeight, 200)}px`;
});
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    $('composer').requestSubmit();
  }
});

$('composer').addEventListener('submit', async (e) => {
  e.preventDefault();
  const text = input.value.trim();
  if (!text || state.sending) return;

  state.sending = true;
  $('send').disabled = true;
  input.value = '';
  input.style.height = 'auto';
  appendMessage({ role: 'user', content: text });

  try {
    if (!state.sessionId) await newSession();
    // client_tools is intentionally empty: a browser executes nothing locally.
    const turn = await api(`/api/v1/sessions/${state.sessionId}/messages`, {
      method: 'POST',
      body: JSON.stringify({ text, client_tools: [] }),
    });
    if (turn.reply) appendMessage({ role: 'assistant', content: turn.reply });
    if (turn.status === 'awaiting_client') {
      appendMessage({
        role: 'tool',
        content: 'The assistant asked for a local action. Run the desktop client to approve it.',
      });
    }
    await loadSessions();
    // A turn may have created or changed a list through the task tools, so the
    // Tasks view is stale from here on. Marking it unloaded is cheaper than
    // refetching a pane nobody is looking at.
    loaded.delete('tasks');
  } catch (err) {
    showError(err);
  } finally {
    state.sending = false;
    $('send').disabled = false;
    input.focus();
  }
});

/* ================= TASKS ================= */

const tasks = { scope: 'agenda', listId: 0, lists: [] };

async function loadLists() {
  const archived = $('show-archived').checked ? '?archived=1' : '';
  const { lists } = await api(`/api/v1/todo/lists${archived}`);
  tasks.lists = lists || [];

  const box = $('lists');
  box.innerHTML = '';
  for (const l of tasks.lists) {
    box.append(el('li', {
      class: tasks.scope === 'list' && tasks.listId === l.id ? 'active' : '',
      title: l.description || l.title,
      onclick: () => { tasks.scope = 'list'; tasks.listId = l.id; renderTasks().catch(showError); },
    },
    el('span', { text: `${l.title}${l.archived ? ' (archived)' : ''}` }),
    el('span', { class: 'muted', text: `  ${l.done_count}/${l.task_count}` })));
  }
  for (const li of document.querySelectorAll('#task-views li')) {
    li.classList.toggle('active', tasks.scope === li.dataset.scope);
    li.onclick = () => { tasks.scope = li.dataset.scope; tasks.listId = 0; renderTasks().catch(showError); };
  }
}

$('show-archived').addEventListener('change', () => loadLists().catch(showError));
$('show-done').addEventListener('change', () => renderTasks().catch(showError));

$('new-list').addEventListener('click', () => {
  openEditor('New list', [
    { name: 'title', label: 'Title' },
    { name: 'description', label: 'Description', type: 'textarea' },
  ], async (v) => {
    await api('/api/v1/todo/lists', { method: 'POST', body: JSON.stringify(v) });
    await loadLists();
    await renderTasks();
    toast('List created.');
  });
});

$('edit-list').addEventListener('click', () => {
  const list = tasks.lists.find((l) => l.id === tasks.listId);
  if (!list) return;
  openEditor('Edit list', [
    { name: 'title', label: 'Title', value: list.title },
    { name: 'description', label: 'Description', type: 'textarea', value: list.description },
    { name: 'archived', label: 'Archived', type: 'checkbox', value: list.archived },
  ], async (v) => {
    await api(`/api/v1/todo/lists/${list.id}`, { method: 'PUT', body: JSON.stringify(v) });
    await loadLists();
    await renderTasks();
    toast('List saved.');
  });
});

$('delete-list').addEventListener('click', () => {
  const list = tasks.lists.find((l) => l.id === tasks.listId);
  if (!list) return;
  confirmDelete(`the list "${list.title}" and its tasks`, async () => {
    await api(`/api/v1/todo/lists/${list.id}`, { method: 'DELETE' });
    tasks.scope = 'agenda';
    tasks.listId = 0;
    await loadLists();
    await renderTasks();
  });
});

$('quick-add').addEventListener('submit', async (e) => {
  e.preventDefault();
  const title = $('quick-title').value.trim();
  if (!title || !tasks.listId) return;
  try {
    await api('/api/v1/todo/tasks', {
      method: 'POST',
      body: JSON.stringify({
        list_id: tasks.listId,
        title,
        due_date: $('quick-due').value,
        priority: $('quick-priority').value,
      }),
    });
    $('quick-title').value = '';
    $('quick-due').value = '';
    await loadLists();
    await renderTasks();
  } catch (err) {
    showError(err);
  }
});

async function renderTasks() {
  const body = $('tasks-body');
  const showDone = $('show-done').checked;
  $('quick-add').hidden = tasks.scope !== 'list';
  $('edit-list').hidden = tasks.scope !== 'list';
  $('delete-list').hidden = tasks.scope !== 'list';

  if (tasks.scope === 'agenda') {
    $('tasks-title').textContent = 'Agenda';
    const agenda = await api('/api/v1/todo/agenda');
    body.innerHTML = '';
    const groups = [
      ['Overdue', agenda.overdue],
      ['Due today', agenda.due_today],
      [`Next ${14} days`, agenda.upcoming],
      ['No due date', agenda.no_date],
    ];
    let any = false;
    for (const [label, items] of groups) {
      if (!items || !items.length) continue;
      any = true;
      body.append(el('div', { class: 'group-head', text: `${label} · ${items.length}` }));
      for (const t of items) body.append(taskRow(t, agenda.today));
    }
    if (!any) body.append(el('div', { class: 'empty muted', text: 'Nothing outstanding.' }));
    await loadLists();
    return;
  }

  const params = new URLSearchParams();
  if (tasks.scope === 'list') params.set('list_id', String(tasks.listId));
  if (showDone) params.set('include_done', '1');
  const { tasks: items, today } = await api(`/api/v1/todo/tasks?${params}`);

  const list = tasks.lists.find((l) => l.id === tasks.listId);
  $('tasks-title').textContent = tasks.scope === 'list' ? (list ? list.title : 'List') : 'All open tasks';

  body.innerHTML = '';
  if (!items.length) {
    body.append(el('div', { class: 'empty muted', text: 'No tasks here.' }));
    return;
  }
  for (const t of items) body.append(taskRow(t, today, tasks.scope !== 'list'));
  await loadLists();
}

function taskRow(t, today, showList = false) {
  const done = t.status === 'done';
  const overdue = !done && t.due_date && t.due_date < today;

  const check = el('input', { type: 'checkbox' });
  check.checked = done;
  check.addEventListener('change', async () => {
    check.disabled = true;
    try {
      await api(`/api/v1/todo/tasks/${t.id}/status`, {
        method: 'POST',
        body: JSON.stringify({ status: check.checked ? 'done' : 'todo' }),
      });
      await renderTasks();
    } catch (err) {
      check.checked = done;
      showError(err);
    } finally {
      check.disabled = false;
    }
  });

  const meta = [];
  if (showList && t.list_title) meta.push(t.list_title);
  if (t.due_date) meta.push(overdue ? `overdue ${t.due_date}` : `due ${t.due_date}`);
  if (t.status === 'doing') meta.push('in progress');

  return el('div', { class: `task${done ? ' done' : ''}` },
    check,
    el('div', { class: 'body' },
      el('div', { class: 't', text: t.title }),
      t.notes ? el('div', { class: 'meta', text: t.notes }) : null,
      meta.length ? el('div', { class: 'meta' },
        el('span', { class: overdue ? 'overdue' : '', text: meta.join(' · ') })) : null),
    t.priority === 'high' ? el('span', { class: 'pill high', text: 'high' }) : null,
    el('div', { class: 'row-actions' },
      el('button', { class: 'ghost-btn', text: 'Edit', onclick: () => editTask(t) }),
      el('button', {
        class: 'ghost-btn danger', text: '✕',
        onclick: () => confirmDelete(`the task "${t.title}"`, async () => {
          await api(`/api/v1/todo/tasks/${t.id}`, { method: 'DELETE' });
          await renderTasks();
        }),
      })));
}

function editTask(t) {
  openEditor('Edit task', [
    { name: 'title', label: 'Title', value: t.title },
    { name: 'notes', label: 'Notes', type: 'textarea', value: t.notes },
    { name: 'status', label: 'Status', type: 'select', value: t.status,
      options: [{ value: 'todo', label: 'To do' }, { value: 'doing', label: 'Doing' }, { value: 'done', label: 'Done' }] },
    { name: 'priority', label: 'Priority', type: 'select', value: t.priority,
      options: [{ value: 'low', label: 'Low' }, { value: 'normal', label: 'Normal' }, { value: 'high', label: 'High' }] },
    { name: 'due_date', label: 'Due date', type: 'date', value: t.due_date },
  ], async (v) => {
    await api(`/api/v1/todo/tasks/${t.id}`, { method: 'PUT', body: JSON.stringify(v) });
    await renderTasks();
    toast('Task saved.');
  });
}

/* ================= CRM ================= */

const crm = { tab: 'dashboard', clients: [], engagements: [] };

for (const li of document.querySelectorAll('#crm-nav li')) {
  li.addEventListener('click', () => {
    crm.tab = li.dataset.tab;
    for (const other of document.querySelectorAll('#crm-nav li')) {
      other.classList.toggle('active', other === li);
    }
    renderCRM().catch(showError);
    closeSidebar();
  });
}

$('crm-new').addEventListener('click', () => {
  if (crm.tab === 'clients') editClient(null);
  else if (crm.tab === 'engagements') editEngagement(null);
  else if (crm.tab === 'time') editTimeEntry(null);
});

async function renderCRM() {
  const body = $('crm-body');
  const titles = {
    dashboard: 'Dashboard', clients: 'Clients', engagements: 'Engagements',
    time: 'Time entries', billing: 'Billing',
  };
  $('crm-title').textContent = titles[crm.tab];
  $('crm-new').hidden = !['clients', 'engagements', 'time'].includes(crm.tab);
  body.innerHTML = '';

  if (crm.tab === 'dashboard') return renderDashboard(body);
  if (crm.tab === 'clients') return renderClients(body);
  if (crm.tab === 'engagements') return renderEngagements(body);
  if (crm.tab === 'time') return renderTimeEntries(body);
  return renderBilling(body);
}

async function renderDashboard(body) {
  const d = await api('/api/v1/crm/dashboard');
  body.append(el('div', { class: 'stats' },
    stat(d.total_clients, 'Clients'),
    stat(d.active_clients, 'Active clients'),
    stat(d.active_engagements, 'Active engagements'),
    stat(hours(d.hours_last_30), 'Hours (30d)'),
    stat(money(d.unbilled_amount), 'Unbilled'),
    stat(money(d.invoiced_amount), 'Invoiced')));

  if (d.top_clients && d.top_clients.length) {
    body.append(el('div', { class: 'group-head', text: 'Top clients' }));
    body.append(table(['Client', 'Hours', 'Value'],
      d.top_clients.map((c) => [c.client_name, hours(c.hours), money(c.amount)]), [false, true, true]));
  }
  if (d.recent_entries && d.recent_entries.length) {
    body.append(el('div', { class: 'group-head', text: 'Recent time' }));
    body.append(table(['Date', 'Client', 'Engagement', 'Hours', 'Value'],
      d.recent_entries.map((e) => [e.entry_date, e.client_name, e.engagement_name, hours(e.hours), money(e.amount)]),
      [false, false, false, true, true]));
  }
}

function stat(n, k) {
  return el('div', { class: 'stat' }, el('div', { class: 'n', text: String(n) }), el('div', { class: 'k', text: k }));
}

function table(headers, rows, numeric = []) {
  return el('div', { class: 'table-wrap' },
    el('table', {},
      el('thead', {}, el('tr', {}, headers.map((h, i) =>
        el('th', { class: numeric[i] ? 'num' : '', text: h })))),
      el('tbody', {}, rows.map((cells) => el('tr', {}, cells.map((c, i) =>
        c && c.nodeType ? el('td', {}, c) : el('td', { class: numeric[i] ? 'num' : '', text: String(c ?? '') })))))));
}

async function renderClients(body) {
  const { clients } = await api('/api/v1/crm/clients');
  crm.clients = clients || [];
  if (!crm.clients.length) {
    body.append(el('div', { class: 'empty muted', text: 'No clients yet.' }));
    return;
  }
  body.append(table(
    ['Name', 'Contact', 'Status', 'Rate', 'Engagements', 'Hours', 'Unbilled', ''],
    crm.clients.map((c) => [
      c.name, c.contact_name || c.email, c.status, money(c.hourly_rate),
      String(c.engagement_count), hours(c.logged_hours), money(c.unbilled_amount),
      el('div', { class: 'row-actions' },
        el('button', { class: 'ghost-btn', text: 'Edit', onclick: () => editClient(c) }),
        el('button', {
          class: 'ghost-btn danger', text: '✕',
          onclick: () => confirmDelete(`client "${c.name}" and all its engagements`, async () => {
            await api(`/api/v1/crm/clients/${c.id}`, { method: 'DELETE' });
            await renderCRM();
          }),
        })),
    ]),
    [false, false, false, true, true, true, true, false]));
}

function editClient(c) {
  const fields = [
    { name: 'name', label: 'Name', value: c?.name },
    { name: 'contact_name', label: 'Contact', value: c?.contact_name },
    { name: 'email', label: 'Email', value: c?.email },
    { name: 'phone', label: 'Phone', value: c?.phone },
    { name: 'status', label: 'Status', type: 'select', value: c?.status || 'Active',
      options: ['Prospect', 'Active', 'Inactive'] },
    { name: 'hourly_rate', label: 'Hourly rate', type: 'number', step: '0.01', value: c?.hourly_rate ?? 0 },
    { name: 'notes', label: 'Notes', type: 'textarea', value: c?.notes },
  ];
  openEditor(c ? 'Edit client' : 'New client', fields, async (v) => {
    if (c) await api(`/api/v1/crm/clients/${c.id}`, { method: 'PUT', body: JSON.stringify(v) });
    else await api('/api/v1/crm/clients', { method: 'POST', body: JSON.stringify(v) });
    await renderCRM();
    toast('Client saved.');
  });
}

async function ensureClients() {
  if (!crm.clients.length) {
    const { clients } = await api('/api/v1/crm/clients');
    crm.clients = clients || [];
  }
  return crm.clients;
}

async function renderEngagements(body) {
  const [{ engagements }] = await Promise.all([api('/api/v1/crm/engagements'), ensureClients()]);
  crm.engagements = engagements || [];
  if (!crm.engagements.length) {
    body.append(el('div', { class: 'empty muted', text: 'No engagements yet.' }));
    return;
  }
  body.append(table(
    ['Engagement', 'Client', 'Status', 'Rate', 'Budget', 'Logged', ''],
    crm.engagements.map((e) => [
      e.name, e.client_name, e.status, money(e.hourly_rate), hours(e.budget_hours), hours(e.logged_hours),
      el('div', { class: 'row-actions' },
        el('button', { class: 'ghost-btn', text: 'Edit', onclick: () => editEngagement(e) }),
        el('button', {
          class: 'ghost-btn', text: 'Invoice',
          title: 'Mark all billable unbilled time on this engagement as invoiced',
          onclick: async () => {
            try {
              const r = await api(`/api/v1/crm/engagements/${e.id}/invoice`, { method: 'POST' });
              toast(`${r.invoiced} entr${r.invoiced === 1 ? 'y' : 'ies'} marked invoiced.`);
              await renderCRM();
            } catch (err) { showError(err); }
          },
        }),
        el('button', {
          class: 'ghost-btn danger', text: '✕',
          onclick: () => confirmDelete(`engagement "${e.name}" and its time entries`, async () => {
            await api(`/api/v1/crm/engagements/${e.id}`, { method: 'DELETE' });
            await renderCRM();
          }),
        })),
    ]),
    [false, false, false, true, true, true, false]));
}

async function editEngagement(e) {
  const clients = await ensureClients();
  if (!clients.length) { toast('Add a client first.', true); return; }
  openEditor(e ? 'Edit engagement' : 'New engagement', [
    { name: 'client_id', label: 'Client', type: 'select', value: e?.client_id ?? clients[0].id,
      options: clients.map((c) => ({ value: c.id, label: c.name })) },
    { name: 'name', label: 'Name', value: e?.name },
    { name: 'status', label: 'Status', type: 'select', value: e?.status || 'Active',
      options: ['Proposed', 'Active', 'On Hold', 'Completed'] },
    { name: 'hourly_rate', label: 'Hourly rate (0 = use client rate)', type: 'number', step: '0.01', value: e?.hourly_rate ?? 0 },
    { name: 'budget_hours', label: 'Budget hours', type: 'number', step: '0.25', value: e?.budget_hours ?? 0 },
    { name: 'start_date', label: 'Start date', type: 'date', value: e?.start_date },
    { name: 'end_date', label: 'End date', type: 'date', value: e?.end_date },
    { name: 'notes', label: 'Notes', type: 'textarea', value: e?.notes },
  ], async (v) => {
    v.client_id = Number(v.client_id);
    if (e) await api(`/api/v1/crm/engagements/${e.id}`, { method: 'PUT', body: JSON.stringify(v) });
    else await api('/api/v1/crm/engagements', { method: 'POST', body: JSON.stringify(v) });
    await renderCRM();
    toast('Engagement saved.');
  });
}

async function ensureEngagements() {
  if (!crm.engagements.length) {
    const { engagements } = await api('/api/v1/crm/engagements');
    crm.engagements = engagements || [];
  }
  return crm.engagements;
}

async function renderTimeEntries(body) {
  const [{ entries }] = await Promise.all([api('/api/v1/crm/time'), ensureEngagements()]);
  if (!entries || !entries.length) {
    body.append(el('div', { class: 'empty muted', text: 'No time logged yet.' }));
    return;
  }
  body.append(table(
    ['Date', 'Client', 'Engagement', 'Description', 'Hours', 'Rate', 'Value', 'Invoiced', ''],
    entries.map((t) => {
      const invoiced = el('input', { type: 'checkbox' });
      invoiced.checked = t.invoiced;
      invoiced.addEventListener('change', async () => {
        try {
          await api(`/api/v1/crm/time/${t.id}/invoiced`, {
            method: 'POST', body: JSON.stringify({ invoiced: invoiced.checked }),
          });
        } catch (err) { invoiced.checked = t.invoiced; showError(err); }
      });
      return [
        t.entry_date, t.client_name, t.engagement_name, t.description,
        hours(t.hours), money(t.rate), t.billable ? money(t.amount) : '—',
        invoiced,
        el('div', { class: 'row-actions' },
          el('button', { class: 'ghost-btn', text: 'Edit', onclick: () => editTimeEntry(t) }),
          el('button', {
            class: 'ghost-btn danger', text: '✕',
            onclick: () => confirmDelete('this time entry', async () => {
              await api(`/api/v1/crm/time/${t.id}`, { method: 'DELETE' });
              await renderCRM();
            }),
          })),
      ];
    }),
    [false, false, false, false, true, true, true, false, false]));
}

async function editTimeEntry(t) {
  const engagements = await ensureEngagements();
  if (!engagements.length) { toast('Add an engagement first.', true); return; }
  openEditor(t ? 'Edit time entry' : 'Log time', [
    { name: 'engagement_id', label: 'Engagement', type: 'select', value: t?.engagement_id ?? engagements[0].id,
      options: engagements.map((e) => ({ value: e.id, label: `${e.client_name} — ${e.name}` })) },
    { name: 'entry_date', label: 'Date', type: 'date', value: t?.entry_date || new Date().toISOString().slice(0, 10) },
    { name: 'hours', label: 'Hours', type: 'number', step: '0.25', value: t?.hours ?? 1 },
    { name: 'description', label: 'Description', type: 'textarea', value: t?.description },
    { name: 'billable', label: 'Billable', type: 'checkbox', value: t ? t.billable : true },
    { name: 'rate', label: 'Rate (0 = use engagement rate)', type: 'number', step: '0.01', value: t?.rate ?? 0 },
  ], async (v) => {
    v.engagement_id = Number(v.engagement_id);
    if (t) await api(`/api/v1/crm/time/${t.id}`, { method: 'PUT', body: JSON.stringify(v) });
    else await api('/api/v1/crm/time', { method: 'POST', body: JSON.stringify(v) });
    await renderCRM();
    toast('Time entry saved.');
  });
}

async function renderBilling(body) {
  const { lines } = await api('/api/v1/crm/billing');
  if (!lines || !lines.length) {
    body.append(el('div', { class: 'empty muted', text: 'Nothing to bill.' }));
    return;
  }
  const totals = lines.reduce((acc, l) => ({
    unbilled: acc.unbilled + (l.unbilled_amount || 0),
    invoiced: acc.invoiced + (l.invoiced_amount || 0),
  }), { unbilled: 0, invoiced: 0 });

  body.append(el('div', { class: 'stats' },
    stat(money(totals.unbilled), 'Unbilled'),
    stat(money(totals.invoiced), 'Invoiced')));

  body.append(table(
    ['Client', 'Engagement', 'Billable hrs', 'Unbilled hrs', 'Unbilled', 'Invoiced'],
    lines.map((l) => [
      l.client_name, l.engagement_name, hours(l.billable_hours),
      hours(l.unbilled_hours), money(l.unbilled_amount), money(l.invoiced_amount),
    ]),
    [false, false, true, true, true, true]));
}

/* ================= COMPANY ================= */

const companyFields = [
  ['legal_name', 'Legal name'], ['trading_name', 'Trading name'], ['tagline', 'Tagline'],
  ['description', 'Description', 'textarea'], ['founded', 'Founded (YYYY)'],
  ['jurisdiction', 'Jurisdiction'], ['registration_number', 'Registration number'],
  ['tax_id', 'Tax ID'], ['email', 'Email', 'email'], ['phone', 'Phone'], ['website', 'Website'],
  ['address_line1', 'Address line 1'], ['address_line2', 'Address line 2'], ['city', 'City'],
  ['region', 'Region'], ['postal_code', 'Postal code'], ['country', 'Country'],
  ['contact_name', 'Contact name'], ['contact_role', 'Contact role'], ['contact_email', 'Contact email', 'email'],
];

async function loadCompany() {
  const { profile, complete } = await api('/api/v1/company');
  const body = $('company-body');
  body.innerHTML = '';

  $('company-state').textContent = complete
    ? `Complete · updated ${profile.updated_at || 'never'}`
    : 'Incomplete — a letterhead needs name, address, city, country and email';

  const form = el('form', { class: 'fields', onsubmit: async (e) => {
    e.preventDefault();
    const payload = {};
    for (const [name] of companyFields) payload[name] = $(`c-${name}`).value;
    try {
      await api('/api/v1/company', { method: 'PUT', body: JSON.stringify(payload) });
      toast('Company profile saved.');
      await loadCompany();
    } catch (err) { showError(err); }
  } });

  for (const [name, label, type] of companyFields) {
    const control = type === 'textarea'
      ? el('textarea', { id: `c-${name}` })
      : el('input', { id: `c-${name}`, type: type || 'text' });
    control.value = profile[name] || '';
    form.append(el('div', { class: 'field' }, el('label', { for: `c-${name}`, text: label }), control));
  }
  form.append(el('div', { class: 'dialog-actions' }, el('button', { type: 'submit', text: 'Save profile' })));
  body.append(form);
}

$('company-clear').addEventListener('click', () => {
  confirmDelete('the company profile', async () => {
    await api('/api/v1/company', { method: 'DELETE' });
    await loadCompany();
  });
});

/* ---------- boot ---------- */

const saved = localStorage.getItem('wintermute_token');
if (saved) {
  state.token = saved;
  api('/api/v1/me').then(start).catch(() => {
    state.token = null;
    localStorage.removeItem('wintermute_token');
  });
}
