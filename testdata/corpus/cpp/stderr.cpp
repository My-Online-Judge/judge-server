#include <iostream>

// Correct stdout plus noise on stderr: accepted (only stdout is compared).
int main()
{
    long long a, b;
    std::cin >> a >> b;
    std::cerr << "debug: a=" << a << " b=" << b << "\n";
    std::cout << a + b << "\n";
    return 0;
}
