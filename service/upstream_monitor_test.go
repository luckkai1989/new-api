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
	"math"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
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
	require.False(t, monitorPricesComplete(channel, prices))
	prices = append(prices, MonitorPrice{Model: "gpt-y", Mode: "token", Comparable: false})
	require.False(t, monitorPricesComplete(channel, prices))
	prices[1].Comparable = true
	require.True(t, monitorPricesComplete(channel, prices))
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
