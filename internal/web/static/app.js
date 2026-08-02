// Minimal chat client for the browser. The browser declares no client-side
// tools, so the server never asks it to touch a filesystem — a turn from here
// either completes or reports an error.
'use strict';

const $ = (id) => document.getElementById(id);
const state = { token: null, sessionId: null, sending: false };

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
  $('model').textContent = me.model || '';
  loadSessions();
}

/* ---------- sessions ---------- */

async function loadSessions() {
  const { sessions } = await api('/api/v1/sessions');
  const list = $('sessions');
  list.innerHTML = '';
  for (const s of sessions) {
    const li = document.createElement('li');
    li.textContent = s.title || 'Untitled';
    li.title = li.textContent;
    if (s.id === state.sessionId) li.className = 'active';
    li.addEventListener('click', () => openSession(s.id));
    list.appendChild(li);
  }
}

async function newSession() {
  const sess = await api('/api/v1/sessions', { method: 'POST', body: JSON.stringify({ title: '' }) });
  state.sessionId = sess.id;
  $('messages').innerHTML = '<div class="empty muted">Ask about your media library, or anything else.</div>';
  await loadSessions();
  return sess.id;
}

async function openSession(id) {
  state.sessionId = id;
  const { messages } = await api(`/api/v1/sessions/${id}/messages`);
  render(messages || []);
  await loadSessions();
  closeSidebar();
}

$('new-session').addEventListener('click', () => newSession().catch(showError));
$('menu').addEventListener('click', () => $('sidebar').classList.toggle('open'));
function closeSidebar() { $('sidebar').classList.remove('open'); }

/* ---------- rendering ---------- */

function render(messages) {
  const box = $('messages');
  box.innerHTML = '';
  for (const m of messages) appendMessage(m);
}

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

  const el = document.createElement('div');
  el.className = `msg ${m.role}${m.is_error ? ' error' : ''}`;
  const role = document.createElement('div');
  role.className = 'role';
  role.textContent = m.role;
  const bubble = document.createElement('div');
  bubble.className = 'bubble';
  bubble.textContent = text;
  el.append(role, bubble);
  box.appendChild(el);
  box.scrollTop = box.scrollHeight;
}

function showError(err) {
  appendMessage({ role: 'tool', content: err.message, is_error: true });
}

/* ---------- composer ---------- */

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
  } catch (err) {
    showError(err);
  } finally {
    state.sending = false;
    $('send').disabled = false;
    input.focus();
  }
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
