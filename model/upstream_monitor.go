/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

// UpstreamMonitor is deliberately separate from Channel so upstream merges do
// not need to know about monitor credentials or sampling state.
type UpstreamMonitor struct {
	ChannelID         int     `json:"channel_id" gorm:"primaryKey"`
	Enabled           bool    `json:"enabled"`
	Platform          string  `json:"platform" gorm:"type:varchar(32)"`
	BaseURL           string  `json:"base_url" gorm:"type:varchar(1024)"`
	UpstreamGroup     string  `json:"upstream_group" gorm:"type:varchar(128)"`
	UserID            string  `json:"user_id" gorm:"type:varchar(128)"`
	Secret            string  `json:"-" gorm:"type:text"`
	WarningBalance    float64 `json:"warning_balance"`
	LastPriceAt       int64   `json:"last_price_at"`
	LastBalanceAt     int64   `json:"last_balance_at"`
	LastBalance       float64 `json:"last_balance"`
	LastError         string  `json:"last_error" gorm:"type:text"`
	LastPrices        string  `json:"last_prices" gorm:"type:text"`
	BalanceState      string  `json:"balance_state" gorm:"type:varchar(32)"`
	ZeroBalanceChecks int     `json:"zero_balance_checks"`
	ProbeState        string  `json:"probe_state" gorm:"type:varchar(32)"`
	ProbeFailures     int     `json:"probe_failures"`
	ChannelState      string  `json:"channel_state" gorm:"type:varchar(32)"`
	LastRouting       string  `json:"last_routing" gorm:"type:varchar(512)"`
	AutoDisabled      bool    `json:"auto_disabled"`
	UpdatedAt         int64   `json:"updated_at"`
}

type UpstreamMonitorPolicy struct {
	ID                     int     `json:"-" gorm:"primaryKey"`
	Enabled                bool    `json:"enabled"`
	AutoPrice              bool    `json:"auto_price"`
	AutoDisableBalance     bool    `json:"auto_disable_balance"`
	PriceIntervalMinutes   int     `json:"price_interval_minutes"`
	BalanceIntervalMinutes int     `json:"balance_interval_minutes"`
	SpikePercent           float64 `json:"spike_percent"`
	DingTalkWebhook        string  `json:"-" gorm:"type:text"`
	DingTalkKeyword        string  `json:"dingtalk_keyword" gorm:"type:varchar(128)"`
}

type UpstreamMonitorGroupState struct {
	Group             string  `json:"group" gorm:"primaryKey;type:varchar(128)"`
	PendingLowerRatio float64 `json:"pending_lower_ratio"`
	ConsecutiveLower  int     `json:"consecutive_lower"`
	LastAlertState    string  `json:"last_alert_state" gorm:"type:varchar(128)"`
	LastAlertAt       int64   `json:"last_alert_at"`
	UpdatedAt         int64   `json:"updated_at"`
}

func GetUpstreamMonitorPolicy() (UpstreamMonitorPolicy, error) {
	var policy UpstreamMonitorPolicy
	err := DB.First(&policy, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return UpstreamMonitorPolicy{ID: 1, PriceIntervalMinutes: 60, BalanceIntervalMinutes: 10, SpikePercent: 30}, nil
	}
	return policy, err
}

func SaveUpstreamMonitorPolicy(policy UpstreamMonitorPolicy) error {
	policy.ID = 1
	if policy.PriceIntervalMinutes < 5 || policy.BalanceIntervalMinutes < 1 || policy.SpikePercent < 0 || policy.SpikePercent > 1000 || math.IsNaN(policy.SpikePercent) {
		return errors.New("invalid monitor interval or spike threshold")
	}
	return DB.Save(&policy).Error
}

func GetUpstreamMonitor(channelID int) (UpstreamMonitor, error) {
	var monitor UpstreamMonitor
	err := DB.First(&monitor, "channel_id = ?", channelID).Error
	return monitor, err
}

func ListEnabledUpstreamMonitors() ([]UpstreamMonitor, error) {
	var monitors []UpstreamMonitor
	err := DB.Where("enabled = ?", true).Find(&monitors).Error
	return monitors, err
}

func SaveUpstreamMonitor(monitor UpstreamMonitor) error {
	monitor.UpdatedAt = time.Now().Unix()
	return DB.Save(&monitor).Error
}

// CompareAndSwapGroupRatio replaces only the value read by the monitor. A
// concurrent administrator edit fails the update and is never overwritten.
func CompareAndSwapGroupRatio(group string, expected, next float64) (bool, error) {
	if group == "" || next < 0 || math.IsNaN(next) || math.IsInf(next, 0) {
		return false, errors.New("invalid group ratio")
	}
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var option Option
		if err := tx.Where(map[string]any{"key": "GroupRatio"}).First(&option).Error; err != nil {
			return err
		}
		var ratios map[string]float64
		if err := json.Unmarshal([]byte(option.Value), &ratios); err != nil {
			return err
		}
		current, ok := ratios[group]
		if !ok || math.Abs(current-expected) > 1e-9 {
			return nil
		}
		ratios[group] = next
		data, err := json.Marshal(ratios)
		if err != nil {
			return err
		}
		result := tx.Model(&Option{}).Where(map[string]any{"key": "GroupRatio", "value": option.Value}).Update("value", string(data))
		if result.Error != nil {
			return result.Error
		}
		changed = result.RowsAffected == 1
		return nil
	})
	if err != nil || !changed {
		return false, err
	}
	var latest Option
	if err := DB.Where(map[string]any{"key": "GroupRatio"}).First(&latest).Error; err != nil {
		return false, err
	}
	if err := updateOptionMap("GroupRatio", latest.Value); err != nil {
		return false, fmt.Errorf("persisted ratio but failed to refresh cache: %w", err)
	}
	InvalidatePricingCache()
	return true, nil
}

func HasGroupRatioOverride(group string) (bool, error) {
	var option Option
	if err := DB.Where(map[string]any{"key": "GroupGroupRatio"}).First(&option).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			for _, groups := range ratio_setting.GetGroupRatioSetting().GroupGroupRatio.ReadAll() {
				if _, ok := groups[group]; ok {
					return true, nil
				}
			}
			return false, nil
		}
		return false, err
	}
	var overrides map[string]map[string]float64
	if err := json.Unmarshal([]byte(option.Value), &overrides); err != nil {
		return false, err
	}
	for _, groups := range overrides {
		if _, ok := groups[group]; ok {
			return true, nil
		}
	}
	return false, nil
}

func GetPersistedGroupRatio(group string) (float64, error) {
	var option Option
	if err := DB.Where(map[string]any{"key": "GroupRatio"}).First(&option).Error; err != nil {
		return 0, err
	}
	var ratios map[string]float64
	if err := json.Unmarshal([]byte(option.Value), &ratios); err != nil {
		return 0, err
	}
	value, ok := ratios[group]
	if !ok {
		return 0, fmt.Errorf("group %q not configured", group)
	}
	return value, nil
}

func ChannelGroupNames(channel *Channel) []string {
	var groups []string
	for _, group := range strings.Split(channel.Group, ",") {
		group = strings.TrimSpace(group)
		if group != "" && ratio_setting.ContainsGroupRatio(group) {
			groups = append(groups, group)
		}
	}
	return groups
}
