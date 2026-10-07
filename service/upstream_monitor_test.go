/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type monitorSubscriptionStub struct {
	active bool
	err    error
}

func (m monitorSubscriptionStub) Prices(context.Context, model.UpstreamMonitor, string) ([]MonitorPrice, error) {
	return nil, nil
}
func (m monitorSubscriptionStub) Balance(context.Context, model.UpstreamMonitor, string) (float64, error) {
	return 0, nil
}
func (m monitorSubscriptionStub) HasActiveSubscription(context.Context, model.UpstreamMonitor, string) (bool, error) {
	return m.active, m.err
}

func floatPtr(v float64) *float64 { return &v }
func intPtr(v int) *int           { return &v }

func TestUpstreamMonitorNewAPIPriceNormalization(t *testing.T) {
	payload := newAPIPricingEnvelope{Success: true, GroupRatio: map[string]float64{"vip": 1.5}, Data: []newAPIPricingItem{
		{ModelName: "gpt-x", QuotaType: intPtr(0), ModelRatio: floatPtr(2), CompletionRatio: floatPtr(4), CacheRatio: floatPtr(0.5), EnableGroups: []string{"vip"}},
		{ModelName: "image-x", QuotaType: intPtr(1), ModelPrice: floatPtr(0.2), EnableGroups: []string{"vip"}},
	}}
	prices, err := normalizeNewAPIPrices(payload, "vip", 500000)
	require.NoError(t, err)
	require.Len(t, prices, 2)
	require.InDelta(t, 6, prices[0].Input, 1e-9)
	require.InDelta(t, 24, prices[0].Output, 1e-9)
	require.InDelta(t, 3, prices[0].CacheRead, 1e-9)
	require.InDelta(t, 7.5, prices[0].CacheWrite, 1e-9)
	require.InDelta(t, 0.3, prices[1].PerRequest, 1e-9)
}

func TestUpstreamMonitorMissingPriceFailsClosed(t *testing.T) {
	payload := newAPIPricingEnvelope{Success: true, GroupRatio: map[string]float64{"vip": 1}, Data: []newAPIPricingItem{
		{ModelName: "gpt-x", QuotaType: intPtr(0), ModelRatio: floatPtr(1), EnableGroups: []string{"vip"}},
	}}
	prices, err := normalizeNewAPIPrices(payload, "vip", 500000)
	require.NoError(t, err)
	require.False(t, prices[0].Comparable)
	_, err = normalizeNewAPIPrices(payload, "missing", 500000)
	require.Error(t, err)
}

func TestUpstreamMonitorNewAPIGroupVisibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		groups  [][]string
		problem string
	}{
		{name: "empty pricing response", problem: "returned_models=0; check pricing visibility"},
		{name: "different group", groups: [][]string{{"default"}, {"default"}}, problem: `returned_models=2, models_without_groups=0, response_groups=["default"]`},
		{name: "case-sensitive group", groups: [][]string{{"codex-plus"}}, problem: `response_groups=["codex-plus"]`},
		{name: "missing group fields", groups: [][]string{nil, {}}, problem: "returned_models=2, models_without_groups=2, response_groups=[]"},
		{name: "mixed missing and different groups", groups: [][]string{nil, {"vip", "default"}}, problem: `returned_models=2, models_without_groups=1, response_groups=["default" "vip"]`},
		{name: "exact group", groups: [][]string{{"Codex-Plus"}}},
		{name: "all groups", groups: [][]string{{"all"}}},
		{name: "bounded group diagnostics", groups: [][]string{{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}, problem: `response_groups=["a" "b" "c" "d" "e" "f" "g" "h"], omitted_groups=1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := newAPIPricingEnvelope{Success: true, GroupRatio: map[string]float64{"Codex-Plus": 1}}
			for i, groups := range tc.groups {
				payload.Data = append(payload.Data, newAPIPricingItem{ModelName: fmt.Sprintf("model-%d", i), QuotaType: intPtr(0), ModelRatio: floatPtr(1), CompletionRatio: floatPtr(1), EnableGroups: groups})
			}
			prices, err := normalizeNewAPIPrices(payload, "Codex-Plus", 500000)
			if tc.problem != "" {
				require.ErrorContains(t, err, tc.problem)
				assert.Contains(t, err.Error(), `configured_group="Codex-Plus"`)
				assert.Nil(t, prices, "a group ratio alone must not make unrelated prices applicable")
				return
			}
			require.NoError(t, err)
			require.Len(t, prices, 1)
			assert.True(t, prices[0].Comparable)
			assert.Equal(t, 2.0, prices[0].Input)
		})
	}
}

func TestUpstreamMonitorNewAPIImageRatioNeedsReview(t *testing.T) {
	payload := newAPIPricingEnvelope{Success: true, GroupRatio: map[string]float64{"vip": 1}, Data: []newAPIPricingItem{
		{ModelName: "image-x", QuotaType: intPtr(0), ModelRatio: floatPtr(1), CompletionRatio: floatPtr(1), ImageRatio: floatPtr(2), EnableGroups: []string{"vip"}},
	}}
	prices, err := normalizeNewAPIPrices(payload, "vip", 500000)
	require.NoError(t, err)
	require.False(t, prices[0].Comparable)
	require.Contains(t, prices[0].Reason, "image or audio")
}

func TestUpstreamMonitorSub2APIPerTokenRates(t *testing.T) {
	var payload sub2PriceEnvelope
	require.NoError(t, json.Unmarshal([]byte(`{"code":0,"data":[{"platforms":[{"groups":[{"id":7,"name":"vip","rate_multiplier":2}],"supported_models":[{"name":"gpt-x","pricing":{"billing_mode":"token","input_price":0.000002,"output_price":0.000008,"cache_read_price":0.000001,"intervals":[]}}]}]}]}`), &payload))
	prices, err := normalizeSub2APIPrices(payload, map[string]float64{"7": 1.5}, "vip")
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.InDelta(t, 3, prices[0].Input, 1e-9)
	require.InDelta(t, 12, prices[0].Output, 1e-9)
	require.InDelta(t, 1.5, prices[0].CacheRead, 1e-9)
	require.Equal(t, 1.5, prices[0].RawGroupRatio)
}

func TestUpstreamMonitorSub2APIImageOutputNeedsReview(t *testing.T) {
	var payload sub2PriceEnvelope
	require.NoError(t, json.Unmarshal([]byte(`{"code":0,"data":[{"platforms":[{"groups":[{"id":7,"name":"vip","rate_multiplier":1}],"supported_models":[{"name":"image-x","pricing":{"billing_mode":"token","input_price":0.000001,"output_price":0.000002,"image_output_price":0.01}}]}]}]}`), &payload))
	prices, err := normalizeSub2APIPrices(payload, nil, "vip")
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.False(t, prices[0].Comparable)
	require.Contains(t, prices[0].Reason, "image output")
}

func TestUpstreamMonitorMappedModelsAndHighestCost(t *testing.T) {
	mapping := `{"public-model":"upstream-model"}`
	channel := model.Channel{Models: "public-model, another-model", ModelMapping: &mapping}
	models := monitorChannelModels(&channel)
	require.Equal(t, "upstream-model", models["public-model"])
	require.Equal(t, "another-model", models["another-model"])
	price, found := findMonitorPrice([]MonitorPrice{
		{Model: "upstream-model", Mode: "token", Comparable: true, Input: 2, Output: 8},
		{Model: "upstream-model", Mode: "token", Comparable: true, Input: 3, Output: 6},
	}, "upstream-model")
	require.True(t, found)
	require.Equal(t, 3.0, price.Input)
	require.Equal(t, 8.0, price.Output)
}

func TestUpstreamMonitorPriceSnapshotCompleteness(t *testing.T) {
	channel := &model.Channel{Models: "gpt-x,gpt-y"}
	prices := []MonitorPrice{{Model: "gpt-x", Mode: "token", Comparable: true}}
	require.ErrorContains(t, validateMonitorPrices(channel, prices), "gpt-y: model is missing")
	prices = append(prices, MonitorPrice{Model: "gpt-y", Mode: "token", Comparable: false})
	require.ErrorContains(t, validateMonitorPrices(channel, prices), "gpt-y: pricing requires manual review")
	prices[1].Comparable = true
	require.NoError(t, validateMonitorPrices(channel, prices))
	channel.ModelMapping = common.GetPointer(`{"gpt-x":"mapped-x"}`)
	prices[0].Model = "mapped-x"
	prices[0].Comparable, prices[0].Reason = false, "tiered expression requires manual review"
	require.ErrorContains(t, validateMonitorPrices(channel, prices), "gpt-x -> mapped-x: tiered expression requires manual review")
}

func TestUpstreamMonitorOptionalModelMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapping *string
		model   string
		invalid bool
	}{
		{name: "unset", model: "gpt-x"},
		{name: "empty", mapping: common.GetPointer(""), model: "gpt-x"},
		{name: "blank", mapping: common.GetPointer(" \t\n"), model: "gpt-x"},
		{name: "empty object", mapping: common.GetPointer("{}"), model: "gpt-x"},
		{name: "null", mapping: common.GetPointer("null"), model: "gpt-x"},
		{name: "mapped", mapping: common.GetPointer(`{"gpt-x":"upstream-x"}`), model: "upstream-x"},
		{name: "broken JSON", mapping: common.GetPointer(`{"gpt-x":`), invalid: true},
		{name: "array", mapping: common.GetPointer(`[]`), invalid: true},
		{name: "non-string target", mapping: common.GetPointer(`{"gpt-x":123}`), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel := &model.Channel{Models: " gpt-x, ", ModelMapping: tc.mapping}
			prices := []MonitorPrice{{Model: tc.model, Mode: "token", Comparable: true}}
			if tc.invalid {
				require.Nil(t, monitorChannelModels(channel))
				require.ErrorContains(t, validateMonitorPrices(channel, prices), "channel model mapping is invalid JSON")
				return
			}
			require.Equal(t, map[string]string{"gpt-x": tc.model}, monitorChannelModels(channel))
			require.NoError(t, validateMonitorPrices(channel, prices))
		})
	}
	require.ErrorContains(t, validateMonitorPrices(&model.Channel{Models: " , "}, nil), "channel has no models;")
}

type monitorCollectionStub struct {
	prices       []MonitorPrice
	priceCalls   int
	balanceCalls int
	priceError   error
}

func (stub *monitorCollectionStub) Prices(context.Context, model.UpstreamMonitor, string) ([]MonitorPrice, error) {
	stub.priceCalls++
	return stub.prices, stub.priceError
}
func (stub *monitorCollectionStub) Balance(context.Context, model.UpstreamMonitor, string) (float64, error) {
	stub.balanceCalls++
	return 5, nil
}
func (*monitorCollectionStub) HasActiveSubscription(context.Context, model.UpstreamMonitor, string) (bool, error) {
	return false, nil
}

func TestUpstreamMonitorCollectionDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{
		{"sqlite", ""}, {"mysql", "TEST_UPSTREAM_BALANCE_MYSQL_DSN"}, {"postgres", "TEST_UPSTREAM_BALANCE_POSTGRES_DSN"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			var driver gorm.Dialector
			if dialect.name == "sqlite" {
				driver = sqlite.Open(":memory:")
			} else {
				dsn := os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip("task-owned test DSN not configured")
				}
				if dialect.name == "mysql" {
					driver = mysql.Open(dsn)
				} else {
					driver = postgres.Open(dsn)
				}
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("collection_%d_", time.Now().UnixNano())}})
			require.NoError(t, err)
			connection, err := db.DB()
			require.NoError(t, err)
			connection.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, connection.Close()) })
			previous := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = previous })
			for _, table := range []any{&model.Channel{}, &model.UpstreamMonitor{}, &model.UpstreamMonitorPolicy{}} {
				require.NoError(t, db.AutoMigrate(table))
				t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
			}
			t.Setenv("UPSTREAM_MONITOR_ENCRYPTION_KEY", "synthetic-collection-test-key-with-at-least-32-characters")
			secret, err := EncryptMonitorSecret("synthetic-account-token")
			require.NoError(t, err)
			require.NoError(t, db.Create(&model.Channel{Id: 1, Type: 1, Models: "gpt-x", ModelMapping: common.GetPointer(""), Group: "default", Status: 1}).Error)
			policy := model.UpstreamMonitorPolicy{Enabled: true, PriceIntervalMinutes: 60, BalanceIntervalMinutes: 10, SpikePercent: 30}
			require.NoError(t, model.SaveUpstreamMonitorPolicy(policy))
			now := time.Now().Unix()
			monitor := model.UpstreamMonitor{ChannelID: 1, Enabled: true, Platform: "newapi", UserID: "1", Secret: secret, LastPriceAt: now, LastBalanceAt: now, LastPrices: `[{"model":"gpt-x","raw_model_ratio":9}]`}
			require.NoError(t, db.Create(&monitor).Error)
			stub := &monitorCollectionStub{prices: []MonitorPrice{{Model: "gpt-x", Mode: "token", RawModelRatio: 1.25, Comparable: true, Input: 2.5, Output: 10}}}
			adapterFor := func(string) (monitorAdapter, error) { return stub, nil }
			result, err := runUpstreamMonitor(context.Background(), "scheduled", MonitorRunOptions{}, adapterFor)
			require.NoError(t, err)
			assert.Zero(t, result.Prices)
			assert.Zero(t, stub.priceCalls)
			assert.Zero(t, stub.balanceCalls)
			result, err = runUpstreamMonitor(context.Background(), "manual", MonitorRunOptions{Force: true}, adapterFor)
			require.NoError(t, err)
			assert.Equal(t, 1, result.Prices)
			assert.Equal(t, 1, result.Balances)
			assert.Zero(t, result.Errors)
			assert.Zero(t, result.Groups, "collection does not enable repricing")
			assert.Equal(t, 1, stub.priceCalls)
			assert.Equal(t, 1, stub.balanceCalls)
			trusted, err := model.GetUpstreamMonitor(1)
			require.NoError(t, err)
			assert.Contains(t, trusted.LastPrices, `"raw_model_ratio":1.25`)
			for _, failure := range []error{nil, errors.New("upstream monitor returned HTTP 401")} {
				stub.prices, stub.priceError = nil, failure
				result, err = runUpstreamMonitor(context.Background(), "failed-manual", MonitorRunOptions{Force: true}, adapterFor)
				require.NoError(t, err)
				assert.Zero(t, result.Prices)
				require.Len(t, result.Issues, 1)
				assert.Equal(t, "prices", result.Issues[0].Phase)
				assert.Equal(t, 1, result.Issues[0].ChannelID)
				saved, err := model.GetUpstreamMonitor(1)
				require.NoError(t, err)
				assert.Equal(t, trusted.LastPrices, saved.LastPrices, "failed or incomplete samples must not replace trusted prices")
				assert.Equal(t, trusted.LastPriceAt, saved.LastPriceAt)
				assert.NotEmpty(t, saved.LastError)
			}
			policy.Enabled = false
			require.NoError(t, model.SaveUpstreamMonitorPolicy(policy))
			_, err = runUpstreamMonitor(context.Background(), "disabled", MonitorRunOptions{Force: true}, adapterFor)
			assert.ErrorContains(t, err, "disabled")
		})
	}
}

func TestUpstreamMonitorBalanceRoundDoesNotCountAsPriceRound(t *testing.T) {
	require.False(t, monitorPriceRoundComplete(nil))
	require.False(t, monitorPriceRoundComplete([]monitorSample{{priceAttempted: true}, {priceAttempted: false}}))
	require.True(t, monitorPriceRoundComplete([]monitorSample{{priceAttempted: true}, {priceAttempted: true}}))
}

func TestUpstreamMonitorRejectsNonPublicTarget(t *testing.T) {
	for _, target := range []string{"http://example.com", "https://127.0.0.1", "https://169.254.169.254", "https://example.com:8080"} {
		require.Error(t, ValidateUpstreamMonitorBaseURL(target), target)
	}
}

func TestUpstreamMonitorRejectsInvalidCost(t *testing.T) {
	require.False(t, validMonitorPrice(MonitorPrice{Mode: "token", Input: math.NaN()}))
	require.False(t, validMonitorPrice(MonitorPrice{Mode: "token", Input: -1}))
}

func TestUpstreamMonitorSecretEncryption(t *testing.T) {
	t.Setenv("UPSTREAM_MONITOR_ENCRYPTION_KEY", "test-only-secret-with-at-least-32-characters")
	encrypted, err := EncryptMonitorSecret("sensitive-token")
	require.NoError(t, err)
	require.NotContains(t, encrypted, "sensitive-token")
	plain, err := DecryptMonitorSecret(encrypted)
	require.NoError(t, err)
	require.Equal(t, "sensitive-token", plain)
}

type upstreamBalanceTestTransport struct {
	status     string
	self       string
	selfStatus int
	paths      []string
	beforeSelf func()
}

func (transport *upstreamBalanceTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.paths = append(transport.paths, request.URL.Path)
	if request.URL.Path == "/api/status" {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(transport.status)), Header: make(http.Header)}, nil
	}
	if request.URL.Path != "/api/user/self" || request.Header.Get("Authorization") != "Bearer nap_test_account_token" || request.Header.Get("New-Api-User") != "1" {
		return nil, fmt.Errorf("unexpected balance request")
	}
	if transport.beforeSelf != nil {
		transport.beforeSelf()
	}
	status := transport.selfStatus
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(transport.self)), Header: make(http.Header)}, nil
}

func TestConfiguredNewAPIAccountBalanceDatabaseMatrix(t *testing.T) {
	// Network DSNs must point at task-owned test databases, never deployment DBs.
	for _, dialect := range []struct{ name, env string }{
		{name: "sqlite"},
		{name: "mysql", env: "TEST_UPSTREAM_BALANCE_MYSQL_DSN"},
		{name: "postgres", env: "TEST_UPSTREAM_BALANCE_POSTGRES_DSN"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			var driver gorm.Dialector
			databaseType := common.DatabaseTypeSQLite
			if dialect.name == "sqlite" {
				driver = sqlite.Open(":memory:")
			} else {
				dsn := os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip("task-owned test DSN not configured")
				}
				if dialect.name == "mysql" {
					driver = mysql.Open(dsn)
					databaseType = common.DatabaseTypeMySQL
				} else {
					driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
					databaseType = common.DatabaseTypePostgreSQL
				}
			}
			db, err := gorm.Open(driver, &gorm.Config{
				NamingStrategy: schema.NamingStrategy{TablePrefix: fmt.Sprintf("balance_%d_", time.Now().UnixNano())},
				Logger:         logger.Default.LogMode(logger.Silent),
			})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			previousDB, previousDatabaseType := model.DB, common.MainDatabaseType()
			model.DB = db
			common.SetMainDatabaseType(databaseType)
			t.Cleanup(func() { model.DB = previousDB; common.SetMainDatabaseType(previousDatabaseType) })
			for _, table := range []any{&model.Channel{}, &model.UpstreamMonitor{}} {
				require.NoError(t, db.AutoMigrate(table))
				t.Cleanup(func() { require.NoError(t, db.Migrator().DropTable(table)) })
			}
			var version string
			versionQuery := "SELECT version()"
			if dialect.name == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)
			t.Setenv("UPSTREAM_MONITOR_ENCRYPTION_KEY", "synthetic-balance-test-key-with-32-characters")
			secret, err := EncryptMonitorSecret("nap_test_account_token")
			require.NoError(t, err)
			for _, test := range []struct {
				name        string
				unit        string
				self        string
				selfStatus  int
				wantBalance float64
				wantError   bool
			}{
				{name: "real account not unlimited API key", unit: "500000", self: `{"success":true,"data":{"quota":1323874798}}`, wantBalance: 2647.749596},
				{name: "zero account balance is saved", unit: "500000", self: `{"success":true,"data":{"quota":0}}`},
				{name: "upstream quota unit not local display rate", unit: "1000000", self: `{"success":true,"data":{"quota":10000000}}`, wantBalance: 10},
				{name: "expired account token", unit: "500000", selfStatus: http.StatusUnauthorized, wantError: true},
				{name: "missing profile read scope", unit: "500000", selfStatus: http.StatusForbidden, wantError: true},
				{name: "unsuccessful account response", unit: "500000", self: `{"success":false,"data":{"quota":5000000}}`, wantError: true},
				{name: "missing account quota", unit: "500000", self: `{"success":true,"data":{}}`, wantError: true},
				{name: "negative account quota", unit: "500000", self: `{"success":true,"data":{"quota":-1}}`, wantError: true},
				{name: "zero quota unit", unit: "0", wantError: true},
				{name: "conversion overflow", unit: "5e-324", self: `{"success":true,"data":{"quota":5000000}}`, wantError: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					channel := model.Channel{Key: "unlimited-generation-key", Balance: 100000000, BalanceUpdatedTime: 123, Status: common.ChannelStatusEnabled}
					require.NoError(t, db.Create(&channel).Error)
					monitor := model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", BaseURL: "https://93.184.216.34", UserID: "1", Secret: secret,
						LastBalance: 7, LastBalanceAt: 123, LastError: "unrelated price error", LastPrices: "preserved", ProbeFailures: 3}
					require.NoError(t, db.Create(&monitor).Error)
					transport := &upstreamBalanceTestTransport{status: fmt.Sprintf(`{"success":true,"data":{"quota_per_unit":%s,"usd_exchange_rate":7.3,"quota_display_type":"CNY"}}`, test.unit), self: test.self, selfStatus: test.selfStatus}
					// Both monitor.Enabled and the global policy remain false/absent.
					balance, configured, err := queryConfiguredNewAPIAccountBalance(context.Background(), channel.Id, newAPIAdapter{client: &http.Client{Transport: transport}})
					assert.True(t, configured)
					if test.wantError {
						require.Error(t, err)
						assert.NotContains(t, err.Error(), "nap_test_account_token")
						assert.Zero(t, balance)
					} else {
						require.NoError(t, err)
						assert.InDelta(t, test.wantBalance, balance, 1e-9)
						assert.Equal(t, []string{"/api/status", "/api/user/self"}, transport.paths)
					}
					var savedChannel model.Channel
					require.NoError(t, db.First(&savedChannel, channel.Id).Error)
					var savedMonitor model.UpstreamMonitor
					require.NoError(t, db.First(&savedMonitor, "channel_id = ?", channel.Id).Error)
					assert.Equal(t, common.ChannelStatusEnabled, savedChannel.Status)
					assert.Equal(t, monitor.Secret, savedMonitor.Secret)
					assert.Equal(t, monitor.LastError, savedMonitor.LastError)
					assert.Equal(t, monitor.LastPrices, savedMonitor.LastPrices)
					assert.Equal(t, monitor.ProbeFailures, savedMonitor.ProbeFailures)
					if test.wantError {
						assert.Equal(t, channel.Balance, savedChannel.Balance)
						assert.Equal(t, channel.BalanceUpdatedTime, savedChannel.BalanceUpdatedTime)
						assert.Equal(t, monitor.LastBalance, savedMonitor.LastBalance)
						assert.Equal(t, monitor.LastBalanceAt, savedMonitor.LastBalanceAt)
					} else {
						assert.InDelta(t, test.wantBalance, savedChannel.Balance, 1e-9)
						assert.Equal(t, savedChannel.Balance, savedMonitor.LastBalance)
						assert.Greater(t, savedChannel.BalanceUpdatedTime, int64(123))
						assert.Equal(t, savedChannel.BalanceUpdatedTime, savedMonitor.LastBalanceAt)
						// Repeating the same query must succeed even with zero/no-op updates.
						_, _, err = queryConfiguredNewAPIAccountBalance(context.Background(), channel.Id, newAPIAdapter{client: &http.Client{Transport: transport}})
						require.NoError(t, err)
					}
				})
			}
			t.Run("unconfigured and other platforms keep original path", func(t *testing.T) {
				for _, platform := range []string{"", "sub2api"} {
					channel := model.Channel{Balance: 100000000}
					require.NoError(t, db.Create(&channel).Error)
					if platform != "" {
						require.NoError(t, db.Create(&model.UpstreamMonitor{ChannelID: channel.Id, Platform: platform}).Error)
					}
					transport := &upstreamBalanceTestTransport{}
					_, configured, err := queryConfiguredNewAPIAccountBalance(context.Background(), channel.Id, newAPIAdapter{client: &http.Client{Transport: transport}})
					require.NoError(t, err)
					assert.False(t, configured)
					assert.Empty(t, transport.paths)
				}
			})
			t.Run("bad credentials never fall back", func(t *testing.T) {
				for _, credentials := range []struct{ userID, secret string }{{"", secret}, {"0", secret}, {"1", ""}, {"1", "invalid-ciphertext"}} {
					channel := model.Channel{Balance: 100000000}
					require.NoError(t, db.Create(&channel).Error)
					require.NoError(t, db.Create(&model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", UserID: credentials.userID, Secret: credentials.secret}).Error)
					transport := &upstreamBalanceTestTransport{}
					_, configured, err := queryConfiguredNewAPIAccountBalance(context.Background(), channel.Id, newAPIAdapter{client: &http.Client{Transport: transport}})
					require.Error(t, err)
					assert.True(t, configured)
					assert.Empty(t, transport.paths)
				}
			})
			t.Run("credential rotation during query cannot overwrite balance", func(t *testing.T) {
				channel := model.Channel{Balance: 100000000}
				require.NoError(t, db.Create(&channel).Error)
				monitor := model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", BaseURL: "https://93.184.216.34", UserID: "1", Secret: secret, LastBalance: 7}
				require.NoError(t, db.Create(&monitor).Error)
				transport := &upstreamBalanceTestTransport{status: `{"success":true,"data":{"quota_per_unit":500000}}`, self: `{"success":true,"data":{"quota":5000000}}`, beforeSelf: func() {
					require.NoError(t, db.Model(&monitor).Update("secret", "rotated-ciphertext").Error)
				}}
				_, configured, err := queryConfiguredNewAPIAccountBalance(context.Background(), channel.Id, newAPIAdapter{client: &http.Client{Transport: transport}})
				require.Error(t, err)
				assert.True(t, configured)
				var savedChannel model.Channel
				require.NoError(t, db.First(&savedChannel, channel.Id).Error)
				assert.Equal(t, channel.Balance, savedChannel.Balance)
				var savedMonitor model.UpstreamMonitor
				require.NoError(t, db.First(&savedMonitor, "channel_id = ?", channel.Id).Error)
				assert.Equal(t, "rotated-ciphertext", savedMonitor.Secret)
				assert.Equal(t, monitor.LastBalance, savedMonitor.LastBalance)
			})
			t.Run("old monitor and settings snapshots preserve newer balance", func(t *testing.T) {
				channel := model.Channel{Balance: 100000000}
				require.NoError(t, db.Create(&channel).Error)
				original := model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", BaseURL: "https://93.184.216.34", UserID: "1", Secret: secret, LastBalance: 7, LastBalanceAt: 123}
				require.NoError(t, db.Create(&original).Error)
				require.NoError(t, model.RecordUpstreamAccountBalance(original, 10))
				oldSample := original
				oldSample.LastBalance = 8
				oldSample.LastPrices = "new-price-sample"
				require.NoError(t, model.SaveUpstreamMonitor(oldSample, original))
				oldSettings := original
				oldSettings.WarningBalance = 2
				require.NoError(t, model.SaveUpstreamMonitor(oldSettings, original))
				var savedMonitor model.UpstreamMonitor
				require.NoError(t, db.First(&savedMonitor, "channel_id = ?", channel.Id).Error)
				assert.Equal(t, 10.0, savedMonitor.LastBalance)
				assert.Greater(t, savedMonitor.LastBalanceAt, int64(123))
				assert.Equal(t, 2.0, savedMonitor.WarningBalance)
				assert.Equal(t, "new-price-sample", savedMonitor.LastPrices)
				var savedChannel model.Channel
				require.NoError(t, db.First(&savedChannel, channel.Id).Error)
				assert.Equal(t, savedChannel.Balance, savedMonitor.LastBalance)
				assert.Equal(t, savedChannel.BalanceUpdatedTime, savedMonitor.LastBalanceAt)
			})
			t.Run("deleted channel cannot save an orphan balance", func(t *testing.T) {
				channel := model.Channel{Balance: 100000000}
				require.NoError(t, db.Create(&channel).Error)
				monitor := model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", LastBalance: 7, LastBalanceAt: 123}
				require.NoError(t, db.Create(&monitor).Error)
				require.NoError(t, db.Delete(&channel).Error)
				require.Error(t, model.RecordUpstreamAccountBalance(monitor, 10))
				var savedMonitor model.UpstreamMonitor
				require.NoError(t, db.First(&savedMonitor, "channel_id = ?", channel.Id).Error)
				assert.Equal(t, monitor.LastBalance, savedMonitor.LastBalance)
				assert.Equal(t, monitor.LastBalanceAt, savedMonitor.LastBalanceAt)
			})
			t.Run("new monitor creation and changed credentials invalidate samples", func(t *testing.T) {
				channel := model.Channel{}
				require.NoError(t, db.Create(&channel).Error)
				original := model.UpstreamMonitor{ChannelID: channel.Id, Platform: "newapi", BaseURL: "https://93.184.216.34", UserID: "1", Secret: secret}
				require.NoError(t, model.SaveUpstreamMonitor(original, model.UpstreamMonitor{}))
				require.Error(t, model.SaveUpstreamMonitor(original, model.UpstreamMonitor{}), "another settings request cannot overwrite an existing configuration")
				require.NoError(t, model.RecordUpstreamAccountBalance(original, 10))
				rotated := original
				rotated.Secret = "rotated-ciphertext"
				require.NoError(t, model.SaveUpstreamMonitor(rotated, original))
				var savedMonitor model.UpstreamMonitor
				require.NoError(t, db.First(&savedMonitor, "channel_id = ?", channel.Id).Error)
				assert.Equal(t, rotated.Secret, savedMonitor.Secret)
				assert.Zero(t, savedMonitor.LastBalance)
				assert.Zero(t, savedMonitor.LastBalanceAt)
				require.Error(t, model.SaveUpstreamMonitor(original, original), "a worker holding old credentials cannot undo the rotation")
			})
		})
	}
}

func TestUpstreamMonitorPriceDecision(t *testing.T) {
	apply, count, spike := decideMonitorPrice(1, 1.2, 30, 0)
	require.True(t, apply)
	require.Zero(t, count)
	require.False(t, spike)
	apply, count, spike = decideMonitorPrice(1, 1.4, 30, 0)
	require.False(t, apply)
	require.True(t, spike)
	apply, count, spike = decideMonitorPrice(1, 0.8, 30, 0)
	require.False(t, apply)
	require.Equal(t, 1, count)
	apply, count, spike = decideMonitorPrice(1, 0.8, 30, count)
	require.True(t, apply)
	require.Equal(t, 2, count)
	require.False(t, spike)
}

func TestUpstreamMonitorDoesNotDisableMultiKeyOrSubscription(t *testing.T) {
	policy := model.UpstreamMonitorPolicy{AutoDisableBalance: true}
	multiKey := &model.Channel{Id: 1, Status: common.ChannelStatusEnabled, ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	monitor := &model.UpstreamMonitor{ChannelID: 1}
	for range 2 {
		updateMonitorBalance(context.Background(), policy, monitor, multiKey, 0, monitorSubscriptionStub{}, "", nil, false)
	}
	require.Equal(t, common.ChannelStatusEnabled, multiKey.Status)
	require.False(t, monitor.AutoDisabled)
	require.Equal(t, 2, monitor.ZeroBalanceChecks)

	singleKey := &model.Channel{Id: 2, Status: common.ChannelStatusEnabled}
	monitor = &model.UpstreamMonitor{ChannelID: 2}
	for range 2 {
		updateMonitorBalance(context.Background(), policy, monitor, singleKey, 0, monitorSubscriptionStub{active: true}, "", nil, false)
	}
	require.Equal(t, common.ChannelStatusEnabled, singleKey.Status)
	require.False(t, monitor.AutoDisabled)
}
