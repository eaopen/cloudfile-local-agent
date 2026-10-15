// Draggable resident entry.
//
// The entry is a view onto the task store, never a second source of truth: it
// asks the desktop process for its visibility and count, and reports nothing
// back. Moving the window is delegated to the operating system through
// `startDragging`, so the entry stays glued to the cursor across displays and
// scale factors; the process saves the resting place itself.
const invoke = window.__TAURI__.core.invoke;
const currentWindow = window.__TAURI__.window.getCurrentWindow();
const listen = window.__TAURI__.event.listen;

const handle = document.querySelector('#handle');
const badge = document.querySelector('#badge');

// Below this many pixels a gesture is a click, not a drag. The OS only takes
// over once the limit is crossed, otherwise the entry could never be clicked.
const DRAG_SLOP = 4;

let pointerId = null;
let originX = 0;
let originY = 0;
let dragging = false;

handle.addEventListener('pointerdown', (event) => {
  if (event.button !== 0) return;
  pointerId = event.pointerId;
  originX = event.screenX;
  originY = event.screenY;
  dragging = false;
  handle.setPointerCapture(pointerId);
});

handle.addEventListener('pointermove', (event) => {
  if (pointerId === null || event.pointerId !== pointerId || dragging) return;
  if (
    Math.abs(event.screenX - originX) < DRAG_SLOP &&
    Math.abs(event.screenY - originY) < DRAG_SLOP
  ) {
    return;
  }
  dragging = true;
  // Hand the gesture to the OS. It owns the pointer from here on, so the
  // matching pointerup may never arrive.
  currentWindow.startDragging().catch(() => {});
});

function finish(event) {
  if (pointerId === null || event.pointerId !== pointerId) return;
  if (handle.hasPointerCapture(pointerId)) handle.releasePointerCapture(pointerId);
  pointerId = null;
  if (!dragging) {
    invoke('open_task_center').catch(() => {});
  }
  dragging = false;
}

handle.addEventListener('pointerup', finish);
handle.addEventListener('pointercancel', finish);

function render(payload) {
  const attention = Number(payload.needs_attention || 0);
  if (attention > 0) {
    badge.hidden = false;
    badge.textContent = String(attention);
    handle.title = `CloudFile 文件任务 · ${attention} 项待处理`;
  } else {
    badge.hidden = true;
    handle.title = 'CloudFile 文件任务（点击展开，拖动移动）';
  }
}

listen('float-state', (event) => render(event.payload)).catch(() => {});
invoke('float_sync').then(render).catch(() => {});
