package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/example/benchmark-prompts/internal/secretbox"
	"github.com/example/benchmark-prompts/internal/store"
)

var master = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}()

type fakeKeys struct {
	records map[string]*store.APIKeyRecord // key = sha256(plainKey)
	err     error
}

func (f *fakeKeys) LookupAPIKey(_ context.Context, keyHash string) (*store.APIKeyRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	rec, ok := f.records[keyHash]
	if !ok {
		return nil, store.ErrNotFound
	}
	return rec, nil
}

func newFake(t *testing.T, plainKey, secret string, enabled bool) *fakeKeys {
	t.Helper()
	enc, err := secretbox.Seal(master, secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return &fakeKeys{records: map[string]*store.APIKeyRecord{
		store.KeyHash(plainKey): {Name: "tester", SecretEnc: enc, Enabled: enabled},
	}}
}

const (
	testKey    = "plain-key"
	testSecret = "plain-secret"
)

func TestCanonicalIsStable(t *testing.T) {
	body := []byte(`{"a":1}`)
	c1 := Canonical(http.MethodPost, "/v1/scores", 1700000000, body)
	c2 := Canonical(http.MethodPost, "/v1/scores", 1700000000, body)
	if c1 != c2 {
		t.Fatalf("待签串必须确定，得到 %q vs %q", c1, c2)
	}
	if strings.Count(c1, "\n") != 3 {
		t.Fatalf("待签串应由 3 个换行分隔 4 段，得到 %q", c1)
	}
	if !strings.Contains(c1, "/v1/scores") {
		t.Fatalf("待签串必须含 path，得到 %q", c1)
	}
	// 改任一段都必须改变结果
	if Canonical(http.MethodGet, "/v1/scores", 1700000000, body) == c1 {
		t.Fatalf("method 变化应改变待签串")
	}
	if Canonical(http.MethodPost, "/v1/scores", 1700000001, body) == c1 {
		t.Fatalf("timestamp 变化应改变待签串")
	}
	if Canonical(http.MethodPost, "/v1/scores", 1700000000, []byte(`{"a":2}`)) == c1 {
		t.Fatalf("body 变化应改变待签串")
	}
}

func TestBearerAuthenticates(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 5*time.Minute)

	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("Authorization", "Bearer "+testKey)

	who, err := a.Authenticate(r, []byte("{}"))
	if err != nil {
		t.Fatalf("合法 Bearer 应通过: %v", err)
	}
	if who != "tester" {
		t.Fatalf("身份应为 tester，得到 %q", who)
	}
}

func TestBearerRejectsUnknownAndDisabled(t *testing.T) {
	enabled := newFake(t, testKey, testSecret, true)
	a := New(enabled, master, 5*time.Minute)

	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("Authorization", "Bearer nope")
	if _, err := a.Authenticate(r, nil); err != ErrUnauthorized {
		t.Fatalf("未知 key 应 ErrUnauthorized，得到 %v", err)
	}

	disabled := newFake(t, testKey, testSecret, false)
	b := New(disabled, master, 5*time.Minute)
	r2 := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r2.Header.Set("Authorization", "Bearer "+testKey)
	if _, err := b.Authenticate(r2, nil); err != ErrUnauthorized {
		t.Fatalf("停用 key 应 ErrUnauthorized，得到 %v", err)
	}

	r3 := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r3.Header.Set("Authorization", "Bearer    ")
	if _, err := a.Authenticate(r3, nil); err != ErrUnauthorized {
		t.Fatalf("空 key 应 ErrUnauthorized，得到 %v", err)
	}
}

func TestNoCredentialsIsUnauthorized(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 5*time.Minute)
	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	if _, err := a.Authenticate(r, nil); err != ErrUnauthorized {
		t.Fatalf("无凭据应 ErrUnauthorized，得到 %v", err)
	}
}

func TestSignatureHappyPath(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 5*time.Minute)
	body := []byte(`{"id":"p_1","value":5}`)
	ts := time.Now().Unix()

	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("X-Api-Key", testKey)
	r.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	r.Header.Set("X-Signature", Sign(testSecret, http.MethodPost, "/v1/scores", ts, body))

	who, err := a.Authenticate(r, body)
	if err != nil {
		t.Fatalf("合法签名应通过: %v", err)
	}
	if who != "tester" {
		t.Fatalf("身份应为 tester，得到 %q", who)
	}
}

func TestSignatureRejectsTampering(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 5*time.Minute)
	body := []byte(`{"id":"p_1","value":5}`)
	ts := time.Now().Unix()
	sig := Sign(testSecret, http.MethodPost, "/v1/scores", ts, body)

	cases := map[string]func(r *http.Request){
		"body 被改": func(r *http.Request) {}, // 用不同 body 调用 Authenticate
		"path 被改": func(r *http.Request) { r.URL.Path = "/v1/prompts" },
		"key 被改":  func(r *http.Request) { r.Header.Set("X-Api-Key", "other") },
		"签名被截断":   func(r *http.Request) { r.Header.Set("X-Signature", sig[:len(sig)-2]+"00") },
	}
	for name, mut := range cases {
		r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
		r.Header.Set("X-Api-Key", testKey)
		r.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
		r.Header.Set("X-Signature", sig)
		mut(r)

		gotBody := body
		if name == "body 被改" {
			gotBody = []byte(`{"id":"p_1","value":1}`)
		}
		if _, err := a.Authenticate(r, gotBody); err != ErrUnauthorized {
			t.Fatalf("%s：应被拒绝，得到 %v", name, err)
		}
	}
}

func TestSignatureRejectsClockSkew(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 300*time.Second)
	body := []byte(`{}`)
	old := time.Now().Add(-10 * time.Minute).Unix()

	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("X-Api-Key", testKey)
	r.Header.Set("X-Timestamp", strconv.FormatInt(old, 10))
	r.Header.Set("X-Signature", Sign(testSecret, http.MethodPost, "/v1/scores", old, body))

	if _, err := a.Authenticate(r, body); err != ErrUnauthorized {
		t.Fatalf("超时的重放请求应被拒绝，得到 %v", err)
	}
}

func TestSignatureRejectsGarbageTimestamp(t *testing.T) {
	a := New(newFake(t, testKey, testSecret, true), master, 5*time.Minute)
	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("X-Api-Key", testKey)
	r.Header.Set("X-Timestamp", "not-a-number")
	r.Header.Set("X-Signature", "deadbeef")
	if _, err := a.Authenticate(r, []byte(`{}`)); err != ErrUnauthorized {
		t.Fatalf("非法时间戳应被拒绝，得到 %v", err)
	}
}

// TestWrongMasterKeyCannotVerify 保证主密钥错误时绝对不会退化成“接受一切”。
// 注意：这属于服务端配置/轮换故障，返回的是错误（映射为 500）而不是 401，
// 自于安全上的不变量是“必须拒绝且不得返回身份”，而不是具体状态码。
func TestWrongMasterKeyCannotVerify(t *testing.T) {
	other := make([]byte, 32)
	for i := range other {
		other[i] = byte(i + 100)
	}
	a := New(newFake(t, testKey, testSecret, true), other, 5*time.Minute)
	body := []byte(`{}`)
	ts := time.Now().Unix()

	r := httptestRequest(t, http.MethodPost, "/v1/scores", nil)
	r.Header.Set("X-Api-Key", testKey)
	r.Header.Set("X-Timestamp", strconv.FormatInt(ts, 10))
	r.Header.Set("X-Signature", Sign(testSecret, http.MethodPost, "/v1/scores", ts, body))

	if who, err := a.Authenticate(r, body); err == nil || who != "" {
		t.Fatalf("主密钥不匹配必须拒给且不得返回身份，得到 who=%q err=%v", who, err)
	}
}

// TestSecretIsNotStoredAsHash 复核复核阶段修掉的那个缺陷：
// secret 必须以可解密形式存储，否则根本无法验签。
func TestSecretIsNotStoredAsHash(t *testing.T) {
	enc, err := secretbox.Seal(master, testSecret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// 密文应可解密回明文
	got, err := secretbox.Open(master, enc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != testSecret {
		t.Fatalf("解密结果不符，得到 %q", got)
	}
	// 且不是 base64(明文)
	if enc == base64.StdEncoding.EncodeToString([]byte(testSecret)) {
		t.Fatalf("secret 不得只是 base64 编码")
	}
	// 同一明文两次加密应不同（随机 nonce）
	enc2, err := secretbox.Seal(master, testSecret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if enc == enc2 {
		t.Fatalf("AES-GCM 每次应使用不同 nonce")
	}
}

func httptestRequest(t *testing.T, method, target string, _ any) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return r
}
