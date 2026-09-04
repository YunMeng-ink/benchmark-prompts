// Package auth 实现两种写入端点鉴权：Bearer API Key 与 HMAC 签名。
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/example/benchmark-prompts/internal/secretbox"
	"github.com/example/benchmark-prompts/internal/store"
)

// ErrUnauthorized 统一映射到 HTTP 401。
var ErrUnauthorized = errors.New("auth: 未授权")

// KeyStore 是 auth 对存储的最小依赖，便于单元测试注入。
type KeyStore interface {
	LookupAPIKey(ctx context.Context, keyHash string) (*store.APIKeyRecord, error)
}

// Authenticator 校验请求身份。
type Authenticator struct {
	keys      KeyStore
	masterKey []byte
	maxSkew   time.Duration
}

// New 构造校验器。masterKey 为解密 secret_enc 的主密钥（32 字节）。
func New(keys KeyStore, masterKey []byte, maxSkew time.Duration) *Authenticator {
	return &Authenticator{keys: keys, masterKey: masterKey, maxSkew: maxSkew}
}

// Identity 是一次成功鉴权的结果。
//
// 带 Scope 是因为自助注册之后「有一把有效 Key」不再等于「可以碰运维端点」；
// 带 KeyHash 是给 GET/DELETE /v1/keys/self 定位调用者自己的那行记录。
type Identity struct {
	Name    string
	Scope   string
	KeyHash string
}

// IsAdmin 判断这把 Key 是否有运维作用域。
func (i *Identity) IsAdmin() bool {
	return i != nil && i.Scope == store.ScopeAdmin
}

// Authenticate 成功返回身份，失败返回 ErrUnauthorized。
//
// 优先级：Authorization: Bearer <key> > X-Api-Key/X-Timestamp/X-Signature。
func (a *Authenticator) Authenticate(r *http.Request, body []byte) (*Identity, error) {
	if az := r.Header.Get("Authorization"); strings.HasPrefix(az, "Bearer ") {
		key := strings.TrimSpace(strings.TrimPrefix(az, "Bearer "))
		if key == "" {
			return nil, ErrUnauthorized
		}
		return a.byAPIKey(r.Context(), key)
	}

	ak := r.Header.Get("X-Api-Key")
	tsv := r.Header.Get("X-Timestamp")
	sig := r.Header.Get("X-Signature")
	if ak != "" && tsv != "" && sig != "" {
		return a.bySignature(r, ak, tsv, sig, body)
	}
	return nil, ErrUnauthorized
}

// byAPIKey 走 Bearer 路径：只存 key 的 sha256，命中即认身份。
// 浏览器前端只能用这条路（secret 不能下发到浏览器，见 docs/api.md §1.3）。
func (a *Authenticator) byAPIKey(ctx context.Context, plainKey string) (*Identity, error) {
	hash := store.KeyHash(plainKey)
	rec, err := a.keys.LookupAPIKey(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("查询 API Key 失败: %w", err)
	}
	if !rec.Enabled {
		return nil, ErrUnauthorized
	}
	return &Identity{Name: rec.Name, Scope: rec.Scope, KeyHash: hash}, nil
}

// bySignature 校验 HMAC：需要可解密的 secret（不能只存哈希，否则无法验签）。
func (a *Authenticator) bySignature(r *http.Request, plainKey, tsv, sig string, body []byte) (*Identity, error) {
	ts, err := strconv.ParseInt(tsv, 10, 64)
	if err != nil {
		return nil, ErrUnauthorized
	}
	skew := time.Since(time.Unix(ts, 0))
	if skew < 0 {
		skew = -skew
	}
	if a.maxSkew > 0 && skew > a.maxSkew {
		return nil, ErrUnauthorized
	}

	hash := store.KeyHash(plainKey)
	rec, err := a.keys.LookupAPIKey(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("查询 API Key 失败: %w", err)
	}
	if !rec.Enabled {
		return nil, ErrUnauthorized
	}

	secret, err := secretbox.Open(a.masterKey, rec.SecretEnc)
	if err != nil {
		return nil, fmt.Errorf("解密 secret 失败: %w", err)
	}
	expected := Sign(secret, r.Method, r.URL.Path, ts, body)
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(strings.TrimSpace(sig)))) {
		return nil, ErrUnauthorized
	}
	return &Identity{Name: rec.Name, Scope: rec.Scope, KeyHash: hash}, nil
}

// Canonical 构造待签名字符串，CLI/SDK 与服务端必须完全一致。
func Canonical(method, path string, ts int64, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(sum[:])
}

// Sign 计算 HMAC-SHA256 签名（hex 小写）。
func Sign(secret, method, path string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(Canonical(method, path, ts, body)))
	return hex.EncodeToString(mac.Sum(nil))
}
