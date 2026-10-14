#!/usr/bin/env python3
"""Register only the isolated development Host, for the current user."""
import argparse, json, os, pathlib, re, sys
p=argparse.ArgumentParser()
p.add_argument('--extension-id',required=True)
p.add_argument('--bin-dir',required=True,type=pathlib.Path)
a=p.parse_args()
if not re.fullmatch('[a-p]{32}',a.extension_id):p.error('invalid Chrome extension ID')
name='com.cloudfile.agent22.dev'
binary=(a.bin_dir/('cloudfile-native-host.exe' if sys.platform=='win32' else 'cloudfile-native-host')).resolve()
desktop=binary.with_name('cloudfile-desktop.exe' if sys.platform=='win32' else 'cloudfile-desktop')
if not binary.is_file() or not desktop.is_file():p.error('build both executables first')
if sys.platform=='darwin':directory=pathlib.Path.home()/'Library/Application Support/Google/Chrome/NativeMessagingHosts'
elif sys.platform=='win32':directory=pathlib.Path(os.environ['LOCALAPPDATA'])/'CloudFileAgent22'/'native-host'
else:p.error('Agent 2.2 registration supports Windows/macOS only')
directory.mkdir(parents=True,exist_ok=True)
manifest=directory/(name+'.json')
manifest.write_text(json.dumps({'name':name,'description':'CloudFile Agent 2.2 isolated development host','path':str(binary),'type':'stdio','allowed_origins':['chrome-extension://'+a.extension_id+'/']},indent=2),encoding='utf-8')
if sys.platform=='win32':
 import winreg
 with winreg.CreateKey(winreg.HKEY_CURRENT_USER,'Software\\Google\\Chrome\\NativeMessagingHosts\\'+name) as key:winreg.SetValueEx(key,'',0,winreg.REG_SZ,str(manifest))
print(manifest)
