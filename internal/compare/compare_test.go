package compare

import (
	"bytes"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"crlf pairs become lf and the trailing one goes", []byte("a\r\nb\r\n"), []byte("a\nb")},
		{"trailing space, tab and newline go", []byte("x \t\n"), []byte("x")},
		{"a lone CR is stripped as trailing space", []byte("\r"), []byte("")},
		{"a lone CR inside is kept", []byte("a\rb"), []byte("a\rb")},
		{"CR before CRLF: only the CR of the pair is dropped", []byte("a\r\r\nb"), []byte("a\r\nb")},
		{"vertical tab and form feed are trailing space", []byte("a\x0b\x0c \n"), []byte("a")},
		{"leading and inner whitespace stay", []byte("  a  b\n"), []byte("  a  b")},
		{"bytes >= 0x80 are untouched", []byte("\xc3\xa9\xff\x80\n"), []byte("\xc3\xa9\xff\x80")},
		{"U+3000 (e3 80 80) is not stripped", []byte("a\xe3\x80\x80"), []byte("a\xe3\x80\x80")},
		{"empty stays empty", []byte(""), []byte("")},
		{"only whitespace becomes empty", []byte(" \t\r\n\x0b\x0c"), []byte("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]byte(nil), tt.in...)
			got := Normalize(in)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !bytes.Equal(in, tt.in) {
				t.Errorf("Normalize modified its input: %q, was %q", in, tt.in)
			}
		})
	}
}

func TestNormalizeNil(t *testing.T) {
	if got := Normalize(nil); len(got) != 0 {
		t.Errorf("Normalize(nil) = %q, want empty", got)
	}
}

func TestMD5Hex(t *testing.T) {
	// "1" is what the Java TestCaseBundleBuilder hashes for an output of "1\n".
	if got, want := MD5Hex([]byte("1")), "c4ca4238a0b923820dcc509a6f75849b"; got != want {
		t.Errorf("MD5Hex(1) = %s, want %s", got, want)
	}
	if got, want := MD5Hex(nil), "d41d8cd98f00b204e9800998ecf8427e"; got != want {
		t.Errorf("MD5Hex(nil) = %s, want %s", got, want)
	}
}

func TestMatches(t *testing.T) {
	const one = "c4ca4238a0b923820dcc509a6f75849b"
	tests := []struct {
		name   string
		actual string
		want   string
		wantOK bool
	}{
		{"exact", "1", one, true},
		{"trailing newline", "1\n", one, true},
		{"CRLF and trailing blanks", "1 \r\n", one, true},
		{"different output", "2\n", one, false},
		{"leading space matters", " 1", one, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md5, ok := Matches([]byte(tt.actual), tt.want)
			if ok != tt.wantOK {
				t.Errorf("Matches ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && md5 != tt.want {
				t.Errorf("Matches md5 = %s, want %s", md5, tt.want)
			}
			if !tt.wantOK && md5 == tt.want {
				t.Errorf("Matches md5 = %s, must differ from the expected one", md5)
			}
		})
	}
}
