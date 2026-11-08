package sandbox

import (
	"syscall"
	"testing"
)

func TestParseRecord(t *testing.T) {
	got, ok := parseRecord([]byte("0 0 1500 2048 123456789\n"))
	want := record{status: 0, utimeUs: 1500, maxRSSKB: 2048, wallNs: 123456789}
	if !ok || got != want {
		t.Fatalf("parseRecord = %+v, %v; want %+v", got, ok, want)
	}
	got, ok = parseRecord([]byte("-9 0 0 0 0\n"))
	if !ok || got.err != -9 {
		t.Fatalf("parseRecord(error line) = %+v, %v", got, ok)
	}
	for _, bad := range []string{
		"", "\n", "0 0 1 2 3", "0 0 1 2\n", "0 0 1 2 3 4\n", "0 0 1 2 x\n", "0 0 1 2 3\n0 0 1 2 3\n", "9999999999999999999 0 0 0 0\n",
	} {
		if r, ok := parseRecord([]byte(bad)); ok {
			t.Errorf("parseRecord(%q) = %+v, want not ok", bad, r)
		}
	}
}

func TestRecordResult(t *testing.T) {
	const sigxcpu = 24
	tests := []struct {
		name string
		rec  record
		want Result
	}{
		{"clean exit", record{status: 0, utimeUs: 500, maxRSSKB: 1024, wallNs: 2_400_000},
			Result{CPUTimeMs: 1, RealTimeMs: 2, MemoryBytes: 1 << 20}},
		{"user time under half a millisecond rounds down", record{status: 0, utimeUs: 499}, Result{}},
		{"exit code", record{status: 3 << 8}, Result{ExitCode: 3}},
		{"killed by signal", record{status: int(syscall.SIGSEGV)}, Result{Signal: int(syscall.SIGSEGV)}},
		{"core dumped", record{status: int(syscall.SIGABRT) | 0x80}, Result{Signal: int(syscall.SIGABRT)}},
		{"cpu limit", record{status: sigxcpu, utimeUs: 2_000_000}, Result{Signal: sigxcpu, CPUTimeMs: 2000}},
		// A stop is not an end: the shim waits for the program to exit or be killed, so a stop
		// reaching the Go side is a shim bug and must not read as a clean exit.
		{"ptrace stop", record{status: int(syscall.SIGSEGV)<<8 | 0x7f}, Result{Error: -4}},
		{"continued", record{status: 0xffff}, Result{Error: -4}},
		{"setup failure carries only the code", record{err: -9, status: 255 << 8, utimeUs: 5000}, Result{Error: -9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.result(); got != tt.want {
				t.Fatalf("result = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRoundHalfUp(t *testing.T) {
	tests := []struct{ n, d, want int64 }{
		{0, 1000, 0}, {499, 1000, 0}, {500, 1000, 1}, {1499, 1000, 1}, {1500, 1000, 2},
		{499_999, 1_000_000, 0}, {500_000, 1_000_000, 1}, {2_500_000, 1_000_000, 3},
	}
	for _, tt := range tests {
		if got := roundHalfUp(tt.n, tt.d); got != tt.want {
			t.Errorf("roundHalfUp(%d, %d) = %d, want %d", tt.n, tt.d, got, tt.want)
		}
	}
}
