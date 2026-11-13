package login

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// ExtractCode 必须吃三种形态：完整深链、裸 code、含 code 的任意地址。
func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"office-raccoon://auth/callback?code=abc123":  "abc123",
		"office-raccoon://auth/callback?authCode=xyz": "xyz",
		"https://example.com/cb?code=deep&state=1":    "deep",
		"随便什么 code=rawvalue 前后有字":                     "rawvalue",
		"barecode123": "barecode123",
		"https://example.com/cb?authorization_code=zz-99": "zz-99",
	}
	for input, want := range cases {
		if got := ExtractCode(input); got != want {
			t.Errorf("ExtractCode(%q) = %q，期望 %q", input, got, want)
		}
	}

	for _, empty := range []string{"", "   ", "https://example.com/nothing", "a b c"} {
		if got := ExtractCode(empty); got != "" {
			t.Errorf("ExtractCode(%q) 应返回空，实际 %q", empty, got)
		}
	}
}

func makeToken(uid string, exp time.Time) string {
	payload := fmt.Sprintf(`{"sub":%q,"sid":"ses-1","iss":"xiaohuanxiong.com/iam","exp":%d}`, uid, exp.Unix())
	return "eyJhbGciOiJIUzI1NiJ9." + b64(payload) + ".sig"
}

func b64(s string) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	data := []byte(s)
	var b strings.Builder
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(table[(n>>18)&63])
		b.WriteByte(table[(n>>12)&63])
		if rem > 1 {
			b.WriteByte(table[(n>>6)&63])
		}
		if rem > 2 {
			b.WriteByte(table[n&63])
		}
	}
	return b.String()
}

func newMgr(t *testing.T, upstream *httptest.Server) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := raccoon.NewClient(upstream.URL, 10*time.Second)
	return New(st, c, upstream.URL, "", ""), st
}

func TestManualLoginRejectsNonJWT(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	m, _ := newMgr(t, up)

	res := m.Manual(context.Background(), AccountInput{Token: "not-a-jwt"})
	if res.OK {
		t.Fatal("非 JWT 应拒绝")
	}
	if !strings.Contains(res.Message, "JWT") {
		t.Fatalf("错误文案应说明原因: %s", res.Message)
	}
}

// 手填路径严格要求 sub：iss 是所有号共用的签发方常量，拿它当 uid 会让整池塌成一个号。
func TestManualLoginRejectsMissingSub(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	m, _ := newMgr(t, up)

	token := "eyJhbGciOiJIUzI1NiJ9." + b64(`{"exp":2000000000,"iss":"xiaohuanxiong.com/iam"}`) + ".sig"
	res := m.Manual(context.Background(), AccountInput{Token: token})
	if res.OK {
		t.Fatal("没有 sub 的 token 必须拒绝，否则 iss 兜底会把整池塌成一个号")
	}
	if !strings.Contains(res.Message, "sub") {
		t.Fatalf("错误文案应指明缺 sub: %s", res.Message)
	}
}

func TestManualLoginAcceptsValidToken(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	m, st := newMgr(t, up)

	token := makeToken("pu-manual-1", time.Now().Add(3*time.Hour))
	res := m.Manual(context.Background(), AccountInput{Token: token, RefreshToken: "rf", Name: "手填号"})
	if !res.OK {
		t.Fatalf("合法 token 应入池: %s", res.Message)
	}
	if res.UID != "pu-manual-1" {
		t.Fatalf("uid 不对: %s", res.UID)
	}

	acc, err := st.GetByUID(context.Background(), "pu-manual-1")
	if err != nil || acc == nil {
		t.Fatal("账号应已入库")
	}
	if acc.TokenExp == 0 {
		t.Fatal("应从 JWT 解出 tokenExp")
	}
	if acc.Name != "手填号" {
		t.Fatalf("名称不对: %s", acc.Name)
	}
	if acc.DeviceID == "" {
		t.Fatal("应派生 deviceId")
	}
}

func TestManualLoginSecondTimeUpdates(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	m, _ := newMgr(t, up)
	ctx := context.Background()

	tok1 := makeToken("pu-1", time.Now().Add(time.Hour))
	m.Manual(ctx, AccountInput{Token: tok1})
	tok2 := makeToken("pu-1", time.Now().Add(2*time.Hour))
	res := m.Manual(ctx, AccountInput{Token: tok2})
	if !res.OK || !res.Updated {
		t.Fatalf("同 uid 第二次应标记 updated，实际 %+v", res)
	}
}

func TestExchangeSuccess(t *testing.T) {
	newTok := makeToken("pu-new-1", time.Now().Add(3*time.Hour))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case raccoon.PathExchange:
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":%q,"refresh_token":"rf-1","office_identity":"personal"}}`, newTok)
		case raccoon.PathBindDevice:
			fmt.Fprint(w, `{"code":0,"data":{}}`)
		}
	}))
	defer up.Close()
	m, st := newMgr(t, up)

	res := m.Exchange(context.Background(), "office-raccoon://auth/callback?code=good-code")
	if !res.OK {
		t.Fatalf("兑换应成功: %s", res.Message)
	}
	if res.UID != "pu-new-1" {
		t.Fatalf("uid 不对: %s", res.UID)
	}
	acc, _ := st.GetByUID(context.Background(), "pu-new-1")
	if acc == nil || acc.RefreshToken != "rf-1" {
		t.Fatalf("凭据未正确入库: %+v", acc)
	}
	if acc.OfficeIdentity != "personal" {
		t.Fatalf("officeIdentity 未入库: %s", acc.OfficeIdentity)
	}
}

// 200035 = 授权码已消费，必须给可执行提示。
func TestExchangeConsumedCode(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":200035,"message":"authorization code not found"}`)
	}))
	defer up.Close()
	m, _ := newMgr(t, up)

	res := m.Exchange(context.Background(), "code=used")
	if res.OK {
		t.Fatal("已消费的码不应成功")
	}
	if !strings.Contains(res.Message, "已失效或已被消费") {
		t.Fatalf("应给出 200035 专属提示，实际: %s", res.Message)
	}
}

func TestExchangeRejectsGarbageInput(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	m, _ := newMgr(t, up)

	res := m.Exchange(context.Background(), "")
	if res.OK || !strings.Contains(res.Message, "无法从粘贴的内容里提取授权码") {
		t.Fatalf("空输入应给明确提示，实际: %+v", res)
	}
}

func TestAuthorizeURL(t *testing.T) {
	m := New(nil, nil, "https://xiaohuanxiong.com", "", "")
	u := m.AuthorizeURL()
	if !strings.Contains(u, "/code/authorize") || !strings.Contains(u, "login_source=desktop") {
		t.Fatalf("授权页 URL 不对: %s", u)
	}
	if !strings.Contains(u, "appname=") {
		t.Fatalf("应带 appname 参数: %s", u)
	}
}

var _ = json.Marshal
