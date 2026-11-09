#include <cstdio>
#include <cstdlib>
#include <iostream>

// 2x the limit (256 MiB) is the whole address space RLIMIT_AS allows: malloc fails, the
// unchecked store faults, and the memory in use stays small. That is a runtime error,
// not MLE: the hard cap works, and MLE is the 1x..2x window (mle.cpp).
int main()
{
    long long a, b;
    std::cin >> a >> b;
    const size_t n = size_t(256) << 20;
    volatile char *p = static_cast<volatile char *>(std::malloc(n)); // null
    for (size_t i = 0; i < n; i += 4096)
        p[i] = 1; // SIGSEGV
    std::printf("%lld\n", a + b);
    return 0;
}
