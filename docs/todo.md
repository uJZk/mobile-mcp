# Rooted Android review conclusions

## Input correctness and cancellation

- The HTTP agent decodes tap/swipe coordinates into zero-valued integers, and zero is a valid coordinate. Missing or `null` fields can therefore become real input at `(0,0)`. Coordinate presence must be validated separately from coordinate range. See [agent/main.go](../android-root-module/agent/main.go).
- Input commands are serialized, but a caller waiting for the serialization lock has no cancellation deadline. A queued gesture can still execute after its HTTP client has timed out. Queue waiting and command execution should honor request cancellation. See [inputserver.go](../android-root-module/agent/inputserver.go) and [android-root.ts](../src/android-root.ts).
- Touch jitter is sampled independently for tap down/up and for movement events. That can move a nominal tap or long press, and can push edge contacts off-screen. Gesture-aware jitter should keep stationary contacts stable and clamp coordinates. See [InputServer.java](../android-root-module/input-server/src/com/mobilenext/mcp/InputServer.java).

## Text and payload compatibility

- The input-server DEX targets API 23, while `KEYCODE_PASTE` is API 24+. Clipboard clearing uses an API 28 method and silently ignores failure on older systems; consequently API 23 text fallback may not paste, and API 23–27 may retain pasted text. Align the supported API range and clipboard behavior with the claims in the [module README](../android-root-module/README.md). See [build.sh](../android-root-module/build.sh) and [InputServer.java](../android-root-module/input-server/src/com/mobilenext/mcp/InputServer.java).
- The MCP text tool has no size limit, but the agent rejects JSON bodies above 64 KiB. Large text therefore fails only after reaching the device endpoint. Preflight or chunk the client input, or expose a matching limit. See [server.ts](../src/server.ts), [android-root.ts](../src/android-root.ts), and [agent/main.go](../android-root-module/agent/main.go).
- APK installation reads the entire file synchronously and copies it into another buffer before upload, while the agent accepts APK bodies up to 2 GiB. Large packages can stall or exhaust the MCP host's memory; use bounded streaming or a lower explicit limit. See [android-root.ts](../src/android-root.ts) and [agent/main.go](../android-root-module/agent/main.go).

## Root-device capability and operational boundaries

- Screen recording is documented as unsupported by the root module, but the generic MCP tool still invokes `mobilecli` for a `root:` device and immediately reports success. Root-device capabilities should be checked before starting unsupported operations. See [server.ts](../src/server.ts) and the [module README](../android-root-module/README.md).
- APK staging uses mode `0644` in `/data/local/tmp`; use a private mode/directory so the package is not more readable than required. See [agent/main.go](../android-root-module/agent/main.go).
- The HTTP server sets a header timeout but no request-body or idle timeout/concurrency bound. Since it listens on all interfaces by default, add resource limits for slow or idle connections. Traffic is also plain HTTP; the [module README](../android-root-module/README.md) documents using only trusted networks or a VPN/tunnel.
- Bracketed IPv6 addresses without an explicit port are parsed as if the suffix were a port, instead of receiving the default port. Add coverage for `[IPv6]` and `[IPv6]:port`. See [android-root.ts](../src/android-root.ts).
- The module build script requires `curl`, `javac`, `java`, and `sha256sum`, but the documented build prerequisites are incomplete; stock macOS commonly uses `shasum` instead. Document the full prerequisites and use a portable SHA-256 check. See [build.sh](../android-root-module/build.sh) and the [module README](../android-root-module/README.md).
- Log rotation is checked only before the agent starts. A long-running process can grow `agent.log` without bound when it repeatedly logs handler errors. See [service.sh](../android-root-module/module/service.sh) and [agent/main.go](../android-root-module/agent/main.go).
