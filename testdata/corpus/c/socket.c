#include <stdio.h>
#include <sys/socket.h>

/* socket() is not in the c_cpp allow-list: the program dies with SIGSYS. */
int main(void)
{
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    printf("%d\n", fd);
    return 0;
}
