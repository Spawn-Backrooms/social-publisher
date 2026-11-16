#!/usr/bin/env python3
"""Create only a local guest filesystem; the guest kernel enforces its UIDs."""
import argparse,gzip,os,shutil,stat,subprocess
from pathlib import Path
parser=argparse.ArgumentParser(description='Build a minimal native PostgreSQL guest initramfs without host mounts or UID changes')
parser.add_argument('--postgres-root',type=Path,required=True,help='Unmodified PostgreSQL directory with bin/, lib/, share/')
parser.add_argument('--busybox',type=Path,required=True,help='Static BusyBox executable')
parser.add_argument('--output-dir',type=Path,required=True)
args=parser.parse_args()
base=args.output_dir.resolve()
base.mkdir(parents=True,exist_ok=True)
root=base/'guestroot'
if root.exists():shutil.rmtree(root)
root.mkdir(exist_ok=True)
for directory in ['bin','sbin','usr/bin','usr/sbin','lib/x86_64-linux-gnu','lib64','etc','proc','sys','dev','dev/shm','tmp','run','pgdata','pgsocket','opt']:
 (root/directory).mkdir(parents=True,exist_ok=True)
shutil.copy2(args.busybox,root/'bin/busybox')
for applet in subprocess.check_output([str(args.busybox),'--list'],text=True).splitlines():
 p=root/'bin'/applet
 if not p.exists():p.symlink_to('busybox')
pg=args.postgres_root.resolve()
shutil.copytree(pg,root/'opt/postgresql',dirs_exist_ok=True,symlinks=True)
# The embedded package carries its private libraries. Supply its unmodified
# host C/C++ runtime dependencies in the guest filesystem.
libs=set()
for program in ['postgres','initdb','pg_ctl']:
 out=subprocess.check_output(['ldd',str(pg/'bin'/program)],text=True)
 for line in out.splitlines():
  fields=line.split()
  if '=>' in fields:
   path=fields[fields.index('=>')+1]
   if path.startswith('/lib/') or path.startswith('/usr/lib/'):libs.add(path)
  elif fields and fields[0].startswith('/lib'):libs.add(fields[0])
for name in libs:
 source=Path(name);destination=root/name.lstrip('/')
 destination.parent.mkdir(parents=True,exist_ok=True)
 shutil.copy2(source.resolve(),destination)
(root/'etc/passwd').write_text('root:x:0:0:root:/root:/bin/sh\npostgres:x:1000:1000:PostgreSQL:/pgdata:/bin/sh\n')
(root/'etc/group').write_text('root:x:0:\npostgres:x:1000:\n')
(root/'etc/nsswitch.conf').write_text('passwd: files\ngroup: files\nhosts: files\n')
(root/'etc/hosts').write_text('127.0.0.1 localhost\n10.0.2.15 native-postgres\n')
(root/'etc/hostname').write_text('native-postgres-guest\n')
init='''#!/bin/sh
export PATH=/bin:/opt/postgresql/bin
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/shm /run /tmp /pgdata /pgsocket
mount -t tmpfs -o mode=1777 tmpfs /dev/shm
chmod 1777 /tmp
hostname native-postgres-guest
ip link set lo up
ip link set eth0 up
ip addr add 10.0.2.15/24 dev eth0
ip route add default via 10.0.2.2
chown postgres:postgres /pgdata /pgsocket
chmod 700 /pgdata
printf 'GUEST_KERNEL: '; uname -a
su -s /bin/sh postgres -c 'printf "GUEST_POSTGRES_ID: "; id; /opt/postgresql/bin/initdb -D /pgdata -A trust --username=publisher --encoding=UTF8 --locale=C' || exec /bin/sh
cat >> /pgdata/postgresql.conf <<'EOF'
listen_addresses = '0.0.0.0'
port = 5432
unix_socket_directories = '/pgsocket'
max_connections = 100
shared_buffers = '128MB'
fsync = on
synchronous_commit = on
full_page_writes = on
jit = off
log_min_messages = warning
EOF
echo 'host all all 10.0.2.2/32 trust' >> /pgdata/pg_hba.conf
chown postgres:postgres /pgdata/postgresql.conf /pgdata/pg_hba.conf
su -s /bin/sh postgres -c '/opt/postgresql/bin/pg_ctl -D /pgdata -l /pgdata/postgres.log -w start' || { cat /pgdata/postgres.log; exec /bin/sh; }
printf '\\nNATIVE_POSTGRES_STARTED\\n'
while :; do sleep 10; done
'''
(root/'init').write_text(init);(root/'init').chmod(0o755)
# Build a newc initramfs. Guest ownership is represented in the archive; no host
# UID remapping, privileged mounts, or host chown to unavailable UIDs is used.
out=base/'native-postgres-initramfs.cpio.gz'
with gzip.open(out,'wb',compresslevel=3) as f:
 def entry(name,mode,data=b'',major=0,minor=0,ino=1):
  names=(name+'\0').encode();fields=[ino,mode,0,0,1,0,len(data),0,0,major,minor,len(names),0]
  hdr=b'070701'+b''.join(f'{x:08x}'.encode() for x in fields)
  f.write(hdr);f.write(names);f.write(b'\0'*((-len(hdr)-len(names))%4));f.write(data);f.write(b'\0'*((-len(data))%4))
 for i,p in enumerate(sorted(root.rglob('*')),1):
  st=p.lstat();name=p.relative_to(root).as_posix()
  if stat.S_ISDIR(st.st_mode):entry(name,stat.S_IFDIR|stat.S_IMODE(st.st_mode),ino=i)
  elif stat.S_ISLNK(st.st_mode):entry(name,stat.S_IFLNK|0o777,os.readlink(p).encode(),ino=i)
  elif stat.S_ISREG(st.st_mode):entry(name,stat.S_IFREG|stat.S_IMODE(st.st_mode),p.read_bytes(),ino=i)
  else:raise RuntimeError(name)
 entry('dev/console',stat.S_IFCHR|0o600,major=5,minor=1,ino=i+1)
 entry('dev/null',stat.S_IFCHR|0o666,major=1,minor=3,ino=i+2)
 entry('TRAILER!!!',0,ino=0)
print(out,out.stat().st_size)
