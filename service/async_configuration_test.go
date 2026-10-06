package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
)

func TestAsyncConfigurationRequiresIdempotentBillingAndLogStorage(t *testing.T) {
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	oldLog, oldStore := common.LogDatabaseType(), GetAsyncObjectStore()
	t.Cleanup(func() {
		common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch
		common.SetLogDatabaseType(oldLog)
		SetAsyncObjectStore(oldStore)
	})
	SetAsyncObjectStore(&asyncMemoryStore{objects: map[string][]byte{}, mime: map[string]string{}})
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	for _, logKind := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		common.SetLogDatabaseType(logKind)
		code, _ := AsyncTaskUnavailableReason()
		assert.Empty(t, code)
	}
	common.SetLogDatabaseType(common.DatabaseTypeClickHouse)
	code, _ := AsyncTaskUnavailableReason()
	assert.Equal(t, "unsupported_log_database", code)
	common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
	common.RedisEnabled = true
	code, _ = AsyncTaskUnavailableReason()
	assert.Equal(t, "unsupported_billing_configuration", code)
	common.RedisEnabled, common.BatchUpdateEnabled = false, true
	code, _ = AsyncTaskUnavailableReason()
	assert.Equal(t, "unsupported_billing_configuration", code)
}
