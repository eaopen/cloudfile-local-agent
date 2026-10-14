#!/usr/bin/env python3
"""Build a reproducible development ZIP. Does not install or replace any Host."""
import pathlib, shutil, subprocess, sys, zipfile
root=pathlib.Path(__file__).resolve().parents[1]
subprocess.run(['cargo','build','--workspace','--release','--locked'],cwd=root,check=True)
target=subprocess.check_output(['rustc','-vV'],cwd=root,text=True).split('host: ')[1].splitlines()[0]
stage=root/'dist'/('cloudfile-agent22-'+target)
stage.mkdir(parents=True,exist_ok=True)
for name in ['cloudfile-native-host','cloudfile-desktop']:
 name+= '.exe' if sys.platform=='win32' else ''
 shutil.copy2(root/'target/release'/name,stage/name)
shutil.copy2(root/'scripts/register-host.py',stage/'register-host.py')
shutil.copy2(root/'README.md',stage/'README.md')
archive=stage.with_suffix('.zip')
with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
 for path in sorted(stage.iterdir()):z.write(path,path.name)
print(f'{archive}: {archive.stat().st_size/1024/1024:.1f} MiB')
