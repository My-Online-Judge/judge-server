# Case 1 never ends (CPU time limit); the others answer.
a, b = map(int, raw_input().split())
if (a, b) == (1, 2):
    while True:
        pass
print a + b
