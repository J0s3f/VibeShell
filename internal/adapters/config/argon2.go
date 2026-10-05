package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. They are written into each
// encoded hash so the reader can bound them before spending any work.
type Params struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
	SaltLen   uint32
	KeyLen    uint32
}

// Hash parameter bounds. Reading a password file with parameters outside
// these bounds fails instead of letting a malformed file request
// unreasonable memory or CPU (PLAN 4.2).
const (
	MinMemoryKiB = 8 * 1024   // 8 MiB
	MaxMemoryKiB = 512 * 1024 // 512 MiB
	MinTime      = 1
	MaxTime      = 10
	MinThreads   = 1
	MaxThreads   = 4
	MinSaltLen   = 8
	MaxSaltLen   = 64
	MinKeyLen    = 16
	MaxKeyLen    = 64
)

// DefaultParams is the shipped cost profile: 128 MiB (m=131072), three
// passes, four lanes, 16-byte salt, 32-byte key — the PLAN 4.2 recommended
// baseline for the VibeShell runtime. Operation is additionally bounded by
// MaxConcurrentAuth and the per-connection attempt budget so the configured
// cost cannot be turned into a denial of service.
var DefaultParams = Params{
	Time:      3,
	MemoryKiB: 131072,
	Threads:   4,
	SaltLen:   16,
	KeyLen:    32,
}

// Validate enforces the bounds on every parameter.
func (p Params) Validate() error {
	if p.Time < MinTime || p.Time > MaxTime {
		return fmt.Errorf("time cost must be between %d and %d (got %d)", MinTime, MaxTime, p.Time)
	}
	if p.Threads < MinThreads || p.Threads > MaxThreads {
		return fmt.Errorf("threads must be between %d and %d (got %d)", MinThreads, MaxThreads, p.Threads)
	}
	if p.MemoryKiB < MinMemoryKiB || p.MemoryKiB > MaxMemoryKiB {
		return fmt.Errorf("memory must be between %d and %d KiB (got %d)", MinMemoryKiB, MaxMemoryKiB, p.MemoryKiB)
	}
	if p.MemoryKiB < uint32(8)*uint32(p.Threads) {
		return fmt.Errorf("memory must be at least 8 KiB per lane (got %d KiB for %d lanes)", p.MemoryKiB, p.Threads)
	}
	if p.SaltLen < MinSaltLen || p.SaltLen > MaxSaltLen {
		return fmt.Errorf("salt length must be between %d and %d bytes (got %d)", MinSaltLen, MaxSaltLen, p.SaltLen)
	}
	if p.KeyLen < MinKeyLen || p.KeyLen > MaxKeyLen {
		return fmt.Errorf("key length must be between %d and %d bytes (got %d)", MinKeyLen, MaxKeyLen, p.KeyLen)
	}
	return nil
}

// GenerateHash derives a new Argon2id hash with a fresh random salt and
// returns it in PHC form ($argon2id$v=19$m=…,t=…,p=…$salt$key).
func GenerateHash(password []byte, p Params) (string, error) {
	if len(password) == 0 {
		return "", fmt.Errorf("refusing to hash an empty password")
	}
	if err := p.Validate(); err != nil {
		return "", fmt.Errorf("argon2id parameters: %w", err)
	}
	salt := make([]byte, int(p.SaltLen))
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id salt: %w", err)
	}
	key := argon2.IDKey(password, salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	return encodePHC(p, salt, key), nil
}

// VerifyHash checks a password against an encoded hash. The comparison is
// constant time; malformed or out-of-bounds encodings return an error
// instead of being attempted.
func VerifyHash(encoded string, password []byte) (bool, error) {
	p, salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey(password, salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen)
	if len(got) != len(want) {
		return false, nil
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func encodePHC(p Params, salt, key []byte) string {
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		enc.EncodeToString(salt), enc.EncodeToString(key))
}

// decodePHC parses and bounds-checks an encoded Argon2id hash. Only
// argon2id at the supported version is accepted, in the exact field order
// the encoder writes, so a corrupt or hostile file cannot smuggle in
// argon2i/argon2d or unbounded parameters.
func decodePHC(encoded string) (Params, []byte, []byte, error) {
	var zero Params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return zero, nil, nil, fmt.Errorf("malformed password hash encoding")
	}
	if parts[1] != "argon2id" {
		return zero, nil, nil, fmt.Errorf("unsupported hash algorithm %q (only argon2id is accepted)", parts[1])
	}
	if want := fmt.Sprintf("v=%d", argon2.Version); parts[2] != want {
		return zero, nil, nil, fmt.Errorf("unsupported argon2 version field %q (want %q)", parts[2], want)
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return zero, nil, nil, fmt.Errorf("malformed argon2id parameter field %q", parts[3])
	}
	memory, err := parseParam(fields[0], "m=")
	if err != nil {
		return zero, nil, nil, err
	}
	timeCost, err := parseParam(fields[1], "t=")
	if err != nil {
		return zero, nil, nil, err
	}
	lanes, err := parseParam(fields[2], "p=")
	if err != nil {
		return zero, nil, nil, err
	}
	if lanes > MaxThreads {
		return zero, nil, nil, fmt.Errorf("password hash parameters out of bounds: lanes %d exceeds %d", lanes, MaxThreads)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("password hash salt is not valid base64")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("password hash key is not valid base64")
	}
	if memory > uint64(^uint32(0)) || timeCost > uint64(^uint32(0)) {
		return zero, nil, nil, fmt.Errorf("password hash parameters out of bounds")
	}
	p := Params{
		Time:      uint32(timeCost),
		MemoryKiB: uint32(memory),
		Threads:   uint8(lanes),
		SaltLen:   uint32(len(salt)),
		KeyLen:    uint32(len(key)),
	}
	if err := p.Validate(); err != nil {
		return zero, nil, nil, fmt.Errorf("password hash parameters out of bounds: %w", err)
	}
	return p, salt, key, nil
}

// parseParam reads one "x=<unsigned decimal>" parameter field strictly.
func parseParam(field, prefix string) (uint64, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, fmt.Errorf("malformed argon2id parameter field %q", field)
	}
	digits := strings.TrimPrefix(field, prefix)
	if digits == "" {
		return 0, fmt.Errorf("empty argon2id parameter %q", prefix)
	}
	value, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("argon2id parameter %q is not an unsigned decimal", field)
	}
	return value, nil
}
