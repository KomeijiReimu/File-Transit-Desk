package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      uint32 = 65536
	argonIterations  uint32 = 3
	argonParallelism uint8  = 2
	argonSaltLength         = 16
	argonKeyLength          = 32
	maxPHCLength            = 512
)

var ErrInvalidPasswordHash = errors.New("invalid argon2id password hash")

type argonParameters struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	key         []byte
}

func Hash(password []byte) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(password, salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonIterations, argonParallelism, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func Verify(phc string, password []byte) (bool, error) {
	params, err := parseArgon2id(phc)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey(password, params.salt, params.iterations, params.memory, params.parallelism, uint32(len(params.key)))
	return subtle.ConstantTimeCompare(actual, params.key) == 1, nil
}

func Validate(phc string) error {
	_, err := parseArgon2id(phc)
	return err
}

func parseArgon2id(phc string) (argonParameters, error) {
	if len(phc) == 0 || len(phc) > maxPHCLength {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	values := map[string]uint64{}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	for _, parameter := range parameters {
		pair := strings.SplitN(parameter, "=", 2)
		if len(pair) != 2 || pair[0] == "" {
			return argonParameters{}, ErrInvalidPasswordHash
		}
		if _, exists := values[pair[0]]; exists {
			return argonParameters{}, ErrInvalidPasswordHash
		}
		value, err := strconv.ParseUint(pair[1], 10, 32)
		if err != nil {
			return argonParameters{}, ErrInvalidPasswordHash
		}
		values[pair[0]] = value
	}
	if len(values) != 3 || values["m"] < 16*1024 || values["m"] > 256*1024 || values["t"] < 1 || values["t"] > 10 || values["p"] < 1 || values["p"] > 8 {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	if values["m"] > uint64(^uint32(0)) || values["t"] > uint64(^uint32(0)) || values["p"] > uint64(^uint8(0)) {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	decoder := base64.RawStdEncoding.Strict()
	salt, err := decoder.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	key, err := decoder.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return argonParameters{}, ErrInvalidPasswordHash
	}
	return argonParameters{memory: uint32(values["m"]), iterations: uint32(values["t"]), parallelism: uint8(values["p"]), salt: salt, key: key}, nil
}
