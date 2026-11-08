#include <cstdio>
#include <cstring>

// Prints 1 GiB. RLIMIT_FSIZE (32 MiB) ends it with SIGXFSZ after the output file
// has reached the cap: runtime error, a truncated response output, no tmpfs left over.
int main()
{
    static char block[1 << 20];
    std::memset(block, 'x', sizeof block);
    for (int i = 0; i < 1024; i++)
        std::fwrite(block, 1, sizeof block, stdout);
    return 0;
}
