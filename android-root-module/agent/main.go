// mobile-mcp-agent runs on a rooted Android device (started by the Magisk /
// KernelSU module) and exposes a small, token-protected HTTP API that lets
// mobile-mcp drive the device without adb and without an accessibility service.
//
// Every operation is a fixed command executed as root with an explicit argv
// (never through a shell), so request parameters cannot inject commands.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const version = "0.1.0"

const (
	commandTimeout = 30 * time.Second
	installTimeout = 5 * time.Minute
	maxJSONBody    = 64 * 1024
	maxApkBody     = 2 << 30
	tmpDir         = "/data/local/tmp"
	uiDumpPath     = tmpDir + "/mobile-mcp-ui.xml"
	devicekitPkg   = "com.mobilenext.devicekit"
)

var (
	packageNameRe = regexp.MustCompile(`^[a-zA-Z0-9._]+$`)
	localeRe      = regexp.MustCompile(`^[a-zA-Z0-9,_-]+$`)
	keycodeRe     = regexp.MustCompile(`^KEYCODE_[A-Z0-9_]+$`)
	displayIDRe   = regexp.MustCompile(`^[0-9]+$`)
	sizeRe        = regexp.MustCompile(`(Physical|Override) size: (\d+)x(\d+)`)
	densityRe     = regexp.MustCompile(`(Physical|Override) density: (\d+)`)
	resumedRe     = regexp.MustCompile(`(?:mResumedActivity|topResumedActivity|ResumedActivity)[:=].*?\s([a-zA-Z0-9_.]+)/`)
	focusRe       = regexp.MustCompile(`mCurrentFocus=.*?\s([a-zA-Z0-9_.]+)/`)
)

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...)}
}

type agent struct {
	token []byte
	// uiautomator can only run one dump at a time
	uiMu sync.Mutex
}

// run executes a command as the agent's own user (root) and returns stdout.
func run(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		output := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if output == "" {
			output = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), output)
	}

	return stdout.Bytes(), nil
}

func getprop(name string) string {
	out, err := run(commandTimeout, "getprop", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lastMatch prefers the "Override" line over "Physical", since wm prints the
// physical value first.
func lastMatch(re *regexp.Regexp, text string) []string {
	matches := re.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	return matches[len(matches)-1]
}

func readJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return badRequest("failed reading body: %v", err)
	}
	if len(body) > maxJSONBody {
		return badRequest("request body too large")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return badRequest("invalid json: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func validatePackage(pkg string) error {
	if !packageNameRe.MatchString(pkg) {
		return badRequest("invalid package name: %q", pkg)
	}
	return nil
}

func validateCoordinate(name string, v int) error {
	if v < 0 || v > 100000 {
		return badRequest("invalid %s coordinate: %d", name, v)
	}
	return nil
}

// handler wraps an endpoint with method checking, auth and error mapping.
func (a *agent) handler(methods []string, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		allowed := false
		for _, m := range methods {
			if r.Method == m {
				allowed = true
			}
		}
		if !allowed {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		auth := r.Header.Get("Authorization")
		given := []byte(strings.TrimPrefix(auth, "Bearer "))
		if !strings.HasPrefix(auth, "Bearer ") || subtle.ConstantTimeCompare(given, a.token) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid or missing token"})
			return
		}

		if err := fn(w, r); err != nil {
			status := http.StatusInternalServerError
			var ae *apiError
			if errors.As(err, &ae) {
				status = ae.status
			}
			log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
	}
}

func (a *agent) info(w http.ResponseWriter, r *http.Request) error {
	sizeOut, err := run(commandTimeout, "wm", "size")
	if err != nil {
		return err
	}
	m := lastMatch(sizeRe, string(sizeOut))
	if m == nil {
		return fmt.Errorf("failed to parse screen size from %q", string(sizeOut))
	}
	width, _ := strconv.Atoi(m[2])
	height, _ := strconv.Atoi(m[3])

	density := 160
	if densityOut, err := run(commandTimeout, "wm", "density"); err == nil {
		if m := lastMatch(densityRe, string(densityOut)); m != nil {
			density, _ = strconv.Atoi(m[2])
		}
	}

	features := ""
	if out, err := run(commandTimeout, "pm", "list", "features"); err == nil {
		features = string(out)
	}
	deviceType := "mobile"
	if strings.Contains(features, "android.software.leanback") || strings.Contains(features, "android.hardware.type.television") {
		deviceType = "tv"
	}

	writeJSON(w, map[string]any{
		"agentVersion": version,
		"manufacturer": getprop("ro.product.manufacturer"),
		"model":        getprop("ro.product.model"),
		"version":      getprop("ro.build.version.release"),
		"sdk":          getprop("ro.build.version.sdk"),
		"deviceType":   deviceType,
		"width":        width,
		"height":       height,
		"density":      density,
	})
	return nil
}

func (a *agent) screenshot(w http.ResponseWriter, r *http.Request) error {
	args := []string{"-p"}
	if display := r.URL.Query().Get("display"); display != "" {
		if !displayIDRe.MatchString(display) {
			return badRequest("invalid display id: %q", display)
		}
		args = append(args, "-d", display)
	}

	png, err := run(commandTimeout, "screencap", args...)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
	return nil
}

func (a *agent) tap(w http.ResponseWriter, r *http.Request) error {
	var req struct{ X, Y int }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := errors.Join(validateCoordinate("x", req.X), validateCoordinate("y", req.Y)); err != nil {
		return err
	}
	if _, err := run(commandTimeout, "input", "tap", strconv.Itoa(req.X), strconv.Itoa(req.Y)); err != nil {
		return err
	}
	ok(w)
	return nil
}

func (a *agent) swipe(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		X1, Y1, X2, Y2 int
		Duration       int `json:"duration"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := errors.Join(
		validateCoordinate("x1", req.X1), validateCoordinate("y1", req.Y1),
		validateCoordinate("x2", req.X2), validateCoordinate("y2", req.Y2),
	); err != nil {
		return err
	}
	if req.Duration <= 0 || req.Duration > 60000 {
		return badRequest("invalid duration: %d", req.Duration)
	}

	_, err := run(commandTimeout+time.Duration(req.Duration)*time.Millisecond, "input", "swipe",
		strconv.Itoa(req.X1), strconv.Itoa(req.Y1), strconv.Itoa(req.X2), strconv.Itoa(req.Y2), strconv.Itoa(req.Duration))
	if err != nil {
		return err
	}
	ok(w)
	return nil
}

func (a *agent) key(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Key string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if !keycodeRe.MatchString(req.Key) {
		return badRequest("invalid key: %q", req.Key)
	}
	if _, err := run(commandTimeout, "input", "keyevent", req.Key); err != nil {
		return err
	}
	ok(w)
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return false
		}
	}
	return true
}

func (a *agent) text(w http.ResponseWriter, r *http.Request) error {
	var req struct{ Text string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.Text == "" {
		ok(w)
		return nil
	}

	if isASCII(req.Text) {
		// argv is passed straight to `input`, no shell quoting needed
		if _, err := run(commandTimeout, "input", "text", req.Text); err != nil {
			return err
		}
		ok(w)
		return nil
	}

	// `input text` only handles ascii; go through devicekit's clipboard if available
	if _, err := run(commandTimeout, "pm", "path", devicekitPkg); err != nil {
		return badRequest("Non-ASCII text is not supported on Android, please install mobilenext devicekit, see https://github.com/mobile-next/devicekit-android")
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(req.Text))
	if _, err := run(commandTimeout, "am", "broadcast", "-a", "devicekit.clipboard.set", "-e", "encoding", "base64", "-e", "text", encoded, "-n", devicekitPkg+"/.ClipboardBroadcastReceiver"); err != nil {
		return err
	}
	if _, err := run(commandTimeout, "input", "keyevent", "KEYCODE_PASTE"); err != nil {
		return err
	}
	_, _ = run(commandTimeout, "am", "broadcast", "-a", "devicekit.clipboard.clear", "-n", devicekitPkg+"/.ClipboardBroadcastReceiver")
	ok(w)
	return nil
}

func (a *agent) ui(w http.ResponseWriter, r *http.Request) error {
	a.uiMu.Lock()
	defer a.uiMu.Unlock()

	var lastErr error
	for tries := 0; tries < 10; tries++ {
		_ = os.Remove(uiDumpPath)
		out, err := run(commandTimeout, "uiautomator", "dump", uiDumpPath)
		if err != nil || strings.Contains(string(out), "null root node") {
			lastErr = fmt.Errorf("uiautomator dump failed: %v %s", err, strings.TrimSpace(string(out)))
			time.Sleep(200 * time.Millisecond)
			continue
		}

		dump, err := os.ReadFile(uiDumpPath)
		_ = os.Remove(uiDumpPath)
		if err != nil {
			lastErr = err
			continue
		}

		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(dump)
		return nil
	}

	return fmt.Errorf("failed to get UIAutomator XML: %v", lastErr)
}

func (a *agent) apps(w http.ResponseWriter, r *http.Request) error {
	// only apps that have a launcher activity are returned
	out, err := run(commandTimeout, "cmd", "package", "query-activities", "-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER")
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	packages := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if pkg, found := strings.CutPrefix(line, "packageName="); found && !seen[pkg] {
			seen[pkg] = true
			packages = append(packages, pkg)
		}
	}

	writeJSON(w, map[string]any{"packages": packages})
	return nil
}

func (a *agent) foreground(w http.ResponseWriter, r *http.Request) error {
	if out, err := run(commandTimeout, "dumpsys", "activity", "activities"); err == nil {
		if m := resumedRe.FindStringSubmatch(string(out)); m != nil {
			writeJSON(w, map[string]string{"packageName": m[1]})
			return nil
		}
	}

	out, err := run(commandTimeout, "dumpsys", "window", "windows")
	if err != nil {
		return err
	}
	if m := focusRe.FindStringSubmatch(string(out)); m != nil {
		writeJSON(w, map[string]string{"packageName": m[1]})
		return nil
	}

	return errors.New("could not determine the foreground app")
}

func (a *agent) launch(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		PackageName string `json:"packageName"`
		Locale      string `json:"locale"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := validatePackage(req.PackageName); err != nil {
		return err
	}

	if req.Locale != "" {
		if !localeRe.MatchString(req.Locale) {
			return badRequest("invalid locale: %q", req.Locale)
		}
		// set-app-locales requires Android 13+ (API 33), ignore on older versions
		_, _ = run(commandTimeout, "cmd", "locale", "set-app-locales", req.PackageName, "--locales", req.Locale)
	}

	if _, err := run(commandTimeout, "monkey", "-p", req.PackageName, "-c", "android.intent.category.LAUNCHER", "1"); err != nil {
		return badRequest("Failed launching app with package name %q, please make sure it exists", req.PackageName)
	}
	ok(w)
	return nil
}

func (a *agent) terminate(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		PackageName string `json:"packageName"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := validatePackage(req.PackageName); err != nil {
		return err
	}
	if _, err := run(commandTimeout, "am", "force-stop", req.PackageName); err != nil {
		return err
	}
	ok(w)
	return nil
}

func (a *agent) install(w http.ResponseWriter, r *http.Request) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	apkPath := filepath.Join(tmpDir, "mobile-mcp-"+hex.EncodeToString(suffix)+".apk")

	f, err := os.OpenFile(apkPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(apkPath)

	n, err := io.Copy(f, io.LimitReader(r.Body, maxApkBody+1))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n == 0 {
		return badRequest("empty apk")
	}
	if n > maxApkBody {
		return badRequest("apk too large")
	}

	out, err := run(installTimeout, "pm", "install", "-r", apkPath)
	if err != nil {
		return badRequest("%v", err)
	}
	writeJSON(w, map[string]string{"status": "ok", "output": strings.TrimSpace(string(out))})
	return nil
}

func (a *agent) uninstall(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		PackageName string `json:"packageName"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if err := validatePackage(req.PackageName); err != nil {
		return err
	}
	if _, err := run(commandTimeout, "pm", "uninstall", req.PackageName); err != nil {
		return badRequest("%v", err)
	}
	ok(w)
	return nil
}

func (a *agent) openURL(w http.ResponseWriter, r *http.Request) error {
	var req struct{ URL string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	if req.URL == "" || strings.HasPrefix(req.URL, "-") || strings.ContainsAny(req.URL, "\x00\r\n") {
		return badRequest("invalid url: %q", req.URL)
	}
	if _, err := run(commandTimeout, "am", "start", "-a", "android.intent.action.VIEW", "-d", req.URL); err != nil {
		return err
	}
	ok(w)
	return nil
}

func (a *agent) orientation(w http.ResponseWriter, r *http.Request) error {
	if r.Method == http.MethodGet {
		out, err := run(commandTimeout, "settings", "get", "system", "user_rotation")
		if err != nil {
			return err
		}
		orientation := "landscape"
		if strings.TrimSpace(string(out)) == "0" {
			orientation = "portrait"
		}
		writeJSON(w, map[string]string{"orientation": orientation})
		return nil
	}

	var req struct{ Orientation string }
	if err := readJSON(r, &req); err != nil {
		return err
	}
	var value string
	switch req.Orientation {
	case "portrait":
		value = "0"
	case "landscape":
		value = "1"
	default:
		return badRequest("invalid orientation: %q", req.Orientation)
	}

	// disable auto-rotation prior to setting the orientation
	if _, err := run(commandTimeout, "settings", "put", "system", "accelerometer_rotation", "0"); err != nil {
		return err
	}
	if _, err := run(commandTimeout, "settings", "put", "system", "user_rotation", value); err != nil {
		return err
	}
	ok(w)
	return nil
}

func (a *agent) routes() *http.ServeMux {
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}

	mux := http.NewServeMux()
	mux.Handle("/v1/info", a.handler(get, a.info))
	mux.Handle("/v1/screenshot", a.handler(get, a.screenshot))
	mux.Handle("/v1/ui", a.handler(get, a.ui))
	mux.Handle("/v1/input/tap", a.handler(post, a.tap))
	mux.Handle("/v1/input/swipe", a.handler(post, a.swipe))
	mux.Handle("/v1/input/key", a.handler(post, a.key))
	mux.Handle("/v1/input/text", a.handler(post, a.text))
	mux.Handle("/v1/apps", a.handler(get, a.apps))
	mux.Handle("/v1/apps/foreground", a.handler(get, a.foreground))
	mux.Handle("/v1/apps/launch", a.handler(post, a.launch))
	mux.Handle("/v1/apps/terminate", a.handler(post, a.terminate))
	mux.Handle("/v1/apps/install", a.handler(post, a.install))
	mux.Handle("/v1/apps/uninstall", a.handler(post, a.uninstall))
	mux.Handle("/v1/url", a.handler(post, a.openURL))
	mux.Handle("/v1/orientation", a.handler([]string{http.MethodGet, http.MethodPost}, a.orientation))
	return mux
}

// loadOrCreateToken reads the token file, generating a random token on first use.
func loadOrCreateToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 16 {
			return "", fmt.Errorf("token in %s is too short (min 16 chars)", path)
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// localFilterListener drops connections whose source is the device itself
// (loopback or one of its own interface addresses) before any HTTP is read,
// so apps running on the phone cannot reach the root agent.
type localFilterListener struct {
	net.Listener
	// localAddrs returns the device's own addresses; it is called per
	// connection because interface addresses change (Wi-Fi, VPN, mobile data).
	localAddrs func() ([]net.Addr, error)
}

func (l *localFilterListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.isLocal(c.RemoteAddr()) {
			log.Printf("dropped connection from local address %s", c.RemoteAddr())
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

func (l *localFilterListener) isLocal(remote net.Addr) bool {
	ap, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		// unknown address format: refuse rather than risk letting a local app in
		return true
	}
	ip := ap.Addr().Unmap().WithZone("")
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := l.localAddrs()
	if err != nil {
		log.Printf("listing interface addresses: %v", err)
		return true
	}
	for _, a := range addrs {
		var candidate net.IP
		switch v := a.(type) {
		case *net.IPNet:
			candidate = v.IP
		case *net.IPAddr:
			candidate = v.IP
		}
		if own, found := netip.AddrFromSlice(candidate); found && own.Unmap() == ip {
			return true
		}
	}
	return false
}

func main() {
	listen := flag.String("listen", "0.0.0.0:8765", "address to listen on")
	tokenFile := flag.String("token-file", "/data/adb/mobile-mcp/token", "file holding the bearer token (created if missing)")
	allowLocal := flag.Bool("allow-local", false, "accept connections from the device itself (loopback and its own IPs)")
	printToken := flag.Bool("print-token", false, "print the token (creating it if needed) and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	token, err := loadOrCreateToken(*tokenFile)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	if *printToken {
		fmt.Println(token)
		return
	}

	if os.Getenv("PATH") == "" {
		os.Setenv("PATH", "/system/bin:/system/xbin:/vendor/bin")
	}

	a := &agent{token: []byte(token)}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	if !*allowLocal {
		ln = &localFilterListener{Listener: ln, localAddrs: net.InterfaceAddrs}
	}

	log.Printf("mobile-mcp-agent %s listening on %s (allow local: %t)", version, *listen, *allowLocal)
	log.Fatal(srv.Serve(ln))
}
