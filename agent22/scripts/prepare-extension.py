#!/usr/bin/env python3
"""Create an unpacked dev extension without modifying the shared extension repo."""
import pathlib, shutil, json
root=pathlib.Path(__file__).resolve().parents[1]
source=root.parents[1]/'cloudfile-chrome-extension'
target=root/'dist'/'chrome-extension-agent22'
if target.exists():shutil.rmtree(target)
shutil.copytree(source,target,ignore=shutil.ignore_patterns('.git','node_modules','.DS_Store'))
p=target/'background.js'
s=p.read_text()
for name in ['com.cloudfile.agent2','com.cloudfile.local_agent','com.cloudfile.current_agent']:
 s=s.replace("'"+name+"'","'com.cloudfile.agent22.dev'")
p.write_text(s)
p=target/'manifest.json'
m=json.loads(p.read_text());m['name']='CloudFile Agent 2.2 Dev';m['permissions']=list(dict.fromkeys(m['permissions']+['scripting']));p.write_text(json.dumps(m,ensure_ascii=False,indent=2))
print(target)

shutil.copy2(root/'scripts/dev-pairing.js',target/'dev-pairing.js')
p=target/'popup.html'
p.write_text(p.read_text().replace('</body>','<script src="dev-pairing.js"></script></body>'))
