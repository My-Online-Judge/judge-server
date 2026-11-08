// Crashes: the case is a system error with error code -11.
int main()
{
    volatile int *p = 0;
    *p = 1;
    return 0;
}
