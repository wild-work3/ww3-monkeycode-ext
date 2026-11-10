package monkeycode

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckinFlowMock(t *testing.T) {
	claimed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/users/wallet/checkin", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{"code":0,"data":{"checked_in":%v}}`, claimed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		t.Logf("POST checkin body=%s", b)
		claimed = true
		fmt.Fprint(w, `{"code":0,"data":{"checked_in":true}}`)
	})
	mux.HandleFunc("/api/v1/public/captcha/challenge", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"challenge":{"c":1,"s":1,"d":1},"token":"tok"}`)
	})
	mux.HandleFunc("/api/v1/public/captcha/redeem", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		t.Logf("redeem body=%s", b)
		fmt.Fprint(w, `{"success":true,"token":"captcha-ok"}`)
	})
	mux.HandleFunc("/api/v1/users/wallet", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"balance":1234567,"daily_token_balance":100000,"daily_token_limit":500000}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(Config{})
	c.Console = srv.URL
	a := &Auth{UID: "u1", ConsoleCookie: "ck"}

	rep, err := c.DailyCheckinReport(context.Background(), a)
	t.Logf("1st: %+v err=%v", rep, err)
	if err != nil || rep.Status != CheckinClaimed {
		t.Fatalf("1st checkin: %+v %v", rep, err)
	}
	rep, err = c.DailyCheckinReport(context.Background(), a)
	t.Logf("2nd: %+v err=%v", rep, err)
	if err != nil || rep.Status != CheckinAlready {
		t.Fatalf("2nd checkin: %+v %v", rep, err)
	}
	bal, items, err := c.UserResourceDetail(context.Background(), a)
	t.Logf("wallet: bal=%d items=%+v err=%v", bal, items, err)
	if err != nil || bal != 1235 {
		t.Fatalf("wallet: %d %v", bal, err)
	}
	for _, it := range items {
		t.Logf("item: name=%s unit=%s remain=%v info_only=%v", it.Name, it.Unit, *it.Remain, it.InfoOnly)
	}
	// 无 cookie 时必须是 Auth 错误
	if _, _, err := c.UserResourceDetail(context.Background(), &Auth{UID: "x"}); err == nil {
		t.Fatal("want auth error without cookie")
	} else {
		t.Logf("no-cookie err: %v", err)
	}
}
