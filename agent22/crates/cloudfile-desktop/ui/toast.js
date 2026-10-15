// Light notice window.
//
// Shown by the desktop process and hidden by it again, so this script only
// paints. It never takes focus and never blocks the user: the process sets
// `ignore_cursor_events` and shows the window without activating it.
const listen = window.__TAURI__.event.listen;

const card = document.querySelector('#card');
const title = document.querySelector('#title');
const text = document.querySelector('#text');

listen('toast', (event) => {
  const payload = event.payload || {};
  const warning = payload.level === 'warning';
  title.textContent = warning ? 'CloudFile · 需要处理' : 'CloudFile · 已保存到云端';
  text.textContent = String(payload.text || '');
  card.className = warning ? 'toast warning' : 'toast info';
  // Restart the fade so two notices in a row both animate.
  card.classList.remove('show');
  void card.offsetWidth;
  card.classList.add('show');
}).catch(() => {});
