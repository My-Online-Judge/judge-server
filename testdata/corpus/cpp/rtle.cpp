#include <iostream>
#include <linux/futex.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

// Sleeps 5 s without using the CPU, so only the real-time limit (3 x the CPU limit = 3 s)
// can stop it. sleep() would not do: nanosleep is not in the c_cpp allow-list and the
// program would die with SIGSYS. A futex wait with a timeout is allowed.
int main()
{
    long long a, b;
    std::cin >> a >> b;
    int word = 0;
    timespec ts = {5, 0};
    syscall(SYS_futex, &word, FUTEX_WAIT, 0, &ts, nullptr, 0);
    std::cout << a + b << "\n";
    return 0;
}
