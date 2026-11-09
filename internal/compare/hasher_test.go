package compare

import (
	"bytes"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// hashInPieces writes b to a new Hasher in pieces of size n (the last one shorter) and
// returns its hex digest.
func hashInPieces(b []byte, n int) string {
	h := NewHasher()
	for len(b) > 0 {
		k := min(n, len(b))
		if w, err := h.Write(b[:k]); w != k || err != nil {
			panic(fmt.Sprintf("Write = %d, %v; want %d, nil", w, err, k))
		}
		b = b[k:]
	}
	return h.Hex()
}

// want is what the whole-buffer functions say, the definition the Hasher must match.
func want(b []byte) string { return MD5Hex(Normalize(b)) }

// The chunk sizes of the spec: one byte at a time, a prime that cuts every pair somewhere,
// and a page.
var chunkSizes = []int{1, 7, 4096}

func TestHasherMatchesNormalizeOnRandomInput(t *testing.T) {
	// Biased alphabets, so that CR, LF and the other whitespace bytes are common and every
	// boundary case (CR before LF, CR before CR, whitespace before and after other bytes)
	// comes up often, next to bytes >= 0x80.
	alphabets := []string{
		"ab",
		"a \r\n",
		"a \t\r\n\x0b\x0c",
		"x\r\n\r\n  ",
		"\r\n",
		" \r",
		"a\xc3\xa9\xff\x80 \r\n\t",
	}
	rng := rand.New(rand.NewSource(20261108))
	for _, alpha := range alphabets {
		for iter := 0; iter < 400; iter++ {
			b := make([]byte, rng.Intn(120))
			for i := range b {
				b[i] = alpha[rng.Intn(len(alpha))]
			}
			exp := want(b)
			for _, n := range chunkSizes {
				if got := hashInPieces(b, n); got != exp {
					t.Fatalf("alphabet %q, chunks of %d, input %q: got %s, want %s", alpha, n, b, got, exp)
				}
			}
			// pieces of random sizes, empty ones included
			h := NewHasher()
			for rest := b; ; {
				k := min(rng.Intn(9), len(rest))
				h.Write(rest[:k])
				rest = rest[k:]
				if len(rest) == 0 {
					break
				}
			}
			if got := h.Hex(); got != exp {
				t.Fatalf("alphabet %q, random pieces, input %q: got %s, want %s", alpha, b, got, exp)
			}
		}
	}
}

func TestHasherMatchesNormalizeOnLargeRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	b := make([]byte, 1<<20+13)
	alpha := "abc  \r\n\t"
	for i := range b {
		b[i] = alpha[rng.Intn(len(alpha))]
	}
	exp := want(b)
	for _, n := range chunkSizes {
		if got := hashInPieces(b, n); got != exp {
			t.Errorf("chunks of %d: got %s, want %s", n, got, exp)
		}
	}
}

// A CR and the LF after it in different pieces: the CR is still dropped; a CR that the next piece
// does not follow with an LF stays.
func TestHasherCRLFSplitAcrossPieces(t *testing.T) {
	tests := []struct {
		name   string
		pieces []string
	}{
		{"CR | LF", []string{"a\r", "\nb"}},
		{"CR | LF at the very end", []string{"a\r", "\n"}},
		{"CR | CR LF", []string{"a\r", "\r\nb"}},
		{"CR | other", []string{"a\r", "b"}},
		{"CR | nothing", []string{"a\r", ""}},
		{"CR | empty | LF", []string{"a\r", "", "\nb"}},
		{"CR alone, then CR LF in one piece", []string{"a", "\r", "\r\n", "b"}},
		{"many CRLF, one byte each", []string{"1", "\r", "\n", "\r", "\n", "2", "\r", "\n"}},
		{"CR | LF | CR | LF between words", []string{"x\r", "\n", "\r", "\ny"}},
		{"leading CR | LF", []string{"\r", "\nz"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHasher()
			for _, p := range tt.pieces {
				h.Write([]byte(p))
			}
			whole := []byte(strings.Join(tt.pieces, ""))
			if got, exp := h.Hex(), want(whole); got != exp {
				t.Errorf("pieces %q: got %s, want %s (Normalize(%q) = %q)", tt.pieces, got, exp, whole, Normalize(whole))
			}
		})
	}
}

// Trailing whitespace is dropped wherever the pieces cut it, and whitespace that turns out not to be
// trailing is kept in full, in order.
func TestHasherTrailingWhitespaceSplitAcrossPieces(t *testing.T) {
	tests := []struct {
		name   string
		pieces []string
	}{
		{"all trailing, one byte at a time", []string{"a", "b", " ", "\n", "\t", " ", "\r", "\n"}},
		{"inner whitespace then more text", []string{"a", " ", " ", "\n", "b"}},
		{"whitespace spans three pieces then text", []string{"ab", "  ", "\r\n", " ", "cd"}},
		{"text, whitespace, text, whitespace", []string{"ab ", " c", "d\n\n"}},
		{"only whitespace", []string{" ", "\n", "\r", "\n", "\x0b", "\x0c", "\t"}},
		{"whitespace, then a byte >= 0x80", []string{"a", "  ", "\xe3\x80\x80"}},
		{"U+3000 is not whitespace", []string{"a\xe3\x80", "\x80"}},
		{"nothing at all", nil},
		{"empty pieces", []string{"", "", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHasher()
			for _, p := range tt.pieces {
				h.Write([]byte(p))
			}
			whole := []byte(strings.Join(tt.pieces, ""))
			if got, exp := h.Hex(), want(whole); got != exp {
				t.Errorf("pieces %q: got %s, want %s", tt.pieces, got, exp)
			}
		})
	}
}

// A whitespace run longer than the Hasher keeps in memory: it is dropped when it is trailing and
// kept, byte for byte, when something follows it; and what the Hasher holds stays small.
func TestHasherWhitespaceRunLongerThanTheBuffer(t *testing.T) {
	long := func(n int) []byte { // a mix, so that a lost or reordered byte changes the digest
		b := make([]byte, n)
		for i := range b {
			b[i] = " \t\n\x0b\x0c"[i%5]
		}
		return b
	}
	tests := []struct {
		name string
		in   []byte
	}{
		{"only whitespace", long(3 * heldMax)},
		{"trailing run after text", append([]byte("word"), long(3*heldMax+1)...)},
		{"run between two words", append(append([]byte("ab"), long(2*heldMax+5)...), "cd"...)},
		{"run, text, run, text", bytes.Join([][]byte{long(heldMax + 1), []byte("x"), long(heldMax), []byte("y"), long(heldMax + 7)}, nil)},
		{"exactly the buffer", append([]byte("a"), long(heldMax)...)},
		{"one byte over the buffer, then text", append(append([]byte("a"), long(heldMax+1)...), "b"...)},
		{"long CRLF run", append(append([]byte("a"), bytes.Repeat([]byte("\r\n"), heldMax)...), "b"...)},
		{"long CRLF run at the end", append([]byte("a"), bytes.Repeat([]byte("\r\n"), heldMax)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exp := want(tt.in)
			for _, n := range []int{1, 7, 4096, 1 << 20} {
				h := NewHasher()
				for rest := tt.in; len(rest) > 0; {
					k := min(n, len(rest))
					h.Write(rest[:k])
					rest = rest[k:]
					if len(h.held) > heldMax {
						t.Fatalf("chunks of %d: %d bytes held, want at most %d", n, len(h.held), heldMax)
					}
				}
				if got := h.Hex(); got != exp {
					t.Errorf("chunks of %d: got %s, want %s", n, got, exp)
				}
			}
		})
	}
}

// Hex does not disturb the stream: calling it between writes, and again, changes nothing.
func TestHasherHexIsRepeatableAndNonDestructive(t *testing.T) {
	b := []byte("one  \r\ntwo \r\n\r\n three \n")
	h := NewHasher()
	for i := 0; i < len(b); i++ {
		h.Write(b[i : i+1])
		prefix := want(b[:i+1])
		if got := h.Hex(); got != prefix {
			t.Fatalf("after %d bytes: got %s, want %s", i+1, got, prefix)
		}
		if again := h.Hex(); again != h.Hex() {
			t.Fatalf("Hex changed between two calls")
		}
	}
	if got := h.Hex(); got != want(b) {
		t.Errorf("final: got %s, want %s", got, want(b))
	}
}

// The empty stream, the digests of the bundle's info file (stripped_output_md5 of "1\n" is the md5 of "1") and
// a Hasher used as an io.Writer.
func TestHasherKnownDigestsAndIOWriter(t *testing.T) {
	if got, exp := NewHasher().Hex(), "d41d8cd98f00b204e9800998ecf8427e"; got != exp {
		t.Errorf("empty stream = %s, want %s (md5 of nothing)", got, exp)
	}
	const one = "c4ca4238a0b923820dcc509a6f75849b" // md5("1")
	for _, tt := range []struct {
		name, in string
		match    bool
	}{
		{"exact", "1", true},
		{"trailing newline", "1\n", true},
		{"CRLF and trailing blanks", "1 \r\n", true},
		{"different output", "2\n", false},
		{"leading space matters", " 1", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHasher()
			if _, err := io.Copy(h, strings.NewReader(tt.in)); err != nil {
				t.Fatal(err)
			}
			if got := h.Hex(); (got == one) != tt.match {
				t.Errorf("Hex = %s, matches %s: %v, want %v", got, one, got == one, tt.match)
			}
		})
	}
}

// Memory: whatever the stream, the Hasher holds at most heldMax bytes of it, even a stream that is
// nothing but whitespace.
func TestHasherHoldsABoundedAmount(t *testing.T) {
	h := NewHasher()
	block := bytes.Repeat([]byte(" \n"), 1<<19) // 1 MiB of whitespace per write
	for i := 0; i < 8; i++ {
		h.Write(block)
		if len(h.held) > heldMax {
			t.Fatalf("%d bytes held after %d MiB of whitespace", len(h.held), i+1)
		}
	}
	if got, exp := h.Hex(), MD5Hex(nil); got != exp {
		t.Errorf("a stream of whitespace = %s, want the md5 of nothing %s", got, exp)
	}
}
