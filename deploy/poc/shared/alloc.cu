// alloc：逐步分配 GPU 内存，观察容器 cgroup memory.max 是否约束 CUDA 分配（规格 §3.2 P1 / P1b / P5）。
//
// 用法：alloc <total_gib> <step_gib> [--managed] [--touch] [--hold <sec>]
//   --managed  用 cudaMallocManaged 代替 cudaMalloc
//   --touch    每块分配后 cudaMemset 全量写入并 cudaDeviceSynchronize
//   --hold     达到 total 后再保持 <sec> 秒（默认 0）
//
// 输出（每行 flush）：
//   start free=<bytes> total=<bytes> cg_current=<bytes|-1> ts=<epoch>
//   allocated=<GiB> free=<bytes> total=<bytes> cg_current=<bytes|-1> ts=<epoch>   （每步一行）
//   reached total                                                                   （成功）
// 退出码：0 成功；2 参数错误；3 cudaMalloc / cudaMallocManaged 失败；4 其它 CUDA 错误。
//
// 只依赖 CUDA 驱动（cudart 静态链接），可拷进只有 CUDA base 镜像的容器里运行。
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

static long long cg_current(void) {
    FILE* f = fopen("/sys/fs/cgroup/memory.current", "r");
    if (f == NULL) return -1;
    long long v = -1;
    if (fscanf(f, "%lld", &v) != 1) v = -1;
    fclose(f);
    return v;
}

// fatal=1：cudaMemGetInfo 失败按 CUDA 错误退出 4；fatal=0：只打印（用于失败路径，保住退出码 3）。
static void report(const char* prefix, int fatal) {
    size_t freeb = 0, totalb = 0;
    cudaError_t err = cudaMemGetInfo(&freeb, &totalb);
    if (err != cudaSuccess) {
        printf("cuda error: %s (cudaMemGetInfo, %s)\n", cudaGetErrorName(err), prefix);
        fflush(stdout);
        if (fatal) exit(4);
        return;
    }
    printf("%s free=%zu total=%zu cg_current=%lld ts=%lld\n", prefix, freeb,
           totalb, cg_current(), (long long)time(NULL));
    fflush(stdout);
}

static void usage(void) {
    fprintf(stderr,
            "usage: alloc <total_gib> <step_gib> [--managed] [--touch] "
            "[--hold <sec>]\n");
    exit(2);
}

static long parse_pos(const char* s) {
    char* end = NULL;
    long v = strtol(s, &end, 10);
    if (end == s || *end != '\0' || v < 0) usage();
    return v;
}

int main(int argc, char** argv) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (argc < 3) usage();
    long total = parse_pos(argv[1]);
    long step = parse_pos(argv[2]);
    if (total <= 0 || step <= 0) usage();
    int managed = 0, touch = 0;
    long hold = 0;
    for (int i = 3; i < argc; ++i) {
        if (strcmp(argv[i], "--managed") == 0) {
            managed = 1;
        } else if (strcmp(argv[i], "--touch") == 0) {
            touch = 1;
        } else if (strcmp(argv[i], "--hold") == 0 && i + 1 < argc) {
            hold = parse_pos(argv[++i]);
        } else {
            usage();
        }
    }
    printf("alloc total=%ld step=%ld managed=%d touch=%d hold=%ld\n", total,
           step, managed, touch, hold);

    CK(cudaFree(0));  // 建立上下文
    report("start", 1);

    long nblocks = (total + step - 1) / step;
    void** blocks = (void**)calloc((size_t)nblocks, sizeof(void*));
    if (blocks == NULL) {
        printf("host calloc failed\n");
        return 4;
    }

    long allocated = 0;
    for (long i = 0; allocated < total; ++i) {
        long n = total - allocated < step ? total - allocated : step;
        size_t bytes = (size_t)n << 30;
        void* p = NULL;
        cudaError_t err = managed
                              ? cudaMallocManaged(&p, bytes, cudaMemAttachGlobal)
                              : cudaMalloc(&p, bytes);
        if (err != cudaSuccess) {
            printf("cudaMalloc failed at %ld: %s (allocated=%ld GiB, next step %ld GiB)\n",
                   allocated, cudaGetErrorName(err), allocated, n);
            fflush(stdout);
            report("at_failure", 0);
            return 3;
        }
        blocks[i] = p;
        if (touch) {
            CK(cudaMemset(p, 0xA5, bytes));
            CK(cudaDeviceSynchronize());
        }
        allocated += n;
        char prefix[64];
        snprintf(prefix, sizeof(prefix), "allocated=%ld", allocated);
        report(prefix, 1);
    }

    printf("reached total\n");
    fflush(stdout);
    if (hold > 0) {
        printf("holding %ld s\n", hold);
        fflush(stdout);
        sleep((unsigned int)hold);
        report("after_hold", 1);
    }
    return 0;
}
