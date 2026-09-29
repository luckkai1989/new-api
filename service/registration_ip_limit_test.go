package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistrationIPLimitCountsSuccessfulAccountsAcrossClients(t *testing.T) {
	t.Setenv("REGISTRATION_IP_LIMIT", "2")
	t.Setenv("REGISTRATION_IP_WINDOW", "1h")
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	previousClient, previousEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = clientA, true
	t.Cleanup(func() {
		common.RDB, common.RedisEnabled = previousClient, previousEnabled
		_ = clientA.Close()
		_ = clientB.Close()
	})
	ctx := context.Background()

	first, err := ReserveRegistrationIP(ctx, "203.0.113.10")
	require.NoError(t, err)
	require.NotNil(t, first)
	first.Commit()

	common.RDB = clientB
	failedCreation, err := ReserveRegistrationIP(ctx, "203.0.113.10")
	require.NoError(t, err)
	failedCreation.ReleaseOnFailure()

	second, err := ReserveRegistrationIP(ctx, "203.0.113.10")
	require.NoError(t, err)
	second.Commit()
	_, err = ReserveRegistrationIP(ctx, "203.0.113.10")
	require.ErrorIs(t, err, ErrRegistrationIPLimitReached)

	otherIP, err := ReserveRegistrationIP(ctx, "203.0.113.11")
	require.NoError(t, err)
	otherIP.Commit()

	server.FastForward(time.Hour + time.Second)
	_, err = ReserveRegistrationIP(ctx, "203.0.113.10")
	assert.NoError(t, err)
}

func TestRegistrationIPLimitRejectsUnavailableStorageAndInvalidIP(t *testing.T) {
	t.Setenv("REGISTRATION_IP_LIMIT", "1")
	t.Setenv("REGISTRATION_IP_WINDOW", "24h")
	previousClient, previousEnabled := common.RDB, common.RedisEnabled
	common.RDB, common.RedisEnabled = nil, false
	t.Cleanup(func() { common.RDB, common.RedisEnabled = previousClient, previousEnabled })

	_, err := ReserveRegistrationIP(context.Background(), "203.0.113.10")
	require.ErrorIs(t, err, ErrRegistrationIPUnavailable)

	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	common.RDB, common.RedisEnabled = client, true
	t.Cleanup(func() { _ = client.Close() })
	_, err = ReserveRegistrationIP(context.Background(), "unknown")
	require.ErrorIs(t, err, ErrRegistrationIPUnavailable)

	t.Setenv("REGISTRATION_IP_LIMIT", "invalid")
	_, err = ReserveRegistrationIP(context.Background(), "203.0.113.10")
	require.True(t, errors.Is(err, ErrRegistrationIPUnavailable))
	t.Setenv("REGISTRATION_IP_LIMIT", "1")
	t.Setenv("REGISTRATION_IP_WINDOW", "invalid")
	_, err = ReserveRegistrationIP(context.Background(), "203.0.113.10")
	require.ErrorIs(t, err, ErrRegistrationIPUnavailable)
	t.Setenv("REGISTRATION_IP_WINDOW", "24h")
	require.NoError(t, client.Close())
	_, err = ReserveRegistrationIP(context.Background(), "203.0.113.10")
	require.ErrorIs(t, err, ErrRegistrationIPUnavailable)
}
