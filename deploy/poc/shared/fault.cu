// fault：故意非法访存，触发 cudaErrorIllegalAddress（规格 §3.2 P3 的 B 容器）。
//
// 用法：fault
// 输出：fault: <错误名>（预期 cudaErrorIllegalAddress）
// 退出码：5 得到 CUDA 错误（预期）；1 同步竟然成功（fault: no error）；4 建立上下文失败。
//
// 只依赖驱动（cudart 静态链接）。
#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
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

// 写进程地址空间的空页（0x10 起 128 字节），GPU 侧必然无映射。
__global__ void bad_write(int* p) { p[threadIdx.x] = 42; }

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    CK(cudaFree(0));  // 建立上下文
    printf("fault: launching illegal write\n");
    fflush(stdout);

    bad_write<<<1, 32>>>((int*)0x10);
    cudaError_t err = cudaGetLastError();
    if (err == cudaSuccess) err = cudaDeviceSynchronize();
    if (err == cudaSuccess) {
        printf("fault: no error\n");
        fflush(stdout);
        return 1;
    }
    printf("fault: %s\n", cudaGetErrorName(err));
    fflush(stdout);
    return 5;
}
