package client

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// Canonical 构造待签名字符串：
//
//	method + "\n" + path + "\n" + timestamp + "\n" + hex(sha256(body))
//
// **必须与服务端 internal/auth.Canonical 逐字节一致。**
// sign_parity_test.go 会用同一批测试向量同时调用两侧实现并断言相等：
// 任何一侧被改动都会立刻失败。否则 SDK 与服务端的签名会静默漂移，
// 表现为"所有写请求都 401"，且极难定位。
func Canonical(method, path string, ts int64, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(sum[:])
}

// Sign 计算 HMAC-SHA256（hex 小写）。导出以便插件/脚本自行签名。
func Sign(secret, method, path string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(Canonical(method, path, ts, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// applyAuth 给请求头加鉴权：有 secret 走 HMAC，否则退回 Bearer API Key。
//
// 浏览器前端只能用 Bearer（secret 不能安全下发），CLI/SDK 两条路都支持。
func applyAuth(h http.Header, key, secret, method, path string, body []byte, now time.Time) {
	if key != "" && secret != "" {
		ts := now.Unix()
		h.Set("X-Api-Key", key)
		h.Set("X-Timestamp", strconv.FormatInt(ts, 10))
		h.Set("X-Signature", Sign(secret, method, path, ts, body))
		return
	}
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
}

// newID 生成随机 hex 串，用于 clientId / deviceId。
func newID(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败在本机是不可恢复的，宁可返回可识别的退化值也不 panic。
		return "randunavailable"
	}
	return hex.EncodeToString(b)
}
