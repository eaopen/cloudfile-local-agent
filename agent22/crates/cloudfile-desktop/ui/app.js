const invoke=window.__TAURI__.core.invoke;
const message=document.querySelector('#message');
const active=new Set();
let refreshing=false;
function say(text){message.textContent=text;}
function stateText(result){
 if(result.state==='published')return '已提交到云端。';
 if(result.state==='unchanged')return '没有需要提交的修改。';
 if(result.state==='pending')return '原提交尚未确认；请先查询原提交结果。';
 if(result.state==='released')return '已释放 Checkout，本地任务已关闭。';
 if(result.state==='renewed')return 'Checkout 已续租。';
 return '操作已完成。';
}
async function action(task,command,args){
 if(active.has(task.task_id))return;
 active.add(task.task_id);say('正在处理，请保留本地文件…');await refresh();
 try{const result=await invoke(command,{taskId:task.task_id,...args});say(stateText(result));}
 catch(e){say(String(e));}
 finally{active.delete(task.task_id);await refresh();}
}
function automaticStatus(task,automatic,busy){
 if(busy)return 'Agent 正在上传…';
 if(task.needs_review)return '工作副本需要检查';
 if(task.pending_upload)return task.pending_action==='checkin'?'原签入待确认，本地修改已保留':task.pending_action==='commit'?'原提交待确认，本地修改已保留':'原上传待确认，本地修改已保留';
 if(automatic?.pause_reason)return '自动上传已暂停，本地修改已保留';
 if(task.local_changed)return automatic?.enabled?'有本地修改，等待文件保存稳定后自动上传':'有本地修改，自动上传已关闭';
 return automatic?.last_synced_at?'已保存云端 · '+new Date(automatic.last_synced_at*1000).toLocaleTimeString():'与云端保存基线一致';
}
function exclusiveStatus(task,busy){
 if(busy)return '正在提交…';
 if(task.checkout_closed)return '已签入并释放 Checkout；如需继续编辑请从网页重新发起';
 if(!task.has_lease)return 'Checkout 租约尚未就绪；请重新打开该文件以完成领取';
 if(!task.lease_acknowledged)return 'Checkout 交付尚未确认；请重新打开该文件';
 if(task.pending_upload)return task.pending_action==='checkin'?'原签入待确认，本地修改已保留':'原提交待确认，本地修改已保留';
 if(task.local_changed)return '有本地修改，可提交保留 Checkout，或签入并释放';
 return '与云端保存基线一致';
}
function button(label,handler,{secondary=false,disabled=false}={}){
 const element=document.createElement('button');
 element.textContent=label;
 if(secondary)element.className='secondary';
 element.disabled=disabled;
 element.onclick=handler;
 return element;
}
function renderOptimistic(info,buttons,task,automatic,busy){
 buttons.append(button('打开文件',async()=>{try{await invoke('open_task',{taskId:task.task_id});}catch(e){say(String(e));}},{disabled:task.needs_review}));
 const save=button(task.pending_upload?'重试原上传':'保存云端',()=>action(task,'save_cloud'),{disabled:busy||task.needs_review||(!task.local_changed&&!task.pending_upload)});
 buttons.append(save);
 buttons.append(button(automatic?.pause_reason?'恢复自动上传':automatic?.enabled?'暂停自动上传':'开启自动上传',async()=>{try{await invoke('set_auto_upload',{taskId:task.task_id,enabled:Boolean(!automatic?.enabled||automatic?.pause_reason)});await refresh();}catch(e){say(String(e));}},{secondary:true,disabled:busy||task.needs_review||task.pending_upload}));
 if(task.pending_upload){buttons.append(button('查询上传结果',()=>action(task,'recover_upload'),{disabled:busy}));}
}
function renderExclusive(info,buttons,task,busy){
 const pending=task.pending_upload;
 buttons.append(button('打开文件',async()=>{try{await invoke('open_task',{taskId:task.task_id});}catch(e){say(String(e));}},{disabled:task.needs_review}));
 if(!task.checkout_closed){
  buttons.append(button('提交',()=>action(task,'commit_task',{checkin:false}),{disabled:busy||task.needs_review||pending}));
  buttons.append(button('签入并释放',()=>action(task,'commit_task',{checkin:true}),{secondary:true,disabled:busy||task.needs_review||pending}));
  if(pending){buttons.append(button('查询提交结果',()=>action(task,'recover_commit'),{disabled:busy}));}
  buttons.append(button('续租',()=>action(task,'renew_checkout'),{secondary:true,disabled:busy||pending||!task.has_lease}));
  buttons.append(button('放弃保留',()=>action(task,'release_checkout'),{secondary:true,disabled:busy||pending||!task.has_lease}));
 }
}
async function refresh(){if(refreshing)return;refreshing=true;try{
 const result=await invoke('task_summary');const container=document.querySelector('#tasks');container.replaceChildren();
 const notice=document.querySelector('.notice');
 if(result.cdc_enabled&&!document.querySelector('#cdc-hint')){const hint=document.createElement('p');hint.id='cdc-hint';hint.textContent='大文件分块上传已启用：不小于 16 MiB 的保存会优先复用云端已有分块，仅在确实节省空间时使用，否则仍为全量传输。';notice.append(hint);}
 if(!result.tasks.length){const empty=document.createElement('div');empty.className='empty';empty.textContent='还没有本地任务。添加服务并连接设备后，从 CloudFile 网页选择本地编辑。';container.append(empty);}
 for(const task of result.tasks){const row=document.createElement('article');row.className='task';row.id=task.task_id;
 const info=document.createElement('div');const name=document.createElement('strong');name.textContent=task.file_name;
 const automatic=task.auto_upload;const busy=active.has(task.task_id)||task.uploading;
 const status=document.createElement('span');status.textContent=task.exclusive?exclusiveStatus(task,busy):automaticStatus(task,automatic,busy);
 info.append(name,status);
 if(automatic?.pause_reason&&!task.exclusive){const reason=document.createElement('p');reason.className='upload-error';reason.textContent=automatic.pause_reason;info.append(reason);}
 const buttons=document.createElement('div');buttons.className='actions';
 if(task.exclusive){renderExclusive(info,buttons,task,busy);}
 else if(task.can_upload){renderOptimistic(info,buttons,task,automatic,busy);}
 else{buttons.append(button('打开文件',async()=>{try{await invoke('open_task',{taskId:task.task_id});}catch(e){say(String(e));}},{disabled:task.needs_review}));
  const hint=document.createElement('small');hint.textContent=task.mode==='local-view'?'只读副本':'旧会话仅支持本地打开；请从网页重新选择本地编辑';buttons.append(hint);}
 row.append(info,buttons);container.append(row);}
 }catch(e){say(String(e));}finally{refreshing=false;}}
async function origins(){const values=await invoke('service_status');const list=document.querySelector('#origins');list.replaceChildren();for(const service of values){const li=document.createElement('li');const label=document.createElement('span');label.textContent=service.service_url+' · '+({paired:'已连接设备',pending:'等待 Chrome 完成配对',unpaired:'尚未连接设备'}[service.state]);li.append(label);
 if(service.state!=='paired'){const element=document.createElement('button');element.textContent=service.state==='pending'?'重新检查':'连接设备';element.onclick=async()=>{try{await invoke('begin_pairing',{origin:service.origin});say('请在 Chrome 打开并登录此 CloudFile 服务，再点击开发扩展中的“连接 Agent 2.2”。有效期 10 分钟。');await origins();}catch(e){say(String(e));}};li.append(element);}list.append(li);}}
document.querySelector('#refresh').onclick=()=>{refresh();origins().catch(e=>say(String(e)));};
document.querySelector('#trust').onsubmit=async e=>{e.preventDefault();try{await invoke('trust_origin',{origin:document.querySelector('#origin').value.trim()});say('服务地址已保存；设备连接状态见服务列表。');await origins();}catch(err){say(String(err));}};
window.__TAURI__.event.listen('tasks-changed',()=>refresh());
window.__TAURI__.event.listen('focus-task',async({payload})=>{await refresh();document.getElementById(payload)?.classList.add('focused');document.getElementById(payload)?.scrollIntoView({block:'center'});});
refresh();origins().catch(e=>say(String(e)));
setInterval(()=>{refresh();origins().catch(()=>{});},10000);
