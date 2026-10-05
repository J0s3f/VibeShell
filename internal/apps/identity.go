package apps

import (
	"crypto/sha256"
	"errors"
	"math/big"

	"j0s.at/vibeshell/internal/domain"
	"j0s.at/vibeshell/internal/ports"
)

// Identity values are 128-bit random identifiers encoded as
// prefix + 26 Crockford base32 symbols (PLAN 4.1, domain identity
// encoding). IDs are minted through the Random port so tests inject
// a deterministic source; only the real composition root uses the
// host randomness adapter.
const identityByteLen = 16 // 128 bits

// crockfordAlphabet is the Crockford base32 alphabet: digits and
// letters excluding I, L, O, and U to avoid transcription errors.
// It matches the identity regex in the domain package.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// crockfordSymbols is the encoded length of a 128-bit value:
// ceil(128/5) = 26 symbols, the top symbol carrying 2 used bits.
const crockfordSymbols = 26

// newIdentity mints a prefixed identity from n random bytes.
func newIdentity(prefix string, random ports.Random) (string, error) {
	if random == nil {
		return "", errors.New("random source required to mint identity")
	}
	raw, err := random.Bytes(identityByteLen)
	if err != nil {
		return "", err
	}
	if len(raw) != identityByteLen {
		return "", errors.New("random source returned the wrong identity length")
	}
	return prefix + "_" + crockfordEncode(raw), nil
}

// NewAppID mints a new application ID.
func NewAppID(random ports.Random) (domain.AppID, error) {
	s, err := newIdentity(domain.PrefixApp, random)
	if err != nil {
		return domain.AppID{}, err
	}
	return domain.ParseAppID(s)
}

// NewAppVersionID mints a new application version ID.
func NewAppVersionID(random ports.Random) (domain.AppVersionID, error) {
	s, err := newIdentity(domain.PrefixAppVer, random)
	if err != nil {
		return domain.AppVersionID{}, err
	}
	return domain.ParseAppVersionID(s)
}

// crockfordEncode encodes data as Crockford base32, left-padded to
// crockfordSymbols symbols. The value is treated as a big-endian
// integer, so equal bytes always encode identically.
func crockfordEncode(data []byte) string {
	value := new(big.Int).SetBytes(data)
	base := big.NewInt(int64(len(crockfordAlphabet)))
	zero := big.NewInt(0)
	encoded := make([]byte, crockfordSymbols)
	for i := crockfordSymbols - 1; i >= 0; i-- {
		digit := new(big.Int)
		value.DivMod(value, base, digit)
		encoded[i] = crockfordAlphabet[digit.Int64()]
		if value.Cmp(zero) == 0 && i > 0 {
			// Remaining symbols are leading zeros.
			for j := i - 1; j >= 0; j-- {
				encoded[j] = crockfordAlphabet[0]
			}
			break
		}
	}
	return string(encoded)
}

// HashSource derives the immutable content hash of generated
// JavaScript source. The hash is a deterministic function of the
// exact source bytes: the same source always yields the same
// ContentID, so a stored hash proves the source was not altered.
// The production registry adapter verifies the hash against the
// content store; this local derivation keeps the contract
// testable without it.
func HashSource(source string) domain.ContentID {
	sum := sha256.Sum256([]byte(source))
	return mustParseContentID(crockfordEncode(sum[:identityByteLen]))
}

func mustParseContentID(encoded string) domain.ContentID {
	id, err := domain.ParseContentID(domain.PrefixContent + "_" + encoded)
	if err != nil {
		// Unreachable: the Crockford alphabet and length match the
		// domain identity format by construction.
		panic("apps: internal hash encoding is not a valid identity: " + err.Error())
	}
	return id
}
