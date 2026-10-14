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
for name in ['cloudfile-native-host','cloudfile-desktop']:
 name+= '.exe' if sys.platform=='win32' else ''
 shutil.copy2(root/'target/release'/name,binary_dir/name)
if sys.platform=='darwin':
 subprocess.run(['codesign','--force','--deep','--sign','-',str(app)],check=True)
shutil.copy2(root/'scripts/register-host.py',stage/'register-host.py')
shutil.copy2(root/'README.md',stage/'README.md')
archive=stage.with_suffix('.zip')
with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
 for path in sorted(stage.rglob('*')):
  if path.is_file():z.write(path,path.relative_to(stage))
print(f'{archive}: {archive.stat().st_size/1024/1024:.1f} MiB')
