/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package controller

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type monitorPolicyInput struct {
	Enabled                bool    `json:"enabled"`
	AutoPrice              bool    `json:"auto_price"`
	AutoDisableBalance     bool    `json:"auto_disable_balance"`
	PriceIntervalMinutes   int     `json:"price_interval_minutes"`
	BalanceIntervalMinutes int     `json:"balance_interval_minutes"`
	SpikePercent           float64 `json:"spike_percent"`
	DingTalkWebhook        string  `json:"dingtalk_webhook"`
	DingTalkKeyword        string  `json:"dingtalk_keyword"`
}

func GetUpstreamMonitorPolicy(c *gin.Context) {
	policy, err := model.GetUpstreamMonitorPolicy()
	if err != nil {
		c.JSON(500, gin.H{"success": false, "message": "monitor policy unavailable"})
		return
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{
		"enabled": policy.Enabled, "auto_price": policy.AutoPrice, "auto_disable_balance": policy.AutoDisableBalance,
		"price_interval_minutes": policy.PriceIntervalMinutes, "balance_interval_minutes": policy.BalanceIntervalMinutes,
		"spike_percent": policy.SpikePercent, "dingtalk_keyword": policy.DingTalkKeyword,
		"has_dingtalk_webhook": policy.DingTalkWebhook != "",
	}})
}

func PutUpstreamMonitorPolicy(c *gin.Context) {
	var input monitorPolicyInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid settings"})
		return
	}
	policy, err := model.GetUpstreamMonitorPolicy()
	if err != nil {
		c.JSON(500, gin.H{"success": false, "message": "monitor policy unavailable"})
		return
	}
	policy.Enabled, policy.AutoPrice, policy.AutoDisableBalance = input.Enabled, input.AutoPrice, input.AutoDisableBalance
	policy.PriceIntervalMinutes, policy.BalanceIntervalMinutes, policy.SpikePercent = input.PriceIntervalMinutes, input.BalanceIntervalMinutes, input.SpikePercent
	policy.DingTalkKeyword = strings.TrimSpace(input.DingTalkKeyword)
	if len(policy.DingTalkKeyword) > 128 || len(input.DingTalkWebhook) > 2048 {
		c.JSON(400, gin.H{"success": false, "message": "notification setting is too long"})
		return
	}
	if input.DingTalkWebhook != "" {
		if !strings.HasPrefix(input.DingTalkWebhook, "https://oapi.dingtalk.com/robot/send?") {
			c.JSON(400, gin.H{"success": false, "message": "invalid DingTalk webhook"})
			return
		}
		policy.DingTalkWebhook, err = service.EncryptMonitorSecret(input.DingTalkWebhook)
		if err != nil {
			c.JSON(400, gin.H{"success": false, "message": err.Error()})
			return
		}
	}
	if err := model.SaveUpstreamMonitorPolicy(policy); err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(200, gin.H{"success": true})
}

type monitorChannelInput struct {
	Enabled        bool    `json:"enabled"`
	Platform       string  `json:"platform"`
	BaseURL        string  `json:"base_url"`
	UpstreamGroup  string  `json:"upstream_group"`
	UserID         string  `json:"user_id"`
	Secret         string  `json:"secret"`
	WarningBalance float64 `json:"warning_balance"`
}

func monitorChannelID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id < 1 {
		c.JSON(400, gin.H{"success": false, "message": "invalid channel ID"})
		return 0, false
	}
	return id, true
}

func GetUpstreamMonitorChannel(c *gin.Context) {
	id, ok := monitorChannelID(c)
	if !ok {
		return
	}
	if _, err := model.GetChannelById(id, false); err != nil {
		c.JSON(404, gin.H{"success": false, "message": "channel not found"})
		return
	}
	monitor, err := model.GetUpstreamMonitor(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(200, gin.H{"success": true, "data": nil})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"success": false, "message": "monitor unavailable"})
		return
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{
		"channel_id": monitor.ChannelID, "enabled": monitor.Enabled, "platform": monitor.Platform,
		"base_url": monitor.BaseURL, "upstream_group": monitor.UpstreamGroup, "user_id": monitor.UserID,
		"has_secret": monitor.Secret != "", "warning_balance": monitor.WarningBalance,
		"last_price_at": monitor.LastPriceAt, "last_balance_at": monitor.LastBalanceAt,
		"last_balance": monitor.LastBalance, "last_error": monitor.LastError, "last_prices": monitor.LastPrices,
		"balance_state": monitor.BalanceState, "channel_state": monitor.ChannelState, "auto_disabled": monitor.AutoDisabled,
		"probe_state": monitor.ProbeState, "probe_failures": monitor.ProbeFailures,
	}})
}

func PutUpstreamMonitorChannel(c *gin.Context) {
	id, ok := monitorChannelID(c)
	if !ok {
		return
	}
	if _, err := model.GetChannelById(id, false); err != nil {
		c.JSON(404, gin.H{"success": false, "message": "channel not found"})
		return
	}
	var input monitorChannelInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(400, gin.H{"success": false, "message": "invalid monitor config"})
		return
	}
	if input.Platform != "newapi" && input.Platform != "sub2api" {
		c.JSON(400, gin.H{"success": false, "message": "unsupported upstream type"})
		return
	}
	if err := service.ValidateUpstreamMonitorBaseURL(input.BaseURL); err != nil {
		c.JSON(400, gin.H{"success": false, "message": err.Error()})
		return
	}
	if strings.TrimSpace(input.UpstreamGroup) == "" || len(input.UpstreamGroup) > 128 || len(input.UserID) > 128 || len(input.BaseURL) > 1024 || len(input.Secret) > 65536 || input.WarningBalance < 0 || math.IsNaN(input.WarningBalance) || math.IsInf(input.WarningBalance, 0) {
		c.JSON(400, gin.H{"success": false, "message": "invalid group or balance threshold"})
		return
	}
	monitor, err := model.GetUpstreamMonitor(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		monitor = model.UpstreamMonitor{ChannelID: id}
	} else if err != nil {
		c.JSON(500, gin.H{"success": false, "message": "monitor unavailable"})
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	upstreamGroup := strings.TrimSpace(input.UpstreamGroup)
	userID := strings.TrimSpace(input.UserID)
	if monitor.Platform != input.Platform || monitor.BaseURL != baseURL || monitor.UpstreamGroup != upstreamGroup || monitor.UserID != userID || input.Secret != "" {
		monitor.LastPriceAt, monitor.LastBalanceAt = 0, 0
		monitor.LastPrices, monitor.LastError = "", ""
		monitor.LastBalance, monitor.BalanceState = 0, ""
		monitor.ZeroBalanceChecks, monitor.ProbeFailures = 0, 0
	}
	monitor.Enabled, monitor.Platform, monitor.BaseURL = input.Enabled, input.Platform, baseURL
	monitor.UpstreamGroup, monitor.UserID, monitor.WarningBalance = upstreamGroup, userID, input.WarningBalance
	if input.Secret != "" {
		monitor.Secret, err = service.EncryptMonitorSecret(input.Secret)
		if err != nil {
			c.JSON(400, gin.H{"success": false, "message": err.Error()})
			return
		}
	}
	if monitor.Enabled && (monitor.Secret == "" || monitor.UserID == "") {
		c.JSON(400, gin.H{"success": false, "message": "credentials are required"})
		return
	}
	if err := model.SaveUpstreamMonitor(monitor); err != nil {
		c.JSON(500, gin.H{"success": false, "message": "failed to save monitor"})
		return
	}
	c.JSON(200, gin.H{"success": true})
}

func RunUpstreamMonitorNow(c *gin.Context) {
	task, created, err := service.EnqueueSystemTask(model.SystemTaskTypeUpstreamMonitor, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "failed to queue monitor"})
		return
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"task_id": task.TaskID, "created": created}})
}

func GetUpstreamMonitorPriceLogs(c *gin.Context) {
	page := common.GetPageQuery(c)
	if page.Page < 1 || page.PageSize < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": "invalid page"})
		return
	}
	logs, total, err := model.ListUpstreamMonitorPriceLogs(page.GetStartIdx(), page.PageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": "price logs unavailable"})
		return
	}
	page.Items, page.Total = logs, int(total)
	c.JSON(http.StatusOK, gin.H{"success": true, "data": page})
}
