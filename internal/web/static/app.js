// Browser client for Wintermute.
//
// Four views over one API: the chat that has always been here, plus the tasks,
// CRM and company modules that moved across from the RCSA application. The
// browser declares no client-side tools, so a turn from here either completes
// or reports an error — file operations belong to the desktop harness.
'use strict';

const $ = (id) => document.getElementById(id);
const state = {
  token: null, sessionId: null, sending: false, view: 'chat',
  // The agent a new chat is opened against, and the one being edited in the
  // Agents view. Null means the unscoped assistant, which is what every
  // session was before agents existed.
  chatAgent: null, agents: [], agentAvailable: {}, selectedAgent: null,
};

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
    // A rejected token is usually a real token that was issued into a different
    // database from the one this server reads: `wintermuted -add-client`
    // defaults to a relative wintermute.db, which is not the file a systemd
    // install uses. Saying only "invalid" sends people to re-read the token
    // they pasted, which is the one thing that is not wrong.
    err.textContent = /invalid token/i.test(e2.message)
      ? `${e2.message} — check it was issued into the database this server reads (sudo scripts/clients.sh list).`
      : e2.message;
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
  // The agent list is fetched at boot rather than on first view, because the
  // chat needs it to say which agent a session is talking to — a question gets
  // a different answer depending on which library is behind it.
  api('/api/v1/agents').then((data) => {
    state.agents = data.agents || [];
    state.agentAvailable = data.available || {};
    renderChatAgentPicker();
  }).catch(() => { /* agents are optional; the chat works without them */ });
}

/* ---------- view switching ---------- */

// Views load on first activation rather than at boot: opening the chat should
// not cost four extra round trips for panes nobody looked at.
const loaded = new Set();
const loaders = {
  agents: () => loadAgents(),
  tasks: () => Promise.all([loadLists(), renderTasks()]),
  crm: () => renderCRM(),
  accounting: () => renderAccounting(),
  company: () => loadCompany(),
  admin: () => renderAdmin(),
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

let sessionIndex = [];

function agentOfSession(id) {
  const found = sessionIndex.find((s) => s.id === id);
  return found ? (found.agent_id || null) : null;
}

async function loadSessions() {
  const { sessions } = await api('/api/v1/sessions');
  sessionIndex = sessions || [];
  const list = $('sessions');
  list.innerHTML = '';
  for (const s of sessions) {
    const label = s.agent_id ? `${s.title || 'Untitled'} · ${s.agent_id}` : (s.title || 'Untitled');
    const li = el('li', {
      text: label,
      title: label,
      class: s.id === state.sessionId ? 'active' : '',
      onclick: () => openSession(s.id).catch(showError),
    });
    list.append(li);
  }
}

async function newSession() {
  const sess = await api('/api/v1/sessions', {
    method: 'POST',
    body: JSON.stringify({ title: '', agent: state.chatAgent || '' }),
  });
  state.sessionId = sess.id;
  state.chatAgent = sess.agent_id || null;
  $('messages').replaceChildren(el('div', { class: 'empty muted', text: emptyChatHint() }));
  await loadSessions();
  return sess.id;
}

// What the composer says it is talking to. Naming the agent matters: the same
// question gets a different answer depending on which library is behind it,
// and a reader who cannot see which one is reading tea leaves.
function emptyChatHint() {
  const agent = state.agents.find((a) => a.id === state.chatAgent);
  if (agent) return `Talking to ${agent.name}. It can read the documents and sources given to it.`;
  return 'Ask about your media library, your tasks, or anything else.';
}

async function openSession(id) {
  state.sessionId = id;
  const known = (state.agents || []).find((a) => a.id === agentOfSession(id));
  state.chatAgent = known ? known.id : agentOfSession(id);
  const { messages } = await api(`/api/v1/sessions/${id}/messages`);
  $('messages').innerHTML = '';
  for (const m of messages || []) appendMessage(m);
  await loadSessions();
  closeSidebar();
}

// The chat's agent picker. Changing it affects the *next* session rather than
// the current one: a transcript belongs to the agent it was held with, and
// re-pointing it halfway would leave the earlier turns unexplainable.
function renderChatAgentPicker() {
  const holder = $('chat-agent');
  if (!holder) return;
  if (!state.agents.length) {
    holder.replaceChildren();
    return;
  }
  const select = el('select', {
    id: 'chat-agent-select',
    title: 'Which agent a new chat is opened against',
    onchange: (e) => { state.chatAgent = e.target.value || null; },
  }, [
    el('option', { value: '', text: 'No agent (general assistant)' }),
    ...state.agents.map((a) => {
      const opt = el('option', { value: a.id, text: a.name });
      if (a.id === state.chatAgent) opt.selected = true;
      return opt;
    }),
  ]);
  holder.replaceChildren(el('span', { class: 'muted', text: 'New chat as' }), select);
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

/* ================= AGENTS =================
   An agent is a named configuration of the assistant: a prompt, a model pin,
   the sources it may consult, and the documents uploaded to it. This view is
   where they are made and fed — including from the GRC application, which
   links here rather than growing an upload page of its own. */

const SOURCE_LABELS = {
  documents: 'Documents uploaded here',
  grc: 'GRC application (NFRs, controls, regulations, policies, risks)',
  web: 'Web search and page fetch',
};

async function loadAgents() {
  const data = await api('/api/v1/agents');
  state.agents = data.agents || [];
  state.agentAvailable = data.available || {};
  renderAgentList();
  if (state.selectedAgent && !state.agents.some((a) => a.id === state.selectedAgent)) {
    state.selectedAgent = null;
  }
  await renderAgent();
}

function renderAgentList() {
  const list = $('agent-list');
  list.replaceChildren();
  if (!state.agents.length) {
    list.append(el('li', { class: 'muted', text: 'No agents yet' }));
    return;
  }
  for (const agent of state.agents) {
    list.append(el('li', {
      text: agent.name,
      title: agent.description || agent.name,
      class: agent.id === state.selectedAgent ? 'active' : '',
      onclick: () => { state.selectedAgent = agent.id; renderAgent().catch(showError); },
    }));
  }
}

function selectedAgent() {
  return state.agents.find((a) => a.id === state.selectedAgent) || null;
}

async function renderAgent() {
  const body = $('agent-body');
  const agent = selectedAgent();
  $('agent-delete').hidden = !agent;
  $('agent-chat').hidden = !agent;
  renderAgentList();

  if (!agent) {
    $('agent-title').textContent = 'Agents';
    body.replaceChildren(el('p', { class: 'muted' },
      'An agent scopes a conversation: it can read the documents uploaded to it and the ',
      'sources it is given, and nothing else. Make one per client, per engagement, or per ',
      'subject — then pick it when you start a chat, or point the GRC application at it.'));
    return;
  }

  $('agent-title').textContent = agent.name;
  body.replaceChildren(el('p', { class: 'muted', text: 'Loading…' }));

  const { documents } = await api(`/api/v1/agents/${encodeURIComponent(agent.id)}/documents`);
  const rows = [];

  rows.push(el('div', { class: 'card' },
    el('div', { class: 'row' },
      el('span', { class: 'muted', text: 'id' }), el('span', { text: agent.id })),
    agent.description ? el('p', { text: agent.description }) : null,
    el('div', { class: 'row' },
      el('span', { class: 'muted', text: 'sources' }),
      el('span', { text: agent.sources.length ? agent.sources.map(sourceLabel).join(', ') : 'none' })),
    (agent.backend || agent.model) ? el('div', { class: 'row' },
      el('span', { class: 'muted', text: 'model' }),
      el('span', { text: [agent.backend, agent.model].filter(Boolean).join(' / ') })) : null,
    el('div', { class: 'row-actions' },
      el('button', { class: 'ghost-btn', text: 'Edit', onclick: () => editAgent(agent) })),
  ));

  if (agent.system_prompt) {
    rows.push(el('div', { class: 'card' },
      el('h3', { text: 'Instructions' }),
      el('pre', { class: 'wrap', text: agent.system_prompt })));
  }

  rows.push(el('div', { class: 'card' },
    el('h3', { text: `Documents (${documents.length})` }),
    el('p', { class: 'muted' },
      'Uploaded here, then searched by this agent during a conversation. ',
      'PDF, text, markdown, HTML, CSV, JSON or YAML.'),
    uploadForm(agent),
    documents.length
      ? el('ul', { class: 'plain' }, documents.map((doc) => documentRow(agent, doc)))
      : el('p', { class: 'muted', text: 'Nothing uploaded yet.' }),
  ));

  body.replaceChildren(...rows.filter(Boolean));
}

function sourceLabel(source) {
  const label = SOURCE_LABELS[source] || source;
  // A source the agent asks for that this server cannot back is a
  // configuration mistake, and silence about it is how it stays one.
  if (state.agentAvailable[source] === false) return `${label} — not configured on this server`;
  return label;
}

function uploadForm(agent) {
  const file = el('input', { type: 'file', id: 'agent-file' });
  const title = el('input', { type: 'text', id: 'agent-doc-title', placeholder: 'Title (optional)' });
  const status = el('span', { class: 'muted' });

  const submit = el('button', {
    class: 'ghost-btn',
    text: 'Upload',
    onclick: async () => {
      if (!file.files || !file.files[0]) { status.textContent = 'Choose a file first.'; return; }
      const form = new FormData();
      form.append('file', file.files[0]);
      form.append('title', title.value);
      status.textContent = 'Reading and indexing…';
      submit.disabled = true;
      try {
        // Not api(): a multipart body must not carry a JSON content type, and
        // the browser has to set its own boundary.
        const res = await fetch(`/api/v1/agents/${encodeURIComponent(agent.id)}/documents`, {
          method: 'POST',
          headers: { Authorization: `Bearer ${state.token}` },
          body: form,
        });
        const payload = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(payload.error || `${res.status} ${res.statusText}`);
        status.textContent = '';
        file.value = '';
        title.value = '';
        toast(`Added "${payload.title}" (${payload.chunk_count} passages)`);
        await loadAgents();
      } catch (err) {
        status.textContent = err.message;
      } finally {
        submit.disabled = false;
      }
    },
  });

  return el('div', { class: 'row-form' }, file, title, submit, status);
}

function documentRow(agent, doc) {
  return el('li', {},
    el('div', {},
      el('strong', { text: doc.title }),
      el('span', { class: 'muted', text: ` — ${doc.chunk_count} passages, ${doc.extract_via}` })),
    el('button', {
      class: 'ghost-btn danger',
      text: 'Remove',
      onclick: async () => {
        if (!confirm(`Remove "${doc.title}" from ${agent.name}?`)) return;
        await api(`/api/v1/agents/${encodeURIComponent(agent.id)}/documents/${doc.id}`,
          { method: 'DELETE' });
        await loadAgents();
      },
    }),
  );
}

function agentFields(agent) {
  const has = (s) => Boolean(agent && agent.sources && agent.sources.includes(s));
  return [
    { name: 'name', label: 'Name', value: agent ? agent.name : '' },
    { name: 'description', label: 'What it is for', value: agent ? agent.description : '' },
    {
      name: 'system_prompt', label: 'Instructions (optional)', type: 'textarea',
      value: agent ? agent.system_prompt : '',
    },
    { name: 'src_documents', label: SOURCE_LABELS.documents, type: 'checkbox', value: agent ? has('documents') : true },
    { name: 'src_grc', label: SOURCE_LABELS.grc, type: 'checkbox', value: has('grc') },
    { name: 'src_web', label: SOURCE_LABELS.web, type: 'checkbox', value: has('web') },
    { name: 'backend', label: 'Backend (optional)', value: agent ? agent.backend : '' },
    { name: 'model', label: 'Model (optional)', value: agent ? agent.model : '' },
  ];
}

function agentPayload(values) {
  const sources = [];
  if (values.src_documents) sources.push('documents');
  if (values.src_grc) sources.push('grc');
  if (values.src_web) sources.push('web');
  return {
    name: values.name,
    description: values.description,
    system_prompt: values.system_prompt,
    backend: values.backend,
    model: values.model,
    sources,
  };
}

function newAgent() {
  openEditor('New agent', agentFields(null), async (values) => {
    const created = await api('/api/v1/agents',
      { method: 'POST', body: JSON.stringify(agentPayload(values)) });
    state.selectedAgent = created.id;
    await loadAgents();
  });
}

function editAgent(agent) {
  openEditor(`Edit ${agent.name}`, agentFields(agent), async (values) => {
    await api(`/api/v1/agents/${encodeURIComponent(agent.id)}`,
      { method: 'PUT', body: JSON.stringify(agentPayload(values)) });
    await loadAgents();
  });
}

$('new-agent').addEventListener('click', () => newAgent());

$('agent-delete').addEventListener('click', async () => {
  const agent = selectedAgent();
  if (!agent) return;
  if (!confirm(`Delete ${agent.name} and its documents? Conversations it held are kept.`)) return;
  await api(`/api/v1/agents/${encodeURIComponent(agent.id)}`, { method: 'DELETE' });
  state.selectedAgent = null;
  await loadAgents();
});

$('agent-chat').addEventListener('click', async () => {
  const agent = selectedAgent();
  if (!agent) return;
  state.chatAgent = agent.id;
  switchView('chat');
  await newSession();
  toast(`New chat as ${agent.name}`);
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

/* ---------- accounting ----------
   The accounting API speaks minor units — integer cents — because the ledger
   cannot use floats and stay balanced. Nothing here converts them back into a
   float for arithmetic; `cents()` is a formatter and that is all it is. */

const cents = (n) => ((Number(n) || 0) / 100).toLocaleString(undefined, {
  minimumFractionDigits: 2, maximumFractionDigits: 2,
});
// Quantities arrive as thousandths so an hours figure stays exact.
const qty = (n) => ((Number(n) || 0) / 1000).toLocaleString(undefined, { maximumFractionDigits: 3 });

const acct = { tab: 'overview', currency: '', accounts: [], vatRates: [] };

for (const li of document.querySelectorAll('#acct-nav li')) {
  li.addEventListener('click', () => {
    acct.tab = li.dataset.tab;
    for (const other of document.querySelectorAll('#acct-nav li')) {
      other.classList.toggle('active', other === li);
    }
    renderAccounting().catch(showError);
    closeSidebar();
  });
}

$('acct-new').addEventListener('click', () => {
  if (acct.tab === 'expenses') editExpense();
  else if (acct.tab === 'funding') editFunding();
  else if (acct.tab === 'invoices') newInvoiceFromTime();
});

async function renderAccounting() {
  const body = $('acct-body');
  const titles = {
    overview: 'Overview', unbilled: 'Ready to bill', invoices: 'Invoices',
    payments: 'Payments', funding: 'Owner funding', expenses: 'Expenses',
    reports: 'Reports', accounts: 'Chart of accounts',
  };
  $('acct-title').textContent = titles[acct.tab];
  $('acct-new').hidden = !['expenses', 'funding', 'invoices'].includes(acct.tab);
  $('acct-new').textContent = acct.tab === 'invoices' ? 'Bill time'
    : acct.tab === 'funding' ? 'Record' : 'New';
  body.innerHTML = '';

  if (!acct.currency) {
    const s = await api('/api/v1/accounting/settings');
    acct.currency = s.currency || '';
  }
  if (acct.tab === 'overview') return renderAcctOverview(body);
  if (acct.tab === 'unbilled') return renderUnbilled(body);
  if (acct.tab === 'invoices') return renderInvoices(body);
  if (acct.tab === 'payments') return renderPayments(body);
  if (acct.tab === 'funding') return renderFunding(body);
  if (acct.tab === 'expenses') return renderExpenses(body);
  if (acct.tab === 'reports') return renderReports(body);
  return renderChartOfAccounts(body);
}

async function renderAcctOverview(body) {
  const d = await api('/api/v1/accounting/dashboard');
  body.append(el('div', { class: 'stats' },
    stat(cents(d.bank_balance), 'Bank'),
    stat(cents(d.outstanding_total), 'Owed to us'),
    stat(cents(d.overdue_total), 'Overdue'),
    stat(cents(d.unbilled_time_amount), 'Unbilled time'),
    stat(cents(d.profit_this_year), 'Profit YTD'),
    stat(cents(Math.abs(d.vat_position)), d.vat_position >= 0 ? 'VAT payable' : 'VAT reclaimable')));

  if (d.draft_invoice_count) {
    body.append(el('div', { class: 'muted', text:
      `${d.draft_invoice_count} draft invoice(s) not yet issued.` }));
  }
  if (d.recent_invoices && d.recent_invoices.length) {
    body.append(el('div', { class: 'group-head', text: 'Recent invoices' }));
    body.append(table(['Number', 'Client', 'Issued', 'Total', 'Outstanding', 'Status'],
      d.recent_invoices.map((i) => [
        i.number || 'draft', i.client_name, i.issue_date || '—',
        cents(i.total), cents(i.total - i.paid), i.status,
      ]), [false, false, false, true, true, false]));
  }
}

async function renderUnbilled(body) {
  const { entries } = await api('/api/v1/accounting/unbilled');
  if (!entries || !entries.length) {
    body.append(el('div', { class: 'empty muted', text: 'Nothing waiting to be billed.' }));
    return;
  }
  // Grouped by client, because billing happens per client — an invoice cannot
  // span two of them, and the module refuses if you try.
  const byClient = new Map();
  for (const e of entries) {
    if (!byClient.has(e.client_id)) byClient.set(e.client_id, { name: e.client_name, rows: [], total: 0 });
    const g = byClient.get(e.client_id);
    g.rows.push(e);
    g.total += e.amount;
  }
  for (const [clientID, g] of byClient) {
    const head = el('div', { class: 'group-head' },
      el('span', { text: `${g.name} — ${cents(g.total)}` }),
      el('button', { class: 'ghost-btn', text: 'Draft invoice' }));
    head.lastChild.addEventListener('click', () => draftFromTime(clientID));
    body.append(head);
    body.append(table(['Date', 'Engagement', 'Description', 'Hours', 'Rate', 'Amount'],
      g.rows.map((e) => [e.entry_date, e.engagement_name, e.description,
        qty(e.hours), cents(e.rate), cents(e.amount)]),
      [false, false, false, true, true, true]));
  }
}

async function draftFromTime(clientID) {
  const inv = await api('/api/v1/accounting/unbilled/draft', {
    method: 'POST', body: JSON.stringify({ client_id: clientID }),
  });
  toast(`Draft ${cents(inv.total)} created — review it under Invoices before issuing.`);
  acct.tab = 'invoices';
  for (const li of document.querySelectorAll('#acct-nav li')) {
    li.classList.toggle('active', li.dataset.tab === 'invoices');
  }
  return renderAccounting();
}

function newInvoiceFromTime() {
  acct.tab = 'unbilled';
  for (const li of document.querySelectorAll('#acct-nav li')) {
    li.classList.toggle('active', li.dataset.tab === 'unbilled');
  }
  renderAccounting().catch(showError);
}

async function renderInvoices(body) {
  const { invoices } = await api('/api/v1/accounting/invoices');
  if (!invoices || !invoices.length) {
    body.append(el('div', { class: 'empty muted', text: 'No invoices yet.' }));
    return;
  }
  body.append(table(
    ['Number', 'Client', 'Issued', 'Due', 'Total', 'Outstanding', 'Status', ''],
    invoices.map((i) => [
      i.number || `draft #${i.id}`, i.client_name, i.issue_date || '—', i.due_date || '—',
      cents(i.total), cents(i.total - i.paid), i.status, invoiceActions(i),
    ]), [false, false, false, false, true, true, false, false]));
}

// Which actions exist depends on status, and the rules are the module's: a
// draft can be issued or deleted, an issued invoice can only be paid, voided or
// credited. Showing an Edit button on an issued invoice would promise something
// the server will refuse.
function invoiceActions(inv) {
  const wrap = el('div', { class: 'row-actions' });
  const add = (label, fn, cls) => {
    const b = el('button', { class: cls || 'ghost-btn', text: label });
    b.addEventListener('click', fn);
    wrap.append(b);
  };
  add('View', () => viewInvoice(inv.id));
  if (inv.status === 'draft') {
    add('Issue', () => issueInvoice(inv), 'ghost-btn danger');
    add('Delete', () => deleteDraft(inv));
  } else if (inv.status !== 'void') {
    if (inv.total !== inv.paid) add('Pay', () => payInvoice(inv));
    add('Credit', () => creditInvoice(inv));
    if (inv.paid === 0) add('Void', () => voidInvoice(inv), 'ghost-btn danger');
  }
  return wrap;
}

async function viewInvoice(id) {
  const inv = await api(`/api/v1/accounting/invoices/${id}`);
  const body = $('acct-body');
  body.innerHTML = '';
  const back = el('button', { class: 'ghost-btn', text: '← Invoices' });
  back.addEventListener('click', () => renderAccounting().catch(showError));
  body.append(back);
  body.append(el('div', { class: 'group-head', text:
    `${inv.number || `Draft #${inv.id}`} — ${inv.client_name} (${inv.status})` }));
  body.append(el('div', { class: 'muted', text:
    `Issued ${inv.issue_date || '—'}, due ${inv.due_date || '—'}` }));
  if (inv.reverse_charge) {
    body.append(el('div', { class: 'muted', text:
      `Reverse charge — customer VAT ${inv.customer_vat_number}` }));
  }
  body.append(table(['Description', 'Qty', 'Unit', 'Net', 'VAT'],
    (inv.lines || []).map((l) => [l.description, qty(l.quantity), cents(l.unit_price),
      cents(l.net), cents(l.vat)]), [false, true, true, true, true]));
  body.append(el('div', { class: 'stats' },
    stat(cents(inv.subtotal), 'Subtotal'),
    stat(cents(inv.vat), 'VAT'),
    stat(cents(inv.total), 'Total'),
    stat(cents(inv.total - inv.paid), 'Outstanding')));
}

// Issuing is irreversible, so the confirmation says so in the words that
// matter rather than "Are you sure?".
async function issueInvoice(inv) {
  const ok = confirm(
    `Issue this invoice for ${cents(inv.total)} to ${inv.client_name}?\n\n` +
    `It will take the next number in the sequence and be posted to the ledger. ` +
    `Afterwards it cannot be edited or deleted — only voided or corrected with a credit note.`);
  if (!ok) return;
  const issued = await api(`/api/v1/accounting/invoices/${inv.id}/issue`, { method: 'POST' });
  toast(`Issued ${issued.number}`);
  return renderAccounting();
}

async function deleteDraft(inv) {
  if (!confirm('Delete this draft? Nothing has been posted, so nothing is lost.')) return;
  await api(`/api/v1/accounting/invoices/${inv.id}`, { method: 'DELETE' });
  return renderAccounting();
}

function payInvoice(inv) {
  const outstanding = inv.total - inv.paid;
  openEditor('Record payment', [
    { name: 'amount', label: `Amount (${acct.currency})`, value: (outstanding / 100).toFixed(2) },
    { name: 'paid_on', label: 'Received on', type: 'date' },
    { name: 'reference', label: 'Reference' },
    { name: 'method', label: 'Method', type: 'select', value: 'bank',
      options: [{ value: 'bank', label: 'Bank' }, { value: 'card', label: 'Card' }, { value: 'cash', label: 'Cash' }] },
  ], async (v) => {
    await api('/api/v1/accounting/payments', {
      method: 'POST',
      body: JSON.stringify({
        invoice_id: inv.id,
        amount: Math.round(parseFloat(v.amount || '0') * 100),
        paid_on: v.paid_on, reference: v.reference, method: v.method,
      }),
    });
    await renderAccounting();
  });
}

function voidInvoice(inv) {
  openEditor(`Void ${inv.number}`, [
    { name: 'reason', label: 'Reason', type: 'textarea' },
  ], async (v) => {
    await api(`/api/v1/accounting/invoices/${inv.id}/void`, {
      method: 'POST', body: JSON.stringify({ reason: v.reason }),
    });
    await renderAccounting();
  });
}

function creditInvoice(inv) {
  openEditor(`Credit note for ${inv.number}`, [
    { name: 'reason', label: 'Reason', type: 'textarea' },
  ], async (v) => {
    const note = await api(`/api/v1/accounting/invoices/${inv.id}/credit`, {
      method: 'POST', body: JSON.stringify({ reason: v.reason }),
    });
    toast(`Credit note drafted for ${cents(note.total)} — issue it to post it.`);
    await renderAccounting();
  });
}

async function renderPayments(body) {
  const { payments } = await api('/api/v1/accounting/payments');
  if (!payments || !payments.length) {
    body.append(el('div', { class: 'empty muted', text: 'No payments recorded.' }));
    return;
  }
  body.append(table(['Date', 'Invoice', 'Client', 'Amount', 'Method', 'Reference'],
    payments.map((p) => [p.paid_on, p.invoice_number, p.client_name,
      cents(p.amount), p.method, p.reference]),
    [false, false, false, true, false, false]));
}

// Owner funding. The outstanding loan leads the page because it is the figure
// with a consequence — it is a debt the business owes — and the server sends it
// rather than the browser deriving it, so there is only one implementation of
// what "still owed" means.
async function renderFunding(body) {
  const { funding, loan_outstanding } = await api('/api/v1/accounting/funding');

  body.append(el('div', { class: 'stats' },
    stat(cents(loan_outstanding), 'Owed to owner'),
    stat(cents((funding || []).filter((f) => f.kind === 'capital')
      .reduce((sum, f) => sum + Number(f.amount), 0)), 'Capital introduced')));

  if (!funding || !funding.length) {
    body.append(el('div', { class: 'empty muted', text:
      'Nothing recorded. Use Record for an opening deposit, a further contribution, '
      + 'or a loan from the owner.' }));
    return;
  }

  const labels = { capital: 'Capital', loan: 'Loan', repayment: 'Repayment' };
  body.append(table(['Date', 'Kind', 'From', 'Amount', 'Posted to', 'Account', 'Reference'],
    funding.map((f) => [
      f.received_on, labels[f.kind] || f.kind, f.from_name,
      // A repayment is money leaving, and showing it with the same sign as a
      // deposit makes a column of numbers that do not add up to the balance.
      (f.kind === 'repayment' ? '−' : '') + cents(f.amount),
      f.owner_account_name, f.cash_account_name, f.reference,
    ]),
    [false, false, false, true, false, false, false]));
}

async function editFunding() {
  await loadAcctLookups();
  const banks = acct.accounts.filter((a) => a.type === 'asset');
  openEditor('Record owner funding', [
    // Kind first, and with no blank option: it decides whether this becomes
    // equity or a debt, and it cannot be worked out afterwards from the amount.
    { name: 'kind', label: 'Kind', type: 'select', options: [
      { value: 'capital', label: 'Capital introduced — equity, not repayable' },
      { value: 'loan', label: 'Loan from owner — repayable' },
      { value: 'repayment', label: 'Repayment of an owner loan' },
    ] },
    { name: 'amount', label: `Amount (${acct.currency})` },
    { name: 'received_on', label: 'Date', type: 'date' },
    { name: 'from_name', label: 'From' },
    { name: 'cash_account_id', label: 'Bank account', type: 'select',
      options: banks.map((a) => ({ value: String(a.id), label: `${a.code} ${a.name}` })) },
    { name: 'reference', label: 'Reference' },
    { name: 'note', label: 'Note' },
  ], async (v) => {
    await api('/api/v1/accounting/funding', {
      method: 'POST',
      body: JSON.stringify({
        kind: v.kind,
        amount: Math.round(parseFloat(v.amount || '0') * 100),
        received_on: v.received_on,
        from_name: v.from_name,
        cash_account_id: Number(v.cash_account_id),
        reference: v.reference,
        note: v.note,
      }),
    });
    await renderAccounting();
  });
}

async function renderExpenses(body) {
  const { expenses } = await api('/api/v1/accounting/expenses');
  if (!expenses || !expenses.length) {
    body.append(el('div', { class: 'empty muted', text: 'No expenses recorded.' }));
    return;
  }
  body.append(table(['Date', 'Vendor', 'Category', 'Net', 'VAT', 'Total', 'Paid from'],
    expenses.map((x) => [x.spent_on, x.vendor, x.account_name,
      cents(x.net), cents(x.vat), cents(x.total), x.paid_from_name]),
    [false, false, false, true, true, true, false]));
}

async function editExpense() {
  await loadAcctLookups();
  const expenseAccounts = acct.accounts.filter((a) => a.type === 'expense' || a.type === 'asset');
  const sources = acct.accounts.filter((a) => a.type === 'asset' || a.type === 'liability');
  openEditor('Record expense', [
    { name: 'vendor', label: 'Vendor' },
    { name: 'description', label: 'Description' },
    { name: 'spent_on', label: 'Date', type: 'date' },
    { name: 'net', label: `Net amount (${acct.currency})` },
    { name: 'account_id', label: 'Category', type: 'select',
      options: expenseAccounts.map((a) => ({ value: String(a.id), label: `${a.code} ${a.name}` })) },
    { name: 'paid_from_id', label: 'Paid from', type: 'select',
      options: sources.map((a) => ({ value: String(a.id), label: `${a.code} ${a.name}` })) },
    { name: 'vat_rate_id', label: 'VAT', type: 'select',
      options: [{ value: '0', label: 'None' }].concat(
        acct.vatRates.map((r) => ({ value: String(r.id), label: `${r.name} (${r.rate_bp / 100}%)` }))) },
    { name: 'vat_reclaimable', label: 'VAT reclaimable', type: 'checkbox', value: true },
  ], async (v) => {
    await api('/api/v1/accounting/expenses', {
      method: 'POST',
      body: JSON.stringify({
        vendor: v.vendor, description: v.description, spent_on: v.spent_on,
        net: Math.round(parseFloat(v.net || '0') * 100),
        account_id: Number(v.account_id), paid_from_id: Number(v.paid_from_id),
        vat_rate_id: Number(v.vat_rate_id), vat_reclaimable: Boolean(v.vat_reclaimable),
      }),
    });
    await renderAccounting();
  });
}

async function loadAcctLookups() {
  if (!acct.accounts.length) {
    const { accounts } = await api('/api/v1/accounting/accounts');
    acct.accounts = accounts || [];
  }
  if (!acct.vatRates.length) {
    const { rates } = await api('/api/v1/accounting/vat-rates');
    acct.vatRates = rates || [];
  }
}

async function renderChartOfAccounts(body) {
  const { accounts } = await api('/api/v1/accounting/accounts');
  acct.accounts = accounts || [];
  body.append(table(['Code', 'Name', 'Type', 'System'],
    acct.accounts.map((a) => [a.code, a.name, a.type, a.system_key || '']),
    [false, false, false, false]));
}

async function renderReports(body) {
  const today = new Date().toISOString().slice(0, 10);
  const yearStart = `${new Date().getFullYear()}-01-01`;
  const picker = el('div', { class: 'row-form' },
    el('label', { text: 'Report' }),
    el('select', { id: 'acct-report' },
      [['profit-loss', 'Profit and loss'], ['balance-sheet', 'Balance sheet'],
       ['trial-balance', 'Trial balance'], ['aged-receivables', 'Aged receivables'],
       ['vat', 'VAT summary']].map(([v, l]) => el('option', { value: v, text: l }))),
    el('label', { text: 'From' }), el('input', { type: 'date', id: 'acct-from', value: yearStart }),
    el('label', { text: 'To' }), el('input', { type: 'date', id: 'acct-to', value: today }),
    el('button', { class: 'ghost-btn', text: 'Run' }));
  picker.lastChild.addEventListener('click', () => runReport().catch(showError));
  body.append(picker);
  body.append(el('div', { id: 'acct-report-out' }));
  return runReport();
}

async function runReport() {
  const out = $('acct-report-out');
  const kind = $('acct-report').value;
  const from = $('acct-from').value;
  const to = $('acct-to').value;
  out.innerHTML = '';

  if (kind === 'profit-loss') {
    const r = await api(`/api/v1/accounting/reports/profit-loss?from=${from}&to=${to}`);
    out.append(el('div', { class: 'group-head', text: 'Income' }));
    out.append(table(['Code', 'Account', 'Amount'],
      r.income.map((l) => [l.code, l.name, cents(l.amount)]), [false, false, true]));
    out.append(el('div', { class: 'group-head', text: 'Expenses' }));
    out.append(table(['Code', 'Account', 'Amount'],
      r.expenses.map((l) => [l.code, l.name, cents(l.amount)]), [false, false, true]));
    out.append(el('div', { class: 'stats' },
      stat(cents(r.total_income), 'Income'),
      stat(cents(r.total_expenses), 'Expenses'),
      stat(cents(r.net_profit), 'Net profit')));
  } else if (kind === 'balance-sheet') {
    const r = await api(`/api/v1/accounting/reports/balance-sheet?as_of=${to}`);
    for (const [label, rows] of [['Assets', r.assets], ['Liabilities', r.liabilities], ['Equity', r.equity]]) {
      out.append(el('div', { class: 'group-head', text: label }));
      out.append(table(['Code', 'Account', 'Amount'],
        rows.map((l) => [l.code, l.name, cents(l.amount)]), [false, false, true]));
    }
    out.append(el('div', { class: 'stats' },
      stat(cents(r.total_assets), 'Assets'),
      stat(cents(r.total_liabilities), 'Liabilities'),
      stat(cents(r.total_equity + r.current_earnings), 'Equity'),
      stat(cents(r.current_earnings), 'Earnings not closed')));
    if (!r.balanced) {
      out.append(el('div', { class: 'error', text:
        'This balance sheet does not balance — something has written to the ledger outside the module.' }));
    }
  } else if (kind === 'trial-balance') {
    const r = await api(`/api/v1/accounting/reports/trial-balance?from=${from}&to=${to}`);
    out.append(table(['Code', 'Account', 'Debit', 'Credit'],
      r.rows.map((l) => [l.code, l.name, cents(l.debit), cents(l.credit)]),
      [false, false, true, true]));
    out.append(el('div', { class: 'stats' },
      stat(cents(r.total_debit), 'Debits'),
      stat(cents(r.total_credit), 'Credits'),
      stat(r.balanced ? 'Yes' : 'NO', 'Balanced')));
  } else if (kind === 'aged-receivables') {
    const r = await api(`/api/v1/accounting/reports/aged-receivables?as_of=${to}`);
    out.append(table(['Client', 'Current', '1–30', '31–60', '61–90', '90+', 'Total'],
      r.rows.map((row) => [row.client_name, cents(row.buckets.current), cents(row.buckets.days_1_30),
        cents(row.buckets.days_31_60), cents(row.buckets.days_61_90),
        cents(row.buckets.days_90_plus), cents(row.buckets.total)]),
      [false, true, true, true, true, true, true]));
    out.append(el('div', { class: 'stats' },
      stat(cents(r.totals.total), 'Total owed'),
      stat(cents(r.totals.total - r.totals.current), 'Overdue')));
  } else {
    const r = await api(`/api/v1/accounting/reports/vat?from=${from}&to=${to}`);
    out.append(table(['Line', 'Amount'], [
      ['Standard-rated sales', cents(r.net_sales_standard)],
      ['Zero-rated sales', cents(r.net_sales_zero)],
      ['Exempt sales', cents(r.net_sales_exempt)],
      ['Reverse-charge sales', cents(r.net_sales_reverse_charge)],
      ['Output VAT', cents(r.output_vat)],
      ['Purchases', cents(r.net_purchases)],
      ['Input VAT', cents(r.input_vat)],
      [r.net_due >= 0 ? 'Payable' : 'Reclaimable', cents(Math.abs(r.net_due))],
    ], [false, true]));
    if (r.note) out.append(el('div', { class: 'error', text: r.note }));
    out.append(el('div', { class: 'muted', text:
      'A summary to fill a return in from, not a filing.' }));
  }
}

/* ---------- admin ----------
   Answers "why is the server behaving like this?" without ssh. Everything here
   is read-only except revoking a client, because the server exposes nothing
   else: configuration comes from the environment and a restart, and pretending
   otherwise with editable fields would be a lie the page cannot honour. */

const admin = { tab: 'status' };

for (const li of document.querySelectorAll('#admin-nav li')) {
  li.addEventListener('click', () => {
    admin.tab = li.dataset.tab;
    for (const other of document.querySelectorAll('#admin-nav li')) {
      other.classList.toggle('active', other === li);
    }
    renderAdmin().catch(showError);
    closeSidebar();
  });
}
$('admin-refresh').addEventListener('click', () => renderAdmin().catch(showError));

const bytes = (n) => {
  const v = Number(n) || 0;
  if (v < 1024) return `${v} B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)} KB`;
  if (v < 1024 * 1024 * 1024) return `${(v / 1024 / 1024).toFixed(1)} MB`;
  return `${(v / 1024 / 1024 / 1024).toFixed(2)} GB`;
};

// Two-column key/value list, the shape most of this page wants.
function facts(pairs) {
  return el('div', { class: 'table-wrap' },
    el('table', {}, el('tbody', {}, pairs.filter(Boolean).map(([k, v]) =>
      el('tr', {},
        el('td', { class: 'muted', text: k }),
        v && v.nodeType ? el('td', {}, v) : el('td', { text: String(v ?? '—') }))))));
}

async function renderAdmin() {
  const body = $('admin-body');
  const titles = {
    status: 'Status', config: 'Configuration', backends: 'Backends',
    hardware: 'Hardware', tools: 'Tools', clients: 'Clients',
  };
  $('admin-title').textContent = titles[admin.tab];
  body.innerHTML = '';

  if (admin.tab === 'status') return renderAdminStatus(body);
  if (admin.tab === 'config') return renderAdminConfig(body);
  if (admin.tab === 'backends') return renderAdminBackends(body);
  if (admin.tab === 'hardware') return renderAdminHardware(body);
  if (admin.tab === 'tools') return renderAdminTools(body);
  return renderAdminClients(body);
}

async function renderAdminStatus(body) {
  const s = await api('/api/v1/admin/status');
  body.append(el('div', { class: 'stats' },
    stat(s.uptime, 'Uptime'),
    stat(s.sessions, 'Sessions'),
    stat(s.messages, 'Messages'),
    stat(s.tool_audit, 'Audited calls'),
    stat(s.clients, 'Clients'),
    stat(s.server_tools, 'Server tools')));

  body.append(el('div', { class: 'group-head', text: 'Database' }));
  body.append(facts([
    ['Path', s.database_path],
    ['Size', bytes(s.database_bytes)],
    // A WAL that keeps growing usually means a reader is holding a transaction
    // open, so it is worth showing next to the database rather than buried.
    ['Write-ahead log', bytes(s.wal_bytes)],
    ['Started', s.started_at],
  ]));
}

async function renderAdminConfig(body) {
  const c = await api('/api/v1/admin/config');
  body.append(el('div', { class: 'muted', text:
    'Read from the environment at startup. Changing any of it needs a restart.' }));
  body.append(el('div', { class: 'group-head', text: 'Server' }));
  body.append(facts([
    ['Listen address', c.addr],
    ['Database', c.database_path],
    ['Backends file', c.backends_path || '(not set — using environment)'],
    ['Go version', c.go_version],
  ]));

  body.append(el('div', { class: 'group-head', text: 'Model' }));
  body.append(facts([
    ['Default backend', c.default_backend],
    ['Fallback backend', c.fallback_backend || '(none — failures are reported, not rerouted)'],
    ['Pool', (c.pool_backends || []).join(', ') || '(none — the batch tool is not offered)'],
    ['Max tokens', c.llm_max_tokens],
    ['Timeout', c.llm_timeout],
    ['Max tool iterations', c.max_tool_iterations],
  ]));

  // Credentials are reported as present or absent and never by value. This page
  // gets left open and screenshotted.
  body.append(el('div', { class: 'group-head', text: 'Credentials' }));
  body.append(facts([
    ['Metadata providers', (c.metadata_providers || []).join(', ') ||
      '(none — the assistant cannot verify a title before renaming)'],
    ['Hugging Face token', c.has_huggingface_token ? 'configured' : 'not set'],
  ]));
  body.append(el('div', { class: 'muted', text:
    'Secrets are never sent to this page — only whether they are set.' }));
}

async function renderAdminBackends(body) {
  const d = await api('/api/v1/backends');
  const rows = (d.backends || []).map((b) => [
    b.name, b.kind, b.model || '—', b.status,
    b.status_note || '', b.name === d.default ? 'default' : (b.name === d.fallback ? 'fallback' : ''),
  ]);
  body.append(table(['Name', 'Kind', 'Model', 'Status', 'Note', 'Role'], rows));

  const refresh = el('button', { class: 'ghost-btn', text: 'Re-probe backends' });
  refresh.addEventListener('click', async () => {
    refresh.disabled = true;
    try {
      await api('/api/v1/backends/refresh', { method: 'POST' });
      toast('Backends re-probed');
      await renderAdmin();
    } catch (err) {
      showError(err);
    } finally {
      refresh.disabled = false;
    }
  });
  body.append(refresh);
  body.append(el('div', { class: 'muted', text:
    'A backend that is down is recorded as unreachable and retried; it never stops the server starting.' }));

  body.append(backendTestBench(d));
}

// Send one question to one backend and see what comes back.
//
// A probe says a backend answers HTTP; this says it answers *questions*, which
// is a different fact and the one that matters before pointing work at it. The
// call deliberately does not fall back to another backend — the whole question
// is whether this one works — and creates no session, so testing leaves no
// transcripts behind.
function backendTestBench(d) {
  const names = (d.backends || []).map((b) => b.name);
  if (!names.length) return el('div', {});

  const pick = el('select', { id: 'test-backend' },
    names.map((n) => el('option', { value: n, text: n + (n === d.default ? ' (default)' : '') })));
  const model = el('input', { type: 'text', id: 'test-model', placeholder: 'model (blank = backend default)' });
  const prompt = el('input', { type: 'text', id: 'test-prompt', placeholder: 'Ask something…' });
  const out = el('div', { class: 'muted' });
  const run = el('button', { class: 'ghost-btn', text: 'Send' });

  run.addEventListener('click', async () => {
    run.disabled = true;
    out.className = 'muted';
    out.textContent = 'Waiting for ' + pick.value + '…';
    const started = Date.now();
    try {
      const res = await api(`/api/v1/backends/${encodeURIComponent(pick.value)}/test`, {
        method: 'POST',
        body: JSON.stringify({ prompt: prompt.value, model: model.value }),
      });
      if (res.error) {
        out.className = 'err';
        out.textContent = `${pick.value} failed after ${res.elapsed_ms} ms: ${res.error}`;
        return;
      }
      out.className = '';
      out.replaceChildren(
        el('div', { class: 'muted', text:
          `${res.backend} · ${res.model} · ${res.elapsed_ms} ms · ` +
          `${res.usage.prompt_tokens}→${res.usage.completion_tokens} tokens` }),
        el('pre', { class: 'wrap', text: res.reply }),
      );
    } catch (err) {
      out.className = 'err';
      out.textContent = `${pick.value} failed after ${Date.now() - started} ms: ${err.message}`;
    } finally {
      run.disabled = false;
    }
  });

  return el('div', { class: 'card' },
    el('h3', { text: 'Send a test question' }),
    el('p', { class: 'muted', text:
      'Goes to the backend you pick and no other — no fallback, no tools, no session. '
      + 'Use it to check a backend answers, and how long it takes.' }),
    el('div', { class: 'row-form' }, pick, model, prompt, run),
    out,
  );
}

// The probe reports megabytes, not bytes.
const mb = (n) => (Number(n) > 0 ? `${(Number(n) / 1024).toFixed(1)} GB` : '—');

async function renderAdminHardware(body) {
  const h = await api('/api/v1/system');

  // Said first, because it decides whether anything below is worth reading.
  // This panel describes the host serving the API; when the models run
  // elsewhere, that is a different machine and the numbers are not about it.
  if (!h.runs_inference) {
    body.append(el('div', { class: 'muted', text:
      'No configured backend runs on this host. The figures below describe this server, '
      + 'not the machine running the models — so VRAM fit estimates are reported as '
      + 'unknown rather than computed from the wrong hardware.' }));
  }

  body.append(facts([
    ['Runs inference', h.runs_inference ? 'yes — models run on this host' : 'no — models run elsewhere'],
    ['CPU', h.cpu_model],
    ['Logical CPUs', h.cpu_cores],
    ['Memory', h.ram_total_mb ? `${mb(h.ram_total_mb)} (${mb(h.ram_available_mb)} available)` : null],
    ['Memory bandwidth', h.ram_bandwidth_gbs ? `${h.ram_bandwidth_gbs} GB/s` : null],
    ['nvidia-smi', h.nvidia_smi_present ? 'present' : 'not found'],
    ['Probed', h.detected_at ? new Date(h.detected_at).toLocaleString() : null],
  ]));

  if (h.gpus && h.gpus.length) {
    body.append(el('div', { class: 'group-head', text: 'GPUs' }));
    body.append(table(['Name', 'VRAM', 'Free', 'Bandwidth', 'Arch', 'Compute', 'Driver'],
      h.gpus.map((g) => [
        g.name, mb(g.total_mb), mb(g.free_mb),
        g.bandwidth_gbs ? `${g.bandwidth_gbs} GB/s` : '—',
        g.arch || '—', g.compute_cap || '—', g.driver || '—',
      ]),
      [false, true, true, true, false, false, false]));

    // Architecture notes exist because they invalidate generic advice — the
    // Pascal FP16 trap being the one that costs an afternoon.
    for (const g of h.gpus) {
      for (const note of g.notes || []) {
        body.append(el('div', { class: 'muted', text: `${g.name}: ${note}` }));
      }
    }
  } else if (h.runs_inference) {
    body.append(el('div', { class: 'muted', text:
      'No GPU reported. If there is one, nvidia-smi is missing or the unit sets PrivateDevices=true.' }));
  }

  if (h.npus && h.npus.length) {
    body.append(el('div', { class: 'group-head', text: 'Neural accelerators' }));
    body.append(table(['Vendor', 'Device', 'Can run an LLM', 'Note'],
      h.npus.map((n) => [n.vendor, n.device, n.llm_capable ? 'yes' : 'no', n.note || ''])));
  }

  for (const w of h.warnings || []) {
    body.append(el('div', { class: 'muted', text: w }));
  }
}

async function renderAdminTools(body) {
  const { tools } = await api('/api/v1/admin/tools');
  body.append(el('div', { class: 'muted', text:
    'Server-side tools the model can call. Risk drives the approval policy: reads may be auto-approved, ' +
    'writes prompt unless opted out, destructive actions always prompt.' }));
  body.append(table(['Tool', 'Risk', 'Description'],
    (tools || []).map((t) => [t.name, t.risk, t.description]),
    [false, false, false]));
}

async function renderAdminClients(body) {
  const { clients } = await api('/api/v1/admin/clients');
  if (!clients || !clients.length) {
    body.append(el('div', { class: 'empty muted', text: 'No clients registered.' }));
    return;
  }
  // store.Client carries no JSON tags, so the wire keys are the Go field names.
  body.append(table(['Name', 'Kind', 'Created', 'Last seen', ''],
    clients.map((c) => [
      c.Name, c.Kind,
      String(c.CreatedAt || '').slice(0, 19).replace('T', ' '),
      c.LastSeenAt ? String(c.LastSeenAt).slice(0, 19).replace('T', ' ') : 'never',
      revokeButton(c.Name),
    ])));
  body.append(el('div', { class: 'muted', text:
    'Tokens are stored only as hashes and cannot be shown again. New ones are issued on the server ' +
    'with scripts/clients.sh add — deliberately not from here, so a stolen browser token cannot mint more.' }));
}

function revokeButton(name) {
  const b = el('button', { class: 'ghost-btn danger', text: 'Revoke' });
  b.addEventListener('click', async () => {
    if (!confirm(`Revoke "${name}"? Its token stops working immediately and cannot be recovered.`)) return;
    try {
      await api(`/api/v1/admin/clients/${encodeURIComponent(name)}`, { method: 'DELETE' });
      toast(`Revoked ${name}`);
      await renderAdmin();
    } catch (err) {
      showError(err);
    }
  });
  return b;
}
