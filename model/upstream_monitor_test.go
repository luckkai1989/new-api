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
	require.NoError(t, db.AutoMigrate(&Option{}))
	require.NoError(t, db.Create(&Option{Key: "GroupRatio", Value: `{"default":1}`}).Error)
	require.NoError(t, db.Create(&Option{Key: "GroupGroupRatio", Value: `{"vip":{"default":0.8}}`}).Error)

	override, err := HasGroupRatioOverride("default")
	require.NoError(t, err)
	require.True(t, override)

	changed, err := CompareAndSwapGroupRatio("default", 1, 1.25)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = CompareAndSwapGroupRatio("default", 1, 2)
	require.NoError(t, err)
	require.False(t, changed)
	value, err := GetPersistedGroupRatio("default")
	require.NoError(t, err)
	require.Equal(t, 1.25, value)
}
