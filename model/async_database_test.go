package model

import (
	"context"
	"fmt"
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

// This is the complete AsyncJob schema from 2951360, before per-job retention.
// Keep the old indexes and nullable fields so the upgrade uses real legacy rows,
// rather than inserting a zero retention value into an already-upgraded table.
type asyncJobRetentionLegacy2951360 struct {
	ID      int64  `gorm:"primaryKey"`
	JobID   string `gorm:"type:varchar(64);uniqueIndex"`
	UserID  int    `gorm:"index"`
	TokenID int    `gorm:"index"`
	BusinessMetadata
	IdempotencyScope           string `gorm:"type:varchar(64);uniqueIndex:idx_async_idempotency,priority:1"`
	IdempotencyKey             string `gorm:"type:varchar(128);uniqueIndex:idx_async_idempotency,priority:2"`
	Fingerprint                string `gorm:"type:varchar(64)"`
	Endpoint                   string `gorm:"type:varchar(191)"`
	Modality                   string `gorm:"type:varchar(16)"`
	Model                      string `gorm:"type:varchar(191)"`
	ContentType                string `gorm:"type:varchar(191)"`
	ClientIP                   string `gorm:"type:varchar(64)"`
	RequestRef                 string `gorm:"type:text"`
	ResultRef                  string `gorm:"type:text"`
	ArtifactRefs               string `gorm:"type:text"`
	Status                     string `gorm:"type:varchar(24);index:idx_async_ready,priority:1"`
	ErrorCode                  string `gorm:"type:varchar(64)"`
	ErrorMessage               string `gorm:"type:varchar(512)"`
	NativeTaskID               string `gorm:"type:varchar(191);index"`
	LeaseOwner                 string `gorm:"type:varchar(96)"`
	LeaseUntil                 int64  `gorm:"index"`
	Generation                 int64
	NextRunAt                  int64 `gorm:"index:idx_async_ready,priority:2"`
	SubmissionStartedAt        int64
	CreatedAt                  int64 `gorm:"index"`
	CompletedAt                int64
	ResultExpiresAt            int64 `gorm:"index"`
	SummaryExpiresAt           int64 `gorm:"index"`
	BillingReserved            int
	BillingQuota               int
	BillingState               string `gorm:"type:varchar(24)"`
	BillingExpectedQuota       int
	BillingSettlementRequested bool
	BillingSource              string `gorm:"type:varchar(24)"`
	BillingPreference          string `gorm:"type:varchar(24)"`
	BillingSubscriptionID      int
	ChannelID                  int
	ResultHTTPStatus           int
}

func TestAsyncRetentionDatabaseMigrationMatrix(t *testing.T) {
	// Only explicitly supplied synthetic test DSNs are read. SQLite fixtures are
	// always new files; network fixtures must be new task-owned databases too.
	cases := []struct{ name, fresh, upgrade string }{
		{name: "sqlite", fresh: "local", upgrade: "local"},
		{name: "mysql", fresh: os.Getenv("TEST_ASYNC_RETENTION_MYSQL_DSN"), upgrade: os.Getenv("TEST_ASYNC_RETENTION_UPGRADE_MYSQL_DSN")},
		{name: "postgres", fresh: os.Getenv("TEST_ASYNC_RETENTION_POSTGRES_DSN"), upgrade: os.Getenv("TEST_ASYNC_RETENTION_UPGRADE_POSTGRES_DSN")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fresh == "" || tc.upgrade == "" {
				t.Skip("explicit per-retention integration test DSNs not configured")
			}
			for _, dsn := range []string{tc.fresh, tc.upgrade} {
				if dsn != "local" {
					require.True(t, strings.Contains(dsn, "127.0.0.1") && strings.Contains(dsn, "/newapi_async_ttl_"), "retention fixtures require dedicated localhost newapi_async_ttl_* databases")
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
			for _, stage := range []struct {
				name, dsn string
				legacy    bool
			}{{name: "fresh", dsn: tc.fresh}, {name: "upgrade_2951360", dsn: tc.upgrade, legacy: true}} {
				t.Run(stage.name, func(t *testing.T) {
					common.SQLitePath = filepath.Join(t.TempDir(), stage.name+".db")
					db, kind := asyncMatrixOpen(t, stage.dsn, false)
					var version string
					versionQuery := "SELECT version()"
					if kind == common.DatabaseTypeSQLite {
						versionQuery = "SELECT sqlite_version()"
					}
					require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
					t.Logf("retention database engine: %s", version)
					DB, LOG_DB = db, db
					common.SetDatabaseTypes(kind, kind)
					initCol()
					require.False(t, db.Migrator().HasTable(&AsyncJob{}), "refuse to overwrite a previously migrated async fixture")
					const completedAt int64 = 1791200000
					legacyExpiry := completedAt + 24*60*60
					if stage.legacy {
						require.NoError(t, db.Table("async_jobs").AutoMigrate(&asyncJobRetentionLegacy2951360{}))
						require.False(t, db.Migrator().HasColumn(&AsyncJob{}, "RetentionSeconds"))
						require.NoError(t, db.Table("async_jobs").Create(&asyncJobRetentionLegacy2951360{
							JobID: "legacy-completed", UserID: 7, TokenID: 8,
							BusinessMetadata: BusinessMetadata{TagLevel1: "System", TagLevel2: "Product"},
							IdempotencyScope: "legacy-scope", IdempotencyKey: "legacy-key", Fingerprint: "legacy-fingerprint",
							Status: AsyncJobCompleted, Model: "legacy-model", ResultRef: "private-old-reference",
							CreatedAt: completedAt - 60, CompletedAt: completedAt, ResultExpiresAt: legacyExpiry,
						}).Error)
						for _, preservedExpiry := range []int64{0, completedAt + 100} {
							require.NoError(t, db.Table("async_jobs").Create(&asyncJobRetentionLegacy2951360{
								JobID: fmt.Sprintf("legacy-submission-%d", preservedExpiry), UserID: 7, TokenID: 8,
								IdempotencyScope: "legacy-submission-scope", IdempotencyKey: fmt.Sprintf("key-%d", preservedExpiry),
								Status: AsyncJobSubmitting, LeaseUntil: completedAt - 1, ResultExpiresAt: preservedExpiry,
							}).Error)
						}
					}
					for range 2 {
						require.NoError(t, migrateDB())
						require.True(t, DB.Migrator().HasColumn(&AsyncJob{}, "RetentionSeconds"))
						if stage.legacy {
							var stored AsyncJob
							require.NoError(t, DB.Where("job_id = ?", "legacy-completed").First(&stored).Error)
							assert.Zero(t, stored.RetentionSeconds)
							assert.EqualValues(t, 24*60*60, stored.EffectiveRetentionSeconds())
							assert.Equal(t, legacyExpiry, stored.ResultExpiresAt, "migration and restart must not extend historical result access")
							assert.Equal(t, "private-old-reference", stored.ResultRef)
							assert.Equal(t, "System", stored.TagLevel1)
							assert.Equal(t, "legacy-fingerprint", stored.Fingerprint)
							var nullableRetention struct{ RetentionSeconds *int64 }
							require.NoError(t, DB.Model(&AsyncJob{}).Select("retention_seconds").Where("job_id = ?", stored.JobID).Scan(&nullableRetention).Error)
							assert.Nil(t, nullableRetention.RetentionSeconds, "the new column has no seven-day default that extends legacy tasks")
						}
						// Close and reopen between startup passes to cover persistent
						// database state, not merely repeated calls on one ORM handle.
						sqlDB, err := DB.DB()
						require.NoError(t, err)
						require.NoError(t, sqlDB.Close())
						DB, kind = asyncMatrixOpen(t, stage.dsn, false)
						LOG_DB = DB
						common.SetDatabaseTypes(kind, kind)
						initCol()
					}
					for _, seconds := range []int64{7 * 24 * 60 * 60, AsyncMinRetentionSeconds, 3600, 30 * 24 * 60 * 60} {
						job := AsyncJob{
							JobID: fmt.Sprintf("retention-%d", seconds), UserID: 7, TokenID: 8,
							IdempotencyScope: "retention-scope", IdempotencyKey: fmt.Sprintf("key-%d", seconds),
							Status: AsyncJobCompleted, RetentionSeconds: seconds, CreatedAt: completedAt - 60,
							CompletedAt: completedAt, ResultExpiresAt: completedAt + seconds,
						}
						require.NoError(t, DB.Create(&job).Error)
						stored, err := GetAsyncJob(context.Background(), job.JobID)
						require.NoError(t, err)
						assert.Equal(t, seconds, stored.RetentionSeconds)
						assert.Equal(t, seconds, stored.EffectiveRetentionSeconds())
						assert.Equal(t, completedAt+seconds, stored.ResultExpiresAt)
						duplicate := job
						duplicate.ID = 0
						duplicate.JobID += "-duplicate"
						assert.Error(t, DB.Create(&duplicate).Error, "migration preserves the idempotency unique index")
					}
					// Unknown submissions retain recovery evidence for at least 24
					// hours, even with short result retention. Confirmed results use
					// the exact requested TTL; completed legacy jobs are not rewritten.
					for _, seconds := range []int64{0, AsyncMinRetentionSeconds, 7 * 24 * 60 * 60, 30 * 24 * 60 * 60} {
						job := AsyncJob{
							JobID: fmt.Sprintf("submission-%d", seconds), UserID: 7, TokenID: 8,
							IdempotencyScope: "submission-scope", IdempotencyKey: fmt.Sprintf("key-%d", seconds),
							Status: AsyncJobSubmitting, RetentionSeconds: seconds, CreatedAt: completedAt - 60,
							LeaseUntil: completedAt - 1,
						}
						require.NoError(t, DB.Create(&job).Error)
					}
					require.NoError(t, MarkExpiredAsyncSubmissions(context.Background(), completedAt))
					require.NoError(t, MarkExpiredAsyncSubmissions(context.Background(), completedAt+1))
					for _, seconds := range []int64{0, AsyncMinRetentionSeconds, 7 * 24 * 60 * 60, 30 * 24 * 60 * 60} {
						stored, err := GetAsyncJob(context.Background(), fmt.Sprintf("submission-%d", seconds))
						require.NoError(t, err)
						assert.Equal(t, AsyncJobUnknown, stored.Status)
						assert.Equal(t, completedAt+max(stored.EffectiveRetentionSeconds(), int64(24*60*60)), stored.ResultExpiresAt, "short result retention cannot delete native submission recovery evidence")
						assert.Equal(t, completedAt, stored.CompletedAt, "repeated expiration scans do not move the retention window")
					}
					if stage.legacy {
						stored, err := GetAsyncJob(context.Background(), "legacy-completed")
						require.NoError(t, err)
						assert.Equal(t, legacyExpiry, stored.ResultExpiresAt)
						for _, preservedExpiry := range []int64{0, completedAt + 100} {
							stored, err := GetAsyncJob(context.Background(), fmt.Sprintf("legacy-submission-%d", preservedExpiry))
							require.NoError(t, err)
							assert.Zero(t, stored.RetentionSeconds)
							assert.Equal(t, AsyncJobUnknown, stored.Status)
							if preservedExpiry == 0 {
								assert.Equal(t, legacyExpiry, stored.ResultExpiresAt, "NULL legacy retention gets only the original 24-hour window")
							} else {
								assert.Equal(t, preservedExpiry, stored.ResultExpiresAt, "a snapshotted absolute deadline is never extended")
							}
						}
					}
				})
			}
		})
	}
}

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
