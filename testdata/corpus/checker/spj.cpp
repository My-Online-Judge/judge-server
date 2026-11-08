#include <cstdio>

// Exit 0 when the contestant's number is the sum of the two in the input, 1 otherwise.
int main(int argc, char **argv)
{
    if (argc < 3)
        return 2;
    std::FILE *in = std::fopen(argv[1], "r");
    std::FILE *out = std::fopen(argv[2], "r");
    if (!in || !out)
        return 2;
    long long a = 0, b = 0, got = 0;
    if (std::fscanf(in, "%lld %lld", &a, &b) != 2)
        return 2;
    if (std::fscanf(out, "%lld", &got) != 1)
        return 1;
    return got == a + b ? 0 : 1;
}
