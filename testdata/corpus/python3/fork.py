import os

# fork is in the general filter's kill list: SIGSYS, a runtime error.
a, b = map(int, input().split())
pid = os.fork()
if pid == 0:
    os._exit(0)
os.waitpid(pid, 0)
print(a + b)
