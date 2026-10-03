package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

// fakeCommands puts shell scripts on PATH that log their argv to a file and
// print canned output, standing in for the android binaries.
func fakeCommands(t *testing.T, outputs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	for _, name := range []string{"input", "wm", "pm", "am", "getprop", "screencap", "cmd", "monkey", "settings", "dumpsys"} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + logPath + "\n"
		if out, found := outputs[name]; found {
			script += "printf '%s\\n' '" + out + "'\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return logPath
}

func calls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func request(t *testing.T, method, path, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	a := &agent{token: []byte(testToken)}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	return rec
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	logPath := fakeCommands(t, nil)
	for _, token := range []string{"", "wrong-token-wrong-token"} {
		rec := request(t, http.MethodPost, "/v1/input/tap", `{"x":1,"y":2}`, token)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: expected 401, got %d", token, rec.Code)
		}
	}
	if c := calls(t, logPath); len(c) != 0 {
		t.Fatalf("expected no commands, got %v", c)
	}
}

func TestTap(t *testing.T) {
	logPath := fakeCommands(t, nil)
	rec := request(t, http.MethodPost, "/v1/input/tap", `{"x":100,"y":200}`, testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	if c := calls(t, logPath); len(c) != 1 || c[0] != "input tap 100 200" {
		t.Fatalf("unexpected calls %v", c)
	}
}

func TestTextIsPassedAsSingleArgument(t *testing.T) {
	logPath := fakeCommands(t, nil)
	rec := request(t, http.MethodPost, "/v1/input/text", `{"text":"a b; rm -rf /"}`, testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	if c := calls(t, logPath); len(c) != 1 || c[0] != "input text a b; rm -rf /" {
		t.Fatalf("unexpected calls %v", c)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	logPath := fakeCommands(t, nil)
	cases := []struct{ path, body string }{
		{"/v1/input/key", `{"key":"KEYCODE_BACK; reboot"}`},
		{"/v1/apps/launch", `{"packageName":"com.example;reboot"}`},
		{"/v1/apps/terminate", `{"packageName":"-a"}`},
		{"/v1/url", `{"url":"--user 0"}`},
		{"/v1/input/tap", `{"x":-1,"y":2}`},
		{"/v1/input/swipe", `{"x1":1,"y1":2,"x2":3,"y2":4,"duration":0}`},
	}
	for _, tc := range cases {
		rec := request(t, http.MethodPost, tc.path, tc.body, testToken)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s %s: expected 400, got %d", tc.path, tc.body, rec.Code)
		}
	}
	if c := calls(t, logPath); len(c) != 0 {
		t.Fatalf("expected no commands, got %v", c)
	}
}

func TestInfoPrefersOverrideSize(t *testing.T) {
	fakeCommands(t, map[string]string{
		"wm":      "Physical size: 1080x2400\nOverride size: 720x1600",
		"getprop": "14",
		"pm":      "feature:android.hardware.touchscreen",
	})
	rec := request(t, http.MethodGet, "/v1/info", "", testToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	var info struct {
		Width, Height int
		DeviceType    string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Width != 720 || info.Height != 1600 || info.DeviceType != "mobile" {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestApps(t *testing.T) {
	fakeCommands(t, map[string]string{
		"cmd": "  packageName=com.android.settings\n  packageName=com.example\n  packageName=com.example",
	})
	rec := request(t, http.MethodGet, "/v1/apps", "", testToken)
	var resp struct{ Packages []string }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if strings.Join(resp.Packages, ",") != "com.android.settings,com.example" {
		t.Fatalf("unexpected packages %v", resp.Packages)
	}
}

func TestForeground(t *testing.T) {
	fakeCommands(t, map[string]string{
		"dumpsys": "    topResumedActivity=ActivityRecord{2b1 u0 com.example.app/.MainActivity t12}",
	})
	rec := request(t, http.MethodGet, "/v1/apps/foreground", "", testToken)
	if !strings.Contains(rec.Body.String(), `"packageName":"com.example.app"`) {
		t.Fatalf("unexpected body %s", rec.Body)
	}
}

func TestLoadOrCreateToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "token")
	first, err := loadOrCreateToken(path)
	if err != nil || len(first) != 48 {
		t.Fatalf("unexpected token %q err %v", first, err)
	}
	second, err := loadOrCreateToken(path)
	if err != nil || second != first {
		t.Fatalf("token not persisted: %q vs %q (%v)", first, second, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected token permissions %v", st.Mode())
	}
}

func TestLocalFilterListenerDropsLocalSources(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()

	l := &localFilterListener{Listener: inner, localAddrs: net.InterfaceAddrs}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})}
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + inner.Addr().String() + "/v1/info")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected loopback connection to be dropped, got status %d", resp.StatusCode)
	}
}

func TestLocalFilterListenerIsLocal(t *testing.T) {
	own := []net.Addr{
		&net.IPNet{IP: net.ParseIP("192.168.1.20"), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
		&net.IPAddr{IP: net.ParseIP("100.64.0.5")},
	}
	l := &localFilterListener{localAddrs: func() ([]net.Addr, error) { return own, nil }}

	cases := map[string]bool{
		"127.0.0.1:5000":           true,
		"127.0.0.2:5000":           true,
		"[::1]:5000":               true,
		"[::ffff:127.0.0.1]:5000":  true,
		"192.168.1.20:5000":        true,
		"[::ffff:192.168.1.20]:80": true,
		"[fe80::1%wlan0]:5000":     true,
		"100.64.0.5:5000":          true,
		"192.168.1.21:5000":        false,
		"10.0.0.7:5000":            false,
		"[2001:db8::1]:5000":       false,
	}
	for addr, want := range cases {
		remote := fakeAddr(addr)
		if got := l.isLocal(remote); got != want {
			t.Errorf("isLocal(%s) = %t, want %t", addr, got, want)
		}
	}
}

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }
