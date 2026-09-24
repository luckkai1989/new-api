/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestUpstreamMonitorGroupRatioCASAndOverride(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	previousDB := DB
	previousRatios := ratio_setting.GroupRatio2JSONString()
	previousMap := common.OptionMap
	DB = db
	common.OptionMap = map[string]string{}
	t.Cleanup(func() {
		DB = previousDB
		common.OptionMap = previousMap
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(previousRatios))
	})
	require.NoError(t, db.AutoMigrate(&Option{}, &UpstreamMonitorPriceLog{}))
	require.NoError(t, db.Create(&Option{Key: "GroupRatio", Value: `{"default":1}`}).Error)
	require.NoError(t, db.Create(&Option{Key: "GroupGroupRatio", Value: `{"vip":{"default":0.8}}`}).Error)

	override, err := HasGroupRatioOverride("default")
	require.NoError(t, err)
	require.True(t, override)

	audit := UpstreamMonitorPriceLog{TaskID: "monitor-task", RequiredRatio: 1.25, Evidence: `[{"channel_id":14}]`}
	changed, err := CompareAndSwapGroupRatio("default", 1, 1.25, audit)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = CompareAndSwapGroupRatio("default", 1, 2, audit)
	require.NoError(t, err)
	require.False(t, changed)
	value, err := GetPersistedGroupRatio("default")
	require.NoError(t, err)
	require.Equal(t, 1.25, value)
	logs, total, err := ListUpstreamMonitorPriceLogs(0, 20)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	require.Equal(t, "default", logs[0].GroupName)
	require.Equal(t, 1.0, logs[0].OldRatio)
	require.Equal(t, 1.25, logs[0].NewRatio)
	require.Equal(t, audit.Evidence, logs[0].Evidence)
}

func TestGroupRatioAuditFailureRollsBackChange(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })
	require.NoError(t, db.AutoMigrate(&Option{}))
	require.NoError(t, db.Create(&Option{Key: "GroupRatio", Value: `{"default":1}`}).Error)

	changed, err := CompareAndSwapGroupRatio("default", 1, 1.25, UpstreamMonitorPriceLog{TaskID: "monitor-task"})
	require.Error(t, err)
	require.False(t, changed)
	value, err := GetPersistedGroupRatio("default")
	require.NoError(t, err)
	require.Equal(t, 1.0, value)
}
