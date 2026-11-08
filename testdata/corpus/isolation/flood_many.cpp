#include <cstdio>
#include <cstring>

// Prints 1 GiB; RLIMIT_FSIZE ends it with SIGXFSZ at the output file cap (16 MiB), so every case of the
// job leaves a capped output file behind until it is judged. Judged on a bundle of twenty cases the
// files would take 20 x 16 MiB if they stayed; the engine deletes each one as soon as its case is
// judged, so the job never holds more than the pool size of them. The test samples the size of /work
// while a second job judges next to this one.
int main()
{
    static char block[1 << 20];
    std::memset(block, 'x', sizeof block);
    for (int i = 0; i < 1024; i++)
        std::fwrite(block, 1, sizeof block, stdout);
    return 0;
}
