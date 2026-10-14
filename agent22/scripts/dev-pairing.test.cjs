const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const fs=require('node:fs');
const source=fs.readFileSync(__dirname+'/dev-pairing.js','utf8');
function harness(changedOrigin=false,servicePath=''){
 const elements=[],native=[],requests=[];
 const context={URL,Error,JSON,document:{cookie:'sessionid=browser-only; sfcsrftoken=test-csrf',body:{append(e){elements.push(e);}},createElement(){return {setAttribute(){}};},querySelector(){return null;}},location:{origin:changedOrigin?'https://other.example':'https://cloudfile.example'},fetch:async(url,options)=>{
  requests.push({url,options});return {ok:true,json:async()=>url.includes('pair-start')?{challenge:'public-server-challenge'}:{device_id:'device',state:'active',revision:'2'}};
 },chrome:{tabs:{query:async()=>[{id:7,url:'https://cloudfile.example/library/'}]},scripting:{executeScript:async({func,args})=>[{result:await func(...args)}]},runtime:{sendNativeMessage:async(host,body)=>{
  assert.equal(host,'com.cloudfile.agent22.dev');native.push(body);
  if(body.type==='pair_enrollment')return {ok:true,service_path:servicePath,enrollment:{public_key:{kty:'EC'}}};
  if(body.type==='pair_sign')return {ok:true,proof:{device_id:'device',nonce:'nonce',signature:'signature'}};
  if(body.type==='pair_confirm')return {ok:true};throw Error('unexpected native call');
 }}}};vm.runInNewContext(source,context);return {elements,native,requests};
}
test('pairs on logged-in page with Seahub CSRF without sending cookies to Agent',async()=>{
 const h=harness();await h.elements[0].onclick();assert.equal(h.requests.length,2);assert.equal(h.requests[0].options.headers['X-CSRFToken'],'test-csrf');assert.equal(h.requests[0].options.credentials,'same-origin');assert.equal(h.requests[0].options.redirect,'error');assert.deepEqual(h.native.map(x=>x.type),['pair_enrollment','pair_sign','pair_confirm']);assert(!JSON.stringify(h.native).includes('browser-only'));assert(!JSON.stringify(h.native).includes('test-csrf'));assert.match(h.elements[1].textContent,/设备已连接/);
});
test('navigation to another origin stops pairing before browser request',async()=>{
 const h=harness(true);await h.elements[0].onclick();assert.equal(h.requests.length,0);assert.equal(h.native.length,1);assert.match(h.elements[1].textContent,/页面已切换/);
});

test('uses only the Agent-configured service prefix for browser pairing',async()=>{
 const h=harness(false,'/seafile');await h.elements[0].onclick();assert.equal(h.requests[0].url,'/seafile/api/v2.1/cloudfile/extensions/local-edit/v1/devices/pair-start/');
 const bad=harness(false,'/../foreign');await bad.elements[0].onclick();assert.equal(bad.requests.length,0);
});
