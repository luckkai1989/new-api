/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

const monitorDisableReason = "upstream monitor: balance exhausted"

type monitorSample struct {
	channel        model.Channel
	monitor        model.UpstreamMonitor
	prices         []MonitorPrice
	valid          bool
	priceAttempted bool
}

type MonitorRunSummary struct {
	Channels int `json:"channels"`
	Prices   int `json:"prices"`
	Balances int `json:"balances"`
	Errors   int `json:"errors"`
	Groups   int `json:"groups"`
}

// RunUpstreamMonitor is called only from the leased system task. It never
// changes prices unless the operator explicitly enables AutoPrice.
func RunUpstreamMonitor(ctx context.Context) (MonitorRunSummary, error) {
	summary := MonitorRunSummary{}
	policy, err := model.GetUpstreamMonitorPolicy()
	if err != nil || !policy.Enabled {
		return summary, err
	}
	monitors, err := model.ListEnabledUpstreamMonitors()
	if err != nil {
		return summary, err
	}
	now := time.Now().Unix()
	priceRoundDue := false
	for _, monitor := range monitors {
		if now-monitor.LastPriceAt >= int64(policy.PriceIntervalMinutes*60) {
			priceRoundDue = true
			break
		}
	}
	samples := make([]monitorSample, 0, len(monitors))
	for _, m := range monitors {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		channel, err := model.GetChannelById(m.ChannelID, true)
		if err != nil {
			summary.Errors++
			continue
		}
		summary.Channels++
		s := monitorSample{channel: *channel, monitor: m}
		adapter, err := newMonitorAdapter(m.Platform)
		if err != nil {
			summary.Errors++
			continue
		}
		secret, err := DecryptMonitorSecret(m.Secret)
		if err != nil {
			summary.Errors++
			m.LastError = "monitor secret could not be decrypted"
			_ = model.SaveUpstreamMonitor(m)
			continue
		}
		priceDue := priceRoundDue
		balanceDue := now-m.LastBalanceAt >= int64(policy.BalanceIntervalMinutes*60)
		probeFailed := false
		if priceDue {
			s.priceAttempted = true
			prices, err := adapter.Prices(ctx, m, secret)
			if err == nil && !monitorPricesComplete(channel, prices) {
				err = errors.New("upstream pricing is incomplete for this channel")
			}
			if err != nil {
				m.LastError = err.Error()
				summary.Errors++
				probeFailed = true
			} else {
				data, _ := json.Marshal(prices)
				m.LastPrices, m.LastPriceAt, m.LastError = string(data), now, ""
				s.prices, s.valid = prices, true
				summary.Prices++
			}
		}
		if balanceDue {
			balance, err := adapter.Balance(ctx, m, secret)
			if err != nil {
				m.LastError = err.Error()
				summary.Errors++
				probeFailed = true
			} else {
				m.LastBalance, m.LastBalanceAt = balance, now
				summary.Balances++
				updateMonitorBalance(ctx, policy, &m, channel, balance, adapter, secret, s.prices, s.valid)
			}
		}
		if priceDue || balanceDue {
			if probeFailed {
				m.ProbeFailures++
				if m.ProbeFailures >= 2 && m.ProbeState != "unavailable" {
					if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 上游监控连续失败", channel.Id)); err == nil {
						m.ProbeState = "unavailable"
					}
				}
			} else {
				m.ProbeFailures = 0
				if m.ProbeState == "unavailable" {
					if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 上游监控恢复", channel.Id)); err == nil {
						m.ProbeState = "healthy"
					}
				} else {
					m.ProbeState = "healthy"
				}
			}
		}
		if m.ChannelState != channelMonitorState(channel) {
			state := channelMonitorState(channel)
			if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 状态变为 %s", channel.Id, state)); err == nil {
				m.ChannelState = state
			}
		}
		routing := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d", channel.Group, channel.Models, channel.GetPriority(), channel.GetWeight()))))
		if m.LastRouting != "" && m.LastRouting != routing {
			if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 路由配置已调整", channel.Id)); err == nil {
				m.LastRouting = routing
			}
		} else {
			m.LastRouting = routing
		}
		if err := model.SaveUpstreamMonitor(m); err != nil {
			summary.Errors++
			s.valid = false
		}
		s.monitor = m
		samples = append(samples, s)
	}
	if policy.AutoPrice {
		summary.Groups = repriceMonitoredGroups(ctx, policy, samples)
	}
	return summary, nil
}

func monitorPricesComplete(channel *model.Channel, prices []MonitorPrice) bool {
	models := monitorChannelModels(channel)
	if len(models) == 0 {
		return false
	}
	for _, upstream := range models {
		price, ok := findMonitorPrice(prices, upstream)
		if !ok || !price.Comparable {
			return false
		}
	}
	return true
}

func channelMonitorState(channel *model.Channel) string {
	if channel.Status == common.ChannelStatusEnabled {
		return "available"
	}
	return "unavailable"
}

func updateMonitorBalance(ctx context.Context, policy model.UpstreamMonitorPolicy, monitor *model.UpstreamMonitor, channel *model.Channel, balance float64, adapter monitorAdapter, secret string, prices []MonitorPrice, priceFresh bool) {
	state := "normal"
	if balance <= 0 {
		state = "exhausted"
		monitor.ZeroBalanceChecks++
	} else if monitor.WarningBalance > 0 && balance <= monitor.WarningBalance {
		state = "low"
		monitor.ZeroBalanceChecks = 0
	} else {
		monitor.ZeroBalanceChecks = 0
	}
	if state != monitor.BalanceState {
		if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 余额状态: %s (%.4f USD)", channel.Id, state, balance)); err == nil {
			monitor.BalanceState = state
		}
	}
	if channel.ChannelInfo.IsMultiKey || !policy.AutoDisableBalance {
		return
	}
	if state == "exhausted" && channel.Status == common.ChannelStatusEnabled {
		if monitor.ZeroBalanceChecks < 2 {
			return
		}
		active, err := adapter.HasActiveSubscription(ctx, *monitor, secret)
		if err != nil || active {
			return
		}
		if model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusAutoDisabled, monitorDisableReason) {
			monitor.AutoDisabled = true
			channel.Status = common.ChannelStatusAutoDisabled
			CloseActiveWebSocketsForChannel(channel.Id, ChannelDisabledCloseReason)
			_ = sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 因余额耗尽自动下线", channel.Id))
		}
	} else if state != "exhausted" && monitor.AutoDisabled {
		if policy.AutoPrice && (!priceFresh || !monitorPricesCovered(channel, prices)) {
			monitor.LastPriceAt = 0
			return
		}
		fresh, err := model.GetChannelById(channel.Id, true)
		if err != nil || fresh.Status != common.ChannelStatusAutoDisabled || fresh.GetOtherInfo()["status_reason"] != monitorDisableReason {
			monitor.AutoDisabled = false
			return
		}
		if probeMonitorChannel(ctx, fresh) && model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusEnabled, "") {
			monitor.AutoDisabled = false
			channel.Status = common.ChannelStatusEnabled
			_ = sendMonitorAlert(ctx, policy, fmt.Sprintf("渠道 #%d 余额恢复且连接测试通过，已自动上线", channel.Id))
		}
	}
}

func monitorPricesCovered(channel *model.Channel, prices []MonitorPrice) bool {
	groups := model.ChannelGroupNames(channel)
	models := monitorChannelModels(channel)
	if len(groups) == 0 || len(models) == 0 {
		return false
	}
	for _, group := range groups {
		current, err := model.GetPersistedGroupRatio(group)
		if err != nil {
			return false
		}
		for local, upstream := range models {
			price, ok := findMonitorPrice(prices, upstream)
			if !ok || !price.Comparable {
				return false
			}
			required, err := requiredMonitorRatio(local, price)
			if err != nil || current+1e-6 < required {
				return false
			}
		}
	}
	return true
}

func probeMonitorChannel(ctx context.Context, channel *model.Channel) bool {
	base := strings.TrimSuffix(strings.TrimRight(channel.GetBaseURL(), "/"), "/v1")
	u, err := validateMonitorURL(base)
	if err != nil {
		return false
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+channel.Key)
	resp, err := monitorHTTPClient().Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func repriceMonitoredGroups(ctx context.Context, policy model.UpstreamMonitorPolicy, samples []monitorSample) int {
	groups := make(map[string][]monitorSample)
	all, err := model.GetAllChannels(0, 0, true, true)
	if err != nil {
		return 0
	}
	recovering := make(map[int]bool)
	for _, sample := range samples {
		if sample.monitor.AutoDisabled && sample.monitor.LastBalance > 0 && sample.valid {
			recovering[sample.channel.Id] = true
		}
	}
	expected := make(map[string]int)
	for _, channel := range all {
		if channel.Status != common.ChannelStatusEnabled && !recovering[channel.Id] {
			continue
		}
		for _, group := range model.ChannelGroupNames(channel) {
			expected[group]++
		}
	}
	for _, sample := range samples {
		if sample.channel.Status != common.ChannelStatusEnabled && !recovering[sample.channel.Id] {
			continue
		}
		for _, group := range model.ChannelGroupNames(&sample.channel) {
			groups[group] = append(groups[group], sample)
		}
	}
	changed := 0
	for group, members := range groups {
		if err := ctx.Err(); err != nil {
			break
		}
		if len(members) != expected[group] {
			state := model.UpstreamMonitorGroupState{Group: group}
			if err := model.DB.FirstOrCreate(&state, model.UpstreamMonitorGroupState{Group: group}).Error; err == nil {
				resetMonitorDecrease(&state)
				alertMonitorGroup(ctx, policy, &state, "enabled channel has no monitor")
			}
			continue
		}
		if repriceMonitorGroup(ctx, policy, group, members) {
			changed++
		}
	}
	return changed
}

func repriceMonitorGroup(ctx context.Context, policy model.UpstreamMonitorPolicy, group string, samples []monitorSample) bool {
	if !monitorPriceRoundComplete(samples) {
		return false
	}
	state := model.UpstreamMonitorGroupState{Group: group}
	if err := model.DB.FirstOrCreate(&state, model.UpstreamMonitorGroupState{Group: group}).Error; err != nil {
		return false
	}
	override, err := model.HasGroupRatioOverride(group)
	if err != nil {
		return false
	}
	if override {
		resetMonitorDecrease(&state)
		alertMonitorGroup(ctx, policy, &state, "user-specific override")
		return false
	}
	current, err := model.GetPersistedGroupRatio(group)
	if err != nil {
		return false
	}
	required := 0.0
	for _, sample := range samples {
		if !sample.valid {
			resetMonitorDecrease(&state)
			return false
		}
		models := monitorChannelModels(&sample.channel)
		if len(models) == 0 {
			resetMonitorDecrease(&state)
			return false
		}
		for local, upstream := range models {
			price, ok := findMonitorPrice(sample.prices, upstream)
			if !ok || !price.Comparable {
				resetMonitorDecrease(&state)
				alertMonitorGroup(ctx, policy, &state, "incomplete pricing")
				return false
			}
			ratio, err := requiredMonitorRatio(local, price)
			if err != nil {
				resetMonitorDecrease(&state)
				alertMonitorGroup(ctx, policy, &state, "uncomparable pricing")
				return false
			}
			required = math.Max(required, ratio)
		}
	}
	if required <= 0 || math.IsNaN(required) || math.IsInf(required, 0) {
		resetMonitorDecrease(&state)
		return false
	}
	apply, lowerCount, spike := decideMonitorPrice(current, required, policy.SpikePercent, state.ConsecutiveLower)
	if spike {
		resetMonitorDecrease(&state)
		alertMonitorGroup(ctx, policy, &state, "price spike; auto repricing paused, channel still serving")
		return false
	}
	state.ConsecutiveLower = lowerCount
	if math.Abs(required-current) < 1e-6 {
		state.LastAlertState = ""
		_ = model.DB.Save(&state).Error
		return false
	}
	if required < current {
		state.PendingLowerRatio = required
	}
	if !apply {
		_ = model.DB.Save(&state).Error
		return false
	}
	changed, err := model.CompareAndSwapGroupRatio(group, current, math.Ceil(required*1e6)/1e6)
	if err != nil || !changed {
		return false
	}
	state.ConsecutiveLower, state.LastAlertState, state.UpdatedAt = 0, "", time.Now().Unix()
	_ = model.DB.Save(&state).Error
	return true
}

func monitorPriceRoundComplete(samples []monitorSample) bool {
	if len(samples) == 0 {
		return false
	}
	for _, sample := range samples {
		if !sample.priceAttempted {
			return false
		}
	}
	return true
}

func decideMonitorPrice(current, required, spikePercent float64, consecutiveLower int) (apply bool, nextLower int, spike bool) {
	if required > current*(1+spikePercent/100) {
		return false, 0, true
	}
	if math.Abs(required-current) < 1e-6 {
		return false, 0, false
	}
	if required < current {
		consecutiveLower++
		return consecutiveLower >= 2, consecutiveLower, false
	}
	return true, 0, false
}

func resetMonitorDecrease(state *model.UpstreamMonitorGroupState) {
	if state.ConsecutiveLower == 0 {
		return
	}
	state.ConsecutiveLower, state.PendingLowerRatio = 0, 0
	_ = model.DB.Save(state).Error
}

func monitorChannelModels(channel *model.Channel) map[string]string {
	models := make(map[string]string)
	mapping := map[string]string{}
	if channel.ModelMapping != nil {
		if err := json.Unmarshal([]byte(*channel.ModelMapping), &mapping); err != nil {
			return nil
		}
	}
	for _, item := range strings.Split(channel.Models, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		upstream := item
		if mapped := strings.TrimSpace(mapping[item]); mapped != "" {
			upstream = mapped
		}
		models[item] = upstream
	}
	return models
}

func findMonitorPrice(prices []MonitorPrice, modelName string) (MonitorPrice, bool) {
	var matched MonitorPrice
	found := false
	for _, price := range prices {
		if price.Model == modelName {
			if !found {
				matched, found = price, true
				continue
			}
			if !price.Comparable || price.Mode != matched.Mode {
				matched.Comparable, matched.Reason = false, "conflicting upstream prices"
				continue
			}
			matched.Input = math.Max(matched.Input, price.Input)
			matched.Output = math.Max(matched.Output, price.Output)
			matched.CacheRead = math.Max(matched.CacheRead, price.CacheRead)
			matched.CacheWrite = math.Max(matched.CacheWrite, price.CacheWrite)
			matched.PerRequest = math.Max(matched.PerRequest, price.PerRequest)
		}
	}
	return matched, found
}

func requiredMonitorRatio(local string, upstream MonitorPrice) (float64, error) {
	if upstream.Mode == "fixed" {
		price, ok := ratio_setting.GetModelPrice(local, false)
		if !ok || price <= 0 {
			return 0, errors.New("local fixed price is missing")
		}
		return upstream.PerRequest / price, nil
	}
	if billing_setting.GetBillingMode(local) == billing_setting.BillingModeTieredExpr {
		return 0, errors.New("local tiered price")
	}
	if _, fixed := ratio_setting.GetModelPrice(local, false); fixed {
		return 0, errors.New("billing mode mismatch")
	}
	ratio, ok, _ := ratio_setting.GetModelRatio(local)
	if !ok || ratio <= 0 || common.QuotaPerUnit <= 0 {
		return 0, errors.New("local token price is missing")
	}
	base := ratio * 1e6 / common.QuotaPerUnit
	components := [][2]float64{{upstream.Input, base}, {upstream.Output, base * ratio_setting.GetCompletionRatio(local)}}
	cacheRead, _ := ratio_setting.GetCacheRatio(local)
	cacheWrite, _ := ratio_setting.GetCreateCacheRatio(local)
	components = append(components, [2]float64{upstream.CacheRead, base * cacheRead}, [2]float64{upstream.CacheWrite, base * cacheWrite})
	required := 0.0
	for _, pair := range components {
		if pair[0] > 0 && pair[1] <= 0 {
			return 0, errors.New("local component has zero price")
		}
		if pair[1] > 0 {
			required = math.Max(required, pair[0]/pair[1])
		}
	}
	return required, nil
}

func alertMonitorGroup(ctx context.Context, policy model.UpstreamMonitorPolicy, state *model.UpstreamMonitorGroupState, reason string) {
	if state.LastAlertState == reason {
		return
	}
	if err := sendMonitorAlert(ctx, policy, fmt.Sprintf("分组 %s 自动调价暂停: %s", state.Group, reason)); err != nil {
		return
	}
	state.LastAlertState, state.LastAlertAt = reason, time.Now().Unix()
	_ = model.DB.Save(state).Error
}

func sendMonitorAlert(ctx context.Context, policy model.UpstreamMonitorPolicy, message string) error {
	if policy.DingTalkWebhook == "" {
		return nil
	}
	webhook, err := DecryptMonitorSecret(policy.DingTalkWebhook)
	if err != nil {
		return err
	}
	u, err := url.Parse(webhook)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !strings.EqualFold(u.Hostname(), "oapi.dingtalk.com") || !strings.HasPrefix(u.Path, "/robot/send") {
		return errors.New("invalid DingTalk webhook URL")
	}
	body, _ := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": policy.DingTalkKeyword + " " + message}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := monitorHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("DingTalk HTTP %d", resp.StatusCode)
	}
	var result struct {
		ErrCode int `json:"errcode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if result.ErrCode != 0 {
		return errors.New("DingTalk rejected monitor alert")
	}
	return nil
}

// Existing status management may have disabled the channel for unrelated
// reasons. Only this exact persisted reason permits monitor recovery.
func MonitorOwnsDisabledChannel(channel *model.Channel) bool {
	return channel.Status == common.ChannelStatusAutoDisabled && channel.GetOtherInfo()["status_reason"] == monitorDisableReason
}
