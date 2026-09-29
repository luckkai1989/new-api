package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/go-redis/redis/v8"
)

var (
	ErrRegistrationIPLimitReached = errors.New("registration limit reached for this IP address")
	ErrRegistrationIPUnavailable  = errors.New("registration is temporarily unavailable")
)

var reserveRegistrationIPScript = redis.NewScript(`
local now = redis.call('TIME')
local millis = now[1] * 1000 + math.floor(now[2] / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', millis - tonumber(ARGV[1]))
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('ZADD', KEYS[1], millis, ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
return 1
`)

var releaseRegistrationIPScript = redis.NewScript(`
redis.call('ZREM', KEYS[1], ARGV[1])
if redis.call('ZCARD', KEYS[1]) == 0 then redis.call('DEL', KEYS[1]) end
return 1
`)

type RegistrationIPReservation struct {
	key   string
	token string
}

// ReserveRegistrationIP reserves one successful-account slot before user creation.
// A failed creation must release it; a successful one expires with the time window.
func ReserveRegistrationIP(ctx context.Context, clientIP string) (*RegistrationIPReservation, error) {
	rawLimit := os.Getenv("REGISTRATION_IP_LIMIT")
	if rawLimit == "" || rawLimit == "0" {
		return nil, nil
	}
	limit, err := strconv.Atoi(rawLimit)
	if err != nil || limit < 1 {
		return nil, fmt.Errorf("invalid REGISTRATION_IP_LIMIT: %w", ErrRegistrationIPUnavailable)
	}
	window := 24 * time.Hour
	if rawWindow := os.Getenv("REGISTRATION_IP_WINDOW"); rawWindow != "" {
		window, err = time.ParseDuration(rawWindow)
		if err != nil || window < time.Second {
			return nil, fmt.Errorf("invalid REGISTRATION_IP_WINDOW: %w", ErrRegistrationIPUnavailable)
		}
	}
	if !common.RedisEnabled || common.RDB == nil {
		return nil, ErrRegistrationIPUnavailable
	}
	ip, err := netip.ParseAddr(clientIP)
	if err != nil {
		return nil, ErrRegistrationIPUnavailable
	}
	// Redis keys do not expose visitor IPs to routine key listings.
	digest := sha256.Sum256([]byte(ip.Unmap().String()))
	key := "registration:ip:v1:" + hex.EncodeToString(digest[:])
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, ErrRegistrationIPUnavailable
	}
	token := hex.EncodeToString(tokenBytes)
	reserved, err := reserveRegistrationIPScript.Run(ctx, common.RDB, []string{key}, window.Milliseconds(), limit, token).Int()
	if err != nil {
		common.SysError("reserve registration IP slot failed: " + err.Error())
		return nil, ErrRegistrationIPUnavailable
	}
	if reserved == 0 {
		return nil, ErrRegistrationIPLimitReached
	}
	return &RegistrationIPReservation{key: key, token: token}, nil
}

func (r *RegistrationIPReservation) Release(ctx context.Context) error {
	if r == nil || r.token == "" {
		return nil
	}
	return releaseRegistrationIPScript.Run(ctx, common.RDB, []string{r.key}, r.token).Err()
}

func (r *RegistrationIPReservation) Commit() {
	if r != nil {
		r.token = ""
	}
}

func (r *RegistrationIPReservation) ReleaseOnFailure() {
	if r == nil || r.token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Release(ctx); err != nil {
		common.SysError("failed to release registration IP slot: " + err.Error())
	}
}
