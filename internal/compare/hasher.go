package compare

import (
	// md5 is the checksum format of the bundle's info file, not a security control.
	"crypto/md5"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
)

// heldMax is how many bytes of a trailing whitespace run a Hasher keeps in memory.
// A longer run is not buffered: it is hashed into a second md5 state that only
// becomes the real one if a non-whitespace byte follows.
const heldMax = 64 << 10

// Hasher computes MD5Hex(Normalize(b)) of a stream b written to it in pieces of any
// size, without ever holding the stream. Normalize drops the CR of every CRLF pair
// and then the trailing ASCII whitespace; neither can be decided from a prefix: a CR
// is dropped only if an LF follows it, and a whitespace byte only if nothing but
// whitespace follows. So the Hasher keeps back a CR that ends a piece (pendCR) and
// the run of whitespace it has just seen (held, or, when the run outgrows heldMax,
// hashed into long), and lets them through when the next byte shows they are not
// trailing. What it keeps back is bounded by heldMax, whatever the stream.
//
// The zero value is not usable: build one with NewHasher. A Hasher is not safe for
// concurrent use.
type Hasher struct {
	h      hash.Hash // md5 of the normalised bytes up to the last byte that is not whitespace
	pendCR bool      // the last byte written was a CR: dropped if an LF comes next, whitespace otherwise
	held   []byte    // the whitespace run after the last other byte, at most heldMax of it
	long   hash.Hash // when the run outgrew held: h's state plus the whole run so far; else nil
}

// NewHasher returns an empty Hasher.
func NewHasher() *Hasher { return &Hasher{h: md5.New()} }

// Write adds p to the stream. It always consumes all of p and never returns an
// error, so a Hasher can be the target of io.Copy and io.MultiWriter.
func (h *Hasher) Write(p []byte) (int, error) {
	n := len(p)
	if h.pendCR && n > 0 {
		h.pendCR = false
		if p[0] != '\n' { // the CR was not half of a CRLF pair: it is a whitespace byte of the stream
			h.hold([]byte{'\r'})
		}
	}
	for i := 0; i < len(p); {
		if !isASCIISpace(p[i]) { // a run of other bytes: it releases the whitespace held before it
			j := i + 1
			for j < len(p) && !isASCIISpace(p[j]) {
				j++
			}
			h.commit(p[i:j])
			i = j
			continue
		}
		// A run of whitespace is held back, minus the CR of each CRLF pair.
		j := i
		for j < len(p) && isASCIISpace(p[j]) {
			if p[j] == '\r' {
				if j+1 == len(p) { // the last byte of this piece: the next piece says what it is
					h.hold(p[i:j])
					h.pendCR = true
					return n, nil
				}
				if p[j+1] == '\n' {
					h.hold(p[i:j])
					i, j = j+1, j+1 // skip the CR; the LF starts the rest of the run
					continue
				}
			}
			j++
		}
		h.hold(p[i:j])
		i = j
	}
	return n, nil
}

// commit adds b, which holds no whitespace, to the hash, after the whitespace held
// back before it.
func (h *Hasher) commit(b []byte) {
	if h.long != nil {
		h.h, h.long = h.long, nil // the run was already in this state's input
	} else if len(h.held) > 0 {
		h.h.Write(h.held)
		h.held = h.held[:0]
	}
	h.h.Write(b)
}

// hold keeps b, whitespace of the stream, back until the next byte shows whether it
// is trailing.
func (h *Hasher) hold(b []byte) {
	switch {
	case len(b) == 0:
	case h.long != nil:
		h.long.Write(b)
	case len(h.held)+len(b) <= heldMax:
		h.held = append(h.held, b...)
	default: // the run is too long to keep: hash it into a copy of the committed state
		h.long = h.cloneState()
		h.long.Write(h.held)
		h.long.Write(b)
		h.held = h.held[:0]
	}
}

// cloneState returns a new md5 with the state of h.h.
func (h *Hasher) cloneState() hash.Hash {
	state, err := h.h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		panic(fmt.Sprintf("compare: save the md5 state: %v", err)) // md5's digest never fails to marshal
	}
	c := md5.New()
	if err := c.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
		panic(fmt.Sprintf("compare: restore the md5 state: %v", err))
	}
	return c
}

// Hex returns the lowercase hex md5 of the normalised stream written so far: what
// MD5Hex(Normalize(all the bytes)) returns for them. The whitespace and the CR held
// back are not part of it, as they are trailing until something else is written. The
// Hasher stays usable.
func (h *Hasher) Hex() string {
	return hex.EncodeToString(h.h.Sum(nil))
}
