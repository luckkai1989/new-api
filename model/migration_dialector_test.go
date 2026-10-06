package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type MigrationIdentityFields struct {
	ID        int    `gorm:"primaryKey"`
	Name      string `gorm:"size:64;unique"`
	Reference string `gorm:"size:64;uniqueIndex"`
	Provider  string `gorm:"size:32;uniqueIndex:,composite:provider_subject"`
	Subject   string `gorm:"size:64;uniqueIndex:,composite:provider_subject"`
}

type BusinessDomainLegacyRow struct {
	ID        int `gorm:"primaryKey"`
	UserID    int
	TagLevel1 *string `gorm:"column:tag_level_1;type:varchar(64)"`
	TagLevel2 *string `gorm:"column:tag_level_2;type:varchar(64)"`
}

type BusinessDomainMigrationRow struct {
	ID     int `gorm:"primaryKey"`
	UserID int
	BusinessMetadata
}

type BusinessMidjourneyLegacyRow struct {
	Id     int `gorm:"primaryKey"`
	UserId int
	MjId   string
}

// External cases only target the task's synthetic, disposable test services.
// They never discover credentials or connections from the local environment.
func TestBusinessDatabaseScopeUpgradeAndCaseSensitiveQueries(t *testing.T) {
	for _, databaseType := range []common.DatabaseType{common.DatabaseTypeSQLite, common.DatabaseTypeMySQL, common.DatabaseTypePostgreSQL} {
		t.Run(string(databaseType), func(t *testing.T) {
			var dialector gorm.Dialector
			switch databaseType {
			case common.DatabaseTypeSQLite:
				dialector = sqlite.Open(":memory:")
			case common.DatabaseTypeMySQL:
				dialector = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: "root:async-test-only@tcp(127.0.0.1:17306)/newapi_async_test?parseTime=true&charset=utf8mb4"}}}
			case common.DatabaseTypePostgreSQL:
				dialector = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: "postgres://postgres:async-test-only@127.0.0.1:17432/newapi_async_test?sslmode=disable", PreferSimpleProtocol: true}}}
			}
			db, err := gorm.Open(dialector, newGormConfig(databaseType != common.DatabaseTypePostgreSQL))
			if err != nil && databaseType != common.DatabaseTypeSQLite {
				t.Skip("synthetic task-only database is not available")
			}
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
			common.SetDatabaseTypes(databaseType, databaseType)
			t.Cleanup(func() { common.SetDatabaseTypes(previousMain, previousLog) })
			const table = "business_scope_upgrade_test"
			require.False(t, db.Migrator().HasTable(table), "refuse to overwrite an existing test table")
			t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
			require.NoError(t, db.Table(table).AutoMigrate(&BusinessDomainLegacyRow{}))
			require.NoError(t, db.Table(table).Create(&BusinessDomainLegacyRow{UserID: 7}).Error)
			for range 2 {
				require.NoError(t, db.Table(table).AutoMigrate(&BusinessDomainMigrationRow{}))
			}
			require.NoError(t, db.Table(table).Create(&[]BusinessDomainMigrationRow{
				{UserID: 7, BusinessMetadata: BusinessMetadata{TagLevel1: "System", TagLevel2: "Product"}},
				{UserID: 7, BusinessMetadata: BusinessMetadata{TagLevel1: "system", TagLevel2: "Product"}},
				{UserID: 8, BusinessMetadata: BusinessMetadata{TagLevel1: "System", TagLevel2: "Product"}},
			}).Error)
			for _, scope := range []BusinessScope{{UserID: 7}, {UserID: 7, TagLevel1: "System", TagLevel2: "Product"}, {UserID: 7, TagLevel1: "system", TagLevel2: "Product"}} {
				var count int64
				require.NoError(t, ApplyBusinessScope(db.Table(table), scope).Count(&count).Error)
				require.Equal(t, int64(1), count, "exact labels, account isolation and NULL legacy domains")
			}
			var count int64
			require.NoError(t, ApplyBusinessFilters(db.Table(table), BusinessLogFilter{"tag_level_1": "System"}, true).Count(&count).Error)
			require.Equal(t, int64(2), count, "log label filters remain case sensitive on MySQL too")

			const mjTable = "business_mj_upgrade_test"
			require.False(t, db.Migrator().HasTable(mjTable), "refuse to overwrite an existing test table")
			t.Cleanup(func() { _ = db.Migrator().DropTable(mjTable) })
			require.NoError(t, db.Table(mjTable).AutoMigrate(&BusinessMidjourneyLegacyRow{}))
			legacyRow := BusinessMidjourneyLegacyRow{UserId: 7, MjId: "legacy-public"}
			require.NoError(t, db.Table(mjTable).Create(&legacyRow).Error)
			for range 2 {
				require.NoError(t, db.Table(mjTable).AutoMigrate(&Midjourney{}))
			}
			var legacy Midjourney
			require.NoError(t, db.Table(mjTable).First(&legacy, legacyRow.Id).Error)
			assert.True(t, legacy.IsLegacyPublicImage(), "upgrade preserves existing public image links")
			assert.Equal(t, "legacy-public", legacy.MjId)
			for _, claimedVersion := range []int{0, 99} {
				created := Midjourney{UserId: 7, MjId: "new-private", BusinessScopeVersion: claimedVersion}
				require.NoError(t, db.Table(mjTable).Create(&created).Error)
				assert.Equal(t, 1, created.BusinessScopeVersion, "the create hook assigns version, never the caller")
				var stored Midjourney
				require.NoError(t, db.Table(mjTable).First(&stored, created.Id).Error)
				assert.False(t, stored.IsLegacyPublicImage(), "new empty-label tasks are never legacy public")
			}
			legacy.TagLevel1 = "new-label"
			assert.False(t, legacy.IsLegacyPublicImage(), "metadata cannot use the legacy exception")
			legacy.BusinessMetadata = BusinessMetadata{AsyncTaskID: "async-new"}
			assert.False(t, legacy.IsLegacyPublicImage(), "async attribution cannot use the legacy exception")
		})
	}
}

type migrationIdentityV1 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(32)"`
}

type migrationIdentityV2 struct {
	MigrationIdentityFields
	Digest string `gorm:"type:char(64)"`
	Note   string `gorm:"size:128"`
}

type migrationConstraintV1 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64"`
}

type migrationConstraintV2 struct {
	ID   int    `gorm:"primaryKey"`
	Name string `gorm:"size:64;unique"`
}

type migrationDecimalV1 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(10,6);default:0"`
}

type migrationDecimalV2 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(12,6);not null;default:0"`
}

type migrationDecimalV3 struct {
	ID    int     `gorm:"primaryKey"`
	Price float64 `gorm:"type:decimal(12,6);not null;default:1.25"`
}

func TestMigrationSchemaStability(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var dsn string
			switch dialect {
			case "sqlite":
				dsn = "local"
				previousPath := common.SQLitePath
				common.SQLitePath = filepath.Join(t.TempDir(), "migration.db")
				t.Cleanup(func() { common.SQLitePath = previousPath })
			case "mysql":
				dsn = os.Getenv("TEST_MYSQL_DSN")
			case "postgres":
				dsn = os.Getenv("TEST_POSTGRES_DSN")
			}
			if dsn == "" {
				t.Skip("test database DSN is not configured")
			}
			t.Setenv("MIGRATION_TEST_DSN", dsn)
			db, _, err := chooseDB("MIGRATION_TEST_DSN", false)
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = sqlDB.Close() })
			recorder := &migrationSQLRecorder{}
			db = db.Session(&gorm.Session{Logger: recorder})

			t.Run("identity_and_indexes", func(t *testing.T) {
				const table = "migration_identity_test"
				t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
				row := migrationIdentityV1{
					MigrationIdentityFields: MigrationIdentityFields{ID: 1, Name: "root", Reference: "token-reference", Provider: "oidc", Subject: "subject"},
					Digest:                  "old-digest",
				}
				require.NoError(t, db.Table(table).Create(&row).Error)
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV1{}))
				assert.Empty(t, recorder.schemaMutations())

				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
				columns, err := db.Table(table).Migrator().ColumnTypes(&migrationIdentityV2{})
				require.NoError(t, err)
				for _, column := range columns {
					if column.Name() == "digest" {
						length, ok := column.Length()
						require.True(t, ok)
						assert.EqualValues(t, 64, length)
					}
				}
				assert.True(t, db.Table(table).Migrator().HasColumn(&migrationIdentityV2{}, "note"))
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationIdentityV2{}))
				assert.Empty(t, recorder.schemaMutations())
				var saved migrationIdentityV2
				require.NoError(t, db.Table(table).First(&saved, 1).Error)
				assert.Equal(t, row.MigrationIdentityFields, saved.MigrationIdentityFields)
				expectedDigest := row.Digest
				if dialect == "postgres" {
					expectedDigest += strings.Repeat(" ", 64-len(row.Digest))
				}
				assert.Equal(t, expectedDigest, saved.Digest)
				for _, duplicate := range []migrationIdentityV2{
					{MigrationIdentityFields: MigrationIdentityFields{Name: "root", Reference: "other-1", Provider: "other", Subject: "1"}},
					{MigrationIdentityFields: MigrationIdentityFields{Name: "other-2", Reference: "token-reference", Provider: "other", Subject: "2"}},
					{MigrationIdentityFields: MigrationIdentityFields{Name: "other-3", Reference: "other-3", Provider: "oidc", Subject: "subject"}},
				} {
					assert.Error(t, db.Table(table).Create(&duplicate).Error)
				}
			})

			t.Run("unique_constraint_changes", func(t *testing.T) {
				const table = "migration_constraint_test"
				t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
				require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
				assert.Error(t, db.Table(table).Create(&migrationConstraintV2{Name: "existing"}).Error)
				recorder.reset()
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV2{}))
				assert.Empty(t, recorder.schemaMutations())
				require.NoError(t, db.Table(table).AutoMigrate(&migrationConstraintV1{}))
				require.NoError(t, db.Table(table).Create(&migrationConstraintV1{Name: "existing"}).Error)
			})

			if dialect == "postgres" {
				t.Run("renamed_unique_constraints", func(t *testing.T) {
					const table = "migration_renamed_unique"
					t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
					tableDB := db.Table(table).Session(&gorm.Session{})
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					original := migrationConstraintV1{Name: "existing"}
					require.NoError(t, tableDB.Create(&original).Error)
					for _, name := range []string{"models_model_name_key", `imported "model" name`} {
						require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT ? UNIQUE (name)", clause.Table{Name: table}, clause.Column{Name: name}).Error)
					}
					require.NoError(t, db.Exec("ALTER TABLE ? ADD CONSTRAINT keep_composite UNIQUE (id, name)", clause.Table{Name: table}).Error)
					require.NoError(t, db.Exec("CREATE INDEX keep_name_lookup ON ? (name)", clause.Table{Name: table}).Error)
					// A unique field must keep the old names and still reject duplicates.
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV2{}))
					require.Error(t, tableDB.Create(&migrationConstraintV2{Name: "existing"}).Error)
					require.NoError(t, db.Exec("CREATE TABLE migration_unique_reference (name varchar(64) REFERENCES migration_renamed_unique(name))").Error)
					t.Cleanup(func() { _ = db.Migrator().DropTable("migration_unique_reference") })
					require.NoError(t, db.Exec("INSERT INTO migration_unique_reference (name) VALUES (?)", "existing").Error)
					require.Error(t, tableDB.AutoMigrate(&migrationConstraintV1{}), "dependent foreign keys must not be cascaded away")
					for _, name := range []string{"models_model_name_key", `imported "model" name`} {
						assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, name), "all old constraints must survive rollback")
					}
					var references int64
					require.NoError(t, db.Table("migration_unique_reference").Count(&references).Error)
					assert.EqualValues(t, 1, references)
					require.NoError(t, db.Migrator().DropTable("migration_unique_reference"))
					// Removing column uniqueness must resolve both actual constraint names.
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					recorder.reset()
					require.NoError(t, tableDB.AutoMigrate(&migrationConstraintV1{}))
					assert.Empty(t, recorder.schemaMutations())
					require.NoError(t, tableDB.Create(&migrationConstraintV1{Name: "existing"}).Error)
					assert.True(t, tableDB.Migrator().HasConstraint(&migrationConstraintV1{}, "keep_composite"))
					assert.True(t, tableDB.Migrator().HasIndex(&migrationConstraintV1{}, "keep_name_lookup"))
					var rows []migrationConstraintV1
					require.NoError(t, tableDB.Order("id").Find(&rows).Error)
					require.Len(t, rows, 2)
					assert.Equal(t, original, rows[0])
				})
			}

			if dialect == "mysql" {
				t.Run("decimal_default_and_real_changes", func(t *testing.T) {
					const table = "migration_decimal_test"
					t.Cleanup(func() { _ = db.Migrator().DropTable(table) })
					require.NoError(t, db.Table(table).AutoMigrate(&migrationDecimalV1{}))
					require.NoError(t, db.Table(table).Create(&migrationDecimalV1{ID: 1, Price: 12.345678}).Error)
					for _, target := range []any{&migrationDecimalV1{}, &migrationDecimalV2{}, &migrationDecimalV3{}} {
						require.NoError(t, db.Table(table).AutoMigrate(target))
						recorder.reset()
						require.NoError(t, db.Table(table).AutoMigrate(target))
						assert.Empty(t, recorder.schemaMutations())
					}
					columns, err := db.Table(table).Migrator().ColumnTypes(&migrationDecimalV3{})
					require.NoError(t, err)
					for _, column := range columns {
						if column.Name() == "price" {
							precision, scale, ok := column.DecimalSize()
							require.True(t, ok)
							assert.EqualValues(t, 12, precision)
							assert.EqualValues(t, 6, scale)
							nullable, ok := column.Nullable()
							require.True(t, ok)
							assert.False(t, nullable)
						}
					}
					require.NoError(t, db.Table(table).Create(&map[string]any{"id": 2}).Error)
					var prices []float64
					require.NoError(t, db.Table(table).Order("id").Pluck("price", &prices).Error)
					assert.Equal(t, []float64{12.345678, 1.25}, prices)
				})
			}
		})
	}
}
