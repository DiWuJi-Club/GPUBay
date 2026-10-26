#!/usr/bin/env python3
"""NCCL all-reduce bandwidth test for Spark multi-node acceptance (plan 13 CP-1 onwards).

Run on every cluster member (login shell, or `. /.spark/cluster.env` first):

  torchrun --nnodes "$NNODES" --nproc-per-node 1 --node-rank "$NODE_RANK" \
    --master-addr "$MASTER_ADDR" --master-port "$MASTER_PORT" \
    nccl_allreduce.py --size-mb 256 --iters 50

Set NCCL_DEBUG=INFO to see which transport NCCL picked (NET/Socket on spc0, or NET/IB ... RoCE).
Prints algbw / busbw per round (same definitions as nccl-tests: busbw = algbw * 2 * (n - 1) / n).
A large --iters keeps it running long enough to restart spark-agent on the hosts mid-test.
"""
import argparse
import os
import time

import torch
import torch.distributed as dist


def main() -> None:
    p = argparse.ArgumentParser()
    p.add_argument("--size-mb", type=float, default=256, help="tensor size per all-reduce, MiB")
    p.add_argument("--iters", type=int, default=20, help="timed all-reduce rounds")
    p.add_argument("--warmup", type=int, default=5)
    p.add_argument("--report-every", type=int, default=10, help="print a line every N rounds")
    a = p.parse_args()

    dist.init_process_group("nccl")
    rank, world = dist.get_rank(), dist.get_world_size()
    local = int(os.environ.get("LOCAL_RANK", "0"))
    torch.cuda.set_device(local)
    n = int(a.size_mb * 1024 * 1024) // 4
    x = torch.ones(n, dtype=torch.float32, device="cuda")
    nbytes = n * 4
    if rank == 0:
        print(f"world={world} size={nbytes / 2**20:.1f}MiB iters={a.iters} "
              f"ifname={os.environ.get('NCCL_SOCKET_IFNAME', '-')} ib_disable={os.environ.get('NCCL_IB_DISABLE', '-')}",
              flush=True)

    for _ in range(a.warmup):
        dist.all_reduce(x)
    torch.cuda.synchronize()

    factor = 2 * (world - 1) / world
    total, t0 = 0.0, time.perf_counter()
    for i in range(1, a.iters + 1):
        s = time.perf_counter()
        dist.all_reduce(x)
        torch.cuda.synchronize()
        dt = time.perf_counter() - s
        total += dt
        if rank == 0 and (i % a.report_every == 0 or i == a.iters):
            algbw = nbytes / dt / 1e9
            print(f"iter {i}: {dt * 1e3:.2f} ms algbw {algbw:.2f} GB/s busbw {algbw * factor:.2f} GB/s", flush=True)

    # correctness: one all-reduce of ones must give world on every element
    x.fill_(1.0)
    dist.all_reduce(x)
    ok = bool(torch.all(x == world).item())
    avg = total / a.iters
    if rank == 0:
        algbw = nbytes / avg / 1e9
        print(f"avg {avg * 1e3:.2f} ms algbw {algbw:.2f} GB/s busbw {algbw * factor:.2f} GB/s "
              f"wall {time.perf_counter() - t0:.1f}s correct={ok}", flush=True)
    dist.destroy_process_group()
    if not ok:
        raise SystemExit("all-reduce result mismatch")


if __name__ == "__main__":
    main()
