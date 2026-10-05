// Package auth prototypes VibeShell secure-mode password authentication
// (PLAN 4.2): PHC-style Argon2id encoded hashes with strictly bounded
// parameters, timing-safe verification with dummy work for unknown users, a
// versioned JSON password file, and bounded admission control for
// authentication attempts.
package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2 version literal accepted by the parser; 19 is the only version the
// Argon2id function has ever shipped in the PHC string format.
const argon2Version = 19

// DefaultHashBytes is the derived-key length produced by NewEncodedHash.
const DefaultHashBytes = 32

// Bounds enforced when reading or producing encoded hashes. The bounds keep a
// malformed or hostile password file from requesting unreasonable CPU or
// memory (PLAN 4.2); aggregate load is bounded separately by admission
// control.
const (
	MinMemoryKiB   = 8 << 10   // 8 MiB
	MaxMemoryKiB   = 512 << 10 // 512 MiB
	MinIterations  = 1
	MaxIterations  = 8
	MinParallelism = 1
	MaxParallelism = 16
	MinSaltBytes   = 8
	MaxSaltBytes   = 64
	MinHashBytes   = 16
	MaxHashBytes   = 64
)

var (
	// ErrMalformedHash reports an encoded hash that does not follow the
	// accepted Argon2id PHC syntax.
	ErrMalformedHash = errors.New("malformed argon2id hash")
	// ErrParamsOutOfRange reports parameters outside the accepted bounds.
	ErrParamsOutOfRange = errors.New("argon2id parameters out of range")
)

// Params are the Argon2id cost parameters of one encoded hash.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
}

// Validate reports whether every parameter lies inside the accepted bounds.
func (p Params) Validate() error {
	switch {
	case p.MemoryKiB < MinMemoryKiB || p.MemoryKiB > MaxMemoryKiB:
		return fmt.Errorf("%w: memory %d KiB outside [%d, %d]",
			ErrParamsOutOfRange, p.MemoryKiB, MinMemoryKiB, MaxMemoryKiB)
	case p.Iterations < MinIterations || p.Iterations > MaxIterations:
		return fmt.Errorf("%w: iterations %d outside [%d, %d]",
			ErrParamsOutOfRange, p.Iterations, MinIterations, MaxIterations)
	case p.Parallelism < MinParallelism || p.Parallelism > MaxParallelism:
		return fmt.Errorf("%w: parallelism %d outside [%d, %d]",
			ErrParamsOutOfRange, p.Parallelism, MinParallelism, MaxParallelism)
	}
	return nil
}

// EncodedHash is a parsed PHC-style Argon2id hash:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
//
// Salt and Hash hold raw bytes; String re-encodes them canonically.
type EncodedHash struct {
	Params Params
	Salt   []byte
	Hash   []byte
}

// NewEncodedHash derives a hash for password with the given parameters and
// salt. The derived key is DefaultHashBytes long.
func NewEncodedHash(password []byte, params Params, salt []byte) (EncodedHash, error) {
	if err := params.Validate(); err != nil {
		return EncodedHash{}, err
	}
	if err := validateSalt(salt); err != nil {
		return EncodedHash{}, err
	}
	derived := argon2.IDKey(password, salt, params.Iterations, params.MemoryKiB, params.Parallelism, DefaultHashBytes)
	return EncodedHash{Params: params, Salt: cloneBytes(salt), Hash: derived}, nil
}

// ParseEncoded parses and validates an encoded Argon2id hash. The grammar is
// strict: exactly the argon2id algorithm, version 19, the m,t,p parameter
// block in that order with no extra parameters, canonical unpadded base64 for
// salt and hash, no trailing garbage, and every value inside the package
// bounds.
func ParseEncoded(encoded string) (EncodedHash, error) {
	parts := strings.Split(encoded, "$")
	// "$a$v$params$salt$hash" splits into six fields, the first empty.
	if len(parts) != 6 || parts[0] != "" {
		return EncodedHash{}, fmt.Errorf("%w: want 5 $-separated fields", ErrMalformedHash)
	}
	if parts[1] != "argon2id" {
		return EncodedHash{}, fmt.Errorf("%w: algorithm %q not accepted", ErrMalformedHash, parts[1])
	}
	if parts[2] != "v=19" {
		return EncodedHash{}, fmt.Errorf("%w: version %q not accepted", ErrMalformedHash, parts[2])
	}

	params, err := parseParams(parts[3])
	if err != nil {
		return EncodedHash{}, err
	}

	salt, err := decodeField("salt", parts[4])
	if err != nil {
		return EncodedHash{}, err
	}
	if err := validateSalt(salt); err != nil {
		return EncodedHash{}, err
	}
	hash, err := decodeField("hash", parts[5])
	if err != nil {
		return EncodedHash{}, err
	}
	if len(hash) < MinHashBytes || len(hash) > MaxHashBytes {
		return EncodedHash{}, fmt.Errorf("%w: hash length %d outside [%d, %d]",
			ErrParamsOutOfRange, len(hash), MinHashBytes, MaxHashBytes)
	}
	return EncodedHash{Params: params, Salt: salt, Hash: hash}, nil
}

// String returns the canonical PHC encoding of h.
func (h EncodedHash) String() string {
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		h.Params.MemoryKiB, h.Params.Iterations, h.Params.Parallelism,
		base64.RawStdEncoding.EncodeToString(h.Salt),
		base64.RawStdEncoding.EncodeToString(h.Hash))
}

func parseParams(field string) (Params, error) {
	// The parameter block must be exactly "m=<KiB>,t=<n>,p=<n>" in that
	// order; named values are parsed instead of split so that "t=,m=" style
	// rearrangements or unknown names are rejected rather than ignored.
	if !strings.HasPrefix(field, "m=") {
		return Params{}, fmt.Errorf("%w: parameter block must start with m=", ErrMalformedHash)
	}
	blocks := strings.Split(field, ",")
	if len(blocks) != 3 {
		return Params{}, fmt.Errorf("%w: parameter block must contain exactly m,t,p", ErrMalformedHash)
	}
	values := make([]uint64, len(blocks))
	names := []string{"m", "t", "p"}
	for i, block := range blocks {
		key, raw, ok := strings.Cut(block, "=")
		if !ok || key != names[i] {
			return Params{}, fmt.Errorf("%w: parameter block must be m,t,p in order", ErrMalformedHash)
		}
		if raw == "" {
			return Params{}, fmt.Errorf("%w: empty %s value", ErrMalformedHash, key)
		}
		for _, r := range raw {
			if r < '0' || r > '9' {
				return Params{}, fmt.Errorf("%w: non-digit in %s value", ErrMalformedHash, key)
			}
		}
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return Params{}, fmt.Errorf("%w: %s value %q does not fit", ErrMalformedHash, key, raw)
		}
		values[i] = value
	}
	if values[2] > uint64(^uint8(0)) {
		return Params{}, fmt.Errorf("%w: parallelism %d does not fit", ErrMalformedHash, values[2])
	}
	params := Params{
		MemoryKiB:   uint32(values[0]),
		Iterations:  uint32(values[1]),
		Parallelism: uint8(values[2]),
	}
	if err := params.Validate(); err != nil {
		return Params{}, err
	}
	return params, nil
}

// decodeField decodes one unpadded canonical base64 field. Re-encoding the
// decoded bytes rejects non-canonical trailing bits, which the decoder alone
// may accept.
func decodeField(name, field string) ([]byte, error) {
	if field == "" {
		return nil, fmt.Errorf("%w: %s is empty", ErrMalformedHash, name)
	}
	raw, err := base64.RawStdEncoding.DecodeString(field)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrMalformedHash, name, err)
	}
	if base64.RawStdEncoding.EncodeToString(raw) != field {
		return nil, fmt.Errorf("%w: %s is not canonical base64", ErrMalformedHash, name)
	}
	return raw, nil
}

func validateSalt(salt []byte) error {
	if len(salt) < MinSaltBytes || len(salt) > MaxSaltBytes {
		return fmt.Errorf("%w: salt length %d outside [%d, %d]",
			ErrParamsOutOfRange, len(salt), MinSaltBytes, MaxSaltBytes)
	}
	return nil
}

func cloneBytes(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
