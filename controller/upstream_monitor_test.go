/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
package controller

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamMonitorChannelPriceSummaries(t *testing.T) {
	for _, backend := range []struct{ kind, env string }{
		{"sqlite", ""}, {"mysql", "AUDIT_MYSQL_DSN"}, {"postgres", "AUDIT_POSTGRES_DSN"},
	} {
		t.Run(backend.kind, func(t *testing.T) {
			dsn := os.Getenv(backend.env)
			if backend.env != "" && dsn == "" {
				t.Skip("set " + backend.env + " to exercise this database")
			}
			db, _ := newAuditTestDatabase(t, backend.kind, dsn)
			previous := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = previous })
			require.NoError(t, db.AutoMigrate(&model.UpstreamMonitor{}))
			rows := []model.UpstreamMonitor{
				{ChannelID: 1, Enabled: true, Platform: "newapi", LastPriceAt: 100, Secret: "private-secret", UserID: "private-user", BaseURL: "https://private.example", LastPrices: `[{"model":"z-first","mode":"token","raw_model_ratio":0.125,"comparable":true},{"model":"a-second","raw_model_ratio":9}]`},
				{ChannelID: 2, Platform: "newapi", LastPriceAt: 101, LastError: "upstream unavailable", LastPrices: `[{"model":"free","mode":"token","raw_model_ratio":0,"comparable":true}]`},
				{ChannelID: 3, Platform: "newapi", LastPriceAt: 102, LastPrices: `[{"model":"legacy-free","mode":"token","comparable":true}]`},
				{ChannelID: 4, Platform: "newapi", LastPriceAt: 103, LastPrices: `[{"model":"fixed","mode":"request","raw_group_ratio":3},{"model":"second","raw_model_ratio":5}]`},
				{ChannelID: 5, Platform: "sub2api", LastPriceAt: 104, LastPrices: `[{"model":"sub-first","mode":"token","raw_group_ratio":2,"comparable":true}]`},
				{ChannelID: 6, Platform: "newapi", LastPriceAt: 105, LastPrices: `[{`},
				{ChannelID: 7, Platform: "newapi", LastPrices: `[{"model":"not-collected","raw_model_ratio":5}]`},
				{ChannelID: 8, LastPriceAt: 106, LastPrices: `[]`},
				{ChannelID: 9, LastPriceAt: 107, LastPrices: `[{"model":"invalid","raw_model_ratio":-1}]`},
				{ChannelID: 10, LastPriceAt: 108, LastPrices: `[{"model":"filtered","raw_model_ratio":8}]`},
			}
			require.NoError(t, db.Create(&rows).Error)
			request := func(ids string) *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/?ids="+url.QueryEscape(ids), nil)
				GetUpstreamMonitorChannelPriceSummaries(ctx)
				return recorder
			}
			recorder := request("1,2,3,4,5,6,7,8,9,999")
			require.Equal(t, http.StatusOK, recorder.Code)
			var response struct {
				Success bool                         `json:"success"`
				Data    []monitorChannelPriceSummary `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.True(t, response.Success)
			require.Len(t, response.Data, 9)
			byID := make(map[int]monitorChannelPriceSummary)
			for _, row := range response.Data {
				byID[row.ChannelID] = row
			}
			assert.Equal(t, "z-first", byID[1].FirstModel)
			require.NotNil(t, byID[1].ModelRatio)
			assert.Equal(t, 0.125, *byID[1].ModelRatio)
			for _, id := range []int{2, 3} {
				require.NotNil(t, byID[id].ModelRatio)
				assert.Zero(t, *byID[id].ModelRatio)
			}
			assert.False(t, byID[2].Enabled)
			assert.Equal(t, "upstream unavailable", byID[2].LastError)
			assert.EqualValues(t, 101, byID[2].LastPriceAt)
			assert.Equal(t, "fixed", byID[4].FirstModel)
			assert.Equal(t, "sub-first", byID[5].FirstModel)
			for _, id := range []int{4, 5, 6, 7, 8, 9} {
				assert.Nil(t, byID[id].ModelRatio)
			}
			assert.Equal(t, "invalid price snapshot", byID[6].LastError)
			assert.Empty(t, byID[7].FirstModel)
			for _, secret := range []string{"private-secret", "private-user", "private.example", "last_prices"} {
				assert.NotContains(t, recorder.Body.String(), secret)
			}
			snapshots, err := model.ListUpstreamMonitorPriceSnapshots([]int{1})
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			assert.Empty(t, snapshots[0].Secret)
			assert.Empty(t, snapshots[0].UserID)
			empty, err := model.ListUpstreamMonitorPriceSnapshots(nil)
			require.NoError(t, err)
			assert.Empty(t, empty)
			for _, ids := range []string{"", "0", "-1", "abc", "1,", strings.Repeat("1,", 500) + "1"} {
				assert.Equal(t, http.StatusBadRequest, request(ids).Code, ids)
			}
			require.NoError(t, db.Migrator().DropTable(&model.UpstreamMonitor{}))
			assert.Equal(t, http.StatusInternalServerError, request("1").Code)
		})
	}
}
