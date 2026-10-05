package main

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"j0s.at/vibeshell/internal/adapters/config"
	"j0s.at/vibeshell/internal/domain"
)

// crockfordAlphabet matches domain.identityRegex: no I, L, O, or U.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// publicIdentity derives the durable user identity for a public-mode
// username. PLAN 4.1 makes a public username intentionally reclaimable: the
// same name must always resume the same world, so the mapping is a pure,
// stable function of the name rather than random.
//
// The mapping hashes the username to 128 bits and encodes them as the
// Crockford base32 value every domain identity requires. It is deliberately
// separate from the secure-mode namespace (PLAN 4.1): a secure user's
// identity comes from the password file, never from this derivation.
func publicIdentity(username string) (domain.UserID, error) {
	sum := sha256.Sum256([]byte("vibeshell:public:" + username))
	value := encodeCrockford(sum[:16])
	id, err := domain.ParseUserID(domain.PrefixUser + "_" + value)
	if err != nil {
		return domain.UserID{}, fmt.Errorf("derive public identity for %q: %w", username, err)
	}
	return id, nil
}

// encodeCrockford encodes bytes MSB-first with the Crockford base32
// alphabet, padding the final group with zero bits. It matches the encoding
// domain identities use.
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

// secureIdentity resolves the durable identity recorded for a secure-mode
// username in the password file. An unknown or malformed entry is an error:
// secure mode never falls back to a derived identity, which would let an
// unauthenticated name reach secure history (PLAN 4.1).
func secureIdentity(store *config.PasswordStore, username string) (domain.UserID, error) {
	if store == nil {
		return domain.UserID{}, fmt.Errorf("secure identity lookup requires the password store")
	}
	file := store.Pin()
	entry, ok := file.Lookup(username)
	if !ok {
		return domain.UserID{}, fmt.Errorf("user %q is not in the password file", username)
	}
	id, err := domain.ParseUserID(entry.Identity)
	if err != nil {
		return domain.UserID{}, fmt.Errorf("password file entry for %q has a malformed identity: %w", username, err)
	}
	return id, nil
}

// homePath is the visible home directory of a principal: /home/<username>
// for a named user and /root for the simulated root identity (PLAN 5.2).
func homePath(username string) domain.ValidPath {
	if username == "root" {
		return domain.MustParsePath("/root")
	}
	return domain.MustParsePath("/home").Join(username)
}
