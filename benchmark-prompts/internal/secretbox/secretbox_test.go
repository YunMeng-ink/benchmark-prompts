package secretbox

import (
	"strings"
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestSealOpenRoundTrip(t *testing.T) {
	k := testKey()
	enc, err := Seal(k, "s3cret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if enc == "" {
		t.Fatalf("密文不能为空")
	}
	if strings.Contains(enc, "s3cret") {
		t.Fatalf("密文不得包含明文")
	}
	got, err := Open(k, enc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != "s3cret" {
		t.Fatalf("解密结果不符，得到 %q", got)
	}
}

func TestEmptySecretRoundTrips(t *testing.T) {
	k := testKey()
	enc, err := Seal(k, "")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	got, err := Open(k, enc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != "" {
		t.Fatalf("空串应能往返，得到 %q", got)
	}
}

func TestWrongKeyIsRejected(t *testing.T) {
	enc, err := Seal(testKey(), "topsecret")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other := testKey()
	other[0] ^= 0xFF
	if _, err := Open(other, enc); err == nil {
		t.Fatalf("错误主密钥必须解密失败（复核阶段修的就是这一点）")
	}
}

func TestTamperingIsRejected(t *testing.T) {
	k := testKey()
	enc, err := Seal(k, "payload")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := Open(k, enc+"AA"); err == nil {
		t.Fatalf("追加密文字节必须被认证标签拒绝")
	}
	if _, err := Open(k, "这不是 base64 !!"); err == nil {
		t.Fatalf("非法 base64 必须报错")
	}
	// 长度不足一个 nonce（12 字节）
	if _, err := Open(k, "c2hvcnQ="); err == nil {
		t.Fatalf("超短密文必须报错")
	}
}

func TestKeyLengthIsValidated(t *testing.T) {
	for _, bad := range [][]byte{nil, make([]byte, 16), make([]byte, 24), make([]byte, 48)} {
		if _, err := Seal(bad, "x"); err == nil {
			t.Fatalf("%d 字节主密钥应被拒绝", len(bad))
		}
		if _, err := Open(bad, "AAAA"); err == nil {
			t.Fatalf("%d 字节主密钥应被拒绝", len(bad))
		}
	}
}

func TestNonceMakesCiphertextNonDeterministic(t *testing.T) {
	k := testKey()
	a, err := Seal(k, "same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	b, err := Seal(k, "same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if a == b {
		t.Fatalf("AES-GCM 每次必须使用不同 nonce，否则密文可被比对")
	}
	ga, _ := Open(k, a)
	gb, _ := Open(k, b)
	if ga != "same" || gb != "same" {
		t.Fatalf("两份密文都应能解密回原文，得到 %q / %q", ga, gb)
	}
}
