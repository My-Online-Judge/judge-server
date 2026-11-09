#include <stdio.h>

int main(void)
{
    long a, b;
    if (scanf("%ld %ld", &a, &b) != 2)
        return 1;
    printf("%ld\n", a + b + 1);
    return 0;
}
