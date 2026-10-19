#!/usr/bin/env python3
"""Build a development ZIP. Does not install or replace any Host."""
import pathlib, plistlib, shutil, subprocess, sys, tomllib, zipfile
root=pathlib.Path(__file__).resolve().parents[1]
subprocess.run(['cargo','build','--workspace','--release','--locked'],cwd=root,check=True)
target=subprocess.check_output(['rustc','-vV'],cwd=root,text=True).split('host: ')[1].splitlines()[0]
stage=root/'dist'/('cloudfile-agent22-'+target)
if stage.exists():shutil.rmtree(stage)
stage.mkdir(parents=True,exist_ok=True)
binary_dir=stage
if sys.platform=='darwin':
 app=stage/'CloudFile Agent 2.2 Dev.app'
 binary_dir=app/'Contents/MacOS'
 binary_dir.mkdir(parents=True)
 version=tomllib.loads((root/'Cargo.toml').read_text())['workspace']['package']['version']
 with (app/'Contents/Info.plist').open('wb') as f:
  plistlib.dump({'CFBundleName':'CloudFile Agent 2.2 Dev','CFBundleDisplayName':'CloudFile Agent 2.2 Dev',
   'CFBundleIdentifier':'com.cloudfile.agent22.dev','CFBundleExecutable':'cloudfile-desktop',
   'CFBundlePackageType':'APPL','CFBundleShortVersionString':version.split('-')[0],
   'CFBundleVersion':version.split('-')[0],'CloudFileAgentBuildVersion':version,
   'NSHighResolutionCapable':True},f)
for name in ['cloudfile-native-host','cloudfile-desktop','cloudfile-install']:
 name+= '.exe' if sys.platform=='win32' else ''
 shutil.copy2(root/'target/release'/name,binary_dir/name)
if sys.platform=='darwin':
 subprocess.run(['codesign','--force','--deep','--sign','-',str(app)],check=True)
# Registration, autostart, upgrade and revert are all performed by the shipped
# binaries, so the archive contains no scripting runtime for the user to run.
shutil.copy2(root/'README.md',stage/'README.md')
# 空白机器上的入口是 cloudfile-install，而**这个开发包里它没有内置公钥**
# （CI 不持有发布私钥）。不写清楚的话，双击它只会得到「没有可用的发布公钥」，
# 看起来像程序坏了。正式引导体由 make-agent22-release.ps1 注入公钥后发出。
(stage/'INSTALL.txt').write_text(
 'CloudFile Agent 2.2（开发包）\n\n'
 '入口\n'
 '  cloudfile-native-host --install --source <origin> --public-key <base64url>\n'
 '                                     一键安装/修复（扩展 ID 是固定的，不用传）\n'
 '  cloudfile-install --source <origin> --public-key <base64url>\n'
 '                                     空白机器上的引导体（本开发包**未内置公钥**）\n\n'
 '说明\n'
 '  这个开发包里 cloudfile-install 没有内置公钥：它只在打包时注入，CI 不持有发布私钥。\n'
 '  所以要用它就必须显式给 --public-key（base64url 的未压缩 P-256 点，65 字节 -> 87 字符），\n'
 '  取值可以问发版方，或用 cloudfile-release\\tools\\dev-release\\sign-agent22-release.py pubkey <私钥> 导出。\n'
 '  没有钉住的公钥时引导体会**拒绝开始安装** —— 这是刻意的：信任锚不能来自通道自己。\n\n'
 '  更新通道要求规范 origin（http://host/dir 或 https://host/dir，不带尾斜杠），\n'
 '  Agent 自身的升级必须有签名清单；明文 http 是默认支持的，不需要额外开关。\n',
 encoding='utf-8')
archive=stage.with_suffix('.zip')
with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
 for path in sorted(stage.rglob('*')):
  if path.is_file():z.write(path,path.relative_to(stage))
print(f'{archive}: {archive.stat().st_size/1024/1024:.1f} MiB')
