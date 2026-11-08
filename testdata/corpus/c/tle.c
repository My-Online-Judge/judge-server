#include <stdio.h>

/* Case 1 never ends (CPU time limit); the others answer. One busy case, not three,
 * so the verdict does not depend on how the CPUs are shared. */
int main(void)
{
    long a, b;
    volatile unsigned long spin = 0;
    if (scanf("%ld %ld", &a, &b) != 2)
        return 1;
    if (a == 1 && b == 2)
        for (;;)
            spin++;
    printf("%ld\n", a + b);
    return 0;
}
