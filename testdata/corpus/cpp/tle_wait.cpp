#include <ctime>
#include <iostream>
#include <linux/futex.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

// Burns about 1.1 s of CPU (the limit is 1 s), then waits 5 s without using it. RLIMIT_CPU (2 s)
// never fires, so the real-time limit (3 s) kills it, with both limits passed: as on qduoj the
// verdict is CPU TLE, which needs the CPU time spent before that kill to be reported (rtle_busy.cpp
// is the same program with less CPU, real TLE). The clock is read once per 65536 iterations so the
// time is user time, not syscalls. The wait is a futex with a timeout (nanosleep is outside the
// c_cpp allow-list).
int main()
{
    long long a, b;
    std::cin >> a >> b;
    for (volatile unsigned long i = 0;; i = i + 1)
        if ((i & 0xFFFF) == 0 && clock() >= CLOCKS_PER_SEC * 11 / 10)
            break;
    int word = 0;
    timespec ts = {5, 0};
    syscall(SYS_futex, &word, FUTEX_WAIT, 0, &ts, nullptr, 0);
    std::cout << a + b << "\n";
    return 0;
}
