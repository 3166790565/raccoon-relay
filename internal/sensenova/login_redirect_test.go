package sensenova

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：follow 必须手动跟跳转。授权页的 login_challenge 只在 302 的
// Location 里出现一次，默认 http.Client 会自动跟到底、把中间跳转吃掉，
// 于是永远取不到 challenge（线上表现为「未能从授权页取到登录 challenge」）。
func TestLoginFollowsRedirectsManually(t *testing.T) {
	const challenge = "challenge-from-iam"

	var platform *httptest.Server // handler 里要拼绝对跳转，先声明后赋值
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/auth", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); len(got) < 8 {
			http.Redirect(w, r, platform.URL+"/?error=invalid_state&error_description=too+weak&state="+got, http.StatusSeeOther)
			return
		}
		w.Header().Set("Set-Cookie", "oauth2_authentication_csrf=csrf-1; Path=/; HttpOnly")
		// 登录页由平台站自己托管（/login?login_challenge=…），与真实链路一致
		http.Redirect(w, r, platform.URL+"/login?login_challenge="+challenge, http.StatusFound)
	})
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		// 真正的登录页：地址里带 login_challenge
		if r.URL.Query().Get("login_challenge") != challenge {
			t.Errorf("登录页 challenge=%q", r.URL.Query().Get("login_challenge"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>login</html>"))
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("callback"))
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "code-1" {
			t.Errorf("换票 code=%q", r.Form.Get("code"))
		}
		if r.Form.Get("code_verifier") == "" {
			t.Error("换票缺少 code_verifier")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fakeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "ext": map[string]any{"tenant_id": "t-1"}}),
			"refresh_token": "refresh-1",
			"expires_in":    3600,
		})
	})
	// 先起服务器拿到自己的地址，handler 里要拼绝对跳转
	platform = httptest.NewUnstartedServer(mux)
	platform.Start()
	defer platform.Close()

	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/iam/authn/v1/auth/checkChallenge":
			if got := r.URL.Query().Get("challenge"); got != challenge {
				t.Errorf("校验的 challenge=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"is_valid": true})
		case "/iam/authn/v1/auth/nova/login":
			if ck := r.Header.Get("Cookie"); !strings.Contains(ck, "oauth2_authentication_csrf=csrf-1") {
				t.Errorf("登录接口没带上授权页的 Cookie：%q", ck)
			}
			var body struct {
				Username  string `json:"username"`
				Password  string `json:"password"`
				Challenge string `json:"challenge"`
				IsEncrypt bool   `json:"is_encrypt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Challenge != challenge {
				t.Errorf("提交的 challenge=%q", body.Challenge)
			}
			if body.Username != "1516020" || body.Password != "pw" || body.IsEncrypt {
				t.Errorf("提交的凭据=%+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"redirect": platform.URL + "/callback?code=code-1&state=s",
			})
		default:
			t.Errorf("IAM 路径=%q", r.URL.Path)
		}
	}))
	defer iam.Close()

	c := NewClient(5 * time.Second)
	c.Platform = platform.URL
	c.IAM = iam.URL
	res, err := c.Login(context.Background(), "1516020", "pw")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken != "refresh-1" || res.TenantID != "t-1" {
		t.Fatalf("登录结果不对: %+v", res)
	}
}

// 锁定与权限拒绝共用 reason=PermissionDenied，必须靠 message 区分，
// 否则锁定期间会把「没校验过密码」误报成「账号或密码不正确」。
func TestLoginReasonSeparatesLockFromPermission(t *testing.T) {
	details := []struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}{
		{Reason: "PermissionDenied", Message: "The account has been locked, please try again after 15 minutes"},
	}
	if got := loginReason(details, "PermissionDenied", 403); !strings.Contains(got, "锁定") {
		t.Fatalf("锁定应报锁定，实际 %q", got)
	}
	details[0].Message = "no access"
	if got := loginReason(details, "PermissionDenied", 403); !strings.Contains(got, "没有权限") {
		t.Fatalf("权限拒绝应报无权限，实际 %q", got)
	}
}

// 授权页直接回 error= 时要说清楚被拒原因，而不是笼统的「取不到 challenge」。
func TestLoginSurfacesAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/auth" {
			http.Redirect(w, r, "/?error=invalid_state&error_description=state+too+weak&state=x", http.StatusSeeOther)
			return
		}
		_, _ = w.Write([]byte("error page"))
	}))
	defer srv.Close()

	c := NewClient(5 * time.Second)
	c.Platform = srv.URL
	_, err := c.Login(context.Background(), "u", "p")
	if err == nil {
		t.Fatal("应当报错")
	}
	if !strings.Contains(err.Error(), "state too weak") {
		t.Fatalf("错误应带上上游原因，实际: %v", err)
	}
}
