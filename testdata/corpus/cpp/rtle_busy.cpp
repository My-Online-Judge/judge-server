#include <ctime>
#include <iostream>
#include <linux/futex.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

// Burns about 300 ms of CPU, then waits 5 s without using it, so only the real-time limit
// (3 x the CPU limit = 3 s) can stop it, with CPU time already spent: the verdict is real TLE
// and the run must still report that CPU time (corpus.json "min_cpu_ms"). A loop that never
// stops cannot be real TLE under the 3x rule: RLIMIT_CPU ends it first, as CPU TLE (the `tle`
// programs). The clock is read once per 65536 iterations so the time is user time, not syscalls.
// The wait is a futex with a timeout, as in rtle.cpp (nanosleep is outside the c_cpp allow-list).
int main()
{
    long long a, b;
    std::cin >> a >> b;
    for (volatile unsigned long i = 0;; i = i + 1)
        if ((i & 0xFFFF) == 0 && clock() >= CLOCKS_PER_SEC * 3 / 10)
            break;
    int word = 0;
    timespec ts = {5, 0};
    syscall(SYS_futex, &word, FUTEX_WAIT, 0, &ts, nullptr, 0);
    std::cout << a + b << "\n";
    return 0;
}
