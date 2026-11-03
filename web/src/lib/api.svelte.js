// Shared UI state and the API client.
export const ui = $state({ authed: null, user: '', task: null, toast: null, dialog: null, guideMenu: true, guideDashboard: true });

async function request(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: {} };
  if (opts.body !== undefined) {
    init.body = JSON.stringify(opts.body);
    init.headers['Content-Type'] = 'application/json';
  }
  if (opts.raw !== undefined) init.body = opts.raw;
  const res = await fetch('/api' + path, init);
  const text = await res.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith('/auth/')) ui.authed = false;
    const err = new Error((data && data.error) || `Request failed (${res.status})`);
    err.data = data;
    throw err;
  }
  return data;
}

export const get = (path) => request(path);
export const post = (path, body = {}) => request(path, { method: 'POST', body });
export const put = (path, body = {}) => request(path, { method: 'PUT', body });
export const del = (path) => request(path, { method: 'DELETE' });
export const postRaw = (path, raw) => request(path, { method: 'POST', raw });

export function toast(message, bad = false) {
  const entry = { message, bad };
  ui.toast = entry;
  setTimeout(() => {
    if (ui.toast === entry || ui.toast?.message === message) ui.toast = null;
  }, bad ? 7000 : 2500);
}

// Runs an async action, reporting any failure as a toast.
export async function attempt(fn, success) {
  try {
    const out = await fn();
    if (success) toast(success);
    return out ?? true;
  } catch (e) {
    toast(e.message, true);
    return null;
  }
}

// Shows a background task's live output. Resolves when it finishes, true on success.
export function followTask(id, onDone) {
  ui.task = { id, title: 'Working', log: '', finished: 0, error: '' };
  const poll = async () => {
    if (ui.task?.id !== id) return;
    try {
      const t = await get('/tasks/' + id);
      if (ui.task?.id !== id) return;
      ui.task = t;
      if (t.finished) {
        onDone?.(!t.error);
        return;
      }
    } catch (e) {
      ui.task = { ...ui.task, error: e.message, finished: 1 };
      return;
    }
    setTimeout(poll, 1000);
  };
  poll();
}

// Styled confirm. Pass typed to make the user retype a value, for destructive actions; mono
// for paths or SQL; enter to let the Enter key confirm, whatever has focus.
export function confirmDialog({ title, message, action = 'Confirm', danger = false, typed = '', mono = false, enter = false }) {
  return new Promise((resolve) => {
    ui.dialog = { title, message, action, danger, typed, mono, enter, input: typed ? '' : null, resolve };
  });
}

export function promptDialog({ title, message = '', value = '', action = 'OK', type = 'text' }) {
  return new Promise((resolve) => {
    ui.dialog = { title, message, action, input: value, prompt: true, type, resolve };
  });
}

export function formatBytes(n) {
  if (n === null || n === undefined || isNaN(n)) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  let v = Number(n);
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 || i === 0 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

export function formatTime(ts) {
  if (!ts) return '-';
  return new Date(ts * 1000).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' });
}

export function formatDuration(seconds) {
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}
