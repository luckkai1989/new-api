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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

// MonitorPrice expresses real USD charged for one million input/output/cache
// tokens, or for one fixed-price request. Raw provider fields are retained so
// operators can audit the normalization.
type MonitorPrice struct {
	Model          string  `json:"model"`
	Mode           string  `json:"mode"`
	Input          float64 `json:"input"`
	Output         float64 `json:"output"`
	CacheRead      float64 `json:"cache_read"`
	CacheWrite     float64 `json:"cache_write"`
	PerRequest     float64 `json:"per_request"`
	RawModelRatio  float64 `json:"raw_model_ratio,omitempty"`
	RawGroupRatio  float64 `json:"raw_group_ratio,omitempty"`
	RawInputPrice  float64 `json:"raw_input_price,omitempty"`
	RawOutputPrice float64 `json:"raw_output_price,omitempty"`
	Comparable     bool    `json:"comparable"`
	Reason         string  `json:"reason,omitempty"`
}

type monitorAdapter interface {
	Prices(context.Context, model.UpstreamMonitor, string) ([]MonitorPrice, error)
	Balance(context.Context, model.UpstreamMonitor, string) (float64, error)
	HasActiveSubscription(context.Context, model.UpstreamMonitor, string) (bool, error)
}

type newAPIAdapter struct{ client *http.Client }
type sub2APIAdapter struct{ client *http.Client }

func newMonitorAdapter(platform string) (monitorAdapter, error) {
	client := monitorHTTPClient()
	switch platform {
	case "newapi":
		return newAPIAdapter{client}, nil
	case "sub2api":
		return sub2APIAdapter{client}, nil
	default:
		return nil, errors.New("unsupported upstream type")
	}
}

func monitorCipher() (cipher.AEAD, error) {
	key := os.Getenv("UPSTREAM_MONITOR_ENCRYPTION_KEY")
	if len(key) < 32 {
		return nil, errors.New("UPSTREAM_MONITOR_ENCRYPTION_KEY must contain at least 32 characters on every node")
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func EncryptMonitorSecret(secret string) (string, error) {
	aead, err := monitorCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(append(nonce, aead.Seal(nil, nonce, []byte(secret), nil)...)), nil
}

func DecryptMonitorSecret(value string) (string, error) {
	aead, err := monitorCipher()
	if err != nil {
		return "", err
	}
	data, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(data) < aead.NonceSize() {
		return "", errors.New("invalid monitor secret")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
	return string(plain), err
}

func validateMonitorURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("monitor URL must be a public HTTPS origin without credentials, query or fragment")
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("monitor URL must use port 443")
	}
	protection := &common.SSRFProtection{DomainFilterMode: false, IpFilterMode: false, ApplyIPFilterForDomain: true}
	if err := protection.ValidateURL(u.String()); err != nil {
		return nil, err
	}
	return u, nil
}

func ValidateUpstreamMonitorBaseURL(raw string) error {
	_, err := validateMonitorURL(raw)
	return err
}

func monitorHTTPClient() *http.Client {
	protection := &common.SSRFProtection{DomainFilterMode: false, IpFilterMode: false, ApplyIPFilterForDomain: true}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 12 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" {
			return nil, errors.New("monitor target must use HTTPS port 443")
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, addr := range ips {
			if err := protection.ValidateResolvedIP(host, addr.IP); err != nil {
				continue
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		}
		return nil, errors.New("monitor target has no permitted public IP")
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func monitorGetJSON(ctx context.Context, client *http.Client, base, path, token, userID string, target any) error {
	u, err := validateMonitorURL(base)
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if userID != "" {
		req.Header.Set("New-Api-User", userID)
	}
	return monitorDoJSON(client, req, target)
}

func monitorDoJSON(client *http.Client, req *http.Request, target any) error {
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("upstream monitor request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream monitor returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
}

type newAPIPricingEnvelope struct {
	Success    bool                `json:"success"`
	Data       []newAPIPricingItem `json:"data"`
	GroupRatio map[string]float64  `json:"group_ratio"`
}
type newAPIPricingItem struct {
	ModelName            string   `json:"model_name"`
	QuotaType            *int     `json:"quota_type"`
	ModelRatio           *float64 `json:"model_ratio"`
	ModelPrice           *float64 `json:"model_price"`
	CompletionRatio      *float64 `json:"completion_ratio"`
	CacheRatio           *float64 `json:"cache_ratio"`
	CreateCacheRatio     *float64 `json:"create_cache_ratio"`
	ImageRatio           *float64 `json:"image_ratio"`
	AudioRatio           *float64 `json:"audio_ratio"`
	AudioCompletionRatio *float64 `json:"audio_completion_ratio"`
	BillingMode          string   `json:"billing_mode"`
	EnableGroups         []string `json:"enable_groups"`
}

func normalizeNewAPIPrices(payload newAPIPricingEnvelope, group string, quotaPerUnit float64) ([]MonitorPrice, error) {
	if !payload.Success || quotaPerUnit <= 0 {
		return nil, errors.New("incomplete upstream NewAPI pricing")
	}
	groupRatio, ok := payload.GroupRatio[group]
	if !ok || groupRatio < 0 {
		return nil, errors.New("upstream group ratio is missing")
	}
	prices := make([]MonitorPrice, 0)
	for _, item := range payload.Data {
		if !containsMonitorGroup(item.EnableGroups, group) {
			continue
		}
		price := MonitorPrice{Model: item.ModelName, RawGroupRatio: groupRatio, Comparable: true}
		if item.ImageRatio != nil || item.AudioRatio != nil || item.AudioCompletionRatio != nil {
			price.Comparable, price.Reason = false, "image or audio pricing requires manual review"
		} else if item.BillingMode == "tiered_expr" {
			price.Comparable, price.Reason = false, "tiered expression requires manual review"
		} else if item.QuotaType == nil {
			price.Comparable, price.Reason = false, "missing billing mode"
		} else if *item.QuotaType == 1 {
			price.Mode = "fixed"
			if item.ModelPrice == nil {
				price.Comparable, price.Reason = false, "missing fixed price"
			} else {
				price.PerRequest = *item.ModelPrice * groupRatio
			}
		} else if *item.QuotaType == 0 {
			price.Mode = "token"
			if item.ModelRatio == nil || item.CompletionRatio == nil {
				price.Comparable, price.Reason = false, "missing token ratios"
			} else {
				price.RawModelRatio = *item.ModelRatio
				price.Input = *item.ModelRatio * groupRatio * 1e6 / quotaPerUnit
				price.Output = price.Input * *item.CompletionRatio
			}
			if item.CacheRatio != nil {
				price.CacheRead = price.Input * *item.CacheRatio
			} else {
				price.CacheRead = price.Input * ratio_setting.DefaultCacheRatio
			}
			if item.CreateCacheRatio != nil {
				price.CacheWrite = price.Input * *item.CreateCacheRatio
			} else {
				price.CacheWrite = price.Input * ratio_setting.DefaultCreateCacheRatio
			}
		} else {
			price.Comparable, price.Reason = false, "unsupported billing mode"
		}
		if price.Model == "" || (price.Comparable && !validMonitorPrice(price)) {
			price.Comparable, price.Reason = false, "invalid or incomplete model pricing"
		}
		prices = append(prices, price)
	}
	if len(prices) == 0 {
		return nil, errors.New("upstream group contains no visible models")
	}
	return prices, nil
}

func containsMonitorGroup(groups []string, group string) bool {
	for _, item := range groups {
		if item == group || item == "all" {
			return true
		}
	}
	return false
}

func validMonitorPrice(p MonitorPrice) bool {
	values := []float64{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.PerRequest}
	for _, v := range values {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return p.Mode == "fixed" || p.Mode == "token"
}

func (a newAPIAdapter) Prices(ctx context.Context, monitor model.UpstreamMonitor, secret string) ([]MonitorPrice, error) {
	var status struct {
		Data struct {
			QuotaPerUnit float64 `json:"quota_per_unit"`
		} `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/status", "", "", &status); err != nil {
		return nil, err
	}
	var payload newAPIPricingEnvelope
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/pricing", secret, monitor.UserID, &payload); err != nil {
		return nil, err
	}
	return normalizeNewAPIPrices(payload, monitor.UpstreamGroup, status.Data.QuotaPerUnit)
}

func (a newAPIAdapter) Balance(ctx context.Context, monitor model.UpstreamMonitor, secret string) (float64, error) {
	var status struct {
		Data struct {
			QuotaPerUnit float64 `json:"quota_per_unit"`
		} `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/status", "", "", &status); err != nil {
		return 0, err
	}
	if status.Data.QuotaPerUnit <= 0 {
		return 0, errors.New("upstream quota unit is missing")
	}
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Quota *float64 `json:"quota"`
		} `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/user/self", secret, monitor.UserID, &payload); err != nil {
		return 0, err
	}
	if !payload.Success || payload.Data.Quota == nil || *payload.Data.Quota < 0 {
		return 0, errors.New("upstream balance is invalid")
	}
	return *payload.Data.Quota / status.Data.QuotaPerUnit, nil
}

func (a newAPIAdapter) HasActiveSubscription(ctx context.Context, monitor model.UpstreamMonitor, secret string) (bool, error) {
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Subscriptions []json.RawMessage `json:"subscriptions"`
		} `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/subscription/self", secret, monitor.UserID, &payload); err != nil {
		return false, err
	}
	if !payload.Success {
		return false, errors.New("NewAPI subscription status unavailable")
	}
	return len(payload.Data.Subscriptions) > 0, nil
}

func (a sub2APIAdapter) auth(ctx context.Context, monitor model.UpstreamMonitor, secret string) (string, error) {
	if strings.TrimSpace(monitor.UserID) == "" {
		return "", errors.New("Sub2API login email is required")
	}
	u, err := validateMonitorURL(monitor.BaseURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/auth/login"
	body, _ := json.Marshal(map[string]string{"email": monitor.UserID, "password": secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var payload struct {
		Code int `json:"code"`
		Data struct {
			AccessToken string `json:"access_token"`
			Requires2FA bool   `json:"requires_2fa"`
		} `json:"data"`
	}
	if err := monitorDoJSON(a.client, req, &payload); err != nil {
		return "", err
	}
	if payload.Code != 0 || payload.Data.Requires2FA || payload.Data.AccessToken == "" {
		return "", errors.New("Sub2API login failed or requires 2FA")
	}
	return payload.Data.AccessToken, nil
}

type sub2PriceEnvelope struct {
	Code int `json:"code"`
	Data []struct {
		Platforms []struct {
			Groups []struct {
				ID             int64   `json:"id"`
				Name           string  `json:"name"`
				RateMultiplier float64 `json:"rate_multiplier"`
			} `json:"groups"`
			SupportedModels []struct {
				Name    string `json:"name"`
				Pricing *struct {
					BillingMode      string            `json:"billing_mode"`
					InputPrice       *float64          `json:"input_price"`
					OutputPrice      *float64          `json:"output_price"`
					CacheReadPrice   *float64          `json:"cache_read_price"`
					CacheWritePrice  *float64          `json:"cache_write_price"`
					ImageOutputPrice *float64          `json:"image_output_price"`
					PerRequestPrice  *float64          `json:"per_request_price"`
					Intervals        []json.RawMessage `json:"intervals"`
				} `json:"pricing"`
			} `json:"supported_models"`
		} `json:"platforms"`
	} `json:"data"`
}

func (a sub2APIAdapter) Prices(ctx context.Context, monitor model.UpstreamMonitor, secret string) ([]MonitorPrice, error) {
	token, err := a.auth(ctx, monitor, secret)
	if err != nil {
		return nil, err
	}
	var payload sub2PriceEnvelope
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/v1/channels/available", token, "", &payload); err != nil {
		return nil, err
	}
	if payload.Code != 0 {
		return nil, errors.New("Sub2API pricing request failed")
	}
	var rates struct {
		Code int                `json:"code"`
		Data map[string]float64 `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/v1/groups/rates", token, "", &rates); err != nil {
		return nil, err
	}
	if rates.Code != 0 {
		return nil, errors.New("Sub2API group rates unavailable")
	}
	return normalizeSub2APIPrices(payload, rates.Data, monitor.UpstreamGroup)
}

func normalizeSub2APIPrices(payload sub2PriceEnvelope, rates map[string]float64, upstreamGroup string) ([]MonitorPrice, error) {
	prices := []MonitorPrice{}
	for _, channel := range payload.Data {
		for _, platform := range channel.Platforms {
			for _, group := range platform.Groups {
				if group.Name != upstreamGroup && strconv.FormatInt(group.ID, 10) != upstreamGroup {
					continue
				}
				multiplier := group.RateMultiplier
				if override, ok := rates[strconv.FormatInt(group.ID, 10)]; ok {
					multiplier = override
				}
				if multiplier < 0 {
					return nil, errors.New("invalid Sub2API group rate")
				}
				for _, item := range platform.SupportedModels {
					if item.Pricing == nil {
						continue
					}
					p := MonitorPrice{Model: item.Name, RawGroupRatio: multiplier, Comparable: true}
					if len(item.Pricing.Intervals) > 0 {
						p.Reason, p.Comparable = "tiered prices require manual review", false
					}
					if item.Pricing.ImageOutputPrice != nil && *item.Pricing.ImageOutputPrice > 0 {
						p.Reason, p.Comparable = "image output price requires manual review", false
					}
					switch item.Pricing.BillingMode {
					case "token":
						p.Mode = "token"
						if item.Pricing.InputPrice == nil || item.Pricing.OutputPrice == nil {
							p.Comparable, p.Reason = false, "missing input or output price"
						} else {
							p.RawInputPrice, p.RawOutputPrice = *item.Pricing.InputPrice, *item.Pricing.OutputPrice
							p.Input, p.Output = *item.Pricing.InputPrice*multiplier*1e6, *item.Pricing.OutputPrice*multiplier*1e6
						}
						if item.Pricing.CacheReadPrice != nil {
							p.CacheRead = *item.Pricing.CacheReadPrice * multiplier * 1e6
						}
						if item.Pricing.CacheWritePrice != nil {
							p.CacheWrite = *item.Pricing.CacheWritePrice * multiplier * 1e6
						}
					case "per_request":
						p.Mode = "fixed"
						if item.Pricing.PerRequestPrice == nil {
							p.Comparable, p.Reason = false, "missing per-request price"
						} else {
							p.PerRequest = *item.Pricing.PerRequestPrice * multiplier
						}
					default:
						p.Comparable, p.Reason = false, "unsupported billing mode"
					}
					if !validMonitorPrice(p) {
						p.Comparable, p.Reason = false, "invalid price"
					}
					prices = append(prices, p)
				}
			}
		}
	}
	if len(prices) == 0 {
		return nil, errors.New("Sub2API group or models not found")
	}
	return prices, nil
}

func (a sub2APIAdapter) Balance(ctx context.Context, monitor model.UpstreamMonitor, secret string) (float64, error) {
	token, err := a.auth(ctx, monitor, secret)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Balance *float64 `json:"balance"`
		} `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/v1/auth/me", token, "", &payload); err != nil {
		return 0, err
	}
	if payload.Code != 0 || payload.Data.Balance == nil || *payload.Data.Balance < 0 || math.IsNaN(*payload.Data.Balance) {
		return 0, errors.New("invalid Sub2API balance")
	}
	return *payload.Data.Balance, nil
}

func (a sub2APIAdapter) HasActiveSubscription(ctx context.Context, monitor model.UpstreamMonitor, secret string) (bool, error) {
	token, err := a.auth(ctx, monitor, secret)
	if err != nil {
		return false, err
	}
	var payload struct {
		Code int               `json:"code"`
		Data []json.RawMessage `json:"data"`
	}
	if err := monitorGetJSON(ctx, a.client, monitor.BaseURL, "/api/v1/subscriptions/active", token, "", &payload); err != nil {
		return false, err
	}
	if payload.Code != 0 {
		return false, errors.New("Sub2API subscription status unavailable")
	}
	return len(payload.Data) > 0, nil
}
