const invoke=window.__TAURI__.core.invoke;
const message=document.querySelector('#message');
const active=new Set();
let refreshing=false;
async function action(task,command){
 if(active.has(task.task_id))return;
 active.add(task.task_id);message.textContent=command==='save_cloud'?'Agent 正在上传，请保留本地文件…':'正在查询原上传结果…';await refresh();
 try{const result=await invoke(command,{taskId:task.task_id});message.textContent=result.state==='published'?'已保存云端。可以继续本地编辑。':result.state==='unchanged'?'没有需要上传的修改。':'原上传尚未发布；可以重试原上传，本地文件已保留。';}
 catch(e){message.textContent=String(e);}
 finally{active.delete(task.task_id);await refresh();}
}
async function refresh(){if(refreshing)return;refreshing=true;try{
 const result=await invoke('task_summary');const container=document.querySelector('#tasks');container.replaceChildren();
 if(!result.tasks.length){const empty=document.createElement('div');empty.className='empty';empty.textContent='还没有本地任务。添加服务并连接设备后，从 CloudFile 网页选择本地编辑。';container.append(empty);}
 for(const task of result.tasks){const row=document.createElement('article');row.className='task';row.id=task.task_id;
 const info=document.createElement('div');const name=document.createElement('strong');name.textContent=task.file_name;
 const status=document.createElement('span');status.textContent=active.has(task.task_id)?'Agent 正在处理…':task.needs_review?'工作副本需要检查':task.pending_upload?'原上传待确认，本地修改已保留':task.local_changed?'有本地修改，尚未保存云端':'与云端保存基线一致';
 info.append(name,status);const buttons=document.createElement('div');buttons.className='actions';
 const button=document.createElement('button');button.textContent='打开文件';button.disabled=task.needs_review;button.onclick=async()=>{try{await invoke('open_task',{taskId:task.task_id});}catch(e){message.textContent=String(e);}};buttons.append(button);
 if(task.can_upload){const save=document.createElement('button');save.textContent=task.pending_upload?'重试原上传':'保存云端';save.disabled=active.has(task.task_id)||task.needs_review||(!task.local_changed&&!task.pending_upload);save.onclick=()=>action(task,'save_cloud');buttons.append(save);
 if(task.pending_upload){const check=document.createElement('button');check.textContent='查询上传结果';check.disabled=active.has(task.task_id);check.onclick=()=>action(task,'recover_upload');buttons.append(check);}}
 else{const hint=document.createElement('small');hint.textContent=task.mode==='local-view'?'只读副本':'旧会话仅支持本地打开；请从网页重新选择本地编辑';buttons.append(hint);}
 row.append(info,buttons);container.append(row);}
 }catch(e){message.textContent=String(e);}finally{refreshing=false;}}
async function origins(){const values=await invoke('service_status');const list=document.querySelector('#origins');list.replaceChildren();for(const service of values){const li=document.createElement('li');const label=document.createElement('span');label.textContent=service.service_url+' · '+({paired:'已连接设备',pending:'等待 Chrome 完成配对',unpaired:'尚未连接设备'}[service.state]);li.append(label);
 if(service.state!=='paired'){const button=document.createElement('button');button.textContent=service.state==='pending'?'重新检查':'连接设备';button.onclick=async()=>{try{await invoke('begin_pairing',{origin:service.origin});message.textContent='请在 Chrome 打开并登录此 CloudFile 服务，再点击开发扩展中的“连接 Agent 2.2”。有效期 10 分钟。';await origins();}catch(e){message.textContent=String(e);}};li.append(button);}list.append(li);}}
document.querySelector('#refresh').onclick=()=>{refresh();origins().catch(e=>message.textContent=String(e));};
document.querySelector('#trust').onsubmit=async e=>{e.preventDefault();try{await invoke('trust_origin',{origin:document.querySelector('#origin').value.trim()});message.textContent='服务已添加，请连接设备。';await origins();}catch(err){message.textContent=String(err);}};
window.__TAURI__.event.listen('focus-task',async({payload})=>{await refresh();document.getElementById(payload)?.classList.add('focused');document.getElementById(payload)?.scrollIntoView({block:'center'});});
refresh();origins().catch(e=>message.textContent=String(e));
setInterval(()=>{refresh();origins().catch(()=>{});},10000);
