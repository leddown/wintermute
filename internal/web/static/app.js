// Browser client for Wintermute.
//
// Several views over one API: the chat that has always been here, the tasks,
// CRM and company modules that moved across from the RCSA application, and the
// portfolio that moved across from morpheus. The
// browser declares no client-side tools, so a turn from here either completes
// or reports an error — file operations belong to the desktop harness.
'use strict';

const $ = (id) => document.getElementById(id);
const state = {
  // 'core' is where the app lands: the Core view opens on its Chat pane, which
  // index.html marks active so the first paint needs no JS.
  token: null, sessionId: null, sending: false, view: 'core',
  // The agent a new chat is opened against, and the one being edited in the
  // Agents view. Null means the unscoped assistant, which is what every
  // session was before agents existed.
  chatAgent: null, agents: [], agentAvailable: {}, selectedAgent: null,
  // The backend a chat runs on. Null means the server default — the same
  // "empty is default" the session row and the API already use, so nothing
  // has to invent a name for it. `backends` is the catalogue with health, so
  // the picker can say which ones are actually answering.
  chatBackend: null, backends: [], defaultBackend: '',
  // Whether the open conversation is being written down, and whether it draws
  // on prior ones. Two independent switches, mirroring the session row: a chat
  // that reads the full history but leaves no trace of itself is a valid and
  // useful combination. Both default to on; going off the record is always an
  // explicit act.
  record: true, recall: true,
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
  SystemGauges.start();
  loadSessions().catch(showError);
  // The agent list is fetched at boot rather than on first view, because the
  // chat needs it to say which agent a session is talking to — a question gets
  // a different answer depending on which library is behind it.
  state.defaultBackend = me.default_backend || '';
  api('/api/v1/agents').then((data) => {
    state.agents = data.agents || [];
    state.agentAvailable = data.available || {};
    renderChatControls();
  }).catch(() => { /* agents are optional; the chat works without them */ });
  // Backends come from the catalogue rather than /me, because /me lists only
  // their names and the picker is worth much less without the health: the
  // failure this exists to get out of is a backend that is down, and a menu
  // that will not say which ones those are just moves the guess.
  api('/api/v1/backends').then((data) => {
    state.backends = data.backends || [];
    renderChatControls();
  }).catch(() => { /* the chat still works on the server default */ });
}

/* ---------- view switching ---------- */

// Views load on first activation rather than at boot: opening the chat should
// not cost four extra round trips for panes nobody looked at.
const loaded = new Set();
const loaders = {
  tasks: () => renderTasks(),
  scratch: () => loadPads(),
  company: () => loadCompany(),
  portfolio: () => loadPortfolio(),
  utilities: () => loadUtilities(),
  huginn: () => renderHuginn(),
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
  // Navigating to the chat's own pane is a request to see the conversation
  // full size, so the dock hands it back — the same rule showCorePane()
  // applies when the tab is clicked directly.
  if (name === 'core' && core.pane === 'chat' && dock.open) closeDock();
  $('chat-away').hidden = !(dock.open && name === 'core' && core.pane === 'chat');
  // The activity gauges poll on a timer. Leaving the view has to stop it, or
  // the server keeps being asked for /proc readings nobody is looking at.
  if (name !== 'utilities') stopActivityPolling();
  // Same rule for the download poll, which lives in Huginn now: leaving the
  // view stops it rather than leaving it asking about jobs behind a hidden
  // section. Its own guard only knows which tab is selected, not which view.
  if (name !== 'huginn') stopRepoPolling();
  // The pad saves on a timer, and switching view is quicker than the timer.
  // Leaving it holding unwritten keystrokes is how a scratch pad loses work.
  if (name !== 'scratch') flushPad();
  if (!loaded.has(name) && loaders[name]) {
    loaded.add(name);
    loaders[name]().catch((err) => { loaded.delete(name); showError(err); });
  }
  // A render replaces the DOM the chaos theme had glitched, so re-sample. This
  // is a no-op on every other theme.
  WintermuteChaos.repaint();
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

/* ---------- core view: the chat and the agents that scope it ---------- */
//
// Two groups over two panes, like the Company view, except the swap covers the
// sidebar as well: neither group has fixed tabs, they have live lists — the
// sessions and the agents — so each group's list travels with its pane.
//
// The chat is not lazy: it loads at boot because it is where the app lands.
// Only the agent list waits for its first open.
const core = { pane: 'chat', agentsLoaded: false };

function showCorePane(name) {
  core.pane = name;
  for (const li of document.querySelectorAll('.core-tabs li')) {
    li.classList.toggle('active', li.dataset.pane === name);
  }
  for (const node of document.querySelectorAll('.view[data-view="core"] .core-pane')) {
    node.hidden = node.dataset.pane !== name;
  }
  // Arriving at the chat tab while the dock holds the transcript is a request
  // to look at the conversation, and the pane it belongs in is now on screen —
  // so the dock hands it back rather than leaving the tab showing a stand-in.
  if (name === 'chat' && dock.open) closeDock();
  $('chat-away').hidden = !(name === 'chat' && dock.open);
  if (name === 'agents' && !core.agentsLoaded) {
    core.agentsLoaded = true;
    loadAgents().catch((err) => { core.agentsLoaded = false; showError(err); });
  }
}

// Whether the transcript is on screen. showError() writes a failed turn into
// the transcript rather than a toast, but only when the user can see it — and
// in the dock it is visible over every view, not just Core.
function chatVisible() {
  return dock.open || (state.view === 'core' && core.pane === 'chat');
}

/* ---------- chat dock ---------- */
//
// Core → Chat, slid out from the right over whatever view is open, so a
// question can be asked without navigating away from the thing that prompted
// it.
//
// The transcript is not duplicated. #chat is *moved* into the dock and moved
// back on close, which is what keeps one composer, one submit handler, one
// scroll position and one set of listeners — a second copy would mean either
// duplicate ids or rewriting every $('messages') in the file. Listeners are
// bound to the elements themselves, so they survive the move untouched.
//
// Because the element can only be in one place, the dock and the Core chat
// pane are mutually exclusive: while the dock has it, that pane shows
// #chat-away in its place.
const dock = { open: false, home: null, next: null };

function openDock() {
  if (dock.open) return;
  const chat = $('chat');
  // Remember exactly where it came from, so closing puts it back in order
  // rather than appending it after the agents pane.
  dock.home = chat.parentElement;
  dock.next = chat.nextElementSibling;
  dock.open = true;

  chat.hidden = false;
  $('dock-slot').append(chat);
  $('dock').hidden = false;
  // One frame between unhiding and the class, or the panel is already at its
  // final position when the transition is applied and nothing slides.
  requestAnimationFrame(() => $('dock').classList.add('open'));
  $('dock-toggle').setAttribute('aria-expanded', 'true');
  $('chat-away').hidden = !(state.view === 'core' && core.pane === 'chat');
  // The composer strip travels with #chat, so it needs no re-render; the dock
  // head says which agent is answering, which the strip alone would not make
  // obvious once it is floating over an unrelated view.
  const agent = (state.agents || []).find((a) => a.id === state.chatAgent);
  $('dock-agent').textContent = agent ? agent.name : '';
  $('input').focus();
}

function closeDock() {
  if (!dock.open) return;
  dock.open = false;
  const chat = $('chat');
  dock.home.insertBefore(chat, dock.next);
  // Back in the pane group, visibility is the tab's business again.
  chat.hidden = core.pane !== 'chat';
  $('dock').classList.remove('open');
  $('dock-toggle').setAttribute('aria-expanded', 'false');
  $('chat-away').hidden = true;
  // Hidden only after the slide finishes, or it vanishes instead of leaving.
  setTimeout(() => { if (!dock.open) $('dock').hidden = true; }, 200);
}

function toggleDock() {
  if (dock.open) closeDock();
  else openDock();
}

$('dock-toggle').addEventListener('click', toggleDock);
$('dock-close').addEventListener('click', closeDock);
$('chat-return').addEventListener('click', closeDock);
document.addEventListener('keydown', (e) => {
  // Esc closes the dock, but not while a dialog is doing its own Esc handling.
  if (e.key === 'Escape' && dock.open && !document.querySelector('dialog[open]')) closeDock();
});

for (const li of document.querySelectorAll('.core-tabs li')) {
  li.addEventListener('click', () => {
    showCorePane(li.dataset.pane);
    closeSidebar();
  });
}

/* ---------- system gauges ---------- */
//
// Live CPU / network / disk dials at the bottom-left, brought across from
// morpheus. The numbers are whole-machine, read from /proc on the server: they
// cover every process and every user, not just wintermute. That is the point —
// the question they answer is "is this box busy, and with what", and on a host
// running a local model that is what you want in front of you while a turn
// sits there apparently doing nothing.
//
// This polls the same /api/v1/utilities/resources the Activity tab uses. The
// sampler behind it is one shared instance averaging over a five-second
// window, so a second reader costs a few /proc reads and cannot skew what the
// Activity tab reports.
const SystemGauges = (() => {
  // The server averages each rate over a few seconds anyway, so polling faster
  // would show the same number more often rather than a more current one.
  const pollInterval = 3000;
  let timer = null;

  // Network and disk have no natural ceiling — a gigabit link and a slow USB
  // disk are orders of magnitude apart — so each ring fills relative to the
  // highest rate seen since the page loaded. The number in the middle is
  // always the real measurement; only the ring is relative.
  const peaks = { net: 1, disk: 1 };

  // Compact enough for a dial centre: megabytes once there are any, kilobytes
  // below that.
  function rate(bytesPerSec) {
    const mb = bytesPerSec / (1024 * 1024);
    if (mb >= 10) return String(Math.round(mb));
    if (mb >= 1) return mb.toFixed(1);
    if (bytesPerSec >= 1024) return `${Math.round(bytesPerSec / 1024)}k`;
    return '0';
  }

  const mb = (n) => (n / (1024 * 1024)).toFixed(1);

  function paint(s) {
    const box = $('system-gauges');
    if (!box) return;
    box.hidden = false;

    const net = (s.net_rx_bytes_per_sec || 0) + (s.net_tx_bytes_per_sec || 0);
    const disk = (s.disk_read_bytes_per_sec || 0) + (s.disk_write_bytes_per_sec || 0);
    peaks.net = Math.max(peaks.net, net);
    peaks.disk = Math.max(peaks.disk, disk);

    const dials = {
      cpu: {
        fill: Math.min((s.cpu_percent || 0) / 100, 1),
        value: s.warming ? '–' : `${Math.round(s.cpu_percent || 0)}%`,
        title: `CPU ${(s.cpu_percent || 0).toFixed(1)}% busy across all cores, all processes`,
      },
      net: {
        fill: net / peaks.net,
        value: s.warming ? '–' : rate(net),
        title: `Network ${mb(net)} MB/s — down ${mb(s.net_rx_bytes_per_sec || 0)}, up ${mb(s.net_tx_bytes_per_sec || 0)}`,
      },
      disk: {
        fill: disk / peaks.disk,
        value: s.warming ? '–' : rate(disk),
        title: `Disk ${mb(disk)} MB/s — read ${mb(s.disk_read_bytes_per_sec || 0)}, write ${mb(s.disk_write_bytes_per_sec || 0)}`,
      },
    };

    for (const [key, d] of Object.entries(dials)) {
      const gauge = box.querySelector(`[data-gauge="${key}"]`);
      if (!gauge) continue;
      gauge.querySelector('.resource-dial').style.setProperty(
        '--fill', `${(Math.min(Math.max(d.fill, 0), 1) * 100).toFixed(1)}%`);
      gauge.querySelector('.resource-value').textContent = d.value;
      gauge.title = s.warming ? 'Measuring…' : d.title;
    }
  }

  async function tick() {
    // A backgrounded tab is not worth polling for, but the timer keeps running
    // so it resumes the moment the tab comes back.
    if (document.hidden) return;
    try {
      paint(await api('/api/v1/utilities/resources'));
    } catch {
      // Leave the last values on screen. These are a diagnostic garnish that
      // sits over every view; a failure here must not disturb any of them —
      // and must never reach showError(), which would write it into the chat.
    }
  }

  function start() {
    if (timer) return;
    tick();
    timer = setInterval(tick, pollInterval);
  }

  return { start };
})();

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

function backendOfSession(id) {
  const found = sessionIndex.find((s) => s.id === id);
  return found ? (found.backend || null) : null;
}

async function loadSessions() {
  const { sessions } = await api('/api/v1/sessions');
  sessionIndex = sessions || [];
  const list = $('sessions');
  list.innerHTML = '';
  for (const s of sessions) {
    const label = s.agent_id ? `${s.title || 'Untitled'} · ${s.agent_id}` : (s.title || 'Untitled');
    // The row opens the session; the × deletes it. stopPropagation is what
    // keeps the delete from also opening the conversation it just removed.
    const del = el('button', {
      class: 'session-del', text: '×', type: 'button',
      title: `Delete "${s.title || 'Untitled'}"`,
      'aria-label': `Delete ${s.title || 'Untitled'}`,
      onclick: (e) => {
        e.stopPropagation();
        deleteSession(s);
      },
    });
    const li = el('li', {
      title: label,
      class: s.id === state.sessionId ? 'active' : '',
      onclick: () => openSession(s.id).catch(showError),
    }, el('span', { class: 'session-label', text: label }), del);
    list.append(li);
  }
}

// Discarding a conversation takes its messages and its audit rows with it, so
// the prompt says so rather than asking about "a session" — the tool calls it
// recorded are the part someone might actually want back, and they are the
// part a title does not hint at.
function deleteSession(s) {
  const name = s.title ? `"${s.title}"` : 'this untitled conversation';
  confirmDelete(`${name} and its transcript and tool audit trail`, async () => {
    await api(`/api/v1/sessions/${s.id}`, { method: 'DELETE' });
    // Deleting the open conversation leaves the pane showing a transcript that
    // no longer exists, so clear it and fall back to the empty state.
    if (state.sessionId === s.id) {
      state.sessionId = null;
      $('messages').replaceChildren(el('div', { class: 'empty muted', text: emptyChatHint() }));
    }
    await loadSessions();
  });
}

async function newSession() {
  const sess = await api('/api/v1/sessions', {
    method: 'POST',
    body: JSON.stringify({
      title: '',
      agent: state.chatAgent || '',
      backend: state.chatBackend || '',
    }),
  });
  state.sessionId = sess.id;
  state.chatAgent = sess.agent_id || null;
  // An agent can pin its own backend, so the session comes back naming what it
  // actually got, which is not always what was asked for. Follow the answer.
  state.chatBackend = sess.backend || null;
  // A new session is always created on the record. If the operator asked for
  // something else before there was a session to ask it of, apply it now —
  // otherwise the choice they made would silently not take effect on the very
  // first message, which is the worst possible moment for this setting to be
  // wrong.
  const wantRecord = state.record;
  const wantRecall = state.recall;
  state.record = sess.record !== false;
  state.recall = sess.recall !== false;
  if (wantRecord !== state.record || wantRecall !== state.recall) {
    const updated = await api(`/api/v1/sessions/${sess.id}/memory`, {
      method: 'PATCH',
      body: JSON.stringify({ record: wantRecord, recall: wantRecall }),
    });
    state.record = updated.record !== false;
    state.recall = updated.recall !== false;
  }
  renderChatControls();
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
  return 'Ask about your models, your tasks, or anything else.';
}

async function openSession(id) {
  state.sessionId = id;
  const known = (state.agents || []).find((a) => a.id === agentOfSession(id));
  state.chatAgent = known ? known.id : agentOfSession(id);
  state.chatBackend = backendOfSession(id);
  const opened = sessionIndex.find((x) => x.id === id);
  state.record = !opened || opened.record !== false;
  state.recall = !opened || opened.recall !== false;
  renderChatControls();
  const { messages } = await api(`/api/v1/sessions/${id}/messages`);
  $('messages').innerHTML = '';
  for (const m of messages || []) appendMessage(m);
  await loadSessions();
  closeSidebar();
}

// The strip under the composer: which agent a new chat opens against, and
// which backend the chat runs on. Both are rendered together because they
// share the one holder, and either can be absent — a server with no agents
// configured still gets the backend picker, and vice versa.
/* ---------- word cloud ---------- */
//
// Ready-made phrasings, shown only when the backend answering is a small one.
//
// A 7B model does not fail at tasks because it cannot do them; it fails on the
// wording. Asked for something "due 18/8/2026" it sends that string to a tool
// that wants YYYY-MM-DD, is told so, tries the same thing again, and burns the
// turn's tool budget on one date. The tools now normalise what they can, but
// the cheaper fix is to not make the model guess: these phrase the request the
// way the tools describe themselves, with a real ISO date already in place.
//
// A large model needs none of this and would only be crowded by it, so the
// strip appears only under smallBackendBytes.

// Under this, a backend is treated as small enough to need help with phrasing.
// Decimal GB, matching how the sizes are written in backends.json.
const smallBackendBytes = 8 * 1000 * 1000 * 1000;

// Placeholders are wrapped in guillemets so inserting a phrase can select the
// first one for typing over. They are not sent to the model as-is: the user
// either replaces them or edits the line.
// Every phrase here names something the task tools can actually do. That is
// the constraint worth keeping: the assistant's reach is the tasks group and
// nothing else (see WINTERMUTE_ASSISTANT_TOOLS), so a suggestion about
// invoices or holdings would be an invitation to a failure.
function wordCloudPhrases() {
  const soon = new Date(Date.now() + 7 * 86400000).toISOString().slice(0, 10);
  const month = new Date().toISOString().slice(0, 7);
  return [
    // Capture
    ['Add task', `Add task «Fix car» to list «Home» due ${soon}`],
    ['New list', 'Create list «Home» with tasks «Fix car», «Book MOT»'],
    ['List + tasks', 'Create list «Trip» with tasks «passport», «insurance», «currency»'],
    ['Note', 'Write a note: «ring the garage back»'],
    // Read
    ['Agenda', 'Show my agenda'],
    ['Open tasks', 'List tasks with status todo'],
    ['Tasks on a list', 'List tasks on list «Home»'],
    ['All lists', 'Show all my to-do lists'],
    ['Search', 'Find tasks matching «car»'],
    ['Finished', 'List tasks with status done'],
    ['My notes', 'List my notes'],
    // Change a task
    ['Complete', 'Mark task #«12» done'],
    ['In progress', 'Set task #«12» status to doing'],
    ['Reschedule', `Change the due date of task #«12» to ${soon}`],
    ['Clear due date', 'Clear the due date on task #«12»'],
    ['Priority', 'Set task #«12» priority to high'],
    ['Rename', 'Change the title of task #«12» to «Fix car brakes»'],
    ['Add notes', 'Add notes to task #«12»: «ring the garage first»'],
    ['Delete', 'Delete task #«12»'],
    // Change a list
    ['Rename list', 'Rename list #«2» to «Garage»'],
    ['Archive list', 'Archive list #«2»'],
    // Notes
    ['Note done', 'Mark note #«5» done'],
    ['Delete note', 'Delete note #«5»'],
    // Calendar
    ['Schedule', `Schedule «MOT test» on ${soon}`],
    ['This month', `Show my calendar for ${month}`],
    ['Delete event', 'Delete calendar event #«3»'],
  ];
}

// activeBackend is the one this chat will actually run on: the session's
// choice, or the server default when it has none.
function activeBackend() {
  const name = state.chatBackend || state.defaultBackend;
  return (state.backends || []).find((b) => b.name === name) || null;
}

function renderWordCloud() {
  const box = $('word-cloud');
  if (!box) return;
  const backend = activeBackend();
  // Zero or missing means undeclared, which is unknown rather than small — a
  // backend nobody annotated must not be treated as the worst case.
  const bytes = backend ? Number(backend.memory_bytes) || 0 : 0;
  if (bytes <= 0 || bytes >= smallBackendBytes) {
    box.classList.remove('open');
    box.hidden = true;
    box.replaceChildren();
    return;
  }

  const chips = wordCloudPhrases().map(([label, phrase]) => {
    const chip = el('button', {
      type: 'button', class: 'chip', text: label, title: phrase,
      onclick: () => insertPhrase(phrase),
    });
    return chip;
  });
  box.replaceChildren(
    el('span', { class: 'muted word-cloud-note', text:
      `${backend.name} is ${backend.memory || 'small'} — these phrasings match what the tools expect:` }),
    ...chips);
  box.hidden = false;
  // Unhide first, then animate on the next frame. Setting both at once leaves
  // the strip already at its resting position when the transition is applied,
  // and nothing moves — the same reason openDock() waits a frame.
  requestAnimationFrame(() => box.classList.add('open'));
}

// insertPhrase puts a phrase in the composer and selects its first
// placeholder, so the next keystroke replaces it rather than appending to it.
function insertPhrase(phrase) {
  const input = $('input');
  input.value = phrase;
  input.style.height = 'auto';
  input.style.height = `${Math.min(input.scrollHeight, 200)}px`;
  input.focus();
  const open = phrase.indexOf('«');
  const close = phrase.indexOf('»');
  if (open >= 0 && close > open) input.setSelectionRange(open + 1, close);
  else input.setSelectionRange(phrase.length, phrase.length);
}

function renderChatControls() {
  const holder = $('chat-agent');
  if (!holder) return;
  const parts = [];
  if (state.agents.length) {
    parts.push(el('span', { class: 'muted', text: 'New chat as' }), chatAgentSelect());
  }
  if (state.backends.length) {
    parts.push(el('span', { class: 'muted', text: 'Backend' }), chatBackendSelect());
  }
  parts.push(memoryControls());
  holder.replaceChildren(...parts);
  // The whole chat pane is marked, not just the badge. Whether this
  // conversation is being kept is the kind of thing that is bad to be wrong
  // about in either direction, so it changes the look of the room rather than
  // hiding in a control strip.
  const chat = $('chat');
  if (chat) chat.classList.toggle('off-the-record', !state.record);
  // The strip and the cloud answer the same question — which model is this
  // going to — so they are rebuilt together whenever the backend changes.
  renderWordCloud();
}

// The recording state, shown as a word rather than a checkbox.
//
// A checkbox says what will happen when you click it; this has to say what is
// true right now, at a glance, without being read carefully. "Recording" and
// "Off the record" are unambiguous in a way that a ticked or unticked box next
// to the word "ephemeral" is not.
function memoryControls() {
  const recording = state.record;
  const badge = el('button', {
    class: `memory-badge ${recording ? 'on' : 'off'}`,
    type: 'button',
    title: recording
      ? 'This conversation is being saved. Click to go off the record — the turns already saved will be deleted.'
      : 'This conversation is not being saved and will be lost when the server restarts. Click to start recording from here on.',
    onclick: () => toggleRecording().catch(showError),
  }, [
    el('span', { class: 'memory-dot', text: recording ? '\u25cf' : '\u25cb' }),
    el('span', { text: recording ? 'Recording' : 'Off the record' }),
  ]);

  const recall = el('label', {
    class: 'memory-recall',
    title: 'Whether this conversation can draw on what was said in earlier ones.',
  }, [
    el('input', {
      type: 'checkbox',
      checked: state.recall,
      onchange: (e) => setMemory(state.record, e.target.checked).catch(showError),
    }),
    el('span', { text: 'Use past chats' }),
  ]);

  return el('span', { class: 'memory-controls' }, [badge, recall]);
}

// Going off the record deletes what has already been written for this
// conversation, so it asks first. Going back on the record does not, because
// it destroys nothing — it only starts keeping what comes next.
async function toggleRecording() {
  if (state.record) {
    const ok = window.confirm(
      'Go off the record?\n\n' +
      'The turns already saved for this conversation will be deleted, and nothing ' +
      'said from here on will be written down. The chat itself keeps working, but ' +
      'it will be lost when the server restarts.\n\nThis cannot be undone.');
    if (!ok) return;
  }
  await setMemory(!state.record, state.recall);
}

async function setMemory(record, recall) {
  if (!state.sessionId) {
    // No session yet: remember the choice for the one the next message opens.
    state.record = record;
    state.recall = recall;
    renderChatControls();
    return;
  }
  const sess = await api(`/api/v1/sessions/${state.sessionId}/memory`, {
    method: 'PATCH',
    body: JSON.stringify({ record, recall }),
  });
  state.record = sess.record !== false;
  state.recall = sess.recall !== false;
  renderChatControls();
  await loadSessions();
}

// The agent picker. Changing it affects the *next* session rather than the
// current one: a transcript belongs to the agent it was held with, and
// re-pointing it halfway would leave the earlier turns unexplainable.
function chatAgentSelect() {
  return el('select', {
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
}

// The backend picker. Unlike the agent, this *does* re-point the open session,
// via PATCH .../model — switching mid-conversation is the point, and is how a
// turn stuck on a local model gets escalated to a stronger one without losing
// the transcript. With no session open there is nothing to patch yet, so the
// choice is just remembered for the next one.
//
// Each option carries the backend's health, because a name alone cannot
// distinguish the backend that is serving from the one refusing connections,
// and picking the dead one is precisely the mistake this is here to prevent.
function chatBackendSelect() {
  const label = (b) => {
    const model = b.model ? ` · ${b.model}` : '';
    const status = b.status === 'ok' ? '' : ` (${b.status})`;
    return `${b.name}${model}${status}`;
  };
  const def = state.defaultBackend ? `Server default (${state.defaultBackend})` : 'Server default';
  return el('select', {
    id: 'chat-backend-select',
    title: 'Which backend answers this chat',
    onchange: (e) => { setChatBackend(e.target.value || null).catch(showError); },
  }, [
    el('option', { value: '', text: def }),
    ...state.backends.map((b) => {
      const opt = el('option', { value: b.name, text: label(b) });
      if (b.name === state.chatBackend) opt.selected = true;
      return opt;
    }),
  ]);
}

async function setChatBackend(name) {
  state.chatBackend = name;
  if (!state.sessionId) return;
  // The model is deliberately left empty: it names a model *within* a backend,
  // and carrying the old one across would ask the new backend for something it
  // has probably never heard of. Empty means the backend's own default.
  await api(`/api/v1/sessions/${state.sessionId}/model`, {
    method: 'PATCH',
    body: JSON.stringify({ backend: name || '', model: '' }),
  });
  const found = sessionIndex.find((s) => s.id === state.sessionId);
  if (found) { found.backend = name || ''; found.model = ''; }
  toast(name ? `This chat now runs on ${name}` : 'This chat now runs on the server default');
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

  // Every message gets a way out of the transcript. The transcript is a log —
  // it is replayed to the model verbatim and nothing here may edit it — so
  // working over a reply means taking a copy somewhere else, and the pad next
  // door is that somewhere. See sendToPad().
  const label = el('div', { class: 'role' },
    el('span', { text: m.role }),
    el('button', {
      class: 'to-pad', type: 'button', text: '→ pad',
      title: 'Copy this message into the scratch pad',
      onclick: () => sendToPad(text),
    }));

  box.append(el('div', { class: `msg ${m.role}${m.is_error ? ' error' : ''}` },
    label,
    el('div', { class: 'bubble', text })));
  box.scrollTop = box.scrollHeight;
}

// The placeholder that stands in for the reply while the turn is in flight.
// A turn against a local model can sit for a minute or more, and without this
// the transcript looks like the message was swallowed: the composer clears,
// and then nothing happens for long enough to try again.
//
// Returned rather than looked up later so the caller removes exactly the node
// it added, whatever else has arrived in the meantime.
function appendPending() {
  const box = $('messages');
  const empty = box.querySelector('.empty');
  if (empty) empty.remove();
  const node = el('div', { class: 'msg assistant pending' },
    el('div', { class: 'role', text: 'assistant' }),
    el('div', { class: 'bubble' },
      el('span', { class: 'dots' }, el('i'), el('i'), el('i')),
      el('span', { class: 'pending-status' })));
  box.append(node);
  box.scrollTop = box.scrollHeight;
  return node;
}

/* ---------- turn progress ---------- */
//
// A POST to /messages does not return until the whole turn is finished, and a
// turn can run the model several times over with tool calls in between. From
// the browser that is one long silence, and three dots cannot tell "thinking
// hard" apart from "the backend died ten seconds ago".
//
// There is nothing to invent here, because the server already writes as it
// goes: the agent loop appends each assistant message and each tool result to
// SQLite before the next iteration (it has to — the transcript is replayed
// from the database every time round). So the work so far is readable *during*
// a turn, and polling it is a real progress feed rather than a guess.
//
// It polls /progress rather than /messages. Reading the transcript to look at
// the end of it means the server loads every message with its content and its
// thinking blocks, serialises the lot, and the browser parses it and throws
// all but the last row away — several times a minute, on an object that grows
// with the conversation. /progress answers the same question with indexed
// counts and one row, and stays the same size however long the session runs.
//
// The poll doubles as the liveness check the numbers alone would not give: if
// it comes back, the server is answering, and if it stops the status says so
// instead of leaving a frozen count that looks the same as a hang.

const TURN_TICK_MS = 1000;   // how often the elapsed figure moves
const TURN_POLL_MS = 4000;   // how often the transcript is re-read
// How long without a new message before the status stops implying progress.
// Comfortably longer than a fast tool call, short enough to notice a stall.
const TURN_QUIET_MS = 20000;

function elapsedLabel(ms) {
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
}

// The server counts the steps — everything after the last user message — so
// this only has to put words to them.
function describeTurn(p) {
  const steps = p.steps || 0;
  if (!p.last_role || p.last_role === 'user') return 'sent — waiting for the model';
  if (p.last_role === 'assistant' && p.tools && p.tools.length) {
    return `step ${steps} · running ${p.tools.join(', ')}`;
  }
  if (p.last_role === 'tool') return `step ${steps} · tool returned, back to the model`;
  return `step ${steps} · writing the reply`;
}

// watchTurn drives the status line under the dots until the caller stops it.
// Returns the stop function; calling it twice is harmless.
function watchTurn(node, sessionId) {
  const label = node.querySelector('.pending-status');
  const started = Date.now();
  let changedAt = Date.now();
  let seen = -1;
  let note = 'sent — waiting for the model';
  let warn = '';

  function paint() {
    const quiet = Date.now() - changedAt;
    const stalled = quiet > TURN_QUIET_MS && !warn
      ? ` · quiet for ${elapsedLabel(quiet)}`
      : '';
    label.textContent = `${elapsedLabel(Date.now() - started)} · ${note}${warn}${stalled}`;
  }

  async function poll() {
    try {
      const progress = await api(`/api/v1/sessions/${sessionId}/progress`);
      if (progress.count !== seen) {
        seen = progress.count;
        changedAt = Date.now();
      }
      note = describeTurn(progress);
      warn = '';
    } catch {
      // Only the progress read failed — the turn request itself is still open,
      // so this is a warning rather than a reason to give up on the turn.
      warn = ' · server not answering, still retrying';
    }
    paint();
  }

  paint();
  const tick = setInterval(paint, TURN_TICK_MS);
  const timer = setInterval(() => { poll(); }, TURN_POLL_MS);
  // One read straight away, so the first status is real rather than a guess
  // that sits there for four seconds.
  poll();

  return () => { clearInterval(tick); clearInterval(timer); };
}

function showError(err) {
  if (chatVisible()) appendMessage({ role: 'tool', content: err.message, is_error: true });
  else toast(err.message, true);
  // "internal error" is what the server says when it is refusing to leak
  // detail, and on its own it is unactionable. The detail is kept server-side
  // and readable — so fetch it rather than making the operator go and read
  // journalctl over SSH to find out what they just did wrong.
  if (/internal error/i.test(err.message || '')) revealServerError();
}

// Pulls the newest recorded failure and shows it. Best-effort throughout: this
// runs while something has already gone wrong, and a failure to explain a
// failure must not become a second error on screen.
let lastRevealedError = 0;
async function revealServerError() {
  try {
    const { errors } = await api('/api/v1/admin/errors?limit=1');
    const newest = (errors || [])[0];
    if (!newest || newest.id === lastRevealedError) return;
    lastRevealedError = newest.id;

    const detail = `${newest.op}: ${newest.error}`;
    if (chatVisible()) appendMessage({ role: 'tool', content: detail, is_error: true });
    else toast(detail, true);
  } catch {
    // Nothing to add. The original error is already on screen.
  }
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

  let pending = null;
  let stopWatch = null;
  try {
    // The session has to exist *before* the message is echoed. newSession()
    // resets the transcript to its empty-state hint, so echoing first meant
    // the opening question of every conversation was wiped a moment after it
    // appeared — the reply arrived with nothing above it to answer.
    if (!state.sessionId) await newSession();
    appendMessage({ role: 'user', content: text });
    pending = appendPending();
    stopWatch = watchTurn(pending, state.sessionId);
    // client_tools is intentionally empty: a browser executes nothing locally.
    const turn = await api(`/api/v1/sessions/${state.sessionId}/messages`, {
      method: 'POST',
      body: JSON.stringify({ text, client_tools: [] }),
    });
    stopWatch();
    stopWatch = null;
    pending.remove();
    pending = null;
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
    // Both have to go whatever happened, including a failure before the
    // request was ever made — otherwise the indicator spins under a dead turn
    // and the poller keeps asking about a session nobody is waiting on.
    if (stopWatch) stopWatch();
    if (pending) pending.remove();
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
  showCorePane('chat');
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
    // The row opens the list; the × deletes it, the same shape the session
    // rows use. Deleting was previously only reachable from the pane header
    // after selecting a list, which is a strange place to look for it when
    // the lists themselves are right here.
    const parts = [
      el('span', { class: 'list-label', text: `${l.title}${l.archived ? ' (archived)' : ''}` }),
      el('span', { class: 'muted list-count', text: `${l.done_count}/${l.task_count}` }),
    ];
    // The notes inbox is the server's own list and the note tools store
    // everything in it, so it gets no delete control. The server refuses it
    // too — this only keeps the UI from offering something it would refuse.
    if (!l.slug) {
      parts.push(el('button', {
        class: 'session-del', text: '×', type: 'button',
        title: `Delete "${l.title}"`,
        'aria-label': `Delete list ${l.title}`,
        onclick: (e) => {
          e.stopPropagation();
          deleteList(l);
        },
      }));
    }
    box.append(el('li', {
      class: tasks.scope === 'list' && tasks.listId === l.id ? 'active' : '',
      title: l.description || l.title,
      onclick: () => { tasks.scope = 'list'; tasks.listId = l.id; renderTasks().catch(showError); },
    }, ...parts));
  }
  for (const li of document.querySelectorAll('#task-views li')) {
    li.classList.toggle('active', tasks.scope === li.dataset.scope);
    li.onclick = () => { tasks.scope = li.dataset.scope; tasks.listId = 0; renderTasks().catch(showError); };
  }
}

$('show-archived').addEventListener('change', () => renderTasks().catch(showError));
$('show-done').addEventListener('change', () => renderTasks().catch(showError));

$('new-list').addEventListener('click', () => {
  openEditor('New list', [
    { name: 'title', label: 'Title' },
    { name: 'description', label: 'Description', type: 'textarea' },
  ], async (v) => {
    await api('/api/v1/todo/lists', { method: 'POST', body: JSON.stringify(v) });
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
    await renderTasks();
    toast('List saved.');
  });
});

// deleteList backs both the × on a sidebar row and the button in the pane
// header, so the two cannot drift into confirming differently.
//
// The count is named in the prompt rather than left to "and its tasks":
// deleting a list takes everything on it and there is no undo, so the number
// is the one fact worth knowing before agreeing.
function deleteList(list) {
  if (!list) return;
  const count = list.task_count || 0;
  const what = count === 0
    ? `the empty list "${list.title}"`
    : `the list "${list.title}" and its ${count} task${count === 1 ? '' : 's'}`;
  confirmDelete(what, async () => {
    await api(`/api/v1/todo/lists/${list.id}`, { method: 'DELETE' });
    // Only fall back to the agenda when the list being deleted is the one on
    // screen; deleting some other row should leave the view where it is.
    if (tasks.scope === 'list' && tasks.listId === list.id) {
      tasks.scope = 'agenda';
      tasks.listId = 0;
    }
    await renderTasks();
  });
}

$('delete-list').addEventListener('click', () => {
  deleteList(tasks.lists.find((l) => l.id === tasks.listId));
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

  // The lists are refreshed here, at the top, rather than at the end of each
  // branch. The pane header names the list being looked at and reads that name
  // out of tasks.lists, so loading them afterwards renders the header from the
  // previous load: rename a list and the sidebar row changes while the heading
  // above the tasks still says the old name. Reloading first also removes the
  // race the lazy loader had, which ran loadLists() and renderTasks() together
  // and let whichever request answered first decide what the header said.
  await loadLists();

  // Notes and the calendar build their own controls into the pane rather than
  // borrowing the task quick-add: a note takes a date but no priority, and the
  // calendar takes a month.
  if (tasks.scope === 'notes' || tasks.scope === 'calendar') {
    await (tasks.scope === 'notes' ? renderNotes(body) : renderCalendar(body));
    return;
  }

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

/* ---------- notes ---------- */

// Notes came across from morpheus with the calendar below. On the server they
// are tasks on one reserved list; here they are read as a stream rather than a
// checklist — newest at the top, and the date shown as where the note lands on
// the calendar rather than as a deadline that can be overdue.

// A note too long for a task title keeps its whole text in the notes field.
// See noteFields in internal/todo/service.go for why it is not truncated.
const noteText = (n) => (n.notes ? n.notes : n.title);

async function renderNotes(body) {
  $('tasks-title').textContent = 'Notes';
  const { notes } = await api('/api/v1/todo/notes');

  body.innerHTML = '';
  body.append(noteAddForm(), importBox({
    path: '/api/v1/todo/notes/import',
    columns: [
      ['body', 'required — the note text'],
      ['event_date', 'optional — YYYY-MM-DD, to put the note on the calendar'],
    ],
    example: 'body,event_date\nBuy milk,\nDentist appointment,2026-07-14',
  }));

  if (!notes.length) {
    body.append(el('div', { class: 'empty muted', text: 'No notes yet.' }));
    return;
  }
  for (const n of notes) body.append(noteRow(n));
}

function noteAddForm() {
  const text = el('input', { type: 'text', placeholder: 'Write a note…' });
  const date = el('input', { type: 'date', title: 'Optional: put this note on the calendar' });

  const add = async () => {
    if (!text.value.trim()) return;
    try {
      await api('/api/v1/todo/notes', {
        method: 'POST',
        body: JSON.stringify({ body: text.value.trim(), event_date: date.value }),
      });
      text.value = '';
      date.value = '';
      await renderTasks();
    } catch (err) {
      showError(err);
    }
  };
  text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); add(); }
  });

  return el('div', { class: 'row-form' }, text, date, el('button', { text: 'Add', onclick: add }));
}

function noteRow(n) {
  const done = n.status === 'done';

  const check = el('input', { type: 'checkbox' });
  check.checked = done;
  check.addEventListener('change', async () => {
    check.disabled = true;
    try {
      await api(`/api/v1/todo/notes/${n.id}/status`, {
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

  const meta = [new Date(n.created_at).toLocaleString()];
  if (n.due_date) meta.push(`📅 ${n.due_date}`);

  return el('div', { class: `task${done ? ' done' : ''}` },
    check,
    el('div', { class: 'body' },
      el('div', { class: 't', text: noteText(n) }),
      el('div', { class: 'meta', text: meta.join(' · ') })),
    el('div', { class: 'row-actions' },
      el('button', {
        class: 'ghost-btn danger', text: '✕',
        onclick: () => confirmDelete('this note', async () => {
          await api(`/api/v1/todo/notes/${n.id}`, { method: 'DELETE' });
          await renderTasks();
        }),
      })));
}

/* ---------- calendar ---------- */

// A month grid over everything dated: events, and the tasks and notes due in
// the month. Only events are editable here — a cell is not the place to change
// what a task says, and both have a view that is.

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
// month is the first day of the month on screen, kept across re-renders so
// adding an event does not jump the view back to today.
const calendar = { month: null };

const pad2 = (n) => String(n).padStart(2, '0');
const dayKey = (d) => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;

function firstOfThisMonth() {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), 1);
}

async function renderCalendar(body) {
  if (!calendar.month) calendar.month = firstOfThisMonth();
  const first = calendar.month;
  const next = new Date(first.getFullYear(), first.getMonth() + 1, 1);

  $('tasks-title').textContent = first.toLocaleDateString(undefined, { month: 'long', year: 'numeric' });

  // The window is asked for as from/to rather than as a month because the
  // server's month and the browser's could disagree by a day either side.
  const cal = await api(`/api/v1/todo/calendar?from=${dayKey(first)}&to=${dayKey(next)}`);

  body.innerHTML = '';
  body.append(
    calendarToolbar(),
    eventAddForm(),
    importBox({
      path: '/api/v1/todo/events/import',
      columns: [
        ['title', 'required — what the event is'],
        ['start', 'required — YYYY-MM-DD for all day, or an RFC3339 timestamp'],
        ['end', 'optional — same formats as start'],
        ['description', 'optional — free text'],
        ['all_day', 'optional — true/false'],
      ],
      example: 'title,start,end,description,all_day\n'
        + 'Team offsite,2026-07-20,,Annual planning,true\n'
        + 'Standup,2026-07-21T09:00:00Z,2026-07-21T09:15:00Z,,false',
    }),
    calendarGrid(cal, first),
  );
}

function calendarToolbar() {
  const go = (first) => { calendar.month = first; renderTasks().catch(showError); };
  const shift = (months) => go(new Date(calendar.month.getFullYear(), calendar.month.getMonth() + months, 1));

  return el('div', { class: 'cal-toolbar' },
    el('button', { class: 'ghost-btn', 'aria-label': 'Previous month', text: '‹', onclick: () => shift(-1) }),
    el('button', { class: 'ghost-btn', text: 'Today', onclick: () => go(firstOfThisMonth()) }),
    el('button', { class: 'ghost-btn', 'aria-label': 'Next month', text: '›', onclick: () => shift(1) }));
}

function eventAddForm() {
  const title = el('input', { type: 'text', placeholder: 'Event title' });
  const date = el('input', { type: 'date' });
  const desc = el('input', { type: 'text', placeholder: 'Description (optional)' });

  const save = async () => {
    if (!title.value.trim() || !date.value) {
      toast('An event needs a title and a date.', true);
      return;
    }
    try {
      // Created all-day: a grid groups by day, and an event stored at a time
      // would drift across the boundary for a reader in another timezone.
      // Timed events go through the assistant or the import.
      await api('/api/v1/todo/events', {
        method: 'POST',
        body: JSON.stringify({
          title: title.value.trim(),
          description: desc.value.trim(),
          start_at: date.value,
          all_day: true,
        }),
      });
      title.value = '';
      desc.value = '';
      await renderTasks();
    } catch (err) {
      showError(err);
    }
  };

  return el('div', { class: 'row-form' }, title, date, desc,
    el('button', { text: 'Add event', onclick: save }));
}

function calendarGrid(cal, first) {
  const grid = el('div', { class: 'cal-grid' });
  for (const w of WEEKDAYS) grid.append(el('div', { class: 'cal-weekday', text: w }));

  // Lead with blanks so the 1st lands under its weekday. Not class "empty":
  // that one is the app's empty-state message, and it carries a 15vh top
  // margin that would push the first week down the page.
  for (let i = 0; i < first.getDay(); i += 1) grid.append(el('div', { class: 'cal-cell blank' }));

  const days = new Date(first.getFullYear(), first.getMonth() + 1, 0).getDate();
  for (let d = 1; d <= days; d += 1) {
    const key = dayKey(new Date(first.getFullYear(), first.getMonth(), d));
    const cell = el('div', { class: `cal-cell${key === cal.today ? ' today' : ''}` },
      el('div', { class: 'cal-daynum', text: String(d) }));
    for (const e of (cal.events || {})[key] || []) cell.append(calEvent(e));
    for (const t of (cal.days || {})[key] || []) cell.append(calDue(t));
    grid.append(cell);
  }
  return grid;
}

function calEvent(e) {
  // A timed event shows its clock time; an all-day one has none to show.
  const label = e.all_day ? e.title : `${e.start_at.slice(11, 16)} ${e.title}`;
  return el('div', { class: 'cal-item event', title: e.description || e.title },
    el('span', { class: 'cal-item-label', text: label }),
    el('button', {
      class: 'cal-item-del', 'aria-label': 'Delete event', text: '✕',
      onclick: () => confirmDelete(`the event "${e.title}"`, async () => {
        await api(`/api/v1/todo/events/${e.id}`, { method: 'DELETE' });
        await renderTasks();
      }),
    }));
}

// A task or note falling on this day. Read-only: notes are managed in the
// Notes view and tasks on their list, which is where their other fields are.
function calDue(t) {
  const isNote = t.list_title === 'Notes';
  return el('div', {
    class: `cal-item due${t.status === 'done' ? ' done' : ''}`,
    title: isNote ? 'Note — manage it in the Notes view' : `Task on ${t.list_title}`,
  }, el('span', { class: 'cal-item-label', text: `${isNote ? '📝' : '☑'} ${t.title}` }));
}

/* ---------- spreadsheet import ---------- */

// Shared by notes and events. The result outlives the re-render that follows a
// successful import: the whole pane is rebuilt to show the new rows, and a
// summary saying two of forty failed is the part worth keeping on screen.
const lastImport = { path: null, result: null, message: null };

function importBox({ path, columns, example }) {
  const file = el('input', { type: 'file', accept: '.csv,.xlsx,.xls' });
  const result = el('div', { class: 'import-result' });
  if (lastImport.path === path) showImportResult(result, lastImport.result, lastImport.message);

  const run = el('button', {
    class: 'ghost-btn', text: 'Import',
    onclick: async () => {
      if (!file.files || !file.files[0]) {
        showImportResult(result, null, 'Choose a file to import first.');
        return;
      }
      const form = new FormData();
      form.append('file', file.files[0]);
      run.disabled = true;
      showImportResult(result, null, 'Importing…');
      try {
        // Not api(): a multipart body must not carry a JSON content type, and
        // the browser has to set its own boundary.
        const res = await fetch(path, {
          method: 'POST',
          headers: { Authorization: `Bearer ${state.token}` },
          body: form,
        });
        const payload = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(payload.error || `${res.status} ${res.statusText}`);
        Object.assign(lastImport, { path, result: payload, message: null });
        await renderTasks();
      } catch (err) {
        Object.assign(lastImport, { path, result: null, message: err.message });
        showImportResult(result, null, err.message);
      } finally {
        run.disabled = false;
      }
    },
  });

  return el('details', { class: 'import-box', open: lastImport.path === path },
    el('summary', { text: 'Import from CSV / Excel' }),
    el('p', { class: 'muted', text: 'The first row must name the columns. Order does not matter and unrecognised columns are ignored.' }),
    el('ul', { class: 'import-help' },
      columns.map(([name, what]) => el('li', {}, el('code', { text: name }), el('span', { class: 'muted', text: ` ${what}` })))),
    el('pre', { class: 'import-example', text: example }),
    el('div', { class: 'row-form' }, file, run),
    result);
}

// Row errors come from the server, so every one of them is set as text.
function showImportResult(box, result, message) {
  box.innerHTML = '';
  if (message) {
    box.append(el('p', { text: message }));
    return;
  }
  if (!result) return;

  box.append(el('p', {
    text: `Imported ${result.imported} of ${result.total_rows} row(s).`
      + (result.failed ? ` ${result.failed} failed.` : ''),
  }));
  if (result.errors && result.errors.length) {
    box.append(el('ul', { class: 'import-errors' },
      result.errors.map((e) => el('li', { text: `Row ${e.row}: ${e.message}` }))));
  }
}

/* ================= SCRATCH ================= */
//
// A pad is a title and a body and nothing else — no format, no rendering, no
// structure to keep valid. What makes it worth having is next door in the
// chat: a reply lands in the transcript, which is a log and is not editable,
// and the pad is where it goes to be worked over.
//
// Saving runs on a timer rather than on a button. A scratch pad someone has to
// remember to save is one that loses work, and it loses it silently — the tab
// closes and the text was never anywhere. The button stays in the header all
// the same, for the moment after a long paste when a timer is not reassuring.

const pad = {
  docs: [],
  // The open document. 0 means none, which is where the pane starts and where
  // it returns to when the open one is deleted.
  id: 0,
  // The text as the server last accepted it. Compared against the boxes to
  // decide whether a save is owed, so an idle pad does not write a row every
  // second for the rest of the session.
  savedTitle: '',
  savedBody: '',
  timer: null,
  saving: false,
  maxBody: 1 << 20,
};

const PAD_SAVE_DELAY = 800;

async function loadPads() {
  const data = await api('/api/v1/scratch');
  pad.docs = data.docs || [];
  if (data.max_body) pad.maxBody = data.max_body;
  renderPads();
  // Arriving at an empty pane is a dead end — the first thing anyone would do
  // is click the top row, so do it for them. Only when nothing is open yet:
  // reloading the list must never move off the document being edited.
  if (!pad.id && pad.docs.length) await openPad(pad.docs[0].id);
  else if (!pad.docs.length) showPad(null);
}

function renderPads() {
  const box = $('pads');
  box.innerHTML = '';
  for (const d of pad.docs) {
    box.append(el('li', {
      class: d.id === pad.id ? 'active' : '',
      title: d.updated_at ? `Changed ${new Date(d.updated_at).toLocaleString()}` : '',
      onclick: () => { openPad(d.id).catch(showError); },
    },
    el('span', { class: 'pad-name', text: d.title }),
    el('span', { class: 'muted list-count', text: compactCount(d.chars || 0) }),
    el('button', {
      class: 'session-del', text: '×', type: 'button',
      title: `Delete "${d.title}"`,
      'aria-label': `Delete pad ${d.title}`,
      onclick: (e) => { e.stopPropagation(); deletePad(d); },
    }),
    d.preview ? el('span', { class: 'pad-peek', text: d.preview }) : null));
  }
  if (!pad.docs.length) box.append(el('li', { class: 'muted', text: 'No pads yet.' }));
}

// showPad puts a document on screen, or clears the pane when given null. It
// also declares the document clean, so it must not be handed text the server
// has not accepted — that would mark unsaved edits as saved.
function showPad(doc) {
  const title = $('pad-title');
  const text = $('pad-text');
  pad.id = doc ? doc.id : 0;
  pad.savedTitle = doc ? doc.title : '';
  pad.savedBody = doc ? doc.body || '' : '';
  title.value = pad.savedTitle;
  text.value = pad.savedBody;
  title.disabled = !doc;
  text.disabled = !doc;
  for (const id of ['pad-copy', 'pad-save', 'pad-download', 'pad-delete']) $(id).hidden = !doc;
  padCount();
  padState(doc ? padSavedLabel(doc.updated_at) : '');
  renderPads();
}

function padSavedLabel(iso) {
  if (!iso) return 'Saved';
  return `Saved ${new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
}

function padState(msg) { $('pad-state').textContent = msg; }

function padCount() {
  const text = $('pad-text').value;
  const words = text.trim() ? text.trim().split(/\s+/).length : 0;
  $('pad-count').textContent = pad.id
    ? `${words} word${words === 1 ? '' : 's'} · ${text.length} character${text.length === 1 ? '' : 's'}`
    : '';
}

async function openPad(id) {
  // The pad being left may have keystrokes the timer has not written yet.
  // Flushing before the fetch rather than after it is what stops the new
  // document's text being saved over the old one's row.
  await flushPad();
  showPad(await api(`/api/v1/scratch/${id}`));
}

function padDirty() {
  return pad.id !== 0
    && ($('pad-title').value !== pad.savedTitle || $('pad-text').value !== pad.savedBody);
}

function schedulePadSave() {
  padCount();
  if (!padDirty()) return;
  padState('Unsaved…');
  clearTimeout(pad.timer);
  pad.timer = setTimeout(() => { savePad().catch(showError); }, PAD_SAVE_DELAY);
}

// savePad writes the open document. It is safe to call when nothing has
// changed, and a second call while one is in flight does nothing rather than
// racing the first onto the same row.
async function savePad() {
  clearTimeout(pad.timer);
  pad.timer = null;
  if (!pad.id || pad.saving || !padDirty()) return;

  const id = pad.id;
  const title = $('pad-title').value;
  const body = $('pad-text').value;
  if (body.length > pad.maxBody) {
    padState('Too long to save');
    showError(new Error(`A pad holds ${compactCount(pad.maxBody)} characters; this one has ${compactCount(body.length)}.`));
    return;
  }
  pad.saving = true;
  padState('Saving…');
  try {
    const doc = await api(`/api/v1/scratch/${id}`, {
      method: 'PUT',
      body: JSON.stringify({ title, body }),
    });
    // Only mark it clean if the pane is still showing the same document —
    // switching away mid-request must not declare the new one saved.
    if (pad.id === id) {
      pad.savedTitle = doc.title;
      pad.savedBody = doc.body || '';
      // The server names a pad saved with a blank title, so say what it chose
      // rather than leaving the box empty and the sidebar reading "Untitled".
      if ($('pad-title').value.trim() === '') {
        $('pad-title').value = doc.title;
        pad.savedTitle = doc.title;
      }
      padState(padSavedLabel(doc.updated_at));
    }
    await refreshPadList();
  } catch (err) {
    padState('Not saved');
    throw err;
  } finally {
    pad.saving = false;
  }
}

// flushPad writes anything outstanding and waits for it: the way out of the
// view, and the way into another document.
async function flushPad() {
  clearTimeout(pad.timer);
  pad.timer = null;
  if (!padDirty()) return;
  try {
    await savePad();
  } catch (err) {
    showError(err);
  }
}

// The sidebar carries titles, previews and sizes, so it is refetched after a
// save — but showPad() must not run, or the caret jumps to the end of the box
// somebody is still typing in.
async function refreshPadList() {
  const data = await api('/api/v1/scratch');
  pad.docs = data.docs || [];
  renderPads();
}

async function newPad(title = '', body = '') {
  await flushPad();
  const doc = await api('/api/v1/scratch', {
    method: 'POST',
    body: JSON.stringify({ title, body }),
  });
  await refreshPadList();
  showPad(doc);
  return doc;
}

function deletePad(doc) {
  confirmDelete(`the pad "${doc.title}"`, async () => {
    await api(`/api/v1/scratch/${doc.id}`, { method: 'DELETE' });
    if (pad.id === doc.id) {
      // Nothing is owed on a row that no longer exists, and writing the timer
      // out after the delete would resurrect it.
      clearTimeout(pad.timer);
      pad.timer = null;
      pad.id = 0;
    }
    await refreshPadList();
    if (!pad.id) {
      if (pad.docs.length) await openPad(pad.docs[0].id);
      else showPad(null);
    }
  });
}

$('new-pad').addEventListener('click', () => {
  newPad().then(() => $('pad-text').focus()).catch(showError);
});
$('pad-title').addEventListener('input', schedulePadSave);
$('pad-text').addEventListener('input', schedulePadSave);
// Leaving a box is a stronger signal than the timer: write it now rather than
// in another second.
$('pad-title').addEventListener('blur', () => { savePad().catch(showError); });
$('pad-text').addEventListener('blur', () => { savePad().catch(showError); });
$('pad-save').addEventListener('click', () => { savePad().catch(showError); });
$('pad-delete').addEventListener('click', () => {
  const doc = pad.docs.find((d) => d.id === pad.id);
  if (doc) deletePad(doc);
});

$('pad-copy').addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText($('pad-text').value);
    toast('Copied.');
  } catch {
    // The clipboard API is refused outright on a page served over plain http,
    // which is how this server is usually reached. Selecting the text leaves
    // the browser's own copy one keystroke away instead of nothing at all.
    $('pad-text').select();
    toast('Selected — press Ctrl+C to copy.');
  }
});

$('pad-download').addEventListener('click', () => {
  const name = ($('pad-title').value.trim() || 'pad').replace(/[^\w.\- ]+/g, '_');
  const url = URL.createObjectURL(new Blob([$('pad-text').value], { type: 'text/plain' }));
  const a = el('a', { href: url, download: `${name}.txt` });
  document.body.append(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
});

// Ctrl/Cmd-S is what everyone presses in a text box, and the browser's own
// answer to it — save this page to disk — is never what was meant.
document.addEventListener('keydown', (e) => {
  if (state.view !== 'scratch') return;
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
    e.preventDefault();
    savePad().catch(showError);
  }
});

// A tab closing is the one exit that cannot be awaited. keepalive lets the
// request outlive the page. It is best effort, which is why the timer is short
// and every other way out of the pad flushes properly.
window.addEventListener('beforeunload', () => {
  if (!padDirty()) return;
  fetch(`/api/v1/scratch/${pad.id}`, {
    method: 'PUT',
    keepalive: true,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${state.token}` },
    body: JSON.stringify({ title: $('pad-title').value, body: $('pad-text').value }),
  }).catch(() => {});
});

// sendToPad backs the button on a message in the transcript. It appends to
// whatever is open, because the shape this is for is a conversation being
// collected into one document; with nothing open it starts a pad and names it
// from the first line of what was sent.
async function sendToPad(text) {
  try {
    await flushPad();
    if (!pad.id) {
      const first = text.split('\n').find((l) => l.trim()) || 'Untitled';
      const made = await newPad(first.trim().slice(0, 60), `${text}\n`);
      toast(`Started the pad "${made.title}".`);
      return;
    }
    // Reread rather than trusting the box: the pad may not have been opened
    // this session, and appending to a stale copy would drop whatever was
    // written into it from somewhere else.
    const doc = await api(`/api/v1/scratch/${pad.id}`);
    showPad(doc);
    const box = $('pad-text');
    box.value = doc.body.trimEnd() ? `${doc.body.trimEnd()}\n\n${text}\n` : `${text}\n`;
    await savePad();
    padCount();
    box.scrollTop = box.scrollHeight;
    toast(`Added to the pad "${doc.title}".`);
  } catch (err) {
    showError(err);
  }
}

/* ================= CRM ================= */

const crm = { tab: 'dashboard', clients: [], engagements: [] };

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

/* ---------- company view: profile, CRM and the books ---------- */
//
// Three groups of tabs over three panes in one view. The groups share a
// selection — clicking Invoices has to deselect Clients — but not a pane:
// each keeps the title, body and New button its renderer already writes to,
// which is why renderCRM() and renderAccounting() needed no changes at all.
// Only one pane is ever visible; `[hidden]` is `!important` in style.css, so
// setting it beats the `display: flex` on .pane.

function showCoPane(name) {
  for (const pane of document.querySelectorAll('.view[data-view="company"] .pane')) {
    pane.hidden = pane.dataset.pane !== name;
  }
}

// A group renders when its tab is clicked rather than at view open: landing on
// the profile should not also fetch the whole ledger.
for (const li of document.querySelectorAll('.co-tabs li')) {
  li.addEventListener('click', () => {
    for (const other of document.querySelectorAll('.co-tabs li')) {
      other.classList.toggle('active', other === li);
    }
    const pane = li.dataset.pane;
    showCoPane(pane);
    if (pane === 'crm') {
      crm.tab = li.dataset.tab;
      renderCRM().catch(showError);
    } else if (pane === 'acct') {
      acct.tab = li.dataset.tab;
      renderAccounting().catch(showError);
    } else {
      loadCompany().catch(showError);
    }
    closeSidebar();
  });
}

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
    status: 'Status', config: 'Configuration', hardware: 'Hardware',
    tools: 'Tools', clients: 'Clients', memory: 'Memory',
    appearance: 'Appearance', faq: 'Help',
  };
  $('admin-title').textContent = titles[admin.tab];
  body.innerHTML = '';

  if (admin.tab === 'status') return renderAdminStatus(body);
  if (admin.tab === 'config') return renderAdminConfig(body);
  if (admin.tab === 'hardware') return renderAdminHardware(body);
  if (admin.tab === 'tools') return renderAdminTools(body);
  if (admin.tab === 'faq') return renderAdminFAQ(body);
  if (admin.tab === 'memory') return renderAdminMemory(body);
  // Purely local, unlike every other tab here: it reads and writes
  // localStorage and asks the server nothing.
  if (admin.tab === 'appearance') return renderAdminAppearance(body);
  return renderAdminClients(body);
}

/* ---------- huginn ----------
   The models themselves: the backends that run them, the repository they are
   kept in, which ones are loaded, the Hub they come from, and the fleet of
   hosts carrying them.

   A second view rather than more tabs under Admin. The renderers are the ones
   Admin already had and are unchanged — each takes the body to draw into, so
   moving a pane between views is a matter of which nav dispatches it and which
   element it is handed. What did have to move with them is the refresh: an
   action inside one of these panes redraws its own view, and calling
   renderAdmin() from here would paint a Huginn pane into the Admin body. */

const huginn = { tab: 'backends' };

for (const li of document.querySelectorAll('#huginn-nav li')) {
  li.addEventListener('click', () => {
    huginn.tab = li.dataset.tab;
    for (const other of document.querySelectorAll('#huginn-nav li')) {
      other.classList.toggle('active', other === li);
    }
    renderHuginn().catch(showError);
    closeSidebar();
  });
}
$('huginn-refresh').addEventListener('click', () => renderHuginn().catch(showError));

async function renderHuginn() {
  const body = $('huginn-body');
  const titles = {
    backends: 'Backends', repo: 'Repository', models: 'Models',
    hub: 'Hub', fleet: 'Fleet',
  };
  $('huginn-title').textContent = titles[huginn.tab];
  body.innerHTML = '';

  if (huginn.tab === 'repo') return renderAdminRepo(body);
  if (huginn.tab === 'models') return renderAdminModels(body);
  if (huginn.tab === 'hub') return renderAdminHub(body);
  if (huginn.tab === 'fleet') return renderAdminFleet(body);
  return renderAdminBackends(body);
}

// The fleet: remote hosts reporting what they are doing.
//
// A host that has stopped reporting is the thing worth seeing first, so
// staleness is shown as a state rather than left to be worked out from a
// timestamp. Anything quiet for more than three report intervals is treated as
// out of contact.
async function renderAdminFleet(body) {
  // The fleet and the backends are shown together because they answer one
  // question between them: this host is busy — is it busy *serving a model*?
  // Neither list can say that alone.
  const [data, models, assigned, repo] = await Promise.all([
    api('/api/v1/nodes'),
    api('/api/v1/models').catch(() => ({ models: [] })),
    // Which models each node should be holding, and what there is to choose
    // from. Both are small, and fetching them here means the store panel on
    // every card is drawn from one round trip rather than one per node.
    api('/api/v1/nodes/assignments').catch(() => ({ assignments: {} })),
    api('/api/v1/repo').catch(() => ({ files: [] })),
  ]);

  // Resident models, grouped by the backend serving them. A node and a backend
  // are matched by name, which is the convention worth stating rather than
  // inferring: name the client after the backend it runs and the two line up.
  const residentByBackend = new Map();
  for (const m of models.models || []) {
    if (!m.loaded) continue;
    residentByBackend.set(m.backend, [...(residentByBackend.get(m.backend) || []), m]);
  }

  if (!data.configured) {
    body.append(el('p', { class: 'muted', text:
      'Fleet telemetry is off. Set WINTERMUTE_METRICS_DB to a file path to switch it on — '
      + 'it is kept in its own database, apart from your conversations.' }));
    return;
  }
  if (!data.nodes.length) {
    body.append(
      el('p', { class: 'muted', text: 'No hosts are reporting yet. On each machine:' }),
      el('pre', { class: 'wrap', text:
        'wintermuted -add-client rig -kind node      # on the server, once per host\n'
        + 'wintermute-node -server https://…:8080 -token wm_…   # on the host' }),
    );
    return;
  }

  for (const n of data.nodes) {
    body.append(nodeCard(n, residentByBackend.get(n.name) || [],
      (assigned.assignments || {})[n.name] || [], repo.files || []));
  }
}

function nodeCard(n, resident, assignments, repoFiles) {
  const s = n.latest;
  const seen = n.last_seen_at ? new Date(n.last_seen_at) : null;
  const ageMs = seen ? Date.now() - seen.getTime() : Infinity;
  // Three missed reports rather than one: a single late push is a busy
  // network, not a machine that has gone away.
  const stale = ageMs > 3 * 60 * 1000;

  const facts = [
    n.hostname && n.hostname !== n.name ? n.hostname : null,
    n.cores ? `${n.cores} cores` : null,
    n.kernel || null,
    s && s.uptime_seconds ? `up ${formatDuration(s.uptime_seconds)}` : null,
  ].filter(Boolean).join(' · ');

  const gaugeRow = [
    gauge('CPU', `${s ? s.cpu_percent.toFixed(0) : 0}%`, s ? s.cpu_percent : 0),
    gauge('Memory', s ? bytes(s.mem_used_bytes) : '—',
      s && s.mem_total_bytes ? (s.mem_used_bytes / s.mem_total_bytes) * 100 : 0),
    gauge('Load', s ? s.load_1.toFixed(2) : '—', s && n.cores ? (s.load_1 / n.cores) * 100 : 0),
  ];
  // GPU gauges only on hosts that have one. A row of zeroes on a CPU-only box
  // reads as a broken card rather than an absent one.
  if (s && n.gpus && n.gpus.length) {
    gaugeRow.push(gauge('GPU', `${s.gpu_util_percent.toFixed(0)}%`, s.gpu_util_percent));
    gaugeRow.push(gauge('VRAM', bytes(s.gpu_mem_used_bytes),
      s.gpu_mem_total_bytes ? (s.gpu_mem_used_bytes / s.gpu_mem_total_bytes) * 100 : 0));
    if (s.gpu_temp_c) {
      // 83C is where consumer NVIDIA cards begin throttling, so the bar is
      // scaled to that rather than to some abstract maximum.
      gaugeRow.push(gauge('Temp', `${s.gpu_temp_c.toFixed(0)}\u00b0C`, (s.gpu_temp_c / 83) * 100));
    }
  }
  if (s) {
    gaugeRow.push(el('span', { class: 'node-rate', text:
      `net ${bytes(s.net_rx_bps)}/s in · ${bytes(s.net_tx_bps)}/s out` }));
  }
  const gauges = s
    ? el('div', { class: 'node-gauges' }, gaugeRow)
    : el('div', { class: 'muted', text: 'no readings yet' });

  const cards = (n.gpus || []).length
    ? el('div', { class: 'node-cards', text:
        n.gpus.map((g) => `${g.name} (${bytes(g.mem_total_bytes)})`).join(' · ') })
    : null;

  // What this host is actually holding in memory — the reason to care that its
  // GPU is warm.
  const models = resident.length
    ? el('div', { class: 'node-models' }, [
        el('span', { class: 'muted', text: 'serving' }),
        ...resident.map((m) => el('span', { class: 'host-chip resident', text: m.id })),
      ])
    : null;

  return el('div', { class: `node-card ${stale ? 'stale' : ''}` }, [
    el('div', { class: 'node-head' }, [
      el('span', { class: 'node-name', text: n.name }),
      stale
        ? el('span', { class: 'node-state out', text: `out of contact · ${relativeTime(seen)}` })
        : el('span', { class: 'node-state ok', text: 'reporting' }),
    ]),
    el('div', { class: 'model-facts', text: facts }),
    cards,
    gauges,
    models,
    nodeStorePanel(n, assignments || [], repoFiles || []),
  ]);
}

// A node's own library of weights, and what it has been assigned.
//
// The distinction the panel has to make legible is between the two, because
// they come apart in both directions and each means something different. An
// assignment with no file is a transfer that has not happened yet — the node
// will fetch it on its next report. A file with no assignment is a model this
// host keeps that nothing asked it to; dropping an assignment never deletes
// anything, so that is the normal state after un-assigning something.
function nodeStorePanel(n, assignments, repoFiles) {
  const store = n.store;
  if (!store) {
    // Absent rather than empty: a node with no -store is a node that only
    // reports metrics, which is a complete and ordinary configuration.
    return assignments.length
      ? el('div', { class: 'node-store' }, [
          el('div', { class: 'node-store-head' }, [
            el('span', { class: 'muted', text: 'model store' }),
            el('span', { class: 'repo-badge missing', text: 'not configured' }),
          ]),
          el('p', { class: 'muted', text:
            `${assignments.length} model${assignments.length === 1 ? '' : 's'} assigned, but this `
            + 'agent has no -store. Restart it with -store and -runtime to have it fetch them.' }),
        ])
      : null;
  }

  const held = new Map();
  for (const f of store.files || []) held.set(f.rel_path, f);

  const rows = [];
  for (const rel of assignments) {
    const f = held.get(rel);
    rows.push(nodeStoreRow(n, rel, f, true));
  }
  // Anything held but not assigned, so the disk cost is visible even when
  // nothing currently asks for it.
  for (const f of store.files || []) {
    if (!assignments.includes(f.rel_path)) rows.push(nodeStoreRow(n, f.rel_path, f, false));
  }

  const facts = [
    store.runtime ? `served by ${store.runtime}` : 'no runtime configured',
    store.free_bytes ? `${bytes(store.free_bytes)} free` : null,
  ].filter(Boolean).join(' · ');

  return el('div', { class: 'node-store' }, [
    el('div', { class: 'node-store-head' }, [
      el('span', { class: 'muted', text: 'model store' }),
      el('span', { class: 'node-store-path', text: store.path }),
    ]),
    el('div', { class: 'muted node-store-facts', text: facts }),
    store.error ? el('div', { class: 'repo-job-error', text: store.error }) : null,
    rows.length ? el('div', { class: 'node-store-rows' }, rows) : null,
    nodeAssignControl(n, assignments, repoFiles),
  ]);
}

function nodeStoreRow(n, rel, file, isAssigned) {
  // Three states worth telling apart, and the middle one is the one people
  // otherwise mistake for a failure.
  let state = 'held';
  let title = 'On this host and ready.';
  if (!file) {
    state = 'pending';
    title = 'Assigned but not here yet. The agent fetches it on its next report.';
  } else if (file.partial) {
    state = 'fetching';
    title = 'A transfer is part-way through. It resumes rather than restarting.';
  } else if (!file.ingested) {
    state = 'unimported';
    title = 'The file is here, but the runtime cannot serve it yet.';
  }

  return el('div', { class: `node-store-row ${state}` }, [
    el('span', { class: `node-store-dot ${state}`, title }),
    el('span', { class: 'node-store-name', text: rel }),
    el('span', { class: `repo-badge ${state}`, title, text: state }),
    file && file.size_bytes ? el('span', { class: 'muted', text: bytes(file.size_bytes) }) : null,
    isAssigned
      ? el('button', {
          class: 'link-btn', type: 'button', text: 'Unassign',
          title: 'Stops this node being expected to hold it. Nothing is deleted from the host.',
          onclick: () => unassignModel(n.name, rel).catch(showError),
        })
      : el('span', { class: 'muted', text: 'not assigned' }),
  ]);
}

function nodeAssignControl(n, assignments, repoFiles) {
  const available = (repoFiles || []).filter((f) => !f.missing && !assignments.includes(f.rel_path));
  if (!available.length) {
    return repoFiles && repoFiles.length
      ? null
      : el('p', { class: 'muted', text:
          'Nothing in the model repository to assign yet — see Huginn → Repository.' });
  }

  const select = el('select', { class: 'champion-select' }, [
    el('option', { value: '', text: 'Assign a model…' }),
    ...available.map((f) => el('option', {
      value: f.rel_path,
      text: `${f.name}${f.size_bytes ? ` (${bytes(f.size_bytes)})` : ''}`,
    })),
  ]);
  select.addEventListener('change', (e) => {
    if (!e.target.value) return;
    assignModel(n.name, e.target.value).catch(showError);
  });
  return el('div', { class: 'node-store-assign' }, select);
}

// Assigning transfers nothing and contacts nothing. It records what the node
// should have; the agent notices on its next report and fetches for itself,
// which is why the toast talks about minutes rather than showing a progress bar.
async function assignModel(node, relPath) {
  await api(`/api/v1/nodes/${encodeURIComponent(node)}/models`, {
    method: 'POST',
    body: JSON.stringify({ rel_path: relPath }),
  });
  toast(`${relPath} assigned to ${node}. It will fetch it on its next report.`);
  await renderHuginn();
}

async function unassignModel(node, relPath) {
  if (!window.confirm(
    `Stop expecting ${node} to hold ${relPath}?\n\n`
    + 'The weights stay on that host — nothing is deleted there. Free the space on the '
    + 'node itself if you need it back.')) return;
  await api(`/api/v1/nodes/${encodeURIComponent(node)}/models/remove`, {
    method: 'POST',
    body: JSON.stringify({ rel_path: relPath }),
  });
  await renderHuginn();
}

// A gauge reads as a bar as well as a number, so a machine in trouble is
// visible without reading anything.
function gauge(label, value, percent) {
  const pct = Math.max(0, Math.min(100, percent || 0));
  return el('span', { class: 'node-gauge', title: `${label}: ${value}` }, [
    el('span', { class: 'gauge-label', text: label }),
    el('span', { class: 'gauge-track' },
      el('span', { class: `gauge-fill ${pct > 90 ? 'hot' : pct > 70 ? 'warm' : ''}`,
        style: `width:${pct.toFixed(1)}%` })),
    el('span', { class: 'gauge-value', text: value }),
  ]);
}

function formatDuration(secs) {
  const d = Math.floor(secs / 86400);
  if (d > 0) return `${d}d`;
  const h = Math.floor(secs / 3600);
  if (h > 0) return `${h}h`;
  return `${Math.floor(secs / 60)}m`;
}

function relativeTime(date) {
  if (!date) return 'never';
  const secs = Math.floor((Date.now() - date.getTime()) / 1000);
  if (secs < 90) return `${secs}s ago`;
  if (secs < 5400) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 172800) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

// The model registry: which model sits on which machine, and what you think of
// it.
//
// Grouped by model rather than by backend, which is the inversion that makes it
// useful. The catalog stores one row per backend-and-model pair, so a model on
// four hosts is four rows — but a judgement about it is one judgement, and
// showing it four times would invite four contradictory notes. Each model
// appears once, with the machines carrying it listed beside it.
async function renderAdminModels(body) {
  const [{ models: list }, { champions }, { tasks }, perf] = await Promise.all([
    api('/api/v1/models'),
    api('/api/v1/models/champions'),
    api('/api/v1/tasks'),
    api('/api/v1/models/performance?days=7').catch(() => ({ performance: [] })),
  ]);

  // Measured speed, keyed the same way the cards are folded. A model on two
  // hosts has two sets of numbers; the card shows the best observed rate,
  // because the question it answers is "how fast can this model go here".
  const measured = new Map();
  for (const p of perf.performance || []) {
    const key = (p.model || '').toLowerCase();
    const prev = measured.get(key);
    if (!prev || p.tokens_per_second > prev.tokens_per_second) measured.set(key, p);
  }

  // Only Ollama backends can load and unload on demand. Knowing which before
  // rendering means a host that cannot be controlled is shown as a plain label
  // rather than a button that would fail.
  const controllableBackends = new Set(
    (state.backends || []).filter((b) => b.kind === 'ollama' || b.kind === 'hailo').map((b) => b.name),
  );

  if (!list || !list.length) {
    body.append(el('p', { class: 'muted', text:
      'No models found. Check the Backends tab — a backend that is unreachable reports nothing.' }));
    return;
  }

  // Fold the per-backend rows into one row per model.
  const byModel = new Map();
  for (const m of list) {
    const key = (m.id || '').toLowerCase();
    if (!byModel.has(key)) byModel.set(key, { model: m, hosts: [] });
    const entry = byModel.get(key);
    entry.hosts.push({
      backend: m.backend,
      loaded: m.loaded,
      vram: m.vram_bytes,
      controllable: controllableBackends.has(m.backend),
    });
    // Prefer a loaded copy as the representative row: its VRAM figure is real
    // rather than an estimate.
    if (m.loaded && !entry.model.loaded) entry.model = m;
  }

  const championTask = new Map();
  for (const c of champions || []) championTask.set(c.model_id, [...(championTask.get(c.model_id) || []), c.task]);

  const taskLabel = new Map((tasks || []).map((t) => [t.task, t.label]));

  body.append(el('p', { class: 'muted', text:
    `${byModel.size} models across ${new Set(list.map((m) => m.backend)).size} backends. `
    + 'Notes and champions are yours — nothing overwrites them when a backend is re-probed.' }));

  const rows = [...byModel.entries()].sort((a, b) => {
    // Champions first, then models you have written about, then the rest.
    const ca = (championTask.get(a[0]) || []).length;
    const cb = (championTask.get(b[0]) || []).length;
    if (ca !== cb) return cb - ca;
    const na = a[1].model.note ? 1 : 0;
    const nb = b[1].model.note ? 1 : 0;
    if (na !== nb) return nb - na;
    return a[0].localeCompare(b[0]);
  });

  for (const [key, entry] of rows) {
    body.append(modelCard(key, entry, championTask.get(key) || [], tasks || [], taskLabel,
      measured.get(key)));
  }
}

function modelCard(key, entry, titles, tasks, taskLabel, perf) {
  const m = entry.model;
  const resident = entry.hosts.filter((h) => h.loaded);

  // Each host is a chip that is also the control for that host's copy:
  // clicking it loads or unloads there. The chip already says which machine
  // and whether it is resident, so making it the button avoids a second row of
  // controls repeating the same names.
  const hostChips = entry.hosts.map((h) => {
    if (!h.controllable) {
      return el('span', {
        class: `host-chip ${h.loaded ? 'resident' : ''}`,
        title: `${h.backend} serves whatever it was started with — it cannot load or unload on demand`,
        text: h.backend,
      });
    }
    return el('button', {
      class: `host-chip control ${h.loaded ? 'resident' : ''}`,
      type: 'button',
      title: h.loaded
        ? `Resident on ${h.backend}. Click to unload and free its VRAM.`
        : `Present on ${h.backend}, not loaded. Click to load it into memory.`,
      onclick: () => controlModel(h.backend, m.id, !h.loaded).catch(showError),
    }, [
      el('span', { class: 'host-dot', text: h.loaded ? '\u25cf' : '\u25cb' }),
      el('span', { text: h.backend }),
    ]);
  });

  const titleChips = titles.map((t) => el('span', {
    class: 'title-chip',
    text: `Best for ${taskLabel.get(t) || t}`,
  }));

  const facts = [
    m.params_b ? `${m.params_b}B` : null,
    m.quant || null,
    m.ctx_len ? `${(m.ctx_len / 1024).toFixed(0)}k ctx` : null,
    resident.length ? `resident on ${resident.length} of ${entry.hosts.length}` : null,
  ].filter(Boolean).join(' · ');

  // What it has actually been doing, as opposed to what it claims to be.
  // Absent until it has been used: an empty row would read as "slow" rather
  // than "not measured yet".
  const measuredRow = perf && perf.calls
    ? el('div', { class: 'model-measured', title:
        `${perf.calls} calls over the last 7 days on ${perf.backend}`
        + (perf.failed ? `, ${perf.failed} failed` : '') }, [
        el('span', { class: 'measured-rate', text:
          perf.tokens_per_second ? `${perf.tokens_per_second.toFixed(1)} tok/s` : 'no usage reported' }),
        el('span', { class: 'measured-sep', text: '·' }),
        el('span', { text: `${formatMs(perf.median_ms)} typical` }),
        el('span', { class: 'measured-sep', text: '·' }),
        el('span', { text: `${perf.calls} call${perf.calls === 1 ? '' : 's'}` }),
        perf.failed
          ? el('span', { class: 'measured-failed', text: `${perf.failed} failed` })
          : null,
      ].filter(Boolean))
    : null;

  const noteBox = el('textarea', {
    class: 'model-note',
    rows: 2,
    placeholder: 'What do you make of it? e.g. "Current best coding."',
    value: m.note || '',
  });
  noteBox.addEventListener('change', () => {
    saveModelNote(m.id, noteBox.value).catch(showError);
  });

  const taskSelect = el('select', { class: 'champion-select' }, [
    el('option', { value: '', text: 'Name it best at…' }),
    ...tasks.map((t) => el('option', { value: t.task, text: t.label })),
  ]);
  taskSelect.addEventListener('change', (e) => {
    if (!e.target.value) return;
    setChampion(e.target.value, m.id).catch(showError);
  });

  return el('div', { class: `model-card ${titles.length ? 'is-champion' : ''}` }, [
    el('div', { class: 'model-head' }, [
      el('span', { class: 'model-id', text: m.id }),
      ...titleChips,
    ]),
    el('div', { class: 'model-facts', text: facts }),
    measuredRow,
    el('div', { class: 'model-hosts' }, hostChips),
    noteBox,
    el('div', { class: 'model-actions' }, [
      taskSelect,
      ...titles.map((t) => el('button', {
        class: 'link-btn', type: 'button',
        text: `Drop "${taskLabel.get(t) || t}"`,
        onclick: () => setChampion(t, '').catch(showError),
      })),
    ]),
  ]);
}

// Unloading frees VRAM that a conversation may be mid-turn on, so it asks
// first. Loading takes memory but interrupts nothing, so it does not.
async function controlModel(backend, modelID, load) {
  if (!load && !window.confirm(
    `Unload ${modelID} from ${backend}?\n\n`
    + 'It frees the VRAM immediately. Any turn currently running on that model '
    + 'will have to load it again, which takes seconds to a minute.')) return;

  toast(load ? `Loading ${modelID} on ${backend}…` : `Unloading ${modelID}…`);
  const res = await api(`/api/v1/models/${load ? 'load' : 'unload'}`, {
    method: 'POST',
    body: JSON.stringify({ backend, model_id: modelID }),
  });
  // The cached backend list carries the kinds this screen filters on, and the
  // health it shows elsewhere; refresh it so a backend that went away during
  // the operation is not still offered as controllable.
  await api('/api/v1/backends')
    .then((data) => { state.backends = data.backends || []; })
    .catch(() => { /* the models list is still worth rendering */ });
  await renderHuginn();
  const held = (res.resident || []).length;
  toast(load
    ? `${modelID} loaded on ${backend}. ${held} model${held === 1 ? '' : 's'} resident there.`
    : `${modelID} unloaded from ${backend}.`);
}

// Milliseconds are the wrong unit to read once a call takes seconds.
function formatMs(ms) {
  if (!ms) return '—';
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

async function saveModelNote(modelID, note) {
  await api('/api/v1/models/note', {
    method: 'POST',
    body: JSON.stringify({ model_id: modelID, note }),
  });
  toast(note.trim() ? 'Note saved.' : 'Note cleared.');
}

// Naming a champion moves the title: whichever model held it for this task
// loses it in the same write, so the UI reloads rather than guessing.
async function setChampion(task, modelID) {
  await api('/api/v1/models/champions', {
    method: 'POST',
    body: JSON.stringify({ task, model_id: modelID }),
  });
  await renderHuginn();
}


/* ---------- the model repository ----------
   Weights the operator keeps on a disk this server owns, as opposed to
   whatever a backend happens to be serving. The two are deliberately separate
   screens: the Models tab answers "what can I run right now", this one answers
   "what do I have, and what is it costing me in disk".

   The page holds three things that must not be thrown away by a refresh — the
   search box, its results, and the expanded repository detail — so the poll
   that follows a running download updates the jobs panel alone and leaves the
   rest of the DOM standing. Re-rendering everything every second would clear
   the search field under the operator's fingers. */

const repoView = { query: '', results: null, detail: null, timer: null, busy: false };

async function renderAdminRepo(body) {
  stopRepoPolling();
  const data = await api('/api/v1/repo');
  const status = data.status || {};

  const statusEl = el('div', { class: 'repo-status' });
  const jobsEl = el('div', { class: 'repo-jobs' });
  const searchEl = el('div', { class: 'repo-search' });
  const filesEl = el('div', { class: 'repo-files' });

  body.append(statusEl, jobsEl, searchEl, filesEl);
  paintRepoStatus(statusEl, status);

  if (!status.configured) {
    statusEl.append(el('p', { class: 'muted', text:
      'Set WINTERMUTE_MODEL_REPO to an absolute path — the mount point of the drive you '
      + 'want to keep weights on — and restart the server.' }));
    return;
  }
  if (!status.available) return;

  if (!status.initialised) {
    // The marker is what tells a mounted drive from an empty mount point, so
    // it is created by a deliberate press rather than on first use. Getting
    // this wrong fills the server's own root filesystem with model weights.
    statusEl.append(el('button', {
      class: 'ghost-btn', type: 'button', text: 'Initialise this directory',
      onclick: () => initRepo().catch(showError),
    }));
    return;
  }

  paintRepoJobs(jobsEl, data.jobs || []);
  paintRepoSearch(searchEl);
  paintRepoFiles(filesEl, data.files || [], data.tags || []);
  if ((data.jobs || []).some((j) => j.state === 'running')) startRepoPolling();
}

function paintRepoStatus(host, status) {
  host.innerHTML = '';
  if (!status.configured) {
    host.append(el('p', { class: 'muted', text: 'No model repository is configured.' }));
    return;
  }

  const state = !status.available ? 'unavailable' : (status.initialised ? 'ready' : 'unclaimed');
  const label = {
    unavailable: 'not available',
    unclaimed: 'not initialised',
    ready: 'ready',
  }[state];

  host.append(el('div', { class: 'repo-head' }, [
    el('span', { class: `repo-dot ${state}` }),
    el('span', { class: 'repo-root', text: status.root || '—' }),
    el('span', { class: `repo-state ${state}`, text: label }),
  ]));

  if (status.detail) host.append(el('p', { class: 'muted repo-detail', text: status.detail }));
  if (!status.available) return;

  // Two figures, and they answer different questions. How full the drive is
  // decides whether the next download fits; what the weights occupy decides
  // what deleting one would win back.
  if (status.total_bytes) {
    const used = status.total_bytes - status.free_bytes;
    const pct = Math.min(100, Math.round((used / status.total_bytes) * 100));
    host.append(el('div', { class: 'repo-disk' }, [
      el('div', { class: 'repo-bar' }, [
        el('div', { class: `repo-bar-fill ${pct > 90 ? 'tight' : ''}`, style: `width:${pct}%` }),
      ]),
      el('div', { class: 'repo-disk-facts muted', text:
        `${bytes(status.free_bytes)} free of ${bytes(status.total_bytes)} · `
        + `${status.file_count} model${status.file_count === 1 ? '' : 's'} `
        + `using ${bytes(status.used_by_repo_bytes)}` }),
    ]));
  }
}

async function initRepo() {
  await api('/api/v1/repo/init', { method: 'POST' });
  toast('Repository initialised.');
  await renderHuginn();
}

/* ---- downloads in flight ---- */

function paintRepoJobs(host, jobs) {
  host.innerHTML = '';
  if (!jobs.length) return;

  host.append(el('h3', { class: 'repo-h', text: 'Downloads' }));
  for (const j of jobs) host.append(repoJobCard(j));
}

function repoJobCard(j) {
  const running = j.state === 'running';
  const pct = j.total_bytes ? Math.min(100, (j.done_bytes / j.total_bytes) * 100) : 0;

  // A transfer that is quietly retrying looks exactly like a slow one, and
  // hashing 12GB back off a USB disk looks exactly like a hang. Both get said
  // out loud rather than left to be inferred from a stalled bar.
  const detail = [];
  if (running && j.phase === 'verifying') detail.push('checking the digest');
  else if (running && j.attempt > 1) detail.push(`retry ${j.attempt}`);
  if (j.total_bytes) detail.push(`${bytes(j.done_bytes)} of ${bytes(j.total_bytes)}`);
  else if (j.done_bytes) detail.push(bytes(j.done_bytes));
  if (running && j.bytes_per_second) detail.push(`${bytes(j.bytes_per_second)}/s`);
  if (j.resumed_bytes) detail.push(`resumed from ${bytes(j.resumed_bytes)}`);

  return el('div', { class: `repo-job ${j.state}` }, [
    el('div', { class: 'repo-job-head' }, [
      el('span', { class: 'repo-job-name', text: j.filename }),
      el('span', { class: `repo-job-state ${j.state}`, text: j.state }),
      running ? el('button', {
        class: 'link-btn', type: 'button', text: 'Cancel',
        title: 'Stops the transfer. What has arrived is kept, so starting it again resumes.',
        onclick: () => cancelRepoJob(j.id).catch(showError),
      }) : null,
    ]),
    el('div', { class: 'repo-bar' }, [
      el('div', {
        // With no total the server has not said how big the file is yet, so
        // the bar admits it rather than sitting at a fictitious 0%.
        class: `repo-bar-fill ${running && !j.total_bytes ? 'indeterminate' : ''} ${j.state}`,
        style: `width:${j.total_bytes ? pct : 100}%`,
      }),
    ]),
    el('div', { class: 'repo-job-facts muted', text: detail.join(' · ') }),
    j.error ? el('div', { class: 'repo-job-error', text: j.error }) : null,
    el('div', { class: 'muted repo-job-src', text: j.hub_id }),
  ]);
}

async function cancelRepoJob(id) {
  const res = await api(`/api/v1/repo/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST' });
  paintRepoJobs(document.querySelector('.repo-jobs'), res.jobs || []);
  toast('Download cancelled. What arrived is kept for a resume.');
}

// Polling is confined to the jobs panel so the search box and its results
// survive it. It stops itself when nothing is running, when the tab is left,
// and while the browser tab is in the background.
function startRepoPolling() {
  if (repoView.timer) return;
  repoView.timer = setInterval(async () => {
    // Downloads can be started from either screen, and the panel that reports
    // them is the same one, so the poll follows it rather than one tab.
    if (huginn.tab !== 'repo' && huginn.tab !== 'hub') return stopRepoPolling();
    if (document.hidden || repoView.busy) return;
    const host = document.querySelector('.repo-jobs');
    if (!host) return stopRepoPolling();

    repoView.busy = true;
    try {
      const { jobs } = await api('/api/v1/repo/jobs');
      const wasRunning = host.querySelectorAll('.repo-job.running').length;
      paintRepoJobs(host, jobs || []);
      const running = (jobs || []).filter((j) => j.state === 'running').length;
      if (!running) {
        stopRepoPolling();
        // Something finished: the file list and the disk figures have both
        // moved, so those are refreshed once rather than on every tick.
        if (wasRunning) await refreshRepoFiles();
      }
    } catch {
      // A failed poll leaves the last state on screen. The download itself is
      // running on the server and is not affected by this page losing touch.
      stopRepoPolling();
    } finally {
      repoView.busy = false;
    }
  }, 1500);
}

function stopRepoPolling() {
  if (repoView.timer) clearInterval(repoView.timer);
  repoView.timer = null;
}

async function refreshRepoFiles() {
  const filesHost = document.querySelector('.repo-files');
  const statusHost = document.querySelector('.repo-status');
  if (!filesHost) return;
  const data = await api('/api/v1/repo');
  if (statusHost) paintRepoStatus(statusHost, data.status || {});
  paintRepoFiles(filesHost, data.files || [], data.tags || []);
}

/* ---- finding something to download ---- */

function paintRepoSearch(host) {
  host.innerHTML = '';
  const input = el('input', {
    class: 'repo-query', type: 'search', placeholder: 'Search Hugging Face for GGUF models…',
    value: repoView.query, autocomplete: 'off', spellcheck: 'false',
  });
  const results = el('div', { class: 'repo-results' });

  const run = async () => {
    repoView.query = input.value.trim();
    if (!repoView.query) return;
    results.innerHTML = '';
    results.append(el('p', { class: 'muted', text: 'Searching…' }));
    try {
      const { results: found } = await api(
        `/api/v1/models/search?q=${encodeURIComponent(repoView.query)}&gguf=true&limit=15`);
      repoView.results = found || [];
      paintRepoResults(results, repoView.results);
    } catch (err) {
      results.innerHTML = '';
      results.append(el('p', { class: 'error', text: err.message }));
    }
  };

  const form = el('form', { class: 'row-form repo-search-form', onsubmit: (e) => { e.preventDefault(); run(); } }, [
    input,
    el('button', { class: 'ghost-btn', type: 'submit', text: 'Search' }),
  ]);

  host.append(el('h3', { class: 'repo-h', text: 'Add a model' }), form, results);
  if (repoView.results) paintRepoResults(results, repoView.results);
}

function paintRepoResults(host, found) {
  host.innerHTML = '';
  if (!found.length) {
    host.append(el('p', { class: 'muted', text: 'Nothing matched. Hugging Face is searched for '
      + 'repositories carrying GGUF files, which is what these backends can load.' }));
    return;
  }
  for (const m of found) host.append(repoResultCard(m));
}

// A search result carries everything the Hub knows in one request, so the card
// answers the two questions that decide a download — what is this, and will it
// run here — without opening anything.
function repoResultCard(m) {
  const caps = m.capabilities || [];
  const params = m.params_b
    ? `${Number(m.params_b).toFixed(m.params_b < 10 ? 1 : 0)}B` : null;
  // What it is.
  const what = [
    params,
    m.architecture || null,
    m.ctx_len ? `${compactCount(m.ctx_len)} ctx` : null,
    caps.includes('tools') ? 'tools' : null,
    caps.includes('vision') ? 'vision' : null,
  ].filter(Boolean).join(' · ');
  // Where it came from and what people make of it.
  const who = [
    m.license || null,
    m.downloads ? `${compactCount(m.downloads)} downloads / 30d` : null,
    m.downloads_all_time ? `${compactCount(m.downloads_all_time)} all time` : null,
    m.likes ? `${compactCount(m.likes)} likes` : null,
    m.updated_at ? `updated ${relativeTime(new Date(m.updated_at))}` : null,
  ].filter(Boolean).join(' · ');

  const detailHost = el('div', { class: 'repo-quants' });
  // The download used to be behind a quiet "Files" link, which is why nobody
  // found it. It is the reason this page exists, so it looks like it.
  const toggle = el('button', {
    class: 'ghost-btn repo-download-btn', type: 'button', text: 'Download…',
    onclick: () => toggleRepoDetail(m.id, detailHost, toggle).catch(showError),
  });

  return el('div', { class: 'repo-result' }, [
    el('div', { class: 'repo-result-head' }, [
      el('span', { class: 'repo-result-id', text: m.id }),
      fitBadge(m.fit, m.host_fits),
      // A gated repository needs a token and accepted terms. Finding that out
      // when the download fails is finding it out too late.
      m.gated ? el('span', { class: 'repo-gated', text: 'gated' }) : null,
      el('span', { class: 'repo-result-spacer' }),
      m.quant_count
        ? el('span', { class: 'muted', text: `${m.quant_count} quantisation${m.quant_count === 1 ? '' : 's'}` })
        : null,
      toggle,
    ]),
    what ? el('div', { class: 'muted', text: what }) : null,
    who ? el('div', { class: 'muted', text: who }) : null,
    // Which upstream model this was quantised from — the quickest way to tell
    // a dozen repackagings of the same weights apart.
    m.base_model && m.base_model !== m.id
      ? el('div', { class: 'muted repo-result-base', text: `quantised from ${m.base_model}` })
      : null,
    detailHost,
  ]);
}

/* ---- the fit badge ----
   A verdict is a statement about one machine, and until this server had a
   fleet there was only ever one it could be about. Now there are several, and
   "fits" without a name is an answer to a question nobody asked: what is being
   decided is which box to put the weights on.

   The name is omitted when the server has no fleet, where it would be noise —
   the badge then reads exactly as it always did. */
function fitBadge(fit, hostFits) {
  if (!fit || !fit.verdict) return null;
  const host = fit.host || '';
  const lines = [];
  if (host) lines.push(`Best of ${(hostFits || []).length || 1} machine(s): ${host}.`);
  // Every other machine's answer, so a red badge does not hide a green one on
  // a box further down the list.
  for (const f of hostFits || []) {
    if (f.host === fit.host) continue;
    lines.push(`${f.host || 'this server'}: ${f.verdict}`);
  }
  // The notes are where the reason lives — which is the part worth reading
  // when the verdict is not the one that was hoped for.
  for (const note of fit.notes || []) lines.push(note);
  return el('span', {
    class: `repo-fit ${fit.verdict}`,
    title: lines.join('\n'),
    text: host ? `${fit.verdict} · ${host}` : fit.verdict,
  });
}

// Counts on this page run from tens to tens of millions, and a column of full
// figures is unreadable at a glance.
function compactCount(n) {
  const v = Number(n) || 0;
  if (v < 1000) return `${v}`;
  if (v < 1000000) return `${(v / 1000).toFixed(v < 10000 ? 1 : 0)}k`;
  return `${(v / 1000000).toFixed(v < 10000000 ? 1 : 0)}M`;
}

// The quantisations are fetched per repository rather than up front: it is one
// Hub request each, and a search returning fifteen results would otherwise make
// fifteen of them for a list the operator will mostly scroll past.
async function toggleRepoDetail(hubID, host, button) {
  if (host.childElementCount) {
    host.innerHTML = '';
    if (button) button.textContent = 'Download…';
    return;
  }
  host.append(el('p', { class: 'muted', text: 'Loading files…' }));
  if (button) button.textContent = 'Hide files';

  const detail = await api(`/api/v1/models/detail/${hubID}`);
  host.innerHTML = '';
  const quants = detail.quants || [];
  if (!quants.length) {
    host.append(el('p', { class: 'muted', text:
      'No GGUF files in this repository — it may only carry the original weights.' }));
    return;
  }
  for (const q of quants) host.append(repoQuantRow(detail, q));
}

function repoQuantRow(detail, q, opts = {}) {
  // The fit verdict is the whole reason this is worth showing in a list: it
  // says which of eight near-identical filenames will actually run here.
  const fit = q.fit || null;
  const parts = q.parts || [];
  return el('div', { class: 'repo-quant' }, [
    el('span', { class: 'repo-quant-name', text: q.quant }),
    // Several files in a repository routinely infer to the same label —
    // Q2_K, Q2_K_L and UD-Q2_K_XL all read as Q2_K — so the label alone does
    // not say which row is which. The file name does, and it is also what
    // ends up on the drive.
    el('span', { class: 'repo-quant-file', text: (q.filename || '').split('/').pop() }),
    fitBadge(fit),
    // The download size and the memory it will occupy are different numbers
    // and both matter: one decides whether it fits on the drive, the other
    // whether it fits in the card.
    q.size_bytes ? el('span', { class: 'muted', text: `${bytes(q.size_bytes)} to fetch` }) : null,
    fit && fit.total_mb
      ? el('span', { class: 'muted', text: `${bytes(fit.total_mb * 1024 * 1024)} to load` })
      : null,
    // Weights past about 50GB ship as shards. Saying so matters: it is several
    // downloads rather than one, and none of them is a model on its own.
    parts.length > 1 ? el('span', { class: 'muted', text: `${parts.length} files` }) : null,
    q.incomplete
      ? el('span', { class: 'error', text: 'incomplete upload — some shards are missing' })
      : el('button', {
        class: 'ghost-btn', type: 'button', text: 'Download',
        disabled: Boolean(opts.disabled),
        title: opts.disabled ? opts.reason : (parts.length > 1
          ? `Fetch all ${parts.length} parts of ${q.quant} into the repository`
          : `Fetch ${q.filename} into the repository`),
        onclick: (e) => startRepoDownload(detail, q, e.target, opts.revision).catch(showError),
      }),
  ]);
}

async function startRepoDownload(detail, q, button, revision) {
  button.disabled = true;
  // A split quantization is one model in several files, and it is only loadable
  // once every shard has arrived. They are started together, as separate jobs,
  // because that is what the progress panel can report on.
  const files = (q.parts && q.parts.length) ? q.parts : [q.filename];
  try {
    for (const filename of files) {
      await api('/api/v1/repo/download', {
        method: 'POST',
        body: JSON.stringify({
          hub_id: detail.id,
          filename,
          // Passed through so the index records the Hub's own parse of the GGUF
          // header rather than a guess made from the filename later.
          quant: q.quant || '',
          params_b: detail.params_b || 0,
          // Empty means main. Pinning is what makes a download reproducible,
          // and the Hub screen is the only place that can offer the choice
          // because it is the only one that lists the refs.
          revision: revision || '',
        }),
      });
    }
    toast(files.length > 1
      ? `Downloading ${q.quant} in ${files.length} parts. It carries on if you leave this page.`
      : `Downloading ${files[0]}. It carries on if you leave this page.`);
    const host = document.querySelector('.repo-jobs');
    if (host) {
      const { jobs } = await api('/api/v1/repo/jobs');
      paintRepoJobs(host, jobs || []);
    }
    startRepoPolling();
  } finally {
    button.disabled = false;
  }
}

/* ---- what is on the drive ---- */

function paintRepoFiles(host, files, vocab) {
  host.innerHTML = '';
  host.append(el('h3', { class: 'repo-h', text: 'On the drive' }));

  if (!files.length) {
    host.append(el('p', { class: 'muted', text:
      'Nothing here yet. Search above, or copy GGUF files onto the drive by hand — '
      + 'the listing walks the directory, so anything you put there shows up.' }));
    return;
  }
  for (const f of files) host.append(repoFileCard(f, vocab));
}

function repoFileCard(f, vocab) {
  const facts = [
    f.params_b ? `${Number(f.params_b).toFixed(f.params_b < 10 ? 1 : 0)}B` : null,
    f.quant || null,
    f.size_bytes ? bytes(f.size_bytes) : null,
    f.estimated ? 'from the filename' : null,
    f.added_at || null,
  ].filter(Boolean).join(' · ');

  const tagHost = el('div', { class: 'repo-tags' });
  paintRepoTags(tagHost, f, vocab);

  return el('div', { class: `repo-file ${f.missing ? 'missing' : ''}` }, [
    el('div', { class: 'repo-file-head' }, [
      el('span', { class: 'repo-file-name', text: f.name }),
      f.missing
        ? el('span', { class: 'repo-badge missing', title:
            'The index remembers this file but it is not on the drive. The drive may be '
            + 'mounted elsewhere, or it was deleted outside wintermute.', text: 'missing' })
        : null,
      // Whether the bytes were checked against a published digest. Absent is a
      // normal state, not a fault: Hugging Face only publishes a content hash
      // for files kept in LFS.
      !f.missing && f.verified
        ? el('span', { class: 'repo-badge verified', title: `sha256 ${f.sha256}`, text: 'verified' })
        : null,
      fitBadge(f.fit),
    ]),
    el('div', { class: 'muted repo-file-facts', text: facts }),
    f.hub_id ? el('div', { class: 'muted repo-file-src', text: f.hub_id }) : null,
    el('div', { class: 'muted repo-file-path', text: f.rel_path }),
    tagHost,
    el('div', { class: 'repo-file-actions' }, [
      el('button', {
        class: 'link-btn danger', type: 'button',
        text: f.missing ? 'Forget' : 'Delete',
        onclick: () => deleteRepoFile(f).catch(showError),
      }),
    ]),
  ]);
}

function paintRepoTags(host, f, vocab) {
  host.innerHTML = '';
  for (const t of f.tags || []) {
    host.append(el('span', { class: 'repo-tag' }, [
      el('span', { text: t }),
      el('button', {
        class: 'repo-tag-x', type: 'button', text: '×', title: `Remove "${t}"`,
        onclick: () => removeRepoTag(f, t, host, vocab).catch(showError),
      }),
    ]));
  }

  // A datalist of what is already in use, so the vocabulary converges instead
  // of growing a synonym every time somebody types.
  const listID = 'repo-tag-vocab';
  const input = el('input', {
    class: 'repo-tag-add', placeholder: '+ label', list: listID, autocomplete: 'off',
  });
  input.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    e.preventDefault();
    const value = input.value.trim();
    if (!value) return;
    input.value = '';
    addRepoTag(f, value, host, vocab).catch(showError);
  });
  host.append(input);
  if (!document.getElementById(listID)) {
    document.body.append(el('datalist', { id: listID },
      (vocab || []).map((t) => el('option', { value: t }))));
  }
}

async function addRepoTag(f, tag, host, vocab) {
  const { tag: saved } = await api('/api/v1/repo/tags', {
    method: 'POST',
    body: JSON.stringify({ rel_path: f.rel_path, tag }),
  });
  if (!f.tags) f.tags = [];
  if (!f.tags.includes(saved)) f.tags.push(saved);
  f.tags.sort();
  paintRepoTags(host, f, vocab);
}

async function removeRepoTag(f, tag, host, vocab) {
  await api('/api/v1/repo/tags/remove', {
    method: 'POST',
    body: JSON.stringify({ rel_path: f.rel_path, tag }),
  });
  f.tags = (f.tags || []).filter((t) => t !== tag);
  paintRepoTags(host, f, vocab);
}

// Deleting weights is hours of downloading thrown away, so it asks for the word
// to be typed rather than for a click — the same bar the memory wipe sets, for
// the same reason. The server checks the confirmation too; this one is a
// courtesy, not the control.
async function deleteRepoFile(f) {
  if (f.missing) {
    // Nothing to erase: the file is already gone and only the record is left.
    if (!window.confirm(`Forget ${f.rel_path}?\n\n`
      + 'The file is not on the drive. This only drops what was recorded about it.')) return;
  } else {
    const typed = window.prompt(
      `Delete ${f.name}?\n\nThis erases ${bytes(f.size_bytes)} from the drive permanently. `
      + `It would have to be downloaded again.\n\nType: delete`);
    if (typed === null) return;
    if (typed.trim().toLowerCase() !== 'delete') {
      showError(new Error('Not deleted — the confirmation did not match.'));
      return;
    }
  }
  await api('/api/v1/repo/delete', {
    method: 'POST',
    body: JSON.stringify({ rel_path: f.rel_path, confirm: 'delete' }),
  });
  toast(f.missing ? 'Record dropped.' : `${f.name} deleted.`);
  await refreshRepoFiles();
}


/* ---------- browsing the Hugging Face Hub ----------
   The Repository tab answers "what do I have"; this one answers "what is out
   there". They are separate screens because they are separate questions, and
   the same split already exists between Models and Repository.

   Every request goes through the server. The browser has no Hub token, it
   would be metered against its own address, and it cannot grade a model
   against this host's hardware. The server can do all three.

   One repository is open at a time, and its panels are fetched only when the
   tab holding them is first opened. That is not laziness: the file tree, the
   card, the refs and the scan are four requests, and the anonymous allowance
   is five hundred every five minutes. */

const hubView = {
  query: '', author: '', pipeline: '', library: '', sort: '', gguf: true,
  // tags narrow the search at the Hub; maxGB narrows the results here. The two
  // are deliberately different mechanisms — see hubTagPicker and hubMemStops.
  tags: [], maxGB: 0,
  results: null, next: '', vocab: null, rate: null, hasToken: false,
  // The machines fit verdicts on this screen are about — see paintHubRate.
  fitHosts: [],
  open: null, repoReady: false, repoNote: '',
};

async function renderAdminHub(body) {
  stopRepoPolling();

  const rateEl = el('div', { class: 'hub-rate' });
  const filterEl = el('div', { class: 'hub-filters' });
  const jobsEl = el('div', { class: 'repo-jobs' });
  const resultsEl = el('div', { class: 'hub-results' });
  body.append(rateEl, filterEl, jobsEl, resultsEl);

  // The repository state decides whether a download can be offered at all.
  // Its absence is not an error here — the Hub is perfectly browsable without
  // anywhere to put what it holds.
  const [status, repo, vocab] = await Promise.all([
    api('/api/v1/hub/status').catch(() => ({})),
    api('/api/v1/repo').catch(() => null),
    hubView.vocab ? Promise.resolve(null) : api('/api/v1/hub/tags').catch(() => ({ tags: {} })),
  ]);
  if (vocab) hubView.vocab = vocab.tags || {};
  if (status.rate_limit) hubView.rate = status.rate_limit;
  hubView.hasToken = Boolean(status.has_token);
  hubView.fitHosts = status.fit_hosts || [];

  const st = (repo && repo.status) || {};
  hubView.repoReady = Boolean(st.available && st.initialised);
  hubView.repoNote = !repo
    ? 'No model repository is configured on this server, so there is nowhere to download to.'
    : (!st.configured ? 'No model repository is configured — see Huginn → Repository.'
      : (!st.available ? 'The model repository is not available right now — see Huginn → Repository.'
        : (!st.initialised ? 'The model repository has not been initialised — see Huginn → Repository.'
          : '')));

  paintHubRate(rateEl);
  paintHubFilters(filterEl, resultsEl);
  if (repo && repo.jobs) {
    paintRepoJobs(jobsEl, repo.jobs);
    if (repo.jobs.some((j) => j.state === 'running')) startRepoPolling();
  }
  if (hubView.results) paintHubResults(resultsEl);
}

/* ---- what is left of the allowance ---- */

// The Hub meters requests in five-minute windows and this server shares one
// address with every browser pointed at it. Shown rather than discovered by
// failing, because the failure arrives in the middle of a search and looks like
// a bug in this page.
function paintHubRate(host) {
  host.innerHTML = '';
  const rl = hubView.rate;
  const hosts = hubView.fitHosts || [];
  // Which machines the badges below are about. A fleet server grades against
  // its nodes and not against itself, and with nothing declared it grades
  // against nothing at all — which is worth saying out loud, because the
  // symptom is a page of "unknown" that looks like a fault in the estimator.
  const graded = hosts.length
    ? el('span', { class: 'muted', title:
        'Fit verdicts describe these machines. A model is graded against each '
        + 'and the badge names the one that runs it best.',
      text: `fit graded on ${hosts.join(', ')}` })
    : el('span', { class: 'muted', title:
        'No machine is declared as one that runs models, so nothing can be '
        + 'graded and every verdict reads "unknown". Set "node" on the backend '
        + 'that serves your models — the name it was registered under with '
        + '-add-client — or run a backend on this host.',
      text: 'no machine declared — fit unknown' });

  const token = hubView.hasToken
    ? el('span', { class: 'hub-token ok', title:
        'Requests are attributed to the configured account, which has its own '
        + 'allowance and can reach gated repositories it has been granted.',
      text: 'token configured' })
    : el('span', { class: 'hub-token', title:
        'Without a token the Hub counts requests against this server’s IP address, '
        + 'shares that count with anything else on it, and refuses gated repositories. '
        + 'Set HUGGINGFACE_TOKEN to raise it.',
      text: 'anonymous' });

  if (!rl || !rl.quota) {
    host.append(el('div', { class: 'hub-rate-head' }, [token, graded]));
    return;
  }
  const used = Math.max(0, rl.quota - rl.remaining);
  const pct = Math.min(100, Math.round((used / rl.quota) * 100));
  host.append(
    el('div', { class: 'hub-rate-head' }, [
      token,
      el('span', { class: 'muted', text:
        `${rl.remaining} of ${rl.quota} Hub requests left, resetting in ${rl.reset_seconds}s` }),
      graded,
    ]),
    el('div', { class: 'repo-bar' }, [
      el('div', { class: `repo-bar-fill ${pct > 85 ? 'tight' : ''}`, style: `width:${pct}%` }),
    ]),
  );
}

/* ---- narrowing the Hub ---- */

function paintHubFilters(host, resultsEl) {
  host.innerHTML = '';

  const search = el('input', {
    class: 'repo-query', type: 'search', placeholder: 'Search the Hugging Face Hub…',
    value: hubView.query, autocomplete: 'off', spellcheck: 'false',
  });
  const author = el('input', {
    class: 'hub-input', type: 'text', placeholder: 'Author', value: hubView.author,
    autocomplete: 'off', spellcheck: 'false',
    title: 'One publisher, e.g. unsloth, Qwen, bartowski.',
  });

  const vocab = hubView.vocab || {};
  const pipeline = hubSelect('Any task', vocab.pipeline_tag, hubView.pipeline);
  const library = hubSelect('Any library', vocab.library, hubView.library);
  // Reordering applies on change rather than waiting for Search. The sort you
  // just picked is the entire intent of the click, and a list that does not
  // move until a second button is pressed reads as a broken control.
  const sort = el('select', {
    class: 'hub-input',
    title: 'Reorders the search at the Hub, not just the results already loaded.',
    onchange: () => run(true).catch(showError),
  }, [
    ['', 'Most downloaded (30d)'],
    ['downloadsAllTime', 'Most downloaded (all time)'],
    ['trendingScore', 'Trending'],
    ['likes', 'Most liked'],
    ['lastModified', 'Recently updated'],
    ['createdAt', 'Recently created'],
  ].map(([v, label]) => el('option', { value: v, selected: hubView.sort === v, text: label })));

  // GGUF is what these backends load, so it is the default. Turning it off is
  // how you go looking at original weights — which this server cannot run, and
  // says so on the card rather than pretending otherwise.
  const gguf = el('input', { type: 'checkbox', id: 'hub-gguf' });
  gguf.checked = hubView.gguf;

  // Tags are a Hub-side filter, so changing them costs a request — which is
  // why the picker applies once when it closes rather than on every tick.
  const tags = hubTagPicker(() => run(true).catch(showError));

  const memLabel = el('span', { class: 'hub-slider-label', text: hubMemLabel() });
  const mem = el('input', {
    class: 'hub-slider', type: 'range', min: '0', step: '1',
    max: String(hubMemStops.length - 1), value: String(hubMemIndex(hubView.maxGB)),
    title: 'Estimated footprint of the weights, KV cache and overhead at '
      + 'Q4_K_M and 8K context — a prediction from the parameter count, not a '
      + 'measurement. The Hub cannot filter on it, so this narrows the results '
      + 'already loaded rather than the search.',
  });
  // The label tracks the drag; the list is repainted on release. Repainting
  // every tick would tear down and rebuild an opened repository panel dozens of
  // times on the way past it.
  mem.addEventListener('input', () => {
    hubView.maxGB = hubMemStops[Number(mem.value)] || 0;
    memLabel.textContent = hubMemLabel();
  });
  mem.addEventListener('change', () => paintHubResults(resultsEl));

  // Pulling the controls into the state is split out because the row can be
  // rebuilt from under the operator — clicking a tag on a card does it — and a
  // search box that quietly reverts to the last thing searched loses whatever
  // was half-typed into it.
  const sync = () => {
    hubView.query = search.value.trim();
    hubView.author = author.value.trim();
    hubView.pipeline = pipeline.value;
    hubView.library = library.value;
    hubView.sort = sort.value;
    hubView.gguf = gguf.checked;
  };
  hubView.sync = sync;

  const run = (reset) => {
    sync();
    if (reset) { hubView.results = null; hubView.next = ''; hubView.open = null; }
    return runHubSearch(resultsEl, reset);
  };

  const form = el('form', {
    class: 'hub-filter-form',
    onsubmit: (e) => { e.preventDefault(); run(true).catch(showError); },
  }, [
    el('div', { class: 'hub-filter-row' }, [search, el('button', { class: 'ghost-btn', type: 'submit', text: 'Search' })]),
    el('div', { class: 'hub-filter-row' }, [
      author, pipeline, library, sort, tags,
      el('label', { class: 'hub-check' }, [gguf, el('span', { text: 'GGUF only' })]),
    ]),
    el('div', { class: 'hub-filter-row hub-slider-row' }, [
      el('span', { class: 'muted', text: 'Memory' }), mem, memLabel,
    ]),
  ]);

  host.append(el('h3', { class: 'repo-h', text: 'Find a model' }), form);
  if (hubView.repoNote) {
    host.append(el('p', { class: 'muted', text: hubView.repoNote }));
  }
}

function hubSelect(placeholder, options, current) {
  const select = el('select', { class: 'hub-input' },
    el('option', { value: '', text: placeholder }));
  for (const o of options || []) {
    select.append(el('option', {
      value: o.id, selected: current === o.id, text: o.label || o.id,
    }));
  }
  return select;
}

/* ---- tags ----
   The Hub indexes its tags by type and will hand over the whole vocabulary —
   five and a half thousand languages, two and a half thousand datasets, eighty
   licences. A control offering all of it is a control nobody reads, so these
   are the groups that change what a model *is*, in the order someone shopping
   for something to run locally would reach for them. Task and library have
   their own selects already and are deliberately not repeated here.

   The ids are fixed here rather than taken from the vocabulary because the
   vocabulary fetch is allowed to fail — the picker still works when it does,
   with the ids as their own labels. */
const hubTagGroups = [
  { key: 'other', label: 'Traits', ids: ['moe', 'merge', 'custom_code', '4-bit', '8-bit'] },
  {
    key: 'language', label: 'Language',
    ids: ['en', 'zh', 'multilingual', 'fr', 'de', 'es', 'ja', 'ko', 'ru', 'pt', 'it', 'ar', 'hi'],
  },
  {
    key: 'licence', label: 'Licence', vocab: 'license',
    ids: ['license:apache-2.0', 'license:mit', 'license:llama3.3', 'license:llama3.2',
      'license:llama3.1', 'license:gemma', 'license:cc-by-4.0', 'license:cc-by-nc-4.0'],
  },
];

// One checkbox dropdown over those groups. A <details> rather than a hand-built
// popover: it is keyboard-reachable and toggles without any state of its own.
function hubTagPicker(onApply) {
  const vocab = hubView.vocab || {};
  const chosen = new Set(hubView.tags || []);
  // What was last sent to the Hub. Compared on close so that opening the panel
  // and changing nothing costs no request — and so that reverting a tick back
  // to where it started costs none either.
  let applied = [...chosen].sort().join('\u0000');

  const summary = el('summary', { class: 'hub-input hub-tagpick-summary' });
  const paintSummary = () => {
    summary.textContent = chosen.size ? `Tags · ${chosen.size}` : 'Tags';
    summary.classList.toggle('on', chosen.size > 0);
  };
  paintSummary();

  const boxes = [];
  const groups = hubTagGroups.map((g) => {
    const known = vocab[g.vocab || g.key] || [];
    const items = g.ids.map((id) => {
      const box = el('input', { type: 'checkbox' });
      box.checked = chosen.has(id);
      box.addEventListener('change', () => {
        if (box.checked) chosen.add(id); else chosen.delete(id);
        paintSummary();
      });
      boxes.push(box);
      const found = known.find((t) => t.id === id);
      return el('label', { class: 'hub-tagpick-item' }, [
        box, el('span', { text: (found && (found.label || found.id)) || id }),
      ]);
    });
    return el('div', { class: 'hub-tagpick-group' }, [
      el('div', { class: 'hub-tagpick-head', text: g.label }),
      el('div', { class: 'hub-tagpick-items' }, items),
    ]);
  });

  const clear = el('button', {
    class: 'ghost-btn hub-tagpick-clear', type: 'button', text: 'Clear',
    onclick: () => {
      chosen.clear();
      for (const b of boxes) b.checked = false;
      paintSummary();
    },
  });

  const details = el('details', { class: 'hub-tagpick' }, [
    summary,
    el('div', { class: 'hub-tagpick-panel' }, [
      ...groups,
      el('div', { class: 'hub-tagpick-foot' }, [
        el('span', { class: 'muted', text: 'Applied when this closes.' }), clear,
      ]),
    ]),
  ]);

  // A <details> does not close when the click lands elsewhere, and a filter
  // panel that stays open over the results it just changed is in the way. The
  // listener exists only while the panel is open, so nothing outlives it.
  const closeOutside = (e) => {
    if (!details.contains(e.target)) details.open = false;
  };

  // Applied on close, once, rather than on every box: each change is a fresh
  // Hub search, and the allowance is five hundred requests in five minutes.
  details.addEventListener('toggle', () => {
    if (details.open) {
      document.addEventListener('click', closeOutside, true);
      return;
    }
    document.removeEventListener('click', closeOutside, true);
    const now = [...chosen].sort();
    if (now.join('\u0000') === applied) return;
    applied = now.join('\u0000');
    hubView.tags = now;
    onApply();
  });
  return details;
}

/* ---- memory ----
   Stops rather than a linear range: the interesting decisions are all in the
   first sixteen gigabytes — the difference between 6 and 8 decides whether a
   card runs it — and nobody needs 90 GB resolved to the gigabyte. The trailing
   0 is "no limit", and it is where the slider starts. */
const hubMemStops = [1, 2, 3, 4, 5, 6, 8, 10, 12, 16, 20, 24, 32, 40, 48, 64, 96, 0];

function hubMemIndex(gb) {
  const at = hubMemStops.indexOf(gb);
  return at < 0 ? hubMemStops.length - 1 : at;
}

function hubMemLabel() {
  return hubView.maxGB ? `${hubView.maxGB} GB or less` : 'Any size';
}

// Split the results against the limit.
//
// A model whose footprint is unknown is kept rather than hidden. The estimate
// needs a parameter count, and when the Hub has no GGUF header and the name
// does not say, there is none — hiding a repository on a fact this page does
// not have would be the page inventing one. The count line says how many.
function hubByMemory(found) {
  if (!hubView.maxGB) return { shown: found, hidden: 0, unknown: 0 };
  const limitMB = hubView.maxGB * 1024;
  let hidden = 0;
  let unknown = 0;
  const shown = found.filter((m) => {
    const total = m.fit && m.fit.total_mb;
    if (!total) { unknown += 1; return true; }
    if (total > limitMB) { hidden += 1; return false; }
    return true;
  });
  return { shown, hidden, unknown };
}

async function runHubSearch(host, reset) {
  const params = new URLSearchParams();
  if (hubView.query) params.set('q', hubView.query);
  if (hubView.author) params.set('author', hubView.author);
  if (hubView.pipeline) params.set('pipeline_tag', hubView.pipeline);
  if (hubView.library) params.set('library', hubView.library);
  if (hubView.sort) params.set('sort', hubView.sort);
  for (const t of hubView.tags || []) params.append('filter', t);
  if (!hubView.gguf) params.set('gguf', 'false');
  params.set('limit', '20');
  if (!reset && hubView.next) params.set('cursor', hubView.next);

  if (reset) {
    host.innerHTML = '';
    host.append(el('p', { class: 'muted', text: 'Searching…' }));
  }

  const data = await api(`/api/v1/hub/search?${params.toString()}`);
  if (data.rate_limit) hubView.rate = data.rate_limit;
  hubView.results = reset ? (data.results || []) : (hubView.results || []).concat(data.results || []);
  hubView.next = data.next || '';
  paintHubResults(host);
  const rateHost = document.querySelector('.hub-rate');
  if (rateHost) paintHubRate(rateHost);
}

function paintHubResults(host) {
  host.innerHTML = '';
  const found = hubView.results || [];
  if (!found.length) {
    host.append(el('p', { class: 'muted', text:
      'Nothing matched. Widen the filters, or clear "GGUF only" to see repositories '
      + 'carrying the original weights.' }));
    return;
  }

  const { shown, hidden, unknown } = hubByMemory(found);

  // Two different emptinesses, and telling them apart is the whole point: the
  // Hub found nothing, or the Hub found things and the slider is hiding them.
  // The advice for one is useless for the other.
  if (!shown.length) {
    host.append(el('p', { class: 'muted', text:
      `All ${found.length} result${found.length === 1 ? '' : 's'} need more than `
      + `${hubView.maxGB} GB. Raise the memory limit to see them.` }));
    return;
  }

  const counted = [
    hidden
      ? `${shown.length} of ${found.length} results · ${hidden} over ${hubView.maxGB} GB`
      : `${found.length} result${found.length === 1 ? '' : 's'}`,
    unknown ? `${unknown} with no known footprint, shown anyway` : null,
  ].filter(Boolean).join(' · ');
  host.append(el('div', { class: 'muted hub-count', text: counted }));
  for (const m of shown) host.append(hubResultCard(m));

  if (hubView.next) {
    host.append(el('button', {
      class: 'ghost-btn hub-more', type: 'button', text: 'Load more',
      onclick: (e) => {
        e.target.disabled = true;
        runHubSearch(host, false).catch((err) => { e.target.disabled = false; showError(err); });
      },
    }));
  }
}

/* ---- the tags on a card ----
   A repository carries fifteen or twenty tags and most of them are already on
   the card as their own fact: the licence is in the second line, the base model
   is under it, the architecture and the task are in the first, and the library
   and the GGUF flag are controls above the list. Repeating those spends a line
   to say nothing. What is left is what the Hub knows and this page does not —
   the languages, the publisher's own marks, and whether it is a chat model. */
const hubTagNoise = new Set([
  // Already shown, or the mechanism that found the result in the first place.
  'gguf', 'transformers', 'safetensors', 'pytorch', 'tf', 'jax', 'onnx', 'peft',
  'diffusers', 'sentence-transformers', 'tensorboard', 'timm', 'keras', 'mlx',
  'transformers.js', 'adapter-transformers',
  // Hosting and bookkeeping. True of most of the Hub and decides nothing here.
  'endpoints_compatible', 'autotrain_compatible', 'text-generation-inference',
  'text-embeddings-inference', 'eval-results', 'model-index', 'has_space',
  'co2_eq_emissions',
]);

// Whole families of machine-generated tags: one per citation, per dataset, per
// base model. Useful to the Hub's own indexes, unreadable on a card.
const hubTagNoisePrefixes = [
  'base_model:', 'license:', 'region:', 'deploy:', 'arxiv:', 'dataset:',
  'doi:', 'bucket:',
];

// Ten is about a line and a half at this size. The rest are counted rather than
// dropped silently, and named in the title.
const hubCardTagCap = 10;

function hubCardTags(m) {
  const shown = new Set([m.pipeline_tag, m.architecture].filter(Boolean));
  return (m.tags || []).filter((t) => typeof t === 'string' && t
    && !shown.has(t) && !hubTagNoise.has(t)
    && !hubTagNoisePrefixes.some((prefix) => t.startsWith(prefix)));
}

// Rendered as buttons because every one of them is a valid Hub filter, and
// "more like this one" is the question a tag on a card is asked. Clicking one
// that is already filtering removes it again.
function hubTagChips(m) {
  const tags = hubCardTags(m);
  if (!tags.length) return null;
  const active = new Set(hubView.tags || []);
  const head = tags.slice(0, hubCardTagCap);
  const rest = tags.slice(hubCardTagCap);
  return el('div', { class: 'hub-card-tags' }, [
    ...head.map((t) => el('button', {
      class: `hub-card-tag ${active.has(t) ? 'on' : ''}`, type: 'button', text: t,
      title: active.has(t) ? `Stop filtering on ${t}.` : `Narrow the search to ${t}.`,
      onclick: () => toggleHubTag(t),
    })),
    rest.length
      ? el('span', { class: 'muted hub-card-tag-more', title: rest.join(', '),
        text: `+${rest.length}` })
      : null,
  ]);
}

// Adding a tag from a card changes the same state the picker owns, so the
// picker is rebuilt rather than left showing a selection that is no longer the
// one being searched.
function toggleHubTag(tag) {
  if (hubView.sync) hubView.sync();
  const now = new Set(hubView.tags || []);
  if (now.has(tag)) now.delete(tag); else now.add(tag);
  hubView.tags = [...now].sort();
  hubView.results = null;
  hubView.next = '';
  hubView.open = null;

  const filterHost = document.querySelector('.hub-filters');
  const resultsHost = document.querySelector('.hub-results');
  if (!resultsHost) return;
  if (filterHost) paintHubFilters(filterHost, resultsHost);
  runHubSearch(resultsHost, true).catch(showError);
}

// One card. The facts are the ones that decide whether to open it at all: what
// it is, whether it runs here, and whether it can be fetched without an
// account.
function hubResultCard(m) {
  const params = m.params_b
    ? `${Number(m.params_b).toFixed(m.params_b < 10 ? 1 : 0)}B` : null;
  const caps = m.capabilities || [];
  const what = [
    params,
    m.architecture || null,
    m.pipeline_tag || null,
    m.ctx_len ? `${compactCount(m.ctx_len)} ctx` : null,
    caps.includes('tools') ? 'tools' : null,
    caps.includes('vision') ? 'vision' : null,
  ].filter(Boolean).join(' · ');
  const who = [
    m.license || null,
    m.downloads ? `${compactCount(m.downloads)} downloads / 30d` : null,
    m.downloads_all_time ? `${compactCount(m.downloads_all_time)} all time` : null,
    m.likes ? `${compactCount(m.likes)} likes` : null,
    m.updated_at ? `updated ${relativeTime(new Date(m.updated_at))}` : null,
  ].filter(Boolean).join(' · ');

  const panel = el('div', { class: 'hub-panel' });
  const open = el('button', {
    class: 'ghost-btn repo-download-btn', type: 'button',
    text: hubView.open && hubView.open.id === m.id ? 'Close' : 'Open',
    onclick: () => toggleHubRepo(m, panel, open).catch(showError),
  });

  const card = el('div', { class: 'repo-result' }, [
    el('div', { class: 'repo-result-head' }, [
      el('span', { class: 'repo-result-id', text: m.id }),
      fitBadge(m.fit, m.host_fits),
      m.gated ? el('span', { class: 'repo-gated', title:
        'This repository needs a token and its terms accepted before it can be fetched.',
      text: 'gated' }) : null,
      el('span', { class: 'repo-result-spacer' }),
      m.quant_count
        ? el('span', { class: 'muted', text: `${m.quant_count} quantisation${m.quant_count === 1 ? '' : 's'}` })
        : null,
      open,
    ]),
    what ? el('div', { class: 'muted', text: what }) : null,
    who ? el('div', { class: 'muted', text: who }) : null,
    m.base_model && m.base_model !== m.id
      ? el('div', { class: 'muted repo-result-base', text: `quantised from ${m.base_model}` })
      : null,
    // Somewhere this can be tried without fetching it first.
    (m.providers || []).length
      ? el('div', { class: 'muted repo-result-base', text:
          `served by ${(m.providers || []).filter((p) => p.status === 'live').map((p) => p.name).join(', ') || '—'}` })
      : null,
    hubTagChips(m),
    panel,
  ]);
  if (hubView.open && hubView.open.id === m.id) paintHubRepo(panel);
  return card;
}

/* ---- one repository, opened ---- */

async function toggleHubRepo(m, panel, button) {
  if (hubView.open && hubView.open.id === m.id) {
    hubView.open = null;
    panel.innerHTML = '';
    button.textContent = 'Open';
    return;
  }
  // Only one at a time. Four panels each is a lot of requests to leave open
  // behind a page of results nobody scrolled back to.
  hubView.open = { id: m.id, tab: 'quants', revision: '', cache: {} };
  for (const other of document.querySelectorAll('.hub-panel')) other.innerHTML = '';
  for (const other of document.querySelectorAll('.repo-download-btn')) other.textContent = 'Open';
  button.textContent = 'Close';
  await paintHubRepo(panel);
}

async function paintHubRepo(panel) {
  const open = hubView.open;
  if (!open) return;
  panel.innerHTML = '';

  const tabs = [
    ['quants', 'Quantisations'],
    ['files', 'Files'],
    ['card', 'Model card'],
    ['revisions', 'Revisions'],
    ['security', 'Security'],
  ];
  const bar = el('div', { class: 'hub-tabs' }, tabs.map(([id, label]) => el('button', {
    class: `hub-tab ${open.tab === id ? 'active' : ''}`, type: 'button', text: label,
    onclick: () => { open.tab = id; paintHubRepo(panel).catch(showError); },
  })));

  const pinned = open.revision
    ? el('span', { class: 'hub-pin', title:
        'Files and downloads below are taken from this revision rather than main.',
      text: `pinned to ${open.revision}` })
    : null;

  const content = el('div', { class: 'hub-panel-body' },
    el('p', { class: 'muted', text: 'Loading…' }));
  panel.append(el('div', { class: 'hub-tab-row' }, [bar, pinned]), content);

  try {
    await paintHubTab(content, open);
  } catch (err) {
    content.innerHTML = '';
    content.append(el('p', { class: 'error', text: err.message }));
  }
}

// Each tab fetches once and remembers. Pinning a revision drops the panels that
// depend on it, because a file list from main is not the file list of a tag.
async function hubFetch(open, key, path) {
  if (open.cache[key]) return open.cache[key];
  const data = await api(path);
  if (data.rate_limit) {
    hubView.rate = data.rate_limit;
    const rateHost = document.querySelector('.hub-rate');
    if (rateHost) paintHubRate(rateHost);
  }
  open.cache[key] = data;
  return data;
}

function hubRev(open, sep = '?') {
  return open.revision ? `${sep}revision=${encodeURIComponent(open.revision)}` : '';
}

async function paintHubTab(host, open) {
  const id = encodeURI(open.id);

  if (open.tab === 'quants') {
    const key = `detail:${open.revision}`;
    const { model } = await hubFetch(open, key, `/api/v1/hub/detail/${id}`);
    host.innerHTML = '';
    const quants = (model && model.quants) || [];
    if (!quants.length) {
      host.append(el('p', { class: 'muted', text:
        'No GGUF files here — this repository carries the original weights, which these '
        + 'backends cannot load directly. The Files tab lists everything in it.' }));
      return;
    }
    const rows = el('div', { class: 'repo-quants' });
    for (const q of quants) {
      rows.append(repoQuantRow(model, q, {
        revision: open.revision,
        disabled: !hubView.repoReady,
        reason: hubView.repoNote,
      }));
    }
    host.append(rows);
    return;
  }

  if (open.tab === 'files') {
    const key = `tree:${open.revision}`;
    const data = await hubFetch(open, key, `/api/v1/hub/tree/${id}${hubRev(open)}`);
    host.innerHTML = '';
    const files = data.files || [];
    if (!files.length) {
      host.append(el('p', { class: 'muted', text: 'This revision has no files.' }));
      return;
    }
    for (const f of files) {
      host.append(el('div', { class: 'hub-file' }, [
        el('span', { class: 'hub-file-path', text: f.path }),
        el('span', { class: 'repo-result-spacer' }),
        // A digest is only published for files kept in LFS, which is every
        // weight of consequence. Its absence is normal, not a fault.
        f.sha256 ? el('span', { class: 'repo-badge', title: `sha256 ${f.sha256}`, text: 'lfs' }) : null,
        f.last_commit && f.last_commit.date
          ? el('span', { class: 'muted', text: relativeTime(new Date(f.last_commit.date)) })
          : null,
        el('span', { class: 'muted hub-file-size', text: f.type === 'directory' ? '' : bytes(f.size) }),
      ]));
    }
    if (data.next) {
      host.append(el('p', { class: 'muted', text:
        'Only the first page is listed. Repositories this large are shards of one model; '
        + 'the Quantisations tab groups them back together.' }));
    }
    return;
  }

  if (open.tab === 'card') {
    const key = `card:${open.revision}`;
    const { card } = await hubFetch(open, key, `/api/v1/hub/card/${id}${hubRev(open)}`);
    host.innerHTML = '';
    if (!card || !card.trim()) {
      host.append(el('p', { class: 'muted', text: 'This repository has no model card.' }));
      return;
    }
    host.append(el('p', { class: 'muted hub-card-note', text:
      'Written by whoever published this repository. Treat it the way you would any page '
      + 'off the internet: it is shown, not trusted, and nothing in it has been acted on.' }));
    // Rendered into DOM nodes by the same subset renderer the FAQ uses. No
    // innerHTML anywhere on the path, so raw HTML in a card appears as the text
    // it is rather than becoming markup in this page.
    host.append(el('div', { class: 'doc hub-card' }, renderMarkdown(card)));
    return;
  }

  if (open.tab === 'revisions') {
    const [refsData, commitsData] = await Promise.all([
      hubFetch(open, 'refs', `/api/v1/hub/refs/${id}`),
      hubFetch(open, `commits:${open.revision}`, `/api/v1/hub/commits/${id}?limit=10${hubRev(open, '&')}`),
    ]);
    host.innerHTML = '';

    const refs = refsData.refs || {};
    const choices = [{ name: '', label: 'main (default)' }]
      .concat((refs.branches || []).filter((b) => b.name !== 'main').map((b) => ({ name: b.name, label: `branch ${b.name}` })))
      .concat((refs.tags || []).map((t) => ({ name: t.name, label: `tag ${t.name}` })));

    const picker = el('select', { class: 'hub-input' }, choices.map((c) => el('option', {
      value: c.name, selected: open.revision === c.name, text: c.label,
    })));
    picker.addEventListener('change', () => {
      open.revision = picker.value;
      // Everything below depends on the revision, so it is discarded rather
      // than shown against a ref it did not come from.
      open.cache = { refs: refsData };
      paintHubRepo(picker.closest('.hub-panel')).catch(showError);
    });

    host.append(
      el('div', { class: 'hub-rev-pick' }, [
        el('span', { class: 'muted', text: 'Fetch from' }), picker,
      ]),
      el('p', { class: 'muted', text:
        'Pinning to a tag is what makes a download reproducible: main moves when the '
        + 'publisher re-uploads, and a tag does not.' }),
    );

    const commits = commitsData.commits || [];
    if (!commits.length) return;
    host.append(el('h4', { class: 'repo-h', text: 'History' }));
    for (const c of commits) {
      host.append(el('div', { class: 'hub-commit' }, [
        el('span', { class: 'hub-commit-title', text: c.title || c.id.slice(0, 12) }),
        el('span', { class: 'repo-result-spacer' }),
        (c.authors || []).length ? el('span', { class: 'muted', text: c.authors.join(', ') }) : null,
        c.date ? el('span', { class: 'muted', text: relativeTime(new Date(c.date)) }) : null,
      ]));
    }
    return;
  }

  // Security.
  const { scan } = await hubFetch(open, 'scan', `/api/v1/hub/scan/${id}`);
  host.innerHTML = '';
  const issues = (scan && scan.files_with_issues) || [];

  if (!scan || !scan.scans_done) {
    // Not the same as clean, and must not be able to read as clean.
    host.append(el('p', { class: 'muted', text:
      'Hugging Face has not finished scanning this repository. That is not the same as a '
      + 'clean result — it means there is not yet a result to show.' }));
  } else if (!issues.length) {
    host.append(el('p', { class: 'muted', text:
      'Hugging Face’s scanners flagged nothing in this repository. That is their verdict '
      + 'on the files, not a guarantee, and it says nothing about what the model will do.' }));
  }

  for (const f of issues) {
    host.append(el('div', { class: 'hub-scan-issue' }, [
      el('span', { class: `repo-badge ${['unsafe', 'suspicious'].includes(f.level) ? 'missing' : ''}`, text: f.level }),
      el('span', { class: 'hub-file-path', text: f.path }),
    ]));
  }
}

/* ---------- the FAQ ----------
   Served as Markdown from the embedded assets and rendered here, rather than
   shipped as a second copy in HTML. One source, and it stays readable in the
   repository — which is where anyone setting the thing up will actually be.

   Rendered into DOM nodes rather than assigned as innerHTML. That is the rule
   the whole UI keeps: every value reaching the DOM goes through el() or
   textContent. The FAQ is our own file rather than user input, so nothing here
   is hostile today, but a Markdown renderer that builds a string and hands it
   to innerHTML is a cross-site scripting hole waiting for the day somebody
   points it at content from somewhere else. */

async function renderAdminFAQ(body) {
  const res = await fetch('/FAQ.md', { headers: { Authorization: `Bearer ${state.token}` } });
  if (!res.ok) {
    body.append(el('p', { class: 'error', text: `Could not load the FAQ (${res.status}).` }));
    return;
  }
  body.append(el('div', { class: 'doc' }, renderMarkdown(await res.text())));
}

// A deliberately small Markdown subset: headings, paragraphs, lists, fenced
// code, tables, and inline code/bold/links. Enough for the FAQ, and small
// enough to read in one sitting — the alternative was embedding a library to
// render one document.
function renderMarkdown(text) {
  const lines = text.split('\n');
  const out = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Fenced code. Taken verbatim, including anything that looks like markup,
    // which is the point of a fence.
    if (line.startsWith('```')) {
      const code = [];
      i++;
      while (i < lines.length && !lines[i].startsWith('```')) code.push(lines[i++]);
      i++;
      out.push(el('pre', { class: 'doc-code' }, el('code', { text: code.join('\n') })));
      continue;
    }

    const heading = /^(#{1,4})\s+(.*)$/.exec(line);
    if (heading) {
      out.push(el(`h${heading[1].length}`, { class: 'doc-h' }, inlineMarkdown(heading[2])));
      i++;
      continue;
    }

    // Tables: a header row, a divider of dashes, then body rows.
    if (line.includes('|') && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[i + 1] || '')) {
      const head = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes('|')) rows.push(splitRow(lines[i++]));
      out.push(el('div', { class: 'doc-table-wrap' },
        el('table', { class: 'doc-table' }, [
          el('thead', {}, el('tr', {}, head.map((c) => el('th', {}, inlineMarkdown(c))))),
          el('tbody', {}, rows.map((r) => el('tr', {}, r.map((c) => el('td', {}, inlineMarkdown(c)))))),
        ])));
      continue;
    }

    if (/^\s*[-*]\s+/.test(line)) {
      const items = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(el('li', {}, inlineMarkdown(lines[i].replace(/^\s*[-*]\s+/, ''))));
        i++;
      }
      out.push(el('ul', { class: 'doc-list' }, items));
      continue;
    }

    if (!line.trim()) { i++; continue; }

    // Everything else is a paragraph, running until a blank line. Markdown's
    // soft wrapping means the FAQ's hard-wrapped source must be rejoined, or
    // every line would become its own paragraph.
    const para = [];
    while (i < lines.length && lines[i].trim() && !lines[i].startsWith('```')
           && !/^#{1,4}\s/.test(lines[i]) && !/^\s*[-*]\s+/.test(lines[i])) {
      para.push(lines[i++]);
    }
    out.push(el('p', { class: 'doc-p' }, inlineMarkdown(para.join(' '))));
  }
  return out;
}

function splitRow(line) {
  return line.replace(/^\s*\|/, '').replace(/\|\s*$/, '').split('|').map((c) => c.trim());
}

// Inline spans, returned as nodes. Code first so that bold or link syntax
// inside a code span is left alone.
function inlineMarkdown(text) {
  const nodes = [];
  const pattern = /`([^`]+)`|\*\*([^*]+)\*\*|\[([^\]]+)\]\(([^)]+)\)/g;
  let last = 0;
  let m;
  while ((m = pattern.exec(text)) !== null) {
    if (m.index > last) nodes.push(document.createTextNode(text.slice(last, m.index)));
    if (m[1] !== undefined) nodes.push(el('code', { class: 'doc-inline-code', text: m[1] }));
    else if (m[2] !== undefined) nodes.push(el('strong', { text: m[2] }));
    else nodes.push(linkNode(m[3], m[4]));
    last = m.index + m[0].length;
  }
  if (last < text.length) nodes.push(document.createTextNode(text.slice(last)));
  return nodes;
}

// A link, unless the target is not a kind of URL worth following.
//
// The renderer above was written for the FAQ, which is our own file, and its
// comment anticipates the day it is pointed at content from somewhere else.
// That day is the model card: prose from a stranger's repository, in which
// [text](javascript:...) is an ordinary thing to write and would otherwise
// become a live script link. Anything that is not plainly http, https or mailto
// is shown as the text it is, so nothing is hidden — it simply cannot be
// clicked into running.
function linkNode(text, href) {
  let scheme = '';
  try {
    scheme = new URL(href, window.location.origin).protocol;
  } catch {
    return document.createTextNode(`${text} (${href})`);
  }
  if (!['http:', 'https:', 'mailto:'].includes(scheme)) {
    return document.createTextNode(`${text} (${href})`);
  }
  return el('a', { href, target: '_blank', rel: 'noopener noreferrer', text });
}

// Recent server failures, at the top of the Status page because when there are
// any they are the most important thing on it.
//
// Absent entirely when nothing has failed, rather than an empty box: a
// permanent "Errors (0)" heading trains people to stop seeing it, which is
// exactly the wrong reflex for the one panel that should be noticed.
async function renderServerErrors(body) {
  let errors = [];
  try {
    ({ errors } = await api('/api/v1/admin/errors?limit=20'));
  } catch {
    return;
  }
  if (!errors || !errors.length) return;

  body.append(el('div', { class: 'group-head', text: 'Recent errors' }));
  body.append(el('p', { class: 'muted', text:
    'What the server was doing when it answered "internal error". Kept in memory '
    + 'and bounded, so this is what has failed recently rather than a history — '
    + 'restarting the server clears it.' }));

  const list = el('div', { class: 'err-list' });
  for (const e of errors) {
    list.append(el('div', { class: 'err-row' }, [
      el('span', { class: 'err-when', text: relativeTime(new Date(e.at)) }),
      el('span', { class: 'err-op', text: e.op }),
      el('span', { class: 'err-detail', text: e.error }),
    ]));
  }
  body.append(list);
}

// Shared memory: the master switch, and the two ways to throw things away.
//
// The two are kept visually apart on purpose. Clearing the index is
// reversible — a backfill rebuilds it from the conversations, which are
// untouched — while wiping the store is not, and a screen that presented them
// as two similar buttons would eventually get the wrong one pressed.
async function renderAdminMemory(body) {
  const m = await api('/api/v1/admin/memory');

  if (!m.configured) {
    body.append(el('p', { class: 'muted', text:
      'No embedder is configured, so nothing is being remembered across conversations. ' +
      'Set WINTERMUTE_EMBED_URL and WINTERMUTE_EMBED_MODEL to switch memory on.' }));
    return;
  }

  const toggle = el('button', {
    class: `memory-badge ${m.enabled ? 'on' : 'off'}`,
    type: 'button',
    onclick: () => setSharedMemory(!m.enabled).catch(showError),
  }, [
    el('span', { class: 'memory-dot', text: m.enabled ? '\u25cf' : '\u25cb' }),
    el('span', { text: m.enabled ? 'Shared memory on' : 'Shared memory off' }),
  ]);

  body.append(
    el('p', { class: 'muted', text: m.enabled
      ? 'Conversations can draw on earlier ones. Individual chats can still opt out.'
      : 'No conversation is being given prior context, whatever its own setting says. '
        + 'What is said is still being recorded and indexed, so turning this back on '
        + 'will not leave a gap.' }),
    toggle,
    facts([
      ['Embedder', m.embedder ? `${m.embedder} (${m.dimension} dimensions)` : 'nothing indexed yet'],
      ['Indexed messages', `${m.indexed} of ${m.messages}`],
      ['Waiting to be indexed', m.queued],
      ['Conversations', m.sessions],
    ]),
  );

  // The reversible one. Odin's two ravens: Huginn is thought, Muninn is
  // memory. The index is thought, derived from what Muninn holds — so renewing
  // Huginn rebuilds it from the conversations, and slaying Muninn below
  // destroys the conversations themselves. One is recoverable from the other;
  // the other is not recoverable from anything.
  body.append(
    el('h3', { text: 'Renew Huginn' }),
    el('p', { class: 'muted', text:
      'Throws away the vectors and the search index, then rebuilds them from the '
      + 'conversations, which are untouched. Recall is degraded until the queue drains. '
      + 'Use this if retrieval is behaving oddly, or after changing the embedding model.' }),
    el('button', {
      class: 'ghost-btn', type: 'button', text: 'Renew Huginn',
      onclick: () => renewHuginn().catch(showError),
    }),
  );

  // The irreversible one, kept apart and styled as the hazard it is.
  body.append(
    el('div', { class: 'danger-zone' }, [
      el('h3', { text: 'Slay Muninn' }),
      el('p', { class: 'muted', text:
        'Deletes all ' + m.sessions + ' conversations on this server, with their messages, '
        + 'their index entries and their audit rows. This is for clearing out test data on a '
        + 'new install. It cannot be undone — though snapshots taken before now still hold it.' }),
      el('button', {
        class: 'danger-btn', type: 'button', text: 'Slay Muninn',
        onclick: () => forgetEverything(m.sessions).catch(showError),
      }),
    ]),
  );
}

async function setSharedMemory(enabled) {
  await api('/api/v1/admin/memory', {
    method: 'PATCH',
    body: JSON.stringify({ enabled }),
  });
  await renderAdmin();
}

async function renewHuginn() {
  if (!window.confirm(
    'Renew Huginn?\n\n'
    + 'Your conversations are kept. The vectors and search index are thrown away and '
    + 'built again from what is stored. Recall is degraded until that finishes.')) return;
  const res = await api('/api/v1/admin/memory/rebuild-index', { method: 'POST' });
  await renderAdmin();
  toast(`Rebuilding: ${res.queued} messages queued.`);
}

// Typed confirmation rather than an OK button. This deletes everything and the
// point of making it awkward is that it should not be possible to do by
// reflex.
async function forgetEverything(sessions) {
  // The typed phrase stays plain rather than themed. A confirmation has one
  // job — to state the consequence unambiguously to someone who may be tired
  // and in a hurry — and "slay muninn" is a worse sentence to be sure about
  // than "delete everything".
  const typed = window.prompt(
    `Slay Muninn?\n\nThis deletes all ${sessions} conversations on this server, permanently.\n\n`
    + 'Messages, index entries and audit rows all go. Snapshots taken before now still hold them.\n\n'
    + 'Type: delete everything');
  if (typed === null) return;
  if (typed.trim() !== 'delete everything') {
    showError(new Error('Not deleted — the confirmation did not match.'));
    return;
  }
  const res = await api('/api/v1/admin/memory/forget-everything', {
    method: 'POST',
    body: JSON.stringify({ confirm: 'delete everything' }),
  });
  await renderAdmin();
  await loadSessions();
  toast(`Deleted ${res.deleted_sessions} conversations.`);
}

async function renderAdminStatus(body) {
  const s = await api('/api/v1/admin/status');
  await renderServerErrors(body);
  body.append(el('div', { class: 'stats' },
    stat(s.uptime, 'Uptime'),
    stat(s.sessions, 'Sessions'),
    stat(s.messages, 'Messages'),
    stat(s.muninn, 'Audited calls'),
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
    ['Max tokens', c.llm_max_tokens],
    ['Timeout', c.llm_timeout],
    ['Max tool iterations', c.max_tool_iterations],
  ]));

  // Credentials are reported as present or absent and never by value. This page
  // gets left open and screenshotted.
  body.append(el('div', { class: 'group-head', text: 'Credentials' }));
  body.append(facts([
    ['Hugging Face token', c.has_huggingface_token ? 'configured' : 'not set'],
  ]));
  body.append(el('div', { class: 'muted', text:
    'Secrets are never sent to this page — only whether they are set.' }));

  // What the token can actually do, which is not something its value shows: a
  // read token and a write one look identical, and a search failing on a gated
  // repository is the first time the difference is usually noticed. Asked of
  // the Hub rather than assumed, and best-effort — this is a detail on a page
  // that is already useful without it.
  if (!c.has_huggingface_token) {
    body.append(el('p', { class: 'muted', text:
      'Without a token the Hub counts requests against this server’s address, shares that '
      + 'count with anything else using it, and refuses gated repositories. Set '
      + 'HUGGINGFACE_TOKEN to raise the allowance and reach repositories you have been '
      + 'granted.' }));
    return;
  }
  try {
    const { identity } = await api('/api/v1/hub/whoami');
    body.append(facts([
      ['Hugging Face account', identity.fullname || identity.name || '—'],
      ['Token role', identity.role || '—'],
      ['Organisations', (identity.orgs || []).join(', ') || '—'],
      ['Scopes', (identity.permissions || []).join(', ') || '(not a fine-grained token)'],
      ['Plan', identity.is_pro ? 'PRO' : 'free'],
    ]));
  } catch (err) {
    body.append(el('p', { class: 'muted', text:
      `A token is configured but the Hub would not say whose it is: ${err.message}` }));
  }
}

// How long ago a backend was last probed. A status is only evidence about the
// moment it was taken, so the age is shown next to it: "ok" from four seconds
// ago and "ok" from yesterday are different claims.
function probedAge(iso) {
  if (!iso) return 'never';
  const secs = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return new Date(iso).toLocaleString();
}

async function renderAdminBackends(body) {
  const d = await api('/api/v1/backends');
  // Which backends this page may edit: the ones stored in the database. The
  // rest come from backends.json, and the server refuses to rewrite those, so
  // offering the control would only produce a 409.
  const declared = new Set(d.declared || []);
  const rows = (d.backends || []).map((b) => [
    b.name, b.kind, b.model || '—', b.status, probedAge(b.probed_at),
    // Which machine serves it, and so whose hardware decides every fit verdict
    // for the models it runs. Blank in the data means this server.
    b.node || (b.kind === 'anthropic' ? '—' : 'this server'),
    b.status_note || '', b.name === d.default ? 'default' : (b.name === d.fallback ? 'fallback' : ''),
    declared.has(b.name) ? 'UI' : 'backends.json',
    d.editable && declared.has(b.name) ? removeBackendButton(b.name) : '',
  ]);
  body.append(table(
    ['Name', 'Kind', 'Model', 'Status', 'Probed', 'Runs on', 'Note', 'Role', 'Source', ''], rows));

  if (d.editable) body.append(addBackendForm(d));

  const refresh = el('button', { class: 'ghost-btn', text: 'Re-probe backends' });
  refresh.addEventListener('click', async () => {
    refresh.disabled = true;
    try {
      await api('/api/v1/backends/refresh', { method: 'POST' });
      toast('Backends re-probed');
      await renderHuginn();
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

function removeBackendButton(name) {
  const btn = el('button', { class: 'ghost-btn danger', text: 'Remove' });
  btn.addEventListener('click', () => {
    // Sessions pinned to this backend are not rewritten: they keep the name,
    // and fall back to the server default once it stops resolving. Saying so
    // matters because the alternative reading — that the conversations go too
    // — is the one that would stop someone clicking.
    confirmDelete(`the "${name}" backend (conversations that used it are kept)`, async () => {
      await api(`/api/v1/backends/${encodeURIComponent(name)}`, { method: 'DELETE' });
      await renderHuginn();
    });
  });
  return btn;
}

// Declare a backend without editing backends.json and restarting.
//
// The API key is named, not entered: the field takes the *environment
// variable* holding the key, which is how backends.json does it and why a
// credential never travels through this form. For Anthropic that is normally
// ANTHROPIC_API_KEY, already set on the server, which is the whole reason this
// form can add Claude at all.
function addBackendForm(d) {
  const kinds = ['anthropic', 'ollama', 'llamacpp', 'vllm', 'openai', 'hailo'];
  const name = el('input', { type: 'text', placeholder: 'name, e.g. claude' });
  const kind = el('select', {}, kinds.map((k) => el('option', { value: k, text: k })));
  const baseURL = el('input', { type: 'text', placeholder: 'http://127.0.0.1:11434/v1' });
  const model = el('input', { type: 'text', placeholder: 'model (blank = backend default)' });
  const keyEnv = el('input', { type: 'text', placeholder: 'API key env var' });
  // Which machine this backend runs on. Nothing infers it from the URL: a
  // hostname says nothing reliable about which box answers, and the cost of
  // guessing wrong is a fit verdict computed against the wrong hardware, which
  // looks exactly as confident as a right one.
  const nodeName = el('input', { type: 'text', placeholder: 'runs on node (blank = this server)' });

  // Anthropic needs no URL and does need a key variable; every local kind is
  // the other way round. Following the kind keeps the form from asking for
  // something the server would reject.
  const follow = () => {
    const anthropic = kind.value === 'anthropic';
    baseURL.disabled = anthropic;
    baseURL.placeholder = anthropic ? 'not needed for anthropic' : 'http://127.0.0.1:11434/v1';
    if (anthropic && !keyEnv.value) keyEnv.value = 'ANTHROPIC_API_KEY';
    // A cloud backend runs on hardware nobody here can see, so there is no
    // node to name. The server refuses it too; disabling the field says so
    // before the form is submitted rather than after.
    nodeName.disabled = anthropic;
    nodeName.placeholder = anthropic
      ? 'not applicable — runs on Anthropic’s hardware'
      : 'runs on node (blank = this server)';
    if (anthropic) nodeName.value = '';
  };
  kind.addEventListener('change', follow);
  follow();

  const form = el('form', { class: 'row-form' },
    name, kind, baseURL, model, keyEnv, nodeName,
    el('button', { type: 'submit', text: 'Add backend' }));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      await api('/api/v1/backends', {
        method: 'POST',
        body: JSON.stringify({
          name: name.value.trim(),
          kind: kind.value,
          base_url: kind.value === 'anthropic' ? '' : baseURL.value.trim(),
          model: model.value.trim(),
          api_key_env: keyEnv.value.trim(),
          node: kind.value === 'anthropic' ? '' : nodeName.value.trim(),
        }),
      });
      toast(`Backend "${name.value.trim()}" added`);
      await renderHuginn();
    } catch (err) {
      showError(err);
    }
  });

  return el('div', {},
    el('div', { class: 'group-head', text: 'Add a backend' }),
    el('p', { class: 'muted', text:
      'Takes effect immediately — no restart. The key itself is never entered '
      + 'or stored here: give the name of the environment variable the server '
      + 'reads it from. Adding one with the same name as a backends.json entry '
      + 'is refused, because the file wins.' }),
    form,
    el('p', { class: 'muted', text:
      `Default backend is "${d.default}". A backend added here is selectable per `
      + 'conversation from the picker under the chat box; it does not become the '
      + 'default, which still comes from backends.json.' }));
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

/* ---------- portfolio ---------- */
//
// The investment ledger that moved across from morpheus: what is held, what was
// traded, what the assistant predicts and what its scheduled review made of it.
//
// Money arrives as integer cents and is only ever divided for display, never
// for arithmetic — the same rule the Go side keeps, for the same reason.

const pf = { tab: 'holdings', status: null };

// Amounts are integer cents, formatted by the accounting view's cents() —
// one formatter for both ledgers, since both hold the same kind of number.

// signedCents marks a gain or a loss, since "-1,204.00" and "1,204.00" are the
// whole story on a P&L line and the minus alone is easy to miss.
function signedCents(n) {
  const v = Number(n) || 0;
  const node = el('span', { class: v < 0 ? 'neg' : v > 0 ? 'pos' : '', text: (v > 0 ? '+' : '') + cents(v) });
  return node;
}

async function loadPortfolio() {
  for (const li of document.querySelectorAll('#pf-nav li')) {
    li.addEventListener('click', () => {
      for (const other of document.querySelectorAll('#pf-nav li')) {
        other.classList.toggle('active', other === li);
      }
      pf.tab = li.dataset.tab;
      renderPortfolio().catch(showError);
    });
  }
  $('pf-refresh').addEventListener('click', () => renderPortfolio().catch(showError));
  await renderPortfolio();
}

async function renderPortfolio() {
  const body = $('pf-body');
  body.textContent = '';
  $('pf-title').textContent = {
    holdings: 'Holdings', trades: 'Trades', forecasts: 'Forecasts',
    watchlist: 'Watchlist', reviews: 'Reviews',
  }[pf.tab];

  // The status line says which outside services are configured, so a page
  // showing cost basis and no market value explains itself.
  pf.status = await api('/api/v1/fintech/status');
  const bits = [
    pf.status.market_data_configured
      ? `market data: ${pf.status.market_data_provider}`
      : 'market data: not configured',
    pf.status.forecasting_configured ? 'forecasting: on' : 'forecasting: off',
    pf.status.kraken_configured ? 'kraken: linked' : null,
    pf.status.broker_configured ? 'paper broker: on' : null,
  ].filter(Boolean);
  $('pf-status').textContent = bits.join(' · ');

  if (pf.tab === 'holdings') return renderHoldings(body);
  if (pf.tab === 'trades') return renderTrades(body);
  if (pf.tab === 'forecasts') return renderForecasts(body);
  if (pf.tab === 'watchlist') return renderWatchlist(body);
  return renderReviews(body);
}

async function renderHoldings(body) {
  const summary = await api('/api/v1/fintech/portfolio');
  body.append(el('div', { class: 'stats' },
    stat(summary.Holdings ? summary.Holdings.length : 0, 'Positions'),
    stat(cents(summary.TotalCostCents), 'Cost basis'),
    stat(cents(summary.TotalValueCents), 'Market value'),
    stat(cents(summary.RealizedPLCents), 'Realised P&L')));

  if (!summary.MarketDataConfigured) {
    body.append(el('div', { class: 'muted', text:
      'No market data provider is configured, so positions are shown at cost. ' +
      'Set MARKET_DATA_API_KEY (and MARKET_DATA_PROVIDER, finnhub or alphavantage) to value them.' }));
  }

  const holdings = summary.Holdings || [];
  if (!holdings.length) {
    body.append(el('div', { class: 'muted', text: 'Nothing held yet. Record a trade under Trades.' }));
    return;
  }
  body.append(table(
    ['Symbol', 'Class', 'Quantity', 'Avg cost', 'Cost', 'Value', 'Unrealised'],
    holdings.map((h) => [
      h.Symbol, h.AssetClass, h.Quantity, cents(h.AvgCostCents),
      cents(h.TotalCostCents), cents(h.CurrentValueCents), signedCents(h.UnrealizedPLCents),
    ]),
    [false, false, true, true, true, true, true]));
}

async function renderTrades(body) {
  body.append(tradeForm());
  const { trades } = await api('/api/v1/fintech/trades');
  if (!trades || !trades.length) {
    body.append(el('div', { class: 'muted', text: 'The ledger is empty.' }));
    return;
  }
  body.append(table(
    ['Executed', 'Symbol', 'Side', 'Quantity', 'Price', 'Fee', 'Total', 'Source'],
    trades.map((t) => [
      (t.ExecutedAt || '').slice(0, 10), t.Symbol, t.Side, t.Quantity,
      cents(t.PriceCents), cents(t.FeeCents), signedCents(t.TotalCents), t.Source,
    ]),
    [false, false, false, true, true, true, true, false]));
}

// tradeForm records a trade made elsewhere. Prices are typed in the units
// anyone reads them in and converted to cents here — asking for "12345" when
// the screen says 123.45 is how a position ends up a hundred times too large.
function tradeForm() {
  const symbol = el('input', { placeholder: 'AAPL', autocomplete: 'off' });
  const side = el('select', {}, el('option', { value: 'buy', text: 'Buy' }), el('option', { value: 'sell', text: 'Sell' }));
  const assetClass = el('select', {},
    el('option', { value: 'equity', text: 'Equity' }),
    el('option', { value: 'etf', text: 'ETF' }),
    el('option', { value: 'crypto', text: 'Crypto' }));
  const quantity = el('input', { placeholder: 'Quantity', autocomplete: 'off' });
  const price = el('input', { placeholder: 'Price', autocomplete: 'off' });
  const fee = el('input', { placeholder: 'Fee', autocomplete: 'off' });
  const date = el('input', { type: 'date' });

  const form = el('form', { class: 'row-form' },
    symbol, side, assetClass, quantity, price, fee, date,
    el('button', { type: 'submit', text: 'Record' }));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const toCents = (v) => Math.round((Number(v) || 0) * 100);
    try {
      await api('/api/v1/fintech/trades', {
        method: 'POST',
        body: JSON.stringify({
          symbol: symbol.value,
          asset_class: assetClass.value,
          side: side.value,
          quantity: quantity.value,
          price_cents: toCents(price.value),
          fee_cents: toCents(fee.value),
          executed_at: date.value ? new Date(date.value + 'T12:00:00Z').toISOString() : '',
        }),
      });
      toast(`Recorded ${side.value} ${quantity.value} ${symbol.value.toUpperCase()}`);
      symbol.value = quantity.value = price.value = fee.value = '';
      await renderPortfolio();
    } catch (err) {
      showError(err);
    }
  });
  return form;
}

async function renderForecasts(body) {
  body.append(forecastForm());
  if (!pf.status.forecasting_configured) {
    body.append(el('div', { class: 'muted', text: 'No model backend is available for forecasting.' }));
  }
  const { forecasts } = await api('/api/v1/fintech/forecasts?limit=25');
  if (!forecasts || !forecasts.length) {
    body.append(el('div', { class: 'muted', text: 'No forecasts yet.' }));
    return;
  }
  for (const f of forecasts) {
    body.append(el('div', { class: 'group-head', text:
      `${f.Symbol} · ${(f.RequestedAt || '').slice(0, 10)} · reference ${cents(f.ReferencePriceCents)}` }));
    body.append(table(
      ['Horizon', 'Target', 'Direction', 'Range', 'Confidence', 'Actual', 'Hit'],
      (f.Horizons || []).map((h) => [
        `${h.HorizonDays}d`,
        (h.TargetDate || '').slice(0, 10),
        h.PredictedDirection,
        `${cents(h.PredictedLowCents)} – ${cents(h.PredictedHighCents)}`,
        (Number(h.Confidence) || 0).toFixed(2),
        h.ActualPriceCents === null || h.ActualPriceCents === undefined ? '—' : cents(h.ActualPriceCents),
        h.WithinPredictedRange === null || h.WithinPredictedRange === undefined
          ? '—' : (h.WithinPredictedRange ? 'in range' : 'missed'),
      ]),
      [false, false, false, true, true, true, false]));
    if (f.Rationale) body.append(el('p', { class: 'muted', text: f.Rationale }));
    body.append(el('div', { class: 'pane-actions' },
      forecastAction('Score matured horizons', `/api/v1/fintech/forecasts/${f.ID}/evaluate`),
      forecastAction('Deep dive', `/api/v1/fintech/forecasts/${f.ID}/enrich`),
      deleteForecastButton(f)));
    if (f.Enrichment && f.Enrichment.summary) {
      body.append(el('p', { class: 'muted', text: f.Enrichment.summary }));
    }
  }
}

function forecastAction(label, path) {
  const b = el('button', { class: 'ghost-btn', text: label });
  b.addEventListener('click', async () => {
    b.disabled = true;
    try {
      await api(path, { method: 'POST' });
      await renderPortfolio();
    } catch (err) {
      showError(err);
    } finally {
      b.disabled = false;
    }
  });
  return b;
}

function deleteForecastButton(f) {
  const b = el('button', { class: 'ghost-btn danger', text: 'Delete' });
  b.addEventListener('click', async () => {
    if (!confirm(`Delete the ${f.Symbol} forecast from ${(f.RequestedAt || '').slice(0, 10)}?`)) return;
    try {
      await api(`/api/v1/fintech/forecasts/${f.ID}`, { method: 'DELETE' });
      await renderPortfolio();
    } catch (err) {
      showError(err);
    }
  });
  return b;
}

function forecastForm() {
  const symbol = el('input', { placeholder: 'Symbol', autocomplete: 'off' });
  const horizons = el('input', { placeholder: 'Horizons, e.g. 3,10,30', autocomplete: 'off', value: '3,10,30' });
  const extra = el('input', { placeholder: 'Extra context (optional)', autocomplete: 'off' });
  const form = el('form', { class: 'row-form' }, symbol, horizons, extra,
    el('button', { type: 'submit', text: 'Forecast' }));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const days = horizons.value.split(',').map((d) => parseInt(d.trim(), 10)).filter(Boolean);
    const button = form.querySelector('button');
    button.disabled = true;
    // A local model thinking about eight horizons is not instant, and a button
    // that does nothing visible for a minute reads as broken.
    toast('Asking the model…');
    try {
      await api('/api/v1/fintech/forecasts', {
        method: 'POST',
        body: JSON.stringify({ symbol: symbol.value, horizon_days: days, context: extra.value }),
      });
      symbol.value = '';
      await renderPortfolio();
    } catch (err) {
      showError(err);
    } finally {
      button.disabled = false;
    }
  });
  return form;
}

async function renderWatchlist(body) {
  const symbol = el('input', { placeholder: 'Symbol', autocomplete: 'off' });
  const horizons = el('input', { placeholder: 'Horizons', value: '5,14,30', autocomplete: 'off' });
  const form = el('form', { class: 'row-form' }, symbol, horizons,
    el('button', { type: 'submit', text: 'Watch' }));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      await api('/api/v1/fintech/watchlist', {
        method: 'POST',
        body: JSON.stringify({
          symbol: symbol.value,
          horizon_days: horizons.value.split(',').map((d) => parseInt(d.trim(), 10)).filter(Boolean),
        }),
      });
      symbol.value = '';
      await renderPortfolio();
    } catch (err) {
      showError(err);
    }
  });
  body.append(form);

  const { watchlist } = await api('/api/v1/fintech/watchlist');
  if (!watchlist || !watchlist.length) {
    body.append(el('div', { class: 'muted', text:
      'Nothing watched. A watched symbol is forecast and reviewed on the schedule, ' +
      'if FINTECH_SCAN_INTERVAL or FINTECH_REVIEW_INTERVAL is set.' }));
    return;
  }
  body.append(table(['Symbol', 'Horizons', 'Last forecast', ''],
    watchlist.map((e) => [
      e.Symbol,
      (e.Horizons || []).join(', '),
      e.LastForecastAt ? String(e.LastForecastAt).slice(0, 10) : 'never',
      unwatchButton(e.Symbol),
    ]),
    [false, false, false, false]));
}

function unwatchButton(symbol) {
  const b = el('button', { class: 'ghost-btn danger', text: 'Remove' });
  b.addEventListener('click', async () => {
    try {
      await api(`/api/v1/fintech/watchlist/${encodeURIComponent(symbol)}`, { method: 'DELETE' });
      await renderPortfolio();
    } catch (err) {
      showError(err);
    }
  });
  return b;
}

async function renderReviews(body) {
  const run = el('button', { text: 'Run a review cycle now' });
  run.addEventListener('click', async () => {
    run.disabled = true;
    // One model call per watched or held symbol, in series. On a local backend
    // that is minutes, not seconds, so the wait is stated rather than implied.
    toast('Reviewing every watched and held symbol — this takes a while…');
    try {
      const { reviewed } = await api('/api/v1/fintech/reviews/run', { method: 'POST' });
      toast(`Reviewed ${reviewed} symbol(s)`);
      await renderPortfolio();
    } catch (err) {
      showError(err);
    } finally {
      run.disabled = false;
    }
  });
  body.append(el('div', { class: 'row-form' }, run));

  const { reviews } = await api('/api/v1/fintech/reviews?limit=50');
  if (!reviews || !reviews.length) {
    body.append(el('div', { class: 'muted', text:
      'No reviews yet. A cycle forecasts every watched and held symbol, then commits to a verdict on each.' }));
    return;
  }
  body.append(table(['Reviewed', 'Symbol', 'Source', 'Verdict', 'Reasoning'],
    reviews.map((r) => [
      (r.ReviewedAt || '').slice(0, 10), r.Symbol, r.Source,
      ratingLabel(r.Rating), r.Rationale,
    ]),
    [false, false, false, false, false]));
}

// ratingLabel spells the five-point scale out. "max_sell" is a database value,
// not something to put in front of a person.
function ratingLabel(rating) {
  return {
    max_sell: 'Sell hard', sell: 'Sell', hold: 'Hold', buy: 'Buy', max_buy: 'Buy hard',
  }[rating] || rating;
}

/* ---------- twire ---------- */
//
// The canary tripwire that moved across from morpheus: fake services on
// well-known ports, a log of everything that connected to one, and the email
// alerting that fires when something does.
//
// Everything here is built with el(), which means textContent, which matters
// more in this view than anywhere else in the app. A canary records the first
// bytes any caller sends and shows them back as the data preview — that string
// is chosen by whoever is probing the network, and morpheus had to escape it by
// hand on the way into an innerHTML template. Here there is no such template to
// escape it for.

// The tripwire is a group of tabs inside the Utilities view rather than a view
// of its own, so it has no loader and no tab state — renderUtilities() routes
// to the three renderers below. What survives here is the state a tab carries
// between renders: the binary path the privilege notice quotes back.
const tw = { status: null, binaryPath: '' };

async function renderCanaries(body) {
  const status = await api('/api/v1/twire/status');
  const canaries = status.canaries || [];
  tw.binaryPath = status.binary_path || '';

  body.append(el('p', { class: 'muted', text:
    'All canaries are off by default. A port that fails to bind is shown as ' +
    "that canary's status rather than stopping the server." }));

  // Ports below 1024 are root's on Linux, and a canary that silently never
  // listened is worse than one that says why. The fix is a capability on the
  // binary, so the exact command is offered rather than described.
  if (canaries.some((c) => c.privilege_required)) {
    body.append(privilegeNotice());
  }

  body.append(table(
    ['Service', 'Port', 'Description', 'Status', 'Hits', ''],
    canaries.map((c) => [
      c.custom ? `${c.name} (custom)` : c.name,
      c.port,
      c.description,
      canaryStatusCell(c),
      c.hit_count,
      canaryActions(c),
    ]),
    [false, true, false, false, true, false]));

  body.append(el('h3', { text: 'Add a custom canary' }));
  body.append(el('p', { class: 'muted', text:
    'Listen on a port the built-in catalog does not cover. The new canary ' +
    'starts disabled — enable it above once added.' }));
  body.append(addCanaryForm());
}

// canaryStatusCell says what a canary is actually doing, which is not the same
// question as whether it is switched on: an enabled canary whose port was taken
// is the case worth seeing.
function canaryStatusCell(c) {
  if (!c.enabled) return el('span', { class: 'muted', text: 'disabled' });
  if (c.listening) return el('span', { text: 'listening' });
  if (c.privilege_required) {
    return el('span', { class: 'error', text: 'needs elevated privilege' });
  }
  return el('span', { class: 'error', text: `bind error: ${c.last_error || 'unknown'}` });
}

function canaryActions(c) {
  const wrap = el('span', { class: 'row-actions' });
  const toggle = el('button', {
    class: 'ghost-btn',
    text: c.enabled ? 'Disable' : 'Enable',
  });
  toggle.addEventListener('click', async () => {
    toggle.disabled = true;
    const action = c.enabled ? 'disable' : 'enable';
    try {
      await api(`/api/v1/twire/canaries/${encodeURIComponent(c.key)}/${action}`, { method: 'POST' });
      await renderUtilities();
    } catch (err) {
      toggle.disabled = false;
      showError(err);
    }
  });
  wrap.append(toggle);

  // Only operator-defined canaries can be removed; the built-in catalog is
  // fixed, and a built-in one you are done with is simply disabled.
  if (c.custom) {
    const remove = el('button', { class: 'ghost-btn danger', text: 'Delete' });
    remove.addEventListener('click', async () => {
      if (!confirm(`Delete the custom canary on port ${c.port}? Recorded events are kept.`)) return;
      try {
        await api(`/api/v1/twire/canaries/${encodeURIComponent(c.key)}`, { method: 'DELETE' });
        await renderUtilities();
      } catch (err) {
        showError(err);
      }
    });
    wrap.append(remove);
  }
  return wrap;
}

function privilegeNotice() {
  const cmd = `sudo setcap cap_net_bind_service=+ep ${tw.binaryPath || '/path/to/wintermuted'}`;
  const code = el('code', { text: cmd });
  const copy = el('button', { class: 'ghost-btn', text: 'Copy' });
  copy.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(cmd);
      copy.textContent = 'Copied';
      setTimeout(() => { copy.textContent = 'Copy'; }, 1200);
    } catch {
      /* The command is selectable; a blocked clipboard is not worth an error. */
    }
  });
  return el('div', { class: 'notice' },
    el('strong', { text: 'Elevated privilege required' }),
    el('span', { text:
      ' — one or more canaries listen on ports below 1024, which Linux reserves ' +
      'for root. Grant the binary the cap_net_bind_service capability and ' +
      'restart the server:' }),
    el('div', { class: 'notice-cmd' }, code, copy));
}

function addCanaryForm() {
  const name = el('input', { placeholder: 'Service name', autocomplete: 'off' });
  const port = el('input', { placeholder: 'Port', autocomplete: 'off' });
  const description = el('input', { placeholder: 'Description (optional)', autocomplete: 'off' });
  const banner = el('input', { placeholder: 'Banner sent on connect (optional)', autocomplete: 'off' });
  const form = el('form', { class: 'row-form' }, name, port, description, banner,
    el('button', { type: 'submit', text: 'Add canary' }));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const n = parseInt(port.value, 10);
    if (!Number.isInteger(n) || n < 1 || n > 65535) {
      showError(new Error('Port must be a number between 1 and 65535.'));
      return;
    }
    try {
      await api('/api/v1/twire/canaries', {
        method: 'POST',
        body: JSON.stringify({
          name: name.value.trim(),
          port: n,
          description: description.value.trim(),
          banner: banner.value,
        }),
      });
      toast(`Added a canary on port ${n}`);
      name.value = port.value = description.value = banner.value = '';
      await renderUtilities();
    } catch (err) {
      showError(err);
    }
  });
  return form;
}

async function renderTwireEvents(body) {
  const { events } = await api('/api/v1/twire/events?limit=100');
  if (!events || !events.length) {
    body.append(el('div', { class: 'empty muted', text:
      'No connection attempts recorded yet. That is the expected state — an ' +
      'entry here means something reached a port nothing should be using.' }));
    return;
  }
  body.append(table(
    ['When', 'Canary', 'Port', 'Source', 'Data preview'],
    events.map((e) => [
      new Date(e.occurred_at).toLocaleString(),
      e.service_name,
      e.port,
      `${e.remote_ip}:${e.remote_port}`,
      // Already flattened to printable ASCII by the server, and rendered as
      // text here regardless.
      el('code', { text: e.data_preview || '—' }),
    ]),
    [false, false, true, false, false]));
}

async function renderTwireAlerts(body) {
  const cfg = await api('/api/v1/twire/alert-config');

  body.append(el('p', { class: 'muted', text:
    'Alerts are sent through Google SMTP (smtp.gmail.com:587), so the password ' +
    'must be a Google App Password — Google rejects account passwords here. ' +
    'Repeat hits from one source are rate-limited to one email every five minutes.' }));

  if (!cfg.secret_configured) {
    body.append(el('div', { class: 'notice' },
      el('strong', { text: 'WINTERMUTE_SECRET is not set' }),
      el('span', { text:
        ' — the App Password cannot be stored, because there is no key to ' +
        'encrypt it with. Everything else on this form saves. Set ' +
        'WINTERMUTE_SECRET and restart, or configure alerting entirely from ' +
        'the SMTP_USERNAME, SMTP_PASSWORD, SMTP_FROM and TWIRE_ALERT_TO ' +
        'environment variables instead.' })));
  }

  const enabled = el('input', { type: 'checkbox' });
  enabled.checked = Boolean(cfg.enabled);
  const username = el('input', { placeholder: 'SMTP username', autocomplete: 'off' });
  username.value = cfg.smtp_username || '';
  const password = el('input', {
    type: 'password',
    autocomplete: 'new-password',
    // The password is never sent back to the browser, so the field starts
    // empty and an empty field means "keep what is stored" rather than "clear
    // it" — otherwise editing a recipient would silently drop the credential.
    placeholder: cfg.password_set ? 'App Password (stored — leave blank to keep)' : 'App Password',
  });
  const from = el('input', { placeholder: 'From address', autocomplete: 'off' });
  from.value = cfg.from || '';
  const recipients = el('input', { placeholder: 'Recipients, comma-separated', autocomplete: 'off' });
  recipients.value = (cfg.recipients || []).join(', ');

  const form = el('form', { class: 'tw-form' },
    el('label', { class: 'check' }, enabled, el('span', { text: ' Send an email when a canary is tripped' })),
    el('label', {}, el('span', { class: 'muted', text: 'SMTP username' }), username),
    el('label', {}, el('span', { class: 'muted', text: 'App Password' }), password),
    el('label', {}, el('span', { class: 'muted', text: 'From' }), from),
    el('label', {}, el('span', { class: 'muted', text: 'Recipients' }), recipients),
    el('div', { class: 'row-actions' },
      el('button', { type: 'submit', text: 'Save' }),
      testAlertButton()));

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const payload = {
      enabled: enabled.checked,
      smtp_username: username.value.trim(),
      from: from.value.trim(),
      recipients: recipients.value.split(',').map((r) => r.trim()).filter(Boolean),
    };
    // Null rather than "" so the server keeps the stored password.
    if (password.value) payload.smtp_password = password.value;
    try {
      await api('/api/v1/twire/alert-config', { method: 'PUT', body: JSON.stringify(payload) });
      toast('Alert settings saved');
      await renderUtilities();
    } catch (err) {
      showError(err);
    }
  });
  body.append(form);
}

function testAlertButton() {
  const b = el('button', { type: 'button', class: 'ghost-btn', text: 'Send a test email' });
  b.addEventListener('click', async () => {
    b.disabled = true;
    try {
      // Against the saved configuration, not the unsaved form: a test that
      // passed on fields nobody committed would prove nothing about what the
      // server will do at three in the morning.
      await api('/api/v1/twire/alert-config/test', { method: 'POST' });
      toast('Test email sent');
    } catch (err) {
      showError(err);
    } finally {
      b.disabled = false;
    }
  });
  return b;
}

/* ---------- appearance ---------- */
//
// The theme's two sets of knobs, which live in this browser's localStorage
// rather than on the server — see theme.js. The theme itself is switched from
// the button in the top bar; this is where its weights are set.

function renderAdminAppearance(body) {
  body.append(el('p', { class: 'muted', text:
    'These settings are stored in this browser only — the theme is a property ' +
    'of the screen you are reading from, not of the server.' }));

  body.append(el('div', { class: 'group-head', text: 'Theme' }));
  // A select rather than the cycle button this replaced: in the top bar the
  // control had to be one small target and the label doubled as the readout,
  // but a settings pane has room to show all three and jump straight to one.
  const theme = el('select', {
    id: 'theme-select',
    onchange: (e) => {
      WintermuteTheme.set(e.target.value);
      // Chaos samples the DOM it glitches, and the pane it was told about is
      // the one being looked at right now.
      WintermuteChaos.repaint();
      toast(`Theme: ${WintermuteTheme.LABELS[WintermuteTheme.current()]}`);
    },
  }, WintermuteTheme.THEMES.map((name) => {
    const opt = el('option', { value: name, text: WintermuteTheme.LABELS[name] });
    if (name === WintermuteTheme.current()) opt.selected = true;
    return opt;
  }));
  body.append(el('form', { class: 'row-form' },
    el('span', { class: 'muted', text: 'Theme' }), theme));

  body.append(el('div', { class: 'group-head', text: 'Text brightness' }));
  body.append(el('p', { class: 'muted', text:
    'Every theme here is dark, and each palette was tuned on one screen. The ' +
    'same colours on a phone in daylight, or on a panel with the contrast ' +
    'wound down, can be genuinely hard to read. This lifts the text and the ' +
    'secondary text towards white without touching the backgrounds, the ' +
    'accents or the warning colours, so the theme still looks like itself. ' +
    '100 is the palette exactly as designed; the palettes already clear AA ' +
    'contrast at that setting, so this is for the screen rather than for the ' +
    'colours.' }));

  // A slider that takes effect as it moves, rather than the number-and-Save
  // the rain uses: this is a setting judged by eye, on the text being read at
  // the time, and having to press Save to see each guess makes that a chore.
  const lift = el('input', {
    type: 'range', class: 'lift-range',
    min: String(WintermuteTheme.MIN_BRIGHTNESS),
    max: String(WintermuteTheme.MAX_BRIGHTNESS),
    step: '5',
  });
  const liftValue = el('span', { class: 'muted lift-value' });
  const showLift = (n) => {
    liftValue.textContent = n === WintermuteTheme.DEFAULT_BRIGHTNESS
      ? `${n}% · palette default` : `${n}%`;
  };
  lift.value = String(WintermuteTheme.brightness());
  showLift(WintermuteTheme.brightness());
  // input fires while dragging and change when it is let go: the first shows
  // the effect, the second is what gets written down.
  lift.addEventListener('input', () => {
    WintermuteTheme.applyBrightness(lift.value);
    showLift(parseInt(lift.value, 10));
  });
  lift.addEventListener('change', () => {
    showLift(WintermuteTheme.setBrightness(lift.value));
    // Chaos paints its glitches from the palette it sampled, so it is told to
    // look again rather than keeping the colours from before the change.
    WintermuteChaos.repaint();
  });
  body.append(el('form', { class: 'row-form', onsubmit: (e) => e.preventDefault() }, [
    el('span', { class: 'muted', text: 'Brightness' }), lift, liftValue,
    el('button', {
      class: 'ghost-btn', type: 'button', text: 'Reset',
      onclick: () => {
        lift.value = String(WintermuteTheme.DEFAULT_BRIGHTNESS);
        showLift(WintermuteTheme.setBrightness(lift.value));
      },
    }),
  ]));

  body.append(el('div', { class: 'group-head', text: 'Matrix rain' }));
  body.append(el('p', { class: 'muted', text:
    'The falling glyphs behind the panes, on the Matrix theme. ' +
    'They are drawn faint on purpose — they fill the space a page of panels ' +
    'leaves over on a wide monitor without competing with it — but how faint ' +
    'that reads depends entirely on the screen. 100 is the default; below it ' +
    'the rain recedes towards invisible, and at the top of the range the ' +
    'glyphs are close to fully opaque.' }));

  const brightness = el('input', {
    type: 'number',
    min: String(WintermuteRain.MIN_BRIGHTNESS),
    max: String(WintermuteRain.MAX_BRIGHTNESS),
  });
  brightness.value = String(WintermuteRain.config().brightness);
  const rainForm = el('form', { class: 'row-form' },
    el('span', { class: 'muted', text: 'Glyph brightness %' }), brightness,
    el('button', { type: 'submit', text: 'Save' }));
  rainForm.addEventListener('submit', (e) => {
    e.preventDefault();
    WintermuteRain.setConfig({ brightness: brightness.value });
    brightness.value = String(WintermuteRain.config().brightness);
    toast('Glyph brightness saved');
  });
  body.append(rainForm);

  body.append(el('div', { class: 'group-head', text: 'Chaos' }));
  body.append(el('p', { class: 'muted', text:
    'The Chaos theme is the Matrix palette plus periodic colour glitches: on ' +
    'every tick, and whenever you switch views, a random sample of the ' +
    'characters on screen turns a random colour, replacing the previous ' +
    'sample. It does not run the falling glyphs — the glitches are the whole ' +
    'effect.' }));

  const chaosCfg = WintermuteChaos.config();
  const interval = el('input', { type: 'number', min: '1', max: '3600' });
  interval.value = String(chaosCfg.intervalSeconds);
  const density = el('input', { type: 'number', min: '0', max: String(WintermuteChaos.DENSITY_BASE) });
  density.value = String(chaosCfg.density);
  const chaosForm = el('form', { class: 'row-form' },
    el('span', { class: 'muted', text: 'Seconds between glitches' }), interval,
    el('span', { class: 'muted', text: `Characters per ${WintermuteChaos.DENSITY_BASE}` }), density,
    el('button', { type: 'submit', text: 'Save' }));
  chaosForm.addEventListener('submit', (e) => {
    e.preventDefault();
    WintermuteChaos.setConfig({ intervalSeconds: interval.value, density: density.value });
    const saved = WintermuteChaos.config();
    interval.value = String(saved.intervalSeconds);
    density.value = String(saved.density);
    toast('Chaos settings saved');
  });
  body.append(chaosForm);

  body.append(el('div', { class: 'group-head', text: '40K fritz' }));
  body.append(el('p', { class: 'muted', text:
    'The 40K theme runs a failing-CRT overlay: scanlines and a vignette that ' +
    'never move, a roll bar drifting down the glass, and a burst every few ' +
    'seconds where the picture tears. The interval is the average wait — each ' +
    'gap is randomised around it, because a fault on a fixed beat stops ' +
    'reading as a fault. Intensity scales the whole layer: 0 leaves the ' +
    'scanlines and nothing else. The overlay never takes a click, and asking ' +
    'your system for reduced motion stops the roll and the bursts.' }));

  const fritzCfg = WintermuteFritz.config();
  const fritzInterval = el('input', {
    type: 'number',
    min: String(WintermuteFritz.MIN_INTERVAL),
    max: String(WintermuteFritz.MAX_INTERVAL),
  });
  fritzInterval.value = String(fritzCfg.intervalSeconds);
  const fritzIntensity = el('input', {
    type: 'number',
    min: String(WintermuteFritz.MIN_INTENSITY),
    max: String(WintermuteFritz.MAX_INTENSITY),
  });
  fritzIntensity.value = String(fritzCfg.intensity);
  const fritzForm = el('form', { class: 'row-form' },
    el('span', { class: 'muted', text: 'Seconds between bursts' }), fritzInterval,
    el('span', { class: 'muted', text: 'Intensity %' }), fritzIntensity,
    el('button', { type: 'submit', text: 'Save' }),
    // A knob whose effect is a brief flicker every several seconds is hard to
    // tune by waiting for one, so the pane can fire one on demand.
    el('button', { type: 'button', class: 'ghost-btn', text: 'Test burst',
      onclick: () => WintermuteFritz.burst() }));
  fritzForm.addEventListener('submit', (e) => {
    e.preventDefault();
    WintermuteFritz.setConfig({
      intervalSeconds: fritzInterval.value, intensity: fritzIntensity.value,
    });
    const saved = WintermuteFritz.config();
    fritzInterval.value = String(saved.intervalSeconds);
    fritzIntensity.value = String(saved.intensity);
    toast('Fritz settings saved');
  });
  body.append(fritzForm);
}

/* ---------- utilities ---------- */
//
// Housekeeping, moved across from morpheus: what the database looks like, what
// the machine is doing right now, what the model calls have cost, and the three
// operations an operator runs by hand — back up, vacuum, prune.
//
// The last two are destructive and are treated as such: prune asks for
// confirmation naming the table and the window, and neither is exposed to the
// assistant as a tool.

const util = { tab: 'diagnostics' };

// bytes() already exists for the admin view. fmtBytes here is its two-decimal
// sibling for the larger figures this page deals in.
function fmtBytes(n) {
  const b = Number(n) || 0;
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  if (b < 1024 * 1024 * 1024) return `${(b / 1024 / 1024).toFixed(1)} MB`;
  return `${(b / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function fmtRate(n) {
  return `${fmtBytes(n)}/s`;
}

function fmtUptime(seconds) {
  const s = Math.floor(Number(seconds) || 0);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return `${d}d ${h}h ${m}m`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m ${s % 60}s`;
}

const num = (n) => (Number(n) || 0).toLocaleString();

// meter draws a proportion as a bar. Used for disk capacity and for CPU, which
// are the two figures on this page where "how close to full" reads faster than
// the number itself.
function meter(fraction) {
  const pct = Math.max(0, Math.min(100, Math.round(fraction * 100)));
  const fill = el('div', { class: 'meter-fill' });
  fill.style.width = `${pct}%`;
  // Green until it matters, amber when it is worth noticing, red when it is a
  // problem. The thresholds are morpheus's.
  fill.dataset.level = pct > 90 ? 'high' : pct > 70 ? 'mid' : 'low';
  return el('div', { class: 'meter' }, fill);
}

function kv(label, value) {
  return el('div', { class: 'kv' },
    el('span', { class: 'muted', text: label }),
    value && value.nodeType ? value : el('span', { text: String(value ?? '—') }));
}

// Both sidebar groups are `.util-tabs`, so one selector binds them and one
// pass clears `active` across the pair — a tripwire tab deselects the
// housekeeping tab and vice versa, because they share the pane.
const UTIL_TITLES = {
  diagnostics: 'Diagnostics', activity: 'Activity', usage: 'API usage',
  backup: 'Backup', maintenance: 'Maintenance',
  canaries: 'Canaries', events: 'Connection attempts', alerts: 'Email alerts',
};

// The sidebar hint follows the group in view: the housekeeping note is wrong
// for a canary page, and the canary note is what makes the tripwire tabs
// legible to someone who has never opened them.
const UTIL_HINTS = {
  utilities: 'Backups, database diagnostics and the two operations that ' +
    'delete or rewrite data. None of these are offered to the assistant as tools.',
  twire: 'Canaries impersonate common services on their usual ports. Nothing ' +
    'on a home network should connect to them, so any hit is a strong sign of ' +
    'scanning or probing.',
};

async function loadUtilities() {
  for (const li of document.querySelectorAll('.util-tabs li')) {
    li.addEventListener('click', () => {
      for (const other of document.querySelectorAll('.util-tabs li')) {
        other.classList.toggle('active', other === li);
      }
      util.tab = li.dataset.tab;
      renderUtilities().catch(showError);
    });
  }
  $('util-refresh').addEventListener('click', () => renderUtilities().catch(showError));
  await renderUtilities();
}

async function renderUtilities() {
  // Any tab change leaves the activity tab, so the poller stops here rather
  // than in each branch below.
  stopActivityPolling();

  const body = $('util-body');
  body.textContent = '';
  $('util-title').textContent = UTIL_TITLES[util.tab];

  if (util.tab === 'canaries' || util.tab === 'events' || util.tab === 'alerts') {
    $('util-hint').textContent = UTIL_HINTS.twire;
    if (util.tab === 'canaries') return renderCanaries(body);
    if (util.tab === 'events') return renderTwireEvents(body);
    return renderTwireAlerts(body);
  }

  $('util-hint').textContent = UTIL_HINTS.utilities;
  if (util.tab === 'diagnostics') return renderDiagnostics(body);
  if (util.tab === 'activity') return renderActivity(body);
  if (util.tab === 'usage') return renderUtilAPIUsage(body);
  if (util.tab === 'backup') return renderBackup(body);
  return renderMaintenance(body);
}

/* ---------- diagnostics ---------- */

async function renderDiagnostics(body) {
  const info = await api('/api/v1/utilities/system-info');

  body.append(el('div', { class: 'stats' },
    stat(fmtUptime(info.uptime_seconds), 'Uptime'),
    stat(fmtBytes(info.database_size_bytes), 'Database'),
    stat(fmtBytes(info.wal_size_bytes), 'WAL'),
    stat(info.go_version, 'Go')));

  body.append(el('div', { class: 'group-head', text: 'Storage' }));
  body.append(kv('Database file', el('code', { text: info.database_path || '—' })));

  const disk = info.disk || {};
  if (disk.total_bytes) {
    body.append(kv(`Disk (${disk.path})`,
      el('span', { text: `${fmtBytes(disk.used_bytes)} of ${fmtBytes(disk.total_bytes)} used, ${fmtBytes(disk.free_bytes)} free` })));
    body.append(meter(disk.used_bytes / disk.total_bytes));
  }
  // The logical size is pages in use; the file on disk only shrinks on VACUUM,
  // so the two diverge after a prune. Saying which one this is avoids the
  // "I deleted everything and nothing got smaller" question.
  body.append(el('p', { class: 'hint muted', text:
    'Database size is pages in use, not the size of the file. Deleting rows frees ' +
    'pages inside the file; the file itself only shrinks when you run a vacuum.' }));

  const tables = info.tables || [];
  if (!tables.length) return;
  body.append(el('div', { class: 'group-head', text: 'Tables' }));
  body.append(table(
    ['Table', 'Rows', 'Size'],
    tables.map((t) => [t.name, num(t.row_count), fmtBytes(t.size_bytes)]),
    [false, true, true]));
  body.append(el('p', { class: 'hint muted', text:
    "Each table's size includes its indexes." }));
}

/* ---------- activity ---------- */

let activityTimer = null;

function stopActivityPolling() {
  if (activityTimer) clearInterval(activityTimer);
  activityTimer = null;
}

// renderActivity shows live CPU, network and disk rates.
//
// The panel is built once and then updated in place on each poll, rather than
// re-rendered: replacing the DOM twice a second fights with text selection and
// makes the numbers flicker.
async function renderActivity(body) {
  body.append(el('p', { class: 'muted', text:
    'What the machine is doing right now, averaged over the last few seconds. ' +
    'Rates are read from /proc and cost nothing to sample; this panel polls ' +
    'only while it is on screen.' }));

  const cpu = el('span', { text: '—' });
  const cpuMeter = el('div', { class: 'meter' }, el('div', { class: 'meter-fill' }));
  const netRx = el('span', { text: '—' });
  const netTx = el('span', { text: '—' });
  const diskRead = el('span', { text: '—' });
  const diskWrite = el('span', { text: '—' });
  const state = el('span', { class: 'muted', text: 'starting…' });

  body.append(el('div', { class: 'group-head', text: 'CPU' }));
  body.append(kv('Busy', cpu), cpuMeter);
  body.append(el('div', { class: 'group-head', text: 'Network' }));
  body.append(kv('Received', netRx), kv('Transmitted', netTx));
  body.append(el('div', { class: 'group-head', text: 'Disk' }));
  body.append(kv('Read', diskRead), kv('Written', diskWrite));
  body.append(el('div', { class: 'hint' }, state));

  async function poll() {
    let s;
    try {
      s = await api('/api/v1/utilities/resources');
    } catch (err) {
      stopActivityPolling();
      state.textContent = `stopped: ${err.message}`;
      return;
    }
    if (s.warming) {
      // The first reading has no predecessor to measure against, so it reports
      // nothing rather than a zero that looks like an idle machine.
      state.textContent = 'warming up — the first reading has nothing to compare against';
      return;
    }
    state.textContent = 'live';
    cpu.textContent = `${s.cpu_percent.toFixed(1)}%`;
    cpuMeter.firstChild.style.width = `${Math.min(100, s.cpu_percent)}%`;
    cpuMeter.firstChild.dataset.level = s.cpu_percent > 90 ? 'high' : s.cpu_percent > 70 ? 'mid' : 'low';
    netRx.textContent = fmtRate(s.net_rx_bytes_per_sec);
    netTx.textContent = fmtRate(s.net_tx_bytes_per_sec);
    diskRead.textContent = fmtRate(s.disk_read_bytes_per_sec);
    diskWrite.textContent = fmtRate(s.disk_write_bytes_per_sec);
  }

  await poll();
  stopActivityPolling();
  activityTimer = setInterval(() => { poll().catch(() => stopActivityPolling()); }, 2000);
}

/* ---------- api usage ---------- */

const USAGE_LABELS = { forecast: 'Forecasts', enrichment: 'Deep dives', review: 'Position reviews' };

async function renderUtilAPIUsage(body) {
  const usage = await api('/api/v1/utilities/api-usage');
  const sources = usage.sources || [];
  const total = usage.total || {};

  if (usage.note) {
    body.append(el('div', { class: 'notice' }, el('span', { text: usage.note })));
  }

  if (!sources.length) {
    body.append(el('div', { class: 'empty muted', text:
      'No model calls recorded yet. Forecasts and position reviews record what ' +
      'they cost; nothing else on this server does.' }));
    return;
  }

  body.append(el('div', { class: 'stats' },
    stat(num(total.request_count), 'Calls'),
    stat(num(total.input_tokens), 'Input tokens'),
    stat(num(total.output_tokens), 'Output tokens'),
    stat(num(total.today_request_count), 'Calls today')));

  body.append(table(
    ['Kind', 'Calls', 'Input', 'Output', 'Calls today'],
    sources.map((s) => [
      USAGE_LABELS[s.name] || s.name,
      num(s.request_count), num(s.input_tokens), num(s.output_tokens),
      num(s.today_request_count),
    ]),
    [false, true, true, true, true]));

  body.append(el('div', { class: 'group-head', text: 'Today' }));
  body.append(kv('Tokens in / out',
    `${num(total.today_input_tokens)} / ${num(total.today_output_tokens)}`));
}

/* ---------- backup ---------- */

function renderBackup(body) {
  body.append(el('p', { class: 'muted', text:
    'Writes a consistent copy of the database into a timestamped directory ' +
    'under the path you give. The copy is taken with SQLite’s VACUUM INTO, ' +
    'so it is safe to run against the live server — unlike copying the file, ' +
    'which can catch a write in progress.' }));
  body.append(el('p', { class: 'hint muted', text:
    'The path is on the server, not on this machine, and the server writes ' +
    'wherever its own user can write.' }));

  const dest = el('input', { placeholder: '/var/backups/wintermute', autocomplete: 'off' });
  const submit = el('button', { type: 'submit', text: 'Back up now' });
  const form = el('form', { class: 'row-form' }, dest, submit);
  const result = el('div', { class: 'pane-body' });

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!dest.value.trim()) {
      showError(new Error('Destination directory is required.'));
      return;
    }
    submit.disabled = true;
    submit.textContent = 'Backing up…';
    result.textContent = '';
    try {
      const res = await api('/api/v1/utilities/backup', {
        method: 'POST',
        body: JSON.stringify({ destination: dest.value.trim() }),
      });
      toast('Backup written');
      result.append(el('div', { class: 'group-head', text: 'Written to' }));
      result.append(el('code', { text: res.destination }));
      result.append(table(['File', 'Size'],
        (res.files || []).map((f) => [f.name, fmtBytes(f.size_bytes)]),
        [false, true]));
    } catch (err) {
      showError(err);
    } finally {
      submit.disabled = false;
      submit.textContent = 'Back up now';
    }
  });

  body.append(form, result);
}

/* ---------- maintenance ---------- */

const PRUNE_TARGETS = [
  { value: 'sessions', label: 'Conversations (and their messages and audit rows)' },
  { value: 'muninn', label: 'Muninn audit rows only (keeps the conversations)' },
  { value: 'inference_samples', label: 'Model timing measurements' },
  { value: 'fintech_ai_usage', label: 'Recorded model-call costs' },
];

function renderMaintenance(body) {
  /* ---- vacuum ---- */
  body.append(el('div', { class: 'group-head', text: 'Vacuum' }));
  body.append(el('p', { class: 'muted', text:
    'Rebuilds the database file, returning space freed by deleted rows to the ' +
    'filesystem, and refreshes the query planner’s statistics. The database ' +
    'is locked for the duration, so a large one is best done when nothing else ' +
    'is using it.' }));

  const vacuumBtn = el('button', { text: 'Run vacuum' });
  const vacuumResult = el('div', { class: 'hint muted' });
  vacuumBtn.addEventListener('click', async () => {
    vacuumBtn.disabled = true;
    vacuumBtn.textContent = 'Running…';
    vacuumResult.textContent = '';
    try {
      const res = await api('/api/v1/utilities/vacuum', { method: 'POST' });
      vacuumResult.textContent = res.reclaimed_bytes > 0
        ? `Done in ${res.duration_ms} ms — reclaimed ${fmtBytes(res.reclaimed_bytes)} ` +
          `(${fmtBytes(res.before_bytes)} → ${fmtBytes(res.after_bytes)}).`
        : `Done in ${res.duration_ms} ms — nothing to reclaim, the file was already compact.`;
      toast('Vacuum complete');
    } catch (err) {
      showError(err);
    } finally {
      vacuumBtn.disabled = false;
      vacuumBtn.textContent = 'Run vacuum';
    }
  });
  body.append(el('div', { class: 'row-form' }, vacuumBtn), vacuumResult);

  /* ---- prune ---- */
  body.append(el('div', { class: 'group-head', text: 'Prune' }));
  body.append(el('p', { class: 'muted', text:
    'Permanently deletes rows older than the given age. Pruning conversations ' +
    'takes their messages and tool audit rows with them.' }));

  const target = el('select', {}, PRUNE_TARGETS.map((t) =>
    el('option', { value: t.value, text: t.label })));
  const days = el('input', { type: 'number', min: '1', value: '90' });
  const pruneBtn = el('button', { type: 'submit', text: 'Prune' });
  const pruneForm = el('form', { class: 'row-form' },
    target, el('span', { class: 'muted', text: 'older than' }), days,
    el('span', { class: 'muted', text: 'days' }), pruneBtn);
  const pruneResult = el('div', { class: 'hint muted' });

  pruneForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const n = parseInt(days.value, 10);
    if (!Number.isInteger(n) || n < 1) {
      showError(new Error('Days must be at least 1.'));
      return;
    }
    const label = PRUNE_TARGETS.find((t) => t.value === target.value).label;
    // Named in full, because this is the one control on the page that destroys
    // data with no copy anywhere and no undo.
    if (!confirm(`Permanently delete "${label}" older than ${n} days?\n\nThis cannot be undone.`)) return;

    pruneBtn.disabled = true;
    pruneBtn.textContent = 'Pruning…';
    pruneResult.textContent = '';
    try {
      const res = await api('/api/v1/utilities/prune', {
        method: 'POST',
        body: JSON.stringify({ target: target.value, older_than_days: n }),
      });
      const rows = Number(res.deleted_rows) || 0;
      pruneResult.textContent =
        `Deleted ${num(rows)} row${rows === 1 ? '' : 's'} from ${res.target} older than ${res.older_than_days} days. ` +
        'Run a vacuum to return the space to the filesystem.';
      toast(`Pruned ${num(rows)} row${rows === 1 ? '' : 's'}`);
    } catch (err) {
      showError(err);
    } finally {
      pruneBtn.disabled = false;
      pruneBtn.textContent = 'Prune';
    }
  });
  body.append(pruneForm, pruneResult);
}
