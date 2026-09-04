package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// KeyIssue 是自助注册成功后返回的凭据。
//
// 明文 Key 只在这一次响应里出现：服务端只存 sha256，事后无法找回，
// 所以调用方必须立刻落盘（bench 配置 / 浏览器 localStorage）。
type KeyIssue struct {
	Key      string `json:"key"`
	Ref      string `json:"ref"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	DeviceID string `json:"deviceId"`
}

// KeySelf 是自己那把 Key 的元信息（不含明文）。
type KeySelf struct {
	Ref       string `json:"ref"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	DeviceID  string `json:"deviceId"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
}

// RegisterKey 用邀请码换一把绑定设备的 writer Key。
//
// 设备指纹沿用打分时的同一个 resolveDeviceID，避免出现"两台设备"的两种定义；
// 服务端按 deviceId 做一设备一 Key 的唯一性。
// 这是写操作，不参与重试（ADR-12）。
func (c *Client) RegisterKey(ctx context.Context, inviteCode, label string) (*KeyIssue, error) {
	code := strings.TrimSpace(inviteCode)
	if code == "" {
		return nil, &Error{Code: CodeValidation, Message: "邀请码不能为空"}
	}
	dev, err := c.resolveDeviceID(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"inviteCode": code, "deviceId": dev, "label": label})
	if err != nil {
		return nil, &Error{Code: CodeBadResponse, Message: "序列化请求失败", Err: err}
	}
	resp, err := c.send(ctx, &request{method: http.MethodPost, path: "/v1/keys", body: body})
	if err != nil {
		return nil, err
	}
	return decodeData[KeyIssue](resp)
}

// KeySelf 查询当前凭据的元信息（作用域、绑定设备、是否仍可用）。
func (c *Client) KeySelf(ctx context.Context) (*KeySelf, error) {
	resp, err := c.send(ctx, &request{method: http.MethodGet, path: "/v1/keys/self", idempotent: true})
	if err != nil {
		return nil, err
	}
	return decodeData[KeySelf](resp)
}

// RevokeKeySelf 停用当前这把 Key。之后再用它请求会得到 401，且不可撤销。
func (c *Client) RevokeKeySelf(ctx context.Context) (string, error) {
	resp, err := c.send(ctx, &request{method: http.MethodDelete, path: "/v1/keys/self"})
	if err != nil {
		return "", err
	}
	out, err := decodeData[struct {
		Ref     string `json:"ref"`
		Revoked bool   `json:"revoked"`
	}](resp)
	if err != nil {
		return "", err
	}
	if !out.Revoked {
		return out.Ref, &Error{Code: CodeBadResponse, Message: "服务端未确认吊销", HTTP: resp.status}
	}
	return out.Ref, nil
}
