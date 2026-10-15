const pairButton=document.createElement('button');pairButton.textContent='连接 Agent 2.2';document.body.append(pairButton);
const pairMessage=document.createElement('p');pairMessage.setAttribute('role','status');document.body.append(pairMessage);
const host='com.cloudfile.agent22.dev';
async function native(body){const result=await chrome.runtime.sendNativeMessage(host,body);if(!result?.ok)throw new Error(result?.error||'Agent 未响应');return result;}
async function serverCall(tabId,expectedOrigin,operation,body,servicePath){
 const results=await chrome.scripting.executeScript({target:{tabId},world:'MAIN',func:async(expected,op,value,prefix)=>{
  if(location.origin!==expected)return {error:'页面已切换，请重新连接'};
  if(!/^([/][a-zA-Z0-9_-]+)*$/.test(prefix))return {error:'服务部署路径无效'};
  if(!['pair-start','pair-confirm'].includes(op))return {error:'无效操作'};
  const cookies=document.cookie.split(';').map(v=>v.trim());
  const cookie=cookies.find(v=>v.startsWith('sfcsrftoken='))||cookies.find(v=>v.startsWith('csrftoken='));
  const token=cookie?decodeURIComponent(cookie.slice(cookie.indexOf('=')+1)):document.querySelector('[name=csrfmiddlewaretoken]')?.value;
  if(!token)return {error:'请先在当前页面登录 CloudFile'};
  try{const response=await fetch(prefix+'/api/v2.1/cloudfile/extensions/local-edit/v1/devices/'+op+'/',{method:'POST',credentials:'same-origin',redirect:'error',headers:{'Content-Type':'application/json','X-CSRFToken':token},body:JSON.stringify(value)});
   if(!response.ok)return {error:'服务未确认设备配对（HTTP '+response.status+'）'};
   return {value:await response.json()};
  }catch{return {error:'配对请求失败，请检查服务连接'};}
 },args:[expectedOrigin,operation,body,servicePath]});
 const result=results[0]?.result;if(!result||result.error)throw new Error(result?.error||'无法访问当前 CloudFile 页面');return result.value;
}
pairButton.onclick=async()=>{pairButton.disabled=true;pairMessage.textContent='正在连接设备…';try{
 const [tab]=await chrome.tabs.query({active:true,currentWindow:true});const url=new URL(tab?.url);if(!['http:','https:'].includes(url.protocol))throw new Error('请先打开并登录 CloudFile 网页');
 const server=url.origin;const {enrollment,service_path:servicePath=''}=await native({type:'pair_enrollment',server});
 const challenge=await serverCall(tab.id,server,'pair-start',enrollment,servicePath);
 const {proof}=await native({type:'pair_sign',server,response:challenge});
 const confirmed=await serverCall(tab.id,server,'pair-confirm',proof,servicePath);
 await native({type:'pair_confirm',server,response:confirmed});pairMessage.textContent='设备已连接，可从网页选择普通本地编辑。';
 }catch(e){pairMessage.textContent=e.message;}finally{pairButton.disabled=false;}};
