#include <cstdio>
#include <iostream>

// Correct answer with Windows line endings: accepted (the comparison drops CR of CRLF).
int main()
{
    long long a, b;
    std::cin >> a >> b;
    std::printf("%lld\r\n", a + b);
    return 0;
}
