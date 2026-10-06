package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These integration tests never discover a production DSN. Network databases
// require explicit opt-in and a task-owned localhost database name.
func TestAsyncDatabaseMigrationMatrix(t *testing.T) {
	cases := []struct{ name, fresh, upgrade, log, sqliteUpgrade, sqliteLog string }{
		{name: "sqlite", fresh: "local", upgrade: "local", log: "local", sqliteUpgrade: os.Getenv("TEST_ASYNC_UPGRADE_SQLITE"), sqliteLog: os.Getenv("TEST_ASYNC_UPGRADE_LOG_SQLITE")},
		{name: "mysql", fresh: os.Getenv("TEST_MYSQL_DSN"), upgrade: os.Getenv("TEST_ASYNC_UPGRADE_MYSQL_DSN"), log: os.Getenv("TEST_ASYNC_LOG_MYSQL_DSN")},
		{name: "postgres", fresh: os.Getenv("TEST_POSTGRES_DSN"), upgrade: os.Getenv("TEST_ASYNC_UPGRADE_POSTGRES_DSN"), log: os.Getenv("TEST_ASYNC_LOG_POSTGRES_DSN")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fresh == "" {
				t.Skip("explicit integration test DSN not configured")
			}
			for _, dsn := range []string{tc.fresh, tc.upgrade, tc.log} {
				if dsn != "" && dsn != "local" {
					require.True(t, strings.Contains(dsn, "127.0.0.1") && strings.Contains(dsn, "/newapi_async_"), "test requires a dedicated localhost newapi_async_* database")
				}
			}
			originalDB, originalLog := DB, LOG_DB
			originalMainKind, originalLogKind, originalSQLite := common.MainDatabaseType(), common.LogDatabaseType(), common.SQLitePath
			t.Cleanup(func() {
				DB, LOG_DB = originalDB, originalLog
				common.SQLitePath = originalSQLite
				common.SetDatabaseTypes(originalMainKind, originalLogKind)
				initCol()
			})
			common.SQLitePath = filepath.Join(t.TempDir(), "fresh.db")
			db, kind := asyncMatrixOpen(t, tc.fresh, false)
			DB, LOG_DB = db, db
			common.SetDatabaseTypes(kind, kind)
			initCol()
			for i := 0; i < 2; i++ {
				require.NoError(t, migrateDB())
				require.NoError(t, migrateLOGDB())
			}
			for _, entry := range []any{&AsyncJob{}, &AsyncJobLedger{}, &AsyncObjectCleanup{}, &AsyncUsageLogOutbox{}} {
				require.True(t, db.Migrator().HasTable(entry))
			}
			if tc.log != "" {
				if tc.name == "sqlite" {
					if tc.sqliteLog == "" {
						common.SQLitePath = filepath.Join(t.TempDir(), "logs.db")
					} else {
						common.SQLitePath = tc.sqliteLog
					}
				}
				logDB, logKind := asyncMatrixOpen(t, tc.log, true)
				LOG_DB = logDB
				common.SetLogDatabaseType(logKind)
				initCol()
				for i := 0; i < 2; i++ {
					require.NoError(t, migrateLOGDB())
				}
				if tc.name != "sqlite" || tc.sqliteLog != "" {
					asyncMatrixPreservedLog(t, logDB)
				}
			}
			asyncMatrixExercise(t)
			if tc.upgrade == "" || tc.name == "sqlite" && tc.sqliteUpgrade == "" {
				t.Log("release upgrade fixture not configured")
				return
			}
			if tc.name == "sqlite" {
				common.SQLitePath = tc.sqliteUpgrade
			}
			upgraded, upgradeKind := asyncMatrixOpen(t, tc.upgrade, false)
			DB = upgraded
			common.SetMainDatabaseType(upgradeKind)
			initCol()
			for i := 0; i < 2; i++ {
				require.NoError(t, migrateDB())
			}
			var user User
			var token Token
			var task Task
			require.NoError(t, upgraded.First(&user, 871001).Error)
			require.NoError(t, upgraded.First(&token, 871002).Error)
			require.NoError(t, upgraded.Where("task_id = ?", "async-upgrade-task").First(&task).Error)
			assert.Equal(t, 123456, user.Quota)
			assert.Empty(t, user.BusinessSystemID)
			assert.Equal(t, 98765, token.RemainQuota)
			assert.Empty(t, token.TagLevel1)
			assert.Empty(t, token.AllowedModalities)
			assert.JSONEq(t, `{"preserved":true}`, string(task.Data))
			assert.Empty(t, task.AsyncJobID)
			asyncMatrixPreservedLog(t, upgraded)
		})
	}
}

func asyncMatrixOpen(t *testing.T, dsn string, log bool) (*gorm.DB, common.DatabaseType) {
	t.Helper()
	t.Setenv("ASYNC_TEST_DSN", dsn)
	db, kind, err := chooseDB("ASYNC_TEST_DSN", log)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, kind
}

func asyncMatrixPreservedLog(t *testing.T, db *gorm.DB) {
	t.Helper()
	var log Log
	require.NoError(t, db.Where("request_id = ?", "async-upgrade-log").First(&log).Error)
	assert.Equal(t, 17, log.Quota)
	assert.Empty(t, log.TagLevel1)
	assert.Nil(t, log.AsyncLogEventID)
}

func asyncMatrixExercise(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	// Only these named test records are replaced for repeatable fixture runs.
	for _, entry := range []any{&AsyncJobLedger{}, &AsyncUsageLogOutbox{}, &AsyncJob{}} {
		require.NoError(t, DB.Where("job_id = ?", "matrix-job").Delete(entry).Error)
	}
	require.NoError(t, DB.Unscoped().Where("id = ?", 882002).Delete(&Token{}).Error)
	require.NoError(t, DB.Unscoped().Where("id = ?", 882001).Delete(&User{}).Error)
	require.NoError(t, LOG_DB.Where("async_task_id = ?", "matrix-job").Delete(&Log{}).Error)
	require.NoError(t, DB.Create(&User{Id: 882001, Username: "async-matrix", Quota: 1000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, DB.Create(&Token{Id: 882002, UserId: 882001, Key: "matrix-fake-key", RemainQuota: 1000, Status: common.TokenStatusEnabled, ExpiredTime: -1, TagLevel1: "System-A", TagLevel2: "Product"}).Error)
	job := &AsyncJob{JobID: "matrix-job", UserID: 882001, TokenID: 882002, IdempotencyScope: "matrix-scope", IdempotencyKey: "matrix-key", Fingerprint: "same", Status: AsyncJobQueued, CreatedAt: now, Model: "test-image", BusinessMetadata: BusinessMetadata{TagLevel1: "System-A", TagLevel2: "Product", BusinessID: "business-1"}}
	_, created, err := CreateAsyncJob(ctx, job)
	require.NoError(t, err)
	require.True(t, created)
	copy := *job
	copy.ID = 0
	copy.JobID = "matrix-duplicate"
	_, created, err = CreateAsyncJob(ctx, &copy)
	require.NoError(t, err)
	require.False(t, created)
	copy.ID = 0
	copy.Fingerprint = "different"
	_, _, err = CreateAsyncJob(ctx, &copy)
	require.ErrorIs(t, err, ErrAsyncJobConflict)
	var winners atomic.Int32
	var claims sync.WaitGroup
	claimErrors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		claims.Add(1)
		go func() {
			defer claims.Done()
			claimed, claimErr := ClaimAsyncJob(ctx, common.GetRandomString(16), now)
			if claimErr != nil {
				claimErrors <- claimErr
			} else if claimed != nil {
				winners.Add(1)
			}
		}()
	}
	claims.Wait()
	close(claimErrors)
	for claimErr := range claimErrors {
		require.NoError(t, claimErr)
	}
	assert.EqualValues(t, 1, winners.Load(), "concurrent instances must have exactly one lease owner")
	_, err = ApplyAsyncBilling(ctx, job.JobID, AsyncReservePhase(100), 100, 0, "wallet_only", job.Model)
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "wallet_only", job.Model)
	require.NoError(t, err)
	_, err = ApplyAsyncBilling(ctx, job.JobID, "settle", 80, 0, "wallet_only", job.Model)
	require.NoError(t, err)
	var user User
	require.NoError(t, DB.First(&user, 882001).Error)
	assert.Equal(t, 920, user.Quota)
	assert.Equal(t, 1, user.RequestCount)
	log := &Log{UserId: job.UserID, TokenId: job.TokenID, Type: LogTypeConsume, Quota: 999, ModelName: job.Model, CreatedAt: now, Other: `{"model_ratio":1,"prompt":"must not persist","url":"https://provider/private"}`, BusinessMetadata: job.BusinessMetadata}
	log.AsyncTaskID = job.JobID
	require.NoError(t, QueueAsyncUsageLog(ctx, log))
	require.NoError(t, FlushAsyncUsageLogs(ctx, now+1))
	// Simulate a process death after writing the independent log DB, before
	// marking delivery in the primary DB. The replay must not duplicate usage.
	require.NoError(t, DB.Model(&AsyncUsageLogOutbox{}).Where("job_id = ?", job.JobID).Updates(map[string]any{"delivered_at": 0, "lease_until": 0, "next_attempt_at": 0}).Error)
	require.NoError(t, FlushAsyncUsageLogs(ctx, now+2))
	var logs []Log
	require.NoError(t, LOG_DB.Where("async_task_id = ?", job.JobID).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 80, logs[0].Quota)
	assert.Equal(t, "System-A", logs[0].TagLevel1)
	assert.NotContains(t, logs[0].Other, "must not persist")
	assert.NotContains(t, logs[0].Other, "provider/private")
	require.NoError(t, TrackAsyncObject(ctx, job.JobID+"/request"))
	require.NoError(t, TrackAsyncObject(ctx, job.JobID+"/request"))
	var count int64
	require.NoError(t, DB.Model(&AsyncObjectCleanup{}).Where("job_id = ?", job.JobID).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	require.NoError(t, ForgetAsyncObject(ctx, job.JobID+"/request"))
}
