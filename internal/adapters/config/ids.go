package config

import (
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// crockfordAlphabet is the Crockford base32 alphabet domain.identityRegex
// accepts: no I, L, O, or U.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// identityRandomBytes is 128 bits, which base32-encodes to the 26
// characters every domain identity requires.
const identityRandomBytes = 16

// NewUserID returns a fresh durable user identity. Password administration
// needs one whenever it creates a secure account, because the password file
// records a stable identity reference rather than inventing one later.
func NewUserID(random ports.Random) (domain.UserID, error) {
	s, err := newIdentity(random, domain.PrefixUser)
	if err != nil {
		return domain.UserID{}, err
	}
	return domain.ParseUserID(s)
}

// NewAccountID returns a fresh provider-account identity for configuration.
func NewAccountID(random ports.Random) (domain.AccountID, error) {
	s, err := newIdentity(random, domain.PrefixAccount)
	if err != nil {
		return domain.AccountID{}, err
	}
	return domain.ParseAccountID(s)
}

// NewRouteID returns a fresh model-route identity for configuration.
func NewRouteID(random ports.Random) (domain.RouteID, error) {
	s, err := newIdentity(random, domain.PrefixRoute)
	if err != nil {
		return domain.RouteID{}, err
	}
	return domain.ParseRouteID(s)
}

func newIdentity(random ports.Random, prefix string) (string, error) {
	if random == nil {
		return "", fmt.Errorf("config: generating a %s identity requires a randomness source", prefix)
	}
	raw, err := random.Bytes(identityRandomBytes)
	if err != nil {
		return "", fmt.Errorf("config: generate %s identity: %w", prefix, err)
	}
	if len(raw) != identityRandomBytes {
		return "", fmt.Errorf("config: randomness source returned %d bytes, want %d", len(raw), identityRandomBytes)
	}
	return prefix + "_" + encodeCrockford(raw), nil
}

// encodeCrockford encodes bytes with the Crockford base32 alphabet: a
// straight MSB-first bit stream, the final group padded with zero bits.
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
