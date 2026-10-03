package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAppProcess puts an app_process script on PATH that runs body after
// checking it was asked to start the InputServer class from CLASSPATH.
func fakeAppProcess(t *testing.T, body string) (jar, logPath string) {
	t.Helper()
	dir := t.TempDir()
	jar = filepath.Join(dir, "input-server.jar")
	logPath = filepath.Join(dir, "starts.log")
	if err := os.WriteFile(jar, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"echo \"$CLASSPATH $*\" >> " + logPath + "\n" +
		body
	if err := os.WriteFile(filepath.Join(dir, "app_process"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return jar, logPath
}

// echoServer replies ok to everything except "fail", and exits on "die".
const echoServer = `echo ready
while read -r cmd rest; do
  case "$cmd" in
    fail) echo "error something broke" ;;
    die) exit 0 ;;
    hang) sleep 5 ;;
    *) echo ok ;;
  esac
done
`

func TestInputServerCalls(t *testing.T) {
	jar, logPath := fakeAppProcess(t, echoServer)
	s := newInputServer(jar)
	defer s.stop()

	if err := s.call(time.Second, "tap", "1", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.call(time.Second, "key", "KEYCODE_HOME"); err != nil {
		t.Fatal(err)
	}
	if err := s.call(time.Second, "fail"); err == nil || err.Error() != "fail: something broke" {
		t.Fatalf("unexpected error %v", err)
	}

	starts := calls(t, logPath)
	if len(starts) != 1 || starts[0] != jar+" /system/bin "+inputServerClass {
		t.Fatalf("expected one start, got %v", starts)
	}
}

func TestInputServerRestartsAfterExit(t *testing.T) {
	jar, logPath := fakeAppProcess(t, echoServer)
	s := newInputServer(jar)
	defer s.stop()

	if err := s.call(time.Second, "die"); err == nil {
		t.Fatal("expected an error when the server exits")
	}
	if err := s.call(time.Second, "tap", "1", "2"); err != nil {
		t.Fatal(err)
	}
	if starts := calls(t, logPath); len(starts) != 2 {
		t.Fatalf("expected a restart, got %v", starts)
	}
}

func TestInputServerRestartsAfterTimeout(t *testing.T) {
	jar, logPath := fakeAppProcess(t, echoServer)
	s := newInputServer(jar)
	defer s.stop()

	err := s.call(100*time.Millisecond, "hang")
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("expected a timeout, got %v", err)
	}
	if err := s.call(time.Second, "tap", "1", "2"); err != nil {
		t.Fatal(err)
	}
	if starts := calls(t, logPath); len(starts) != 2 {
		t.Fatalf("expected a restart, got %v", starts)
	}
}

func TestInputServerStartupFailure(t *testing.T) {
	jar, _ := fakeAppProcess(t, "echo 'error ClassNotFoundException: nope'\nexit 1\n")
	s := newInputServer(jar)

	err := s.call(time.Second, "tap", "1", "2")
	if err == nil || !strings.Contains(err.Error(), "ClassNotFoundException") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestInputServerMissingJar(t *testing.T) {
	s := newInputServer(filepath.Join(t.TempDir(), "missing.jar"))
	if err := s.call(time.Second, "tap", "1", "2"); err == nil {
		t.Fatal("expected an error for a missing jar")
	}
}
