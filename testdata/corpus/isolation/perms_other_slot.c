#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* Tries to read what belongs to ANOTHER job slot (spec 5.1a). Every line of the input is a path: the
 * test makes the second slot's job directory tree, a marker file inside it, the other slot's tmp dir
 * and passes their paths here (a program does not get another job's paths in its argv, but it can
 * find them in /proc/<pid>/cmdline). The program prints one line per path: "<path> OK <first bytes>"
 * when it could open it, "<path> <errno name>" when not. Another slot's files must be EACCES (or
 * ENOENT), the own slot's marker, given as one of the lines, OK. Runs under c_cpp: open() read-only. */
int main(void)
{
    char path[4096];

    while (fgets(path, sizeof path, stdin)) {
        char buf[64];
        ssize_t n;
        int fd;

        path[strcspn(path, "\n")] = 0;
        fd = open(path, O_RDONLY);
        if (fd < 0) {
            printf("%s %s\n", path, errno == EACCES ? "EACCES" : errno == ENOENT ? "ENOENT" : "OTHER");
            continue;
        }
        n = read(fd, buf, sizeof buf - 1);
        buf[n > 0 ? n : 0] = 0;
        printf("%s OK %s\n", path, buf);
        close(fd);
    }
    return 0;
}
