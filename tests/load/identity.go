package load

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
)

// crockfordAlphabet matches domain.identityRegex: no I, L, O, or U.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// publicIdentity derives the durable user identity for a public-mode username.
// It mirrors the composition root's public-mode mapping so a harness session
// lands in exactly the world a real public session would use: the same name
// always resumes the same world.
func publicIdentity(username string) (domain.UserID, error) {
	sum := sha256.Sum256([]byte("vibeshell:public:" + username))
	id, err := domain.ParseUserID(domain.PrefixUser + "_" + encodeCrockford(sum[:16]))
	if err != nil {
		return domain.UserID{}, fmt.Errorf("derive public identity for %q: %w", username, err)
	}
	return id, nil
}

// encodeCrockford encodes bytes MSB-first with the Crockford base32 alphabet,
// padding the final group with zero bits. It matches the encoding domain
// identities use.
func encodeCrockford(data []byte) string {
	var out strings.Builder
	out.Grow((len(data)*8 + 4) / 5)
	var acc uint64
	var bits uint
	for _, b := range data {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(crockfordAlphabet[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		out.WriteByte(crockfordAlphabet[(acc<<(5-bits))&0x1f])
	}
	return out.String()
}
