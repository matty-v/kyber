#!/bin/bash
set -euo pipefail

: "${EFS_FILE_SYSTEM_ID:?required}"
: "${EFS_ACCESS_POINT_ID:?required}"
: "${EFS_MOUNT_TARGET_IP:?required}"
: "${EBS_VOLUME_ID:?required}"
: "${RUNTIME_IMAGE:?required}"

result_dir=/var/tmp/kyber-efs-phase1-results
mkdir -p "$result_dir" /mnt/phase1-efs /mnt/phase1-ebs
result="$result_dir/results.txt"
: > "$result"

log() {
    printf '%s\n' "$*" | tee -a "$result"
}

wait_for_bootstrap() {
    local deadline=$((SECONDS + 600))
    while [ ! -f /var/lib/kyber-phase1-ready ]; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            log "FAIL bootstrap timeout"
            exit 1
        fi
        sleep 5
    done
}

find_ebs_device() {
    local serial="${EBS_VOLUME_ID//-/}"
    lsblk -ndo NAME,SERIAL | awk -v serial="$serial" '$2 == serial { print "/dev/" $1; exit }'
}

mount_filesystems() {
    local device deadline=$((SECONDS + 180))
    while :; do
        device=$(find_ebs_device)
        [ -n "$device" ] && break
        if [ "$SECONDS" -ge "$deadline" ]; then
            log "FAIL EBS device discovery timeout"
            exit 1
        fi
        sleep 3
    done
    if ! blkid "$device" >/dev/null 2>&1; then
        mkfs.ext4 -F "$device"
    fi
    mountpoint -q /mnt/phase1-ebs || mount "$device" /mnt/phase1-ebs
    mountpoint -q /mnt/phase1-efs || mount -t efs \
        -o "tls,accesspoint=${EFS_ACCESS_POINT_ID},mounttargetip=${EFS_MOUNT_TARGET_IP}" \
        "${EFS_FILE_SYSTEM_ID}:/" /mnt/phase1-efs
}

record_semantics() {
    local label=$1 root=$2
    local probe="$root/semantics"
    rm -rf "$probe"
    mkdir -p "$probe"

    printf 'sentinel\n' > "$probe/source"
    ln "$probe/source" "$probe/hardlink"
    ln -s source "$probe/symlink"
    mv "$probe/source" "$probe/renamed"
    sync "$probe/renamed"

    if setfattr -n user.kyber.phase1 -v present "$probe/renamed" 2>/dev/null; then
        log "$label xattr=SUPPORTED"
    else
        log "$label xattr=UNSUPPORTED"
    fi
    log "$label hardlinks=$(stat -c %h "$probe/renamed")"
    log "$label symlink=$(readlink "$probe/symlink")"
    log "$label capacity_bytes=$(df --output=size "$root" | tail -1 | tr -d ' ')"
}

run_rootfs() {
    local label=$1 root=$2
    local target="$root/rootfs-case"
    rm -rf "$target"
    mkdir -p "$target"

    log "$label rootfs_seed=START"
    if /usr/bin/time -p -o "$result_dir/${label}-seed.time" \
        docker run --rm --entrypoint /usr/local/bin/kyber-rootfs \
        -v "$target:/persist" "$RUNTIME_IMAGE" prepare /persist/agentroot \
        >>"$result" 2>&1; then
        log "$label rootfs_seed=PASS"
    else
        log "$label rootfs_seed=FAIL"
    fi

    log "$label rootfs_second_boot=START"
    if /usr/bin/time -p -o "$result_dir/${label}-second.time" \
        docker run --rm --entrypoint /usr/local/bin/kyber-rootfs \
        -v "$target:/persist" "$RUNTIME_IMAGE" prepare /persist/agentroot \
        >>"$result" 2>&1; then
        log "$label rootfs_second_boot=PASS"
    else
        log "$label rootfs_second_boot=FAIL"
    fi

    cat "$result_dir/${label}-seed.time" "$result_dir/${label}-second.time" >> "$result"
}

run_git_workload() {
    local label=$1 root=$2
    local target="$root/git-case"
    rm -rf "$target"
    if /usr/bin/time -p -o "$result_dir/${label}-git.time" \
        git clone --quiet --depth=1 https://github.com/matty-v/kyber.git "$target" &&
        git -C "$target" status --short >/dev/null; then
        log "$label git_clone_status=PASS"
    else
        log "$label git_clone_status=FAIL"
    fi
    cat "$result_dir/${label}-git.time" >> "$result"
}

wait_for_bootstrap
mount_filesystems
docker pull "$RUNTIME_IMAGE" >/dev/null

for label in ebs efs; do
    root="/mnt/phase1-$label"
    record_semantics "$label" "$root"
    run_rootfs "$label" "$root"
    run_git_workload "$label" "$root"
done

log "qualification_complete=true"
cat "$result"
