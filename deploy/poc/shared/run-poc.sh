#!/usr/bin/env bash
# 共享机 PoC 执行脚本（计划 10 Task 1～2，规格 §3.2）。在 GPU 机器上以 docker 组用户运行，不需要 root。
#
# 用法：run-poc.sh <step>
#   build  构建镜像 spark/poc-shared:1（本目录为构建上下文）
#   p1     P1  GPU 分配计入 cgroup：A --memory 32g 跑 alloc 48 1 --touch；B --memory 56g 同时 burn 60（邻居不受影响）
#   p1b    P1b 托管内存：同 p1，alloc 加 --managed
#   solo   P2 的基线：单容器 --memory 56g 跑 burn 60（独跑吞吐）
#   p2     P2  并发可用：两容器各 --memory 56g 同时 burn 60；约 15 s 时宿主机 nvidia-smi
#   p3     P3  故障隔离：A burn 120；约 20 s 后 B 跑 fault；宿主机 nvidia-smi 仍正常
#   p4     P4  公平性（记录）：两容器同时 burn 60；运行中顺带采集 P6（容器内 nvidia-smi / ps）
#   p5     P5  宿主机安全：两容器各 --memory 56g 同时 alloc 60 1 --touch --hold 20；宿主机每 2 s 记 MemAvailable
#   p6     P6  信息泄漏（记录）：同 p4 的负载，日志单独以 p6 命名
#   p7     P7  CPU 配额：A --cpus 5 跑 stress --cpu 20 --timeout 60；B 同时 burn 60；每 5 s docker stats
#   clean  删除 poc-a / poc-b 容器与 /tmp/poc
#
# 日志：/tmp/poc/<step>-<a|b>.log（容器输出 + exit= + inspect: OOMKilled / ExitCode），
#       采样日志 /tmp/poc/<step>-sampler.log、p2/p3/p4-nvsmi.log、p6-*.log、p7-stats.log。
# 程序的非零退出是预期结果之一（alloc 3、fault 5、OOM-kill 137），脚本只记录不中止。
# dmesg（OOM 记录、Xid、oops）需要 root，由协调人另行 `sudo dmesg` 采集。
#
# 环境变量：POC_IMAGE（默认 spark/poc-shared:1）、POC_NET（默认 spark0）、POC_LOGDIR（默认 /tmp/poc）。
set -eu

IMAGE="${POC_IMAGE:-spark/poc-shared:1}"
NET="${POC_NET:-spark0}"
LOGDIR="${POC_LOGDIR:-/tmp/poc}"
HERE="$(cd "$(dirname "$0")" && pwd)"

usage() {
    echo "usage: $0 {build|p1|p1b|solo|p2|p3|p4|p5|p6|p7|clean}" >&2
    exit 2
}

say() { echo "== $*"; }

rm_ctrs() { docker rm -f poc-a poc-b >/dev/null 2>&1 || true; }

on_signal() {
    say "interrupted, removing containers"
    rm_ctrs
    exit 130
}

preflight() {
    mkdir -p "$LOGDIR"
    if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "image $IMAGE not found; run: $0 build" >&2
        exit 1
    fi
    if ! docker network inspect "$NET" >/dev/null 2>&1; then
        echo "docker network $NET not found (normally created by spark-agent); set POC_NET or create it" >&2
        exit 1
    fi
    rm_ctrs
    trap on_signal INT TERM
}

# run_ctr <a|b> <step> <mem_gib> <prog> [args...]
# 前台运行一个容器（调用方需要并行时自己加 &）；退出后把 exit= 与 OOMKilled / ExitCode 写进日志再删除容器。
# 总是返回 0。
run_ctr() {
    local tag="$1" step="$2" mem="$3"
    shift 3
    local name="poc-$tag"
    local log="$LOGDIR/$step-$tag.log"
    local shm=$((mem / 2))
    local cmd=(docker run --name "$name" --gpus all --network "$NET"
        --memory "${mem}g" --memory-swap "${mem}g" --shm-size "${shm}g"
        --cpus 5 --pids-limit 8192 --oom-score-adj 500
        --cap-drop SYS_NICE --cap-add IPC_LOCK --cap-add SYS_PTRACE
        --ulimit memlock=-1:-1 "$IMAGE" "$@")
    echo "+ ${cmd[*]}" | tee "$log"
    echo "start_ts=$(date +%s)" >>"$log"
    local rc=0
    # 没有 pipefail：管道状态取 tee 的（恒 0），set -e 不会因程序非零退出而中止；程序退出码取 PIPESTATUS[0]。
    "${cmd[@]}" 2>&1 | tee -a "$log"
    rc="${PIPESTATUS[0]}"
    {
        echo "end_ts=$(date +%s)"
        echo "exit=$rc"
        echo "inspect: $(docker inspect -f 'OOMKilled={{.State.OOMKilled}} ExitCode={{.State.ExitCode}} Error={{.State.Error}}' "$name" 2>&1 || true)"
    } | tee -a "$log"
    docker rm "$name" >/dev/null 2>&1 || true
    return 0
}

# cg_dir <container>：输出容器 cgroup v2 目录（systemd 或 cgroupfs 驱动），找不到输出空。
cg_dir() {
    local id
    id="$(docker inspect -f '{{.Id}}' "$1" 2>/dev/null)" || return 0
    local d
    for d in "/sys/fs/cgroup/system.slice/docker-$id.scope" "/sys/fs/cgroup/docker/$id"; do
        if [ -d "$d" ]; then
            echo "$d"
            return 0
        fi
    done
    return 0
}

# sampler <interval_s> <logfile> <stopfile> <container>...
# 每 interval 秒一行：ts、宿主机 MemAvailable、每个容器的 memory.current / memory.peak / oom_kill 计数。
# 直到 stopfile 出现。
sampler() {
    local interval="$1" logf="$2" stopf="$3"
    shift 3
    : >"$logf"
    while [ ! -e "$stopf" ]; do
        local line c d cur peak oomk
        line="ts=$(date +%s) $(grep MemAvailable /proc/meminfo 2>/dev/null | tr -s ' ' || true)"
        for c in "$@"; do
            d="$(cg_dir "$c")"
            if [ -n "$d" ]; then
                cur="$(cat "$d/memory.current" 2>/dev/null || echo -1)"
                peak="$(cat "$d/memory.peak" 2>/dev/null || echo -1)"
                oomk="$(awk '$1=="oom_kill"{print $2}' "$d/memory.events" 2>/dev/null || echo -1)"
                line="$line | $c current=$cur peak=$peak oom_kill=${oomk:--1}"
            else
                line="$line | $c -"
            fi
        done
        echo "$line" >>"$logf"
        sleep "$interval"
    done
}

# stats_sampler <interval_s> <logfile> <stopfile>：docker stats 采样直到 stopfile 出现。
stats_sampler() {
    local interval="$1" logf="$2" stopf="$3"
    : >"$logf"
    while [ ! -e "$stopf" ]; do
        {
            echo "ts=$(date +%s)"
            docker stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' 2>&1 || true
        } >>"$logf"
        sleep "$interval"
    done
}

# burn_gaps <log>：统计 t= 行数、相邻 t 的最大差、最小 gflops（P3 判断中断用）。
burn_gaps() {
    awk '/^t=/ {
            split($1, a, "="); split($2, b, "=");
            t = a[2] + 0; g = b[2] + 0;
            if (n > 0 && t - prev > gap) gap = t - prev;
            if (n == 0 || g < min) min = g;
            prev = t; n++
         }
         END { printf "t_lines=%d max_t_gap=%d min_gflops=%.1f\n", n, gap, min }' "$1"
}

# summary <step>：打印该步各日志的关键行。
summary() {
    local step="$1" f
    say "summary $step"
    for f in "$LOGDIR/$step"-a.log "$LOGDIR/$step"-b.log; do
        [ -e "$f" ] || continue
        echo "-- $f"
        grep -E '^(reached total|cudaMalloc failed|cuda error|fault:|avg_gflops|exit=|inspect:)' "$f" || true
        grep -E '^allocated=' "$f" | tail -n 1 || true
        if grep -q '^t=' "$f"; then burn_gaps "$f"; fi
    done
    if [ -e "$LOGDIR/solo-a.log" ] && [ "$step" != solo ]; then
        echo "-- solo baseline: $(grep '^avg_gflops' "$LOGDIR/solo-a.log" || echo none)"
    fi
}

# 两容器同时 burn 60；约 15 s 时宿主机 nvidia-smi；probes=1 时同时采集 P6。
pair_burn() {
    local step="$1" probes="$2"
    run_ctr a "$step" 56 burn 60 &
    local pa=$!
    run_ctr b "$step" 56 burn 60 &
    local pb=$!
    sleep 15
    say "host nvidia-smi -> $LOGDIR/$step-nvsmi.log"
    nvidia-smi >"$LOGDIR/$step-nvsmi.log" 2>&1 || true
    if [ "$probes" = 1 ]; then
        local p6="$step"
        [ "$step" = p6 ] || p6="p6"
        say "P6 probes -> $LOGDIR/$p6-{nvsmi,ps,ps-count}.log"
        echo "+ docker exec poc-b nvidia-smi"
        docker exec poc-b nvidia-smi >"$LOGDIR/$p6-nvsmi.log" 2>&1 || true
        echo "+ docker exec poc-b ps aux"
        docker exec poc-b ps aux >"$LOGDIR/$p6-ps.log" 2>&1 || true
        echo "+ docker exec poc-b sh -c 'ps aux | wc -l'"
        docker exec poc-b sh -c 'ps aux | wc -l' >"$LOGDIR/$p6-ps-count.log" 2>&1 || true
        echo "+ docker exec poc-b nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv"
        docker exec poc-b nvidia-smi --query-compute-apps=pid,process_name,used_memory --format=csv \
            >>"$LOGDIR/$p6-nvsmi.log" 2>&1 || true
        cat "$LOGDIR/$p6-nvsmi.log" "$LOGDIR/$p6-ps.log" "$LOGDIR/$p6-ps-count.log"
    fi
    wait "$pa" || true
    wait "$pb" || true
}

step_p1() {
    local step="$1"
    shift
    local stopf="$LOGDIR/.stop-$step"
    rm -f "$stopf"
    say "sampler 1 s -> $LOGDIR/$step-sampler.log"
    sampler 1 "$LOGDIR/$step-sampler.log" "$stopf" poc-a poc-b &
    local psam=$!
    run_ctr b "$step" 56 burn 60 &
    local pb=$!
    sleep 3
    run_ctr a "$step" 32 alloc 48 1 --touch "$@" &
    local pa=$!
    wait "$pa" || true
    wait "$pb" || true
    touch "$stopf"
    wait "$psam" || true
    rm -f "$stopf"
    summary "$step"
    say "next: sudo dmesg | tail -50  (look for 'Memory cgroup out of memory')"
}

step_p3() {
    run_ctr a p3 56 burn 120 &
    local pa=$!
    sleep 20
    run_ctr b p3 56 fault
    sleep 2
    say "host nvidia-smi after fault -> $LOGDIR/p3-nvsmi.log"
    nvidia-smi >"$LOGDIR/p3-nvsmi.log" 2>&1 || true
    cat "$LOGDIR/p3-nvsmi.log"
    wait "$pa" || true
    summary p3
    say "next: sudo dmesg | tail -50  (look for Xid)"
}

step_p5() {
    local stopf="$LOGDIR/.stop-p5"
    rm -f "$stopf"
    say "sampler 2 s -> $LOGDIR/p5-sampler.log"
    sampler 2 "$LOGDIR/p5-sampler.log" "$stopf" poc-a poc-b &
    local psam=$!
    run_ctr a p5 56 alloc 60 1 --touch --hold 20 &
    local pa=$!
    run_ctr b p5 56 alloc 60 1 --touch --hold 20 &
    local pb=$!
    wait "$pa" || true
    wait "$pb" || true
    touch "$stopf"
    wait "$psam" || true
    rm -f "$stopf"
    summary p5
    say "min MemAvailable (kB): $(awk '{for (i = 1; i <= NF; i++) if ($i == "MemAvailable:") print $(i + 1)}' "$LOGDIR/p5-sampler.log" | sort -n | head -n 1)"
    say "next: sudo dmesg | tail -80  (OOM records, oops, Xid); systemctl status sshd"
}

step_p7() {
    local stopf="$LOGDIR/.stop-p7"
    rm -f "$stopf"
    say "docker stats every 5 s -> $LOGDIR/p7-stats.log"
    stats_sampler 5 "$LOGDIR/p7-stats.log" "$stopf" &
    local psam=$!
    run_ctr a p7 56 stress --cpu 20 --timeout 60 &
    local pa=$!
    run_ctr b p7 56 burn 60 &
    local pb=$!
    wait "$pa" || true
    wait "$pb" || true
    touch "$stopf"
    wait "$psam" || true
    rm -f "$stopf"
    cat "$LOGDIR/p7-stats.log"
    summary p7
}

[ $# -eq 1 ] || usage
step="$1"

case "$step" in
build)
    echo "+ docker build -t $IMAGE $HERE"
    docker build -t "$IMAGE" "$HERE"
    ;;
p1)
    preflight
    step_p1 p1
    ;;
p1b)
    preflight
    step_p1 p1b --managed
    ;;
solo)
    preflight
    run_ctr a solo 56 burn 60
    summary solo
    ;;
p2)
    preflight
    pair_burn p2 0
    summary p2
    ;;
p3)
    preflight
    step_p3
    ;;
p4)
    preflight
    pair_burn p4 1
    summary p4
    ;;
p5)
    preflight
    step_p5
    ;;
p6)
    preflight
    pair_burn p6 1
    summary p6
    ;;
p7)
    preflight
    step_p7
    ;;
clean)
    echo "+ docker rm -f poc-a poc-b; rm -rf $LOGDIR"
    rm_ctrs
    rm -rf "$LOGDIR"
    ;;
*)
    usage
    ;;
esac
