// burn：在 GPU 上持续做 N×N 单精度矩阵乘，每秒打印吞吐（规格 §3.2 P2 / P3 / P4 / P7）。
//
// 用法：burn <sec> [n]
//   sec  运行秒数（墙钟）
//   n    矩阵边长，默认 4096，必须是 32 的倍数（每次 launch 2*n^3 FLOP）
//
// 输出（每行 flush）：
//   burn sec=<sec> n=<n>
//   t=<s> gflops=<x> kernel_ms=<avg>   每秒一行；t 为整数秒（墙钟）。GPU 停顿时 t 会跳号，
//                                      连续两行 t 差 > 5 即视为中断（P3）。
//   avg_gflops=<x>                     结束时：总 FLOP / 总墙钟
// 退出码：0 成功；2 参数错误；4 CUDA 错误（打印 cuda error: <名>）。
//
// 不用 cuBLAS：自带共享内存分块 SGEMM，cudart 静态链接，只依赖驱动。
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#define CK(expr)                                                             \
    do {                                                                     \
        cudaError_t ck_err_ = (expr);                                        \
        if (ck_err_ != cudaSuccess) {                                        \
            printf("cuda error: %s (%s at %s:%d)\n",                         \
                   cudaGetErrorName(ck_err_), #expr, __FILE__, __LINE__);    \
            fflush(stdout);                                                  \
            exit(4);                                                         \
        }                                                                    \
    } while (0)

#define TS 32   // 分块边长
#define TY 8    // 每块 32×8 线程
#define RPT 4   // 每线程算 4 行（TS / TY）

// C = A * B，行主序，n 为 TS 的倍数。block (TS, TY)，grid (n/TS, n/TS)。
__global__ void sgemm_tiled(const float* __restrict__ A,
                            const float* __restrict__ B, float* __restrict__ C,
                            int n) {
    __shared__ float As[TS][TS];
    __shared__ float Bs[TS][TS];
    const int tx = threadIdx.x;
    const int ty = threadIdx.y;
    const int row0 = blockIdx.y * TS;
    const int col = blockIdx.x * TS + tx;
    float acc[RPT];
    for (int r = 0; r < RPT; ++r) acc[r] = 0.0f;

    for (int k0 = 0; k0 < n; k0 += TS) {
        for (int r = 0; r < RPT; ++r) {
            const int lr = ty + r * TY;
            As[lr][tx] = A[(size_t)(row0 + lr) * n + (k0 + tx)];
            Bs[lr][tx] = B[(size_t)(k0 + lr) * n + col];
        }
        __syncthreads();
#pragma unroll
        for (int k = 0; k < TS; ++k) {
            const float b = Bs[k][tx];
#pragma unroll
            for (int r = 0; r < RPT; ++r) acc[r] += As[ty + r * TY][k] * b;
        }
        __syncthreads();
    }
    for (int r = 0; r < RPT; ++r)
        C[(size_t)(row0 + ty + r * TY) * n + col] = acc[r];
}

__global__ void fill(float* p, size_t count, float scale) {
    size_t i = (size_t)blockIdx.x * blockDim.x + threadIdx.x;
    size_t stride = (size_t)gridDim.x * blockDim.x;
    for (; i < count; i += stride) p[i] = scale * (float)((i % 97) + 1) / 97.0f;
}

static double now_s(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

static void usage(void) {
    fprintf(stderr, "usage: burn <sec> [n]   (n multiple of 32, default 4096)\n");
    exit(2);
}

static long parse_pos(const char* s) {
    char* end = NULL;
    long v = strtol(s, &end, 10);
    if (end == s || *end != '\0' || v <= 0) usage();
    return v;
}

int main(int argc, char** argv) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (argc < 2 || argc > 3) usage();
    const long sec = parse_pos(argv[1]);
    const long nl = argc == 3 ? parse_pos(argv[2]) : 4096;
    if (nl % TS != 0 || nl > 32768) usage();
    const int n = (int)nl;
    printf("burn sec=%ld n=%d\n", sec, n);

    const size_t count = (size_t)n * (size_t)n;
    float *A = NULL, *B = NULL, *C = NULL;
    CK(cudaMalloc((void**)&A, count * sizeof(float)));
    CK(cudaMalloc((void**)&B, count * sizeof(float)));
    CK(cudaMalloc((void**)&C, count * sizeof(float)));
    fill<<<1024, 256>>>(A, count, 1.0f);
    CK(cudaGetLastError());
    fill<<<1024, 256>>>(B, count, 0.5f);
    CK(cudaGetLastError());
    CK(cudaDeviceSynchronize());

    cudaEvent_t ev0, ev1;
    CK(cudaEventCreate(&ev0));
    CK(cudaEventCreate(&ev1));

    const dim3 block(TS, TY);
    const dim3 grid(n / TS, n / TS);
    const double flop_per_launch = 2.0 * (double)n * (double)n * (double)n;

    const double t0 = now_s();
    double win_start = 0.0;       // 当前统计窗口起点（相对 t0）
    double next_report = 1.0;
    double win_flop = 0.0, total_flop = 0.0;
    double win_kernel_ms = 0.0;
    long win_launches = 0;
    double elapsed = 0.0;

    while (elapsed < (double)sec) {
        CK(cudaEventRecord(ev0, 0));
        sgemm_tiled<<<grid, block>>>(A, B, C, n);
        CK(cudaGetLastError());
        CK(cudaEventRecord(ev1, 0));
        CK(cudaEventSynchronize(ev1));
        float ms = 0.0f;
        CK(cudaEventElapsedTime(&ms, ev0, ev1));

        win_flop += flop_per_launch;
        total_flop += flop_per_launch;
        win_kernel_ms += ms;
        ++win_launches;

        elapsed = now_s() - t0;
        if (elapsed >= next_report) {
            const double dt = elapsed - win_start;
            printf("t=%ld gflops=%.1f kernel_ms=%.2f\n", (long)elapsed,
                   dt > 0 ? win_flop / dt / 1e9 : 0.0,
                   win_launches > 0 ? win_kernel_ms / win_launches : 0.0);
            win_start = elapsed;
            win_flop = 0.0;
            win_kernel_ms = 0.0;
            win_launches = 0;
            next_report = (double)((long)elapsed + 1);
        }
    }

    // 抽查一个元素，防止结果完全无用；数值不判定，只打印。
    float probe = 0.0f;
    CK(cudaMemcpy(&probe, C, sizeof(float), cudaMemcpyDeviceToHost));
    printf("avg_gflops=%.1f elapsed=%.1f c00=%g\n",
           elapsed > 0 ? total_flop / elapsed / 1e9 : 0.0, elapsed, probe);
    fflush(stdout);

    CK(cudaEventDestroy(ev0));
    CK(cudaEventDestroy(ev1));
    CK(cudaFree(A));
    CK(cudaFree(B));
    CK(cudaFree(C));
    return 0;
}
