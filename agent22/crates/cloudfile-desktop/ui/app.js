// CloudFile task centre.
//
// Every panel here is a view onto the durable store owned by the desktop
// process: nothing is cached across refreshes and nothing decides on its own
// whether a task is finished. The window only renders state and forwards the
// user's choice.
const invoke = window.__TAURI__.core.invoke;
const listen = window.__TAURI__.event.listen;

const message = document.querySelector('#message');
const tasks = document.querySelector('#tasks');
const counts = document.querySelector('#counts');
const exitBanner = document.querySelector('#confirm-exit');
const exitText = document.querySelector('#confirm-exit-text');
const updateBanner = document.querySelector('#update-hint');
const hostState = document.querySelector('#host-state');

let busy = new Set();
let refreshing = false;
let lastAssessment = null;

function say(text) {
  message.textContent = text;
}

function stateText(result) {
  if (result.state === 'published') return '已提交到云端。';
  if (result.state === 'unchanged') return '没有需要提交的修改。';
  if (result.state === 'pending') return '原提交尚未确认；请先查询原提交结果。';
  if (result.state === 'released') return '已释放 Checkout，本地任务已关闭。';
  if (result.state === 'renewed') return 'Checkout 已续租。';
  if (result.state === 'running') return '更新已在后台开始；完成前不会替换正在运行的版本。';
  return '操作已完成。';
}

async function action(task, command, args) {
  if (busy.has(task.task_id)) return;
  busy.add(task.task_id);
  say('正在处理，请保留本地文件…');
  refresh();
  try {
    const result = await invoke(command, { taskId: task.task_id, ...args });
    say(stateText(result));
  } catch (error) {
    say(String(error));
  } finally {
    busy.delete(task.task_id);
    refresh();
  }
}

// The exit banner maps the guard's kebab-case reasons to wording that says what
// the agent did with the file, never that the file is "safe". Automatic upload
// pause reasons are already user-facing text and are shown verbatim.
const REASON_TEXT = {
  'unreadable-state': '任务状态无法读取，已停止自动处理',
  'pending-intent': '原提交尚未确认，本地修改已保留',
  'unacknowledged-lease': 'Checkout 交付尚未确认，请重新打开该文件',
  'active-checkout': '独占 Checkout 仍在续租中',
};

function automaticStatus(task, automatic, working) {
  if (working) return 'Agent 正在上传…';
  if (task.needs_review) return '工作副本需要检查';
  if (task.pending_upload) {
    if (task.pending_action === 'checkin') return '原签入待确认，本地修改已保留';
    if (task.pending_action === 'commit') return '原提交待确认，本地修改已保留';
    return '原上传待确认，本地修改已保留';
  }
  if (automatic && automatic.pause_reason) return '自动上传已暂停，本地修改已保留';
  if (task.local_changed) {
    return automatic && automatic.enabled
      ? '有本地修改，等待文件保存稳定后自动上传'
      : '有本地修改，自动上传已关闭';
  }
  if (automatic && automatic.last_synced_at) {
    return '已保存云端 · ' + new Date(automatic.last_synced_at * 1000).toLocaleTimeString();
  }
  return '与云端保存基线一致';
}

function exclusiveStatus(task, working) {
  if (working) return '正在提交…';
  if (task.checkout_closed) return '已签入并释放 Checkout；如需继续编辑请从网页重新发起';
  if (!task.has_lease) return 'Checkout 租约尚未就绪；请重新打开该文件以完成领取';
  if (!task.lease_acknowledged) return 'Checkout 交付尚未确认；请重新打开该文件';
  if (task.pending_upload) {
    return task.pending_action === 'checkin'
      ? '原签入待确认，本地修改已保留'
      : '原提交待确认，本地修改已保留';
  }
  if (task.local_changed) return '有本地修改，可提交保留 Checkout，或签入并释放';
  return '与云端保存基线一致';
}

function button(label, handler, { secondary = false, disabled = false } = {}) {
  const element = document.createElement('button');
  element.textContent = label;
  if (secondary) element.className = 'secondary';
  element.disabled = disabled;
  element.onclick = handler;
  return element;
}

function openButton(task) {
  return button(
    '打开文件',
    async () => {
      try {
        await invoke('open_task', { taskId: task.task_id });
      } catch (error) {
        say(String(error));
      }
    },
    { disabled: task.needs_review },
  );
}

function renderOptimistic(buttons, task, automatic, working) {
  buttons.append(openButton(task));
  buttons.append(
    button(task.pending_upload ? '重试原上传' : '保存云端', () => action(task, 'save_cloud'), {
      disabled: working || task.needs_review || (!task.local_changed && !task.pending_upload),
    }),
  );
  const paused = Boolean(automatic && automatic.pause_reason);
  buttons.append(
    button(
      paused ? '恢复自动上传' : automatic && automatic.enabled ? '暂停自动上传' : '开启自动上传',
      async () => {
        try {
          await invoke('set_auto_upload', {
            taskId: task.task_id,
            enabled: !(automatic && automatic.enabled) || paused,
          });
          await refresh();
        } catch (error) {
          say(String(error));
        }
      },
      { secondary: true, disabled: working || task.needs_review || task.pending_upload },
    ),
  );
  if (task.pending_upload) {
    buttons.append(button('查询上传结果', () => action(task, 'recover_upload'), { disabled: working }));
  }
}

function renderExclusive(buttons, task, working) {
  buttons.append(openButton(task));
  if (task.checkout_closed) return;
  const pending = task.pending_upload;
  buttons.append(
    button('提交', () => action(task, 'commit_task', { checkin: false }), {
      disabled: working || task.needs_review || pending,
    }),
  );
  buttons.append(
    button('签入并释放', () => action(task, 'commit_task', { checkin: true }), {
      secondary: true,
      disabled: working || task.needs_review || pending,
    }),
  );
  if (pending) {
    buttons.append(button('查询提交结果', () => action(task, 'recover_commit'), { disabled: working }));
  }
  buttons.append(
    button('续租', () => action(task, 'renew_checkout'), {
      secondary: true,
      disabled: working || pending || !task.has_lease,
    }),
  );
  buttons.append(
    button('放弃保留', () => action(task, 'release_checkout'), {
      secondary: true,
      disabled: working || pending || !task.has_lease,
    }),
  );
}

function renderTask(task) {
  const row = document.createElement('article');
  row.className = 'task';
  row.id = task.task_id;

  const info = document.createElement('div');
  info.className = 'info';
  const name = document.createElement('strong');
  name.textContent = task.file_name;
  const status = document.createElement('span');
  const working = busy.has(task.task_id) || task.uploading;
  const automatic = task.auto_upload;
  status.textContent = task.exclusive
    ? exclusiveStatus(task, working)
    : automaticStatus(task, automatic, working);
  const mode = document.createElement('small');
  mode.className = 'hint';
  mode.textContent = task.exclusive ? '独占编辑' : '普通编辑';
  info.append(name, mode, status);

  if (automatic && automatic.pause_reason && !task.exclusive) {
    const reason = document.createElement('p');
    reason.className = 'upload-error';
    reason.textContent = automatic.pause_reason;
    info.append(reason);
  }

  const buttons = document.createElement('div');
  buttons.className = 'actions';
  if (task.exclusive) {
    renderExclusive(buttons, task, working);
  } else if (task.can_upload) {
    renderOptimistic(buttons, task, automatic, working);
  } else {
    buttons.append(openButton(task));
    const hint = document.createElement('small');
    hint.className = 'hint';
    hint.textContent =
      task.mode === 'local-view'
        ? '只读副本'
        : '旧会话仅支持本地打开；请从网页重新选择本地编辑';
    buttons.append(hint);
  }

  row.append(info, buttons);
  return row;
}

const GROUPS = [
  ['needs-attention', '待处理', '需要你决定'],
  ['in-progress', '进行中', '本地已修改或正在上传'],
  ['done', '已完成', '与云端一致'],
];

function renderSummary(result) {
  counts.replaceChildren();
  for (const [key, label] of GROUPS) {
    const cell = document.createElement('div');
    cell.className = 'count';
    const value = document.createElement('b');
    value.textContent = String(result.counts[key] || 0);
    const caption = document.createElement('span');
    caption.textContent = label;
    cell.append(value, caption);
    counts.append(cell);
  }

  tasks.replaceChildren();
  const notice = document.querySelector('.notice');
  if (result.cdc_enabled && !document.querySelector('#cdc-hint')) {
    const hint = document.createElement('p');
    hint.id = 'cdc-hint';
    hint.textContent =
      '大文件分块上传已启用：不小于 16 MiB 的保存会优先复用云端已有分块，仅在确实节省空间时使用，否则仍为全量传输。';
    notice.append(hint);
  }

  if (!result.tasks.length) {
    const empty = document.createElement('div');
    empty.className = 'empty';
    empty.textContent = '还没有本地任务。添加服务并连接设备后，从 CloudFile 网页选择本地编辑。';
    tasks.append(empty);
    return;
  }

  for (const [key, label, hint] of GROUPS) {
    const members = result.tasks.filter((task) => task.group === key);
    if (!members.length) continue;
    const header = document.createElement('div');
    header.className = 'group';
    const heading = document.createElement('h2');
    heading.textContent = label;
    const note = document.createElement('small');
    note.textContent = `${members.length} 项 · ${hint}`;
    header.append(heading, note);
    tasks.append(header);
    for (const task of members) tasks.append(renderTask(task));
  }
}

async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const result = await invoke('task_summary');
    renderSummary(result);
    // The entry mirrors the same list, so a task can never be visible in one
    // view and hidden in the other.
    invoke('float_sync').catch(() => {});
  } catch (error) {
    say(String(error));
  } finally {
    refreshing = false;
  }
}

async function renderExitState() {
  try {
    const assessment = await invoke('exit_assessment');
    lastAssessment = assessment;
    const target = document.querySelector('#exit-state');
    target.textContent = assessment.safe_to_exit
      ? '退出状态：可以安全退出'
      : `退出状态：${assessment.active_checkouts} 个独占任务 · ${assessment.uncertain} 项待确认`;
  } catch {
    /* The banner already covers the actionable case. */
  }
}

function showExitBanner(assessment) {
  if (assessment && assessment.safe_to_exit) {
    exitBanner.hidden = true;
    return;
  }
  const risks = (assessment && assessment.risks) || [];
  exitText.textContent = risks.length
    ? risks
        .slice(0, 4)
        .map((risk) => {
          const reason = REASON_TEXT[risk.reasons[0]] || risk.reasons[0];
          return `${risk.file_name}：${reason}`;
        })
        .join('；')
    : '仍有独占编辑任务或未确认的上传。';
  exitBanner.hidden = false;
}

async function loadSettings() {
  const current = await invoke('settings_get');
  document.querySelector('#notify_on_saved').checked = current.settings.notify_on_saved;
  document.querySelector('#notify_on_review').checked = current.settings.notify_on_review;
  document.querySelector('#float_enabled').checked = current.settings.float_enabled;
  document.querySelector('#float_hidden_when_empty').checked =
    current.settings.float_hidden_when_empty;
  document.querySelector('#autostart').checked = current.autostart_enabled;
  document.querySelector('#workspace_root').value = current.settings.workspace_root || '';
  document.querySelector('#version').textContent = `版本 ${current.version} · 安装目录 ${current.install_root}`;

  const registration = current.registered_host;
  if (!registration) {
    hostState.textContent = '未找到本机 Host 目录，无法注册。';
  } else if (registration.foreign) {
    hostState.textContent = `Host ${registration.host} 由其他程序注册，可备份后接管。`;
  } else if (registration.registered) {
    hostState.textContent = `Host ${registration.host} 已注册（扩展 ${registration.extension_id}）→ ${registration.manifest_path}`;
  } else {
    hostState.textContent = `Host ${registration.host} 尚未注册；注册由 Agent 完成，不需要 Python。`;
  }
  return current;
}

async function renderOrigins() {
  const values = await invoke('service_status');
  const list = document.querySelector('#origins');
  list.replaceChildren();
  for (const service of values) {
    const item = document.createElement('li');
    const label = document.createElement('span');
    const stateTextMap = {
      paired: '已连接设备',
      pending: '等待 Chrome 完成配对',
      unpaired: '尚未连接设备',
    };
    label.textContent = `${service.service_url} · ${stateTextMap[service.state] || service.state}`;
    item.append(label);
    if (service.state !== 'paired') {
      const element = document.createElement('button');
      element.textContent = service.state === 'pending' ? '重新检查' : '连接设备';
      element.className = 'secondary';
      element.onclick = async () => {
        try {
          await invoke('begin_pairing', { origin: service.origin });
          say('请在 Chrome 打开并登录此 CloudFile 服务，再点击扩展中的“连接 Agent 2.2”。有效期 10 分钟。');
          await renderOrigins();
        } catch (error) {
          say(String(error));
        }
      };
      item.append(element);
    }
    list.append(item);
  }
}

function wire() {
  document.querySelector('#refresh').onclick = () => {
    refresh();
    renderOrigins().catch((error) => say(String(error)));
    renderExitState();
  };

  document.querySelector('#open-workspace').onclick = async () => {
    try {
      await invoke('open_workspace');
      say('已打开工作目录。');
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#settings').onsubmit = async (event) => {
    event.preventDefault();
    try {
      await invoke('settings_set', {
        notifyOnSaved: document.querySelector('#notify_on_saved').checked,
        notifyOnReview: document.querySelector('#notify_on_review').checked,
        floatEnabled: document.querySelector('#float_enabled').checked,
        floatHiddenWhenEmpty: document.querySelector('#float_hidden_when_empty').checked,
        workspaceRoot: document.querySelector('#workspace_root').value,
      });
      const autostart = await invoke('autostart_set', {
        enabled: document.querySelector('#autostart').checked,
      });
      document.querySelector('#autostart').checked = autostart.autostart_enabled;
      say('本机设置已保存。');
      invoke('float_sync').catch(() => {});
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#trust').onsubmit = async (event) => {
    event.preventDefault();
    try {
      await invoke('trust_origin', {
        origin: document.querySelector('#origin').value.trim(),
      });
      say('服务地址已保存；设备连接状态见服务列表。');
      await renderOrigins();
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#register').onsubmit = async (event) => {
    event.preventDefault();
    const extensionId = document.querySelector('#extension-id').value.trim();
    try {
      const outcome = await invoke('register_host', { extensionId, takeover: false });
      say(
        outcome.backed_up
          ? `Host 已注册，原注册已备份到 ${outcome.previous_manifest_path}。`
          : 'Host 已注册。',
      );
      await loadSettings();
    } catch (error) {
      // A host owned by another program is replaced on purpose, never silently.
      const replace = window.confirm(
        `注册未完成：${error}\n\n是否备份并接管现有注册？接管会保留原 Manifest 以便回退。`,
      );
      if (!replace) {
        say('已取消；原 Host 注册未被改动。');
        return;
      }
      try {
        const outcome = await invoke('register_host', { extensionId, takeover: true });
        say(`Host 已接管；原注册备份在 ${outcome.previous_manifest_path}。`);
        await loadSettings();
      } catch (retryError) {
        say(String(retryError));
      }
    }
  };

  document.querySelector('#host-rollback').onclick = async () => {
    try {
      say(`已回退到原注册：${await invoke('rollback_host')}`);
      await loadSettings();
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#host-unregister').onclick = async () => {
    try {
      await invoke('unregister_host');
      say('本机 Host 已卸载；不会删除本地工作副本。');
      await loadSettings();
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#check-update').onclick = async () => {
    try {
      const result = await invoke('check_update');
      if (!result.update_source_set) {
        say('尚未配置升级地址；开发版不自动检查。');
        return;
      }
      if (!result.update_available) {
        say(result.update_error ? `检查未通过：${result.update_error}` : '当前已是最新版本。');
        return;
      }
      document.querySelector('#update-title').textContent = `发现新版本 ${result.latest_version}`;
      document.querySelector('#update-notes').textContent = result.latest_notes || '暂无版本说明。';
      updateBanner.hidden = false;
      say('发现可用升级；确认后再下载。');
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#update-apply').onclick = async () => {
    try {
      let result = await invoke('run_update', { force: false });
      if (result.state === 'blocked') {
        // Exactly the exit dialog's trade-off: an upgrade stops the process
        // that renews Checkouts, so it is never silent when something is at risk.
        const risks = (result.assessment && result.assessment.risks) || [];
        const detail = risks
          .slice(0, 4)
          .map((risk) => `${risk.file_name}：${REASON_TEXT[risk.reasons[0]] || risk.reasons[0]}`)
          .join('\n');
        const proceed = window.confirm(
          `仍有任务未处理，升级会中断后台续租：\n${detail}\n\n本地工作副本与待确认的 Intent 都会保留。是否继续升级？`,
        );
        if (!proceed) {
          say('已取消升级；当前版本保持不变。');
          return;
        }
        result = await invoke('run_update', { force: true });
      }
      say(stateText(result));
      updateBanner.hidden = true;
    } catch (error) {
      say(String(error));
    }
  };

  document.querySelector('#update-dismiss').onclick = () => {
    updateBanner.hidden = true;
  };

  document.querySelector('#exit-cancel').onclick = () => {
    exitBanner.hidden = true;
    say('已取消退出；后台续租继续运行。');
  };

  document.querySelector('#exit-force').onclick = async () => {
    try {
      await invoke('quit_app', { force: true });
    } catch (error) {
      say(String(error));
    }
  };

  listen('tasks-changed', () => refresh());
  listen('focus-task', async ({ payload }) => {
    await refresh();
    const target = document.getElementById(payload);
    if (target) {
      target.classList.add('focused');
      target.scrollIntoView({ block: 'center' });
    }
  });
  // The tray and the window ask the same question, so the banner is driven by
  // the process, not by this script's own guess.
  listen('confirm-exit', ({ payload }) => {
    showExitBanner(payload);
    refresh();
  });
}

wire();
refresh();
loadSettings().catch((error) => say(String(error)));
renderOrigins().catch((error) => say(String(error)));
renderExitState();
setInterval(() => {
  refresh();
  renderExitState();
  renderOrigins().catch(() => {});
}, 10000);
