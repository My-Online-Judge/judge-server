int main(void)
{
    volatile int *p = 0;
    *p = 1; /* SIGSEGV */
    return 0;
}
