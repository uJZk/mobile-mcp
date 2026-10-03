package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
