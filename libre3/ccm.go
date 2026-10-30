package libre3

import (
	"crypto/aes"
	"crypto/subtle"
	"errors"
)

// The sensor's tag length for both the Phase 5/6 exchange and the data plane.
const tagLen = 4

var errAuth = errors.New("ccm: message authentication failed")

// ccmSeal is AES-128-CCM (NIST SP 800-38C) without associated data, as
// RoundWhiteDiscKit's AESCCM uses it: a 7-byte nonce for Phase 5/6, a 13-byte
// one for the data plane. It returns ciphertext || tag.
func ccmSeal(key, nonce, plaintext []byte) []byte {
	mac := ccmMAC(key, nonce, plaintext)
	out, s0 := ccmCTR(key, nonce, plaintext)
	for i := 0; i < tagLen; i++ {
		out = append(out, mac[i]^s0[i])
	}
	return out
}

func ccmOpen(key, nonce, sealed []byte) ([]byte, error) {
	if len(sealed) < tagLen {
		return nil, errAuth
	}
	ct, tag := sealed[:len(sealed)-tagLen], sealed[len(sealed)-tagLen:]
	pt, s0 := ccmCTR(key, nonce, ct)
	mac := ccmMAC(key, nonce, pt)
	want := make([]byte, tagLen)
	for i := range want {
		want[i] = mac[i] ^ s0[i]
	}
	if subtle.ConstantTimeCompare(want, tag) != 1 {
		return nil, errAuth
	}
	return pt, nil
}

func ccmMAC(key, nonce, pt []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	l := 15 - len(nonce)
	b := make([]byte, 16)
	b[0] = byte((tagLen-2)/2)<<3 | byte(l-1)
	copy(b[1:], nonce)
	q := len(pt)
	for i := 0; i < l; i++ {
		b[15-i] = byte(q)
		q >>= 8
	}
	b = append(b, pt...)
	if r := len(b) % 16; r != 0 {
		b = append(b, make([]byte, 16-r)...)
	}
	y := make([]byte, 16)
	for i := 0; i < len(b); i += 16 {
		for j := 0; j < 16; j++ {
			y[j] ^= b[i+j]
		}
		block.Encrypt(y, y)
	}
	return y
}

// ccmCTR returns the input XORed with the keystream from counter 1, and S0.
func ccmCTR(key, nonce, in []byte) ([]byte, []byte) {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	l := 15 - len(nonce)
	a := make([]byte, 16)
	a[0] = byte(l - 1)
	copy(a[1:], nonce)
	counter := func(n uint64) []byte {
		c := append([]byte(nil), a...)
		for i := 0; i < l; i++ {
			c[15-i] = byte(n)
			n >>= 8
		}
		block.Encrypt(c, c)
		return c
	}
	s0 := counter(0)
	out := make([]byte, 0, len(in)+tagLen)
	for i := 0; i < len(in); i += 16 {
		s := counter(uint64(i/16 + 1))
		for j := 0; j < 16 && i+j < len(in); j++ {
			out = append(out, in[i+j]^s[j])
		}
	}
	return out, s0
}
