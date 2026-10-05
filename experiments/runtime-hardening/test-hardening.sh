#!/bin/sh
# In-container attack-surface and environment inventory for the hardened runtime
# image. Run INSIDE the container through the host orchestrator:
#
#   'podman exec -i <container> sh' < test-hardening.sh
#
# This script only observes. It never prints PASS or FAIL: every verdict in the
# qualification lives in run-hardening-tests.ps1, where the command that produced
# the value is printed next to it. A previous revision of this file printed PASS
# lines for conditions it never asserted, which produced unreproducible receipts.

echo "### identity"
echo "uid=$(id -u) gid=$(id -g) user=$(id -un) groups=$(id -Gn)"
echo "uname=$(uname -srm)"
echo "pid1_cmdline=$(tr '\0' ' ' < /proc/1/cmdline)"

echo
echo "### kernel-enforced state (/proc/self/status)"
grep -E '^(NoNewPrivs|Seccomp|Seccomp_filters|CapInh|CapPrm|CapEff|CapBnd|CapAmb):' /proc/self/status

echo
echo "### cgroup limits (cgroup v2 unified)"
echo "cgroup_fs=$(stat -fc %T /sys/fs/cgroup)"
echo "pids.max=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo unavailable)"
echo "memory.max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo unavailable)"
echo "cpus.max=$(cat /sys/fs/cgroup/cpus.max 2>/dev/null || echo unavailable)"

echo
echo "### root mount options"
grep -E '^[a-z0-9]+ / ' /proc/mounts

echo
echo "### listening sockets (/proc/net/tcp,tcp6 LISTEN=0A)"
echo "tcp_listen=$(awk 'NR>1 && $4=="0A"' /proc/net/tcp 2>/dev/null | wc -l)"
echo "tcp6_listen=$(awk 'NR>1 && $4=="0A"' /proc/net/tcp6 2>/dev/null | wc -l)"

echo
echo "### present tooling"
for group in "compilers:cc gcc make cmake ld" \
             "package_managers:apt apt-get dpkg yum dnf rpm apk" \
             "vcs:git hg svn" \
             "shells:bash sh dash zsh fish ksh" \
             "interpreters:perl python python3 python2 lua ruby node deno" \
             "network:curl wget nc netcat socat openssl ssh sshd scp sftp rsync busybox" \
             "archives:tar gzip bzip2 xz unzip zip"; do
    label=${group%%:*}
    words=${group#*:}
    found=""
    for tool in $words; do
        if command -v "$tool" >/dev/null 2>&1; then
            found="$found $tool"
        fi
    done
    if [ -n "$found" ]; then
        echo "$label=PRESENT:$found"
    else
        echo "$label=absent"
    fi
done

echo
echo "### setuid binaries"
setuid_list=$(find / -perm -4000 -type f 2>/dev/null | tr '\n' ' ')
echo "setuid_count=$(printf '%s' "$setuid_list" | wc -w)"
echo "setuid_paths=${setuid_list:-<none>}"

echo
echo "### world-writable paths outside /tmp"
find / -xdev -type d -perm -0002 2>/dev/null | grep -v '^/tmp' | head -20 || true

echo
echo "### file capability xattrs"
command -v getcap >/dev/null 2>&1 && getcap -r / 2>/dev/null || echo "getcap=absent"