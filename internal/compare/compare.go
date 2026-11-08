// Package compare turns a program's output into the bytes the judge compares
// and hashes. It mirrors TestCaseBundleBuilder.comparedOutput in
// oj-problem-service, which produced the expected stripped_output_md5.
package compare

import (
	// md5 is the checksum format of the bundle's info file, not a security control.
	"crypto/md5"
	"encoding/hex"
)

// Normalize returns b without the CR of every CRLF pair (a CR not followed by
// LF stays) and without trailing bytes in {0x20, 0x09, 0x0A, 0x0D, 0x0B, 0x0C}.
// It works on bytes, so a non-UTF-8 output is handled exactly. b is not
// modified.
func Normalize(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			continue
		}
		out = append(out, b[i])
	}
	n := len(out)
	for n > 0 && isASCIISpace(out[n-1]) {
		n--
	}
	return out[:n]
}

func isASCIISpace(c byte) bool {
	return c == 0x20 || c == 0x09 || c == 0x0A || c == 0x0D || c == 0x0B || c == 0x0C
}

// MD5Hex returns the lowercase hex md5 of b.
func MD5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// Matches normalises actual and reports its md5 and whether that md5 equals
// expectedStrippedMD5 (lowercase hex, as stored in the bundle's info file).
func Matches(actual []byte, expectedStrippedMD5 string) (md5Hex string, ok bool) {
	md5Hex = MD5Hex(Normalize(actual))
	return md5Hex, md5Hex == expectedStrippedMD5
}
