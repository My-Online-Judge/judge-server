#include <iostream>

// Case 1 never ends (CPU time limit); the others answer. One busy case, not three,
// so the verdict does not depend on how the CPUs are shared.
int main()
{
    long long a, b;
    volatile unsigned long spin = 0;
    std::cin >> a >> b;
    if (a == 1 && b == 2)
        for (;;)
            spin = spin + 1;
    std::cout << a + b << "\n";
    return 0;
}
