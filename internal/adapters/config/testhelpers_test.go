package config

import "crypto/rand"

// fixedRandom returns real random bytes; the deterministic identity shape
// is checked downstream (ID parse), only length matters here.
type fixedRandom struct{}

func (fixedRandom) Bytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func (fixedRandom) Intn(n int) int {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int(uint64(b[0])<<56|uint64(b[1])<<48|uint64(b[2])<<40|uint64(b[3])<<32|uint64(b[4])<<24|uint64(b[5])<<16|uint64(b[6])<<8|uint64(b[7])) % n
}
