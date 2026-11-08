#include <cstdio>
#include <cstdlib>
#include <iostream>

// The corpus limit is 128 MiB and the sandbox sets RLIMIT_AS to twice that (256 MiB).
// 192 MiB fits the address space but not the limit: memory > limit, so MLE.
// (Asking for 256 MiB, 2x the limit, is refused by RLIMIT_AS first: see mle2x.cpp.)
// The pages are touched through a volatile pointer: with a plain memset and a read back,
// the compiler folds the whole allocation away.
int main()
{
    long long a, b;
    std::cin >> a >> b;
    const size_t n = size_t(192) << 20;
    volatile char *p = static_cast<volatile char *>(std::malloc(n));
    if (!p)
        return 3;
    for (size_t i = 0; i < n; i += 4096)
        p[i] = 1;
    std::printf("%lld\n", a + b);
    return 0;
}
