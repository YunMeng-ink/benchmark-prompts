package client

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/auth"
	"github.com/example/benchmark-prompts/internal/secretbox"
	"github.com/example/benchmark-prompts/internal/store"
)

// 本文件是 pkg/client 唯一 import internal/ 的地方，且只在测试期。
//
// 原因：SDK 与服务端各自持有一份 Canonical/Sign 实现（生产代码不能互相依赖，
// 否则公开 SDK 会把内部包暴露给使用者）。两份实现一旦漂移，症状是
// "所有写请求 401"，排查成本极高，所以在这里把它们焊死。

func TestSignatureParityWithServer(t *testing.T) {
	vectors := []struct {
		name   string
		method string
		path   string
		secret string
		ts     int64
		body   string
	}{
		{"评分", http.MethodPost, "/v1/scores", "sec-1", 1700000000, `{"id":"p_1","value":5,"deviceId":"d1"}`},
		{"上传", http.MethodPost, "/v1/prompts", "sec-2", 1700000001, `{"p":"多行\n正文","t":["a"],"clientId":"c1"}`},
		{"空body", http.MethodPost, "/v1/scores", "sec-3", 0, ``},
		{"GET", http.MethodGet, "/v1/meta", "sec-4", 1, ``},
		{"负时间戳", http.MethodPost, "/v1/prompts", "sec-5", -5, `{"p":"x"}`},
		{"中文secret", http.MethodPost, "/v1/scores", "密钥-6", 1700000002, `{"value":1}`},
		{"含空格path", http.MethodGet, "/v1/prompts/p_a b", "sec-7", 1700000003, ``},
	}

	for _, v := range vectors {
		body := []byte(v.body)

		sc, ac := Canonical(v.method, v.path, v.ts, body), auth.Canonical(v.method, v.path, v.ts, body)
		if sc != ac {
			t.Fatalf("%s：待签串漂移\n  SDK   : %q\n  服务端: %q", v.name, sc, ac)
		}
		ss, as := Sign(v.secret, v.method, v.path, v.ts, body), auth.Sign(v.secret, v.method, v.path, v.ts, body)
		if ss != as {
			t.Fatalf("%s：签名漂移 SDK=%s 服务端=%s", v.name, ss, as)
		}
		if len(ss) != 64 {
			t.Fatalf("%s：签名应为 sha256 hex（64 字符），得到 %d", v.name, len(ss))
		}
	}
}

// TestSDKSignatureAcceptedByServerVerifier 是真正的闭环：
// 用服务端生产代码（auth.Authenticator）去验证 SDK 产出的签名头。
func TestSDKSignatureAcceptedByServerVerifier(t *testing.T) {
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 3)
	}
	const (
		plainKey    = "e2e-key"
		plainSecret = "e2e-secret"
	)

	enc, err := secretbox.Seal(master, plainSecret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	keys := &fakeKeys{records: map[string]*store.APIKeyRecord{
		store.KeyHash(plainKey): {Name: "e2e", SecretEnc: enc, Enabled: true},
	}}
	verifier := auth.New(keys, master, 5*time.Minute)

	body := []byte(`{"id":"p_1","value":4,"deviceId":"d"}`)
	ts := time.Now().Unix()

	newReq := func(signedBody, actualBody []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/scores", bytes.NewReader(actualBody))
		r.Header.Set("X-Api-Key", plainKey)
		r.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
		// 注意：签名永远只对 signedBody 计算。
		// 如果这里用 actualBody 去签名，就等于供了一个自洽的合法签名，
		// 测试会变成"服务端居然接受了"的假阳性。
		r.Header.Set("X-Signature", Sign(plainSecret, http.MethodPost, "/v1/scores", ts, signedBody))
		return r
	}

	who, err := verifier.Authenticate(newReq(body, body), body)
	if err != nil {
		t.Fatalf("服务端应接受 SDK 产出的签名: %v", err)
	}
	if who == nil || who.Name != "e2e" {
		t.Fatalf("身份应为 e2e，得到 %v", who)
	}

	// 攻击场景：沿用旧签名（或旧头）但偷换 payload，必须被拒
	tampered := []byte(`{"id":"p_1","value":1,"deviceId":"d"}`)
	if _, err := verifier.Authenticate(newReq(body, tampered), tampered); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("沿用旧签名偷换 body 必须被拒，得到 %v", err)
	}

	// 反向同理：签名与 body 不一致就不能通过
	if _, err := verifier.Authenticate(newReq(tampered, body), body); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("签名与 body 不一致必须被拒，得到 %v", err)
	}
}

// TestSDKBearerAcceptedByServerVerifier 覆盖前端/脚本常用的 Bearer 路径。
func TestSDKBearerAcceptedByServerVerifier(t *testing.T) {
	master := make([]byte, 32)
	enc, err := secretbox.Seal(master, "whatever")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	keys := &fakeKeys{records: map[string]*store.APIKeyRecord{
		store.KeyHash("bearer-only"): {Name: "web", SecretEnc: enc, Enabled: true},
	}}
	verifier := auth.New(keys, master, 5*time.Minute)

	r := httptest.NewRequest(http.MethodPost, "/v1/scores", bytes.NewReader([]byte(`{}`)))
	applyAuth(r.Header, "bearer-only", "", http.MethodPost, "/v1/scores", []byte(`{}`), time.Now())

	who, err := verifier.Authenticate(r, []byte(`{}`))
	if err != nil {
		t.Fatalf("Bearer 应被接受: %v", err)
	}
	if who == nil || who.Name != "web" {
		t.Fatalf("身份应为 web，得到 %v", who)
	}
}

func TestApplyAuthPrefersHMAC(t *testing.T) {
	now := time.Unix(1700000000, 0)

	h := http.Header{}
	applyAuth(h, "k1", "sec", http.MethodPost, "/v1/scores", []byte("{}"), now)
	if h.Get("X-Api-Key") != "k1" || h.Get("X-Timestamp") == "" || h.Get("X-Signature") == "" {
		t.Fatalf("有 secret 时应走 HMAC，得到 %v", h)
	}
	if h.Get("Authorization") != "" {
		t.Fatalf("两种凭据同时存在时不应再发 Bearer")
	}

	h2 := http.Header{}
	applyAuth(h2, "k1", "", http.MethodPost, "/v1/scores", []byte("{}"), now)
	if got := h2.Get("Authorization"); got != "Bearer k1" {
		t.Fatalf("无 secret 时应退回 Bearer，得到 %q", got)
	}
	if h2.Get("X-Signature") != "" {
		t.Fatalf("Bearer 模式不应发签名头")
	}

	h3 := http.Header{}
	applyAuth(h3, "", "", http.MethodGet, "/v1/meta", nil, now)
	if len(h3) != 0 {
		t.Fatalf("无凭据不应添加任何头，得到 %v", h3)
	}

	h4 := http.Header{}
	applyAuth(h4, "", "sec-only", http.MethodGet, "/v1/meta", nil, now)
	if h4.Get("X-Signature") != "" {
		t.Fatalf("缺 key 时不应发签名（服务端无法定位密钥）")
	}
}

func TestNewIDIsUniqueEnough(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i := 0; i < 200; i++ {
		id := newID(8)
		if len(id) != 16 {
			t.Fatalf("期望 16 位 hex，得到 %q", id)
		}
		if seen[id] {
			t.Fatalf("出现重复 ID %s，随机性不足以作幂等键", id)
		}
		seen[id] = true
	}
}

// fakeKeys 实现 auth.KeyStore。
type fakeKeys struct {
	records map[string]*store.APIKeyRecord
}

func (f *fakeKeys) LookupAPIKey(_ context.Context, keyHash string) (*store.APIKeyRecord, error) {
	rec, ok := f.records[keyHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return rec, nil
}
