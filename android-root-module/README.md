# mobile-mcp root module

A Magisk / KernelSU / APatch module that lets mobile-mcp control a **rooted** Android device over the network, **without adb and without an accessibility service**.

```
mobile-mcp (computer) ──HTTP + bearer token──▶ mobile-mcp-agent (phone, root)
                                                  ├─ InputServer (app_process, stdin/stdout)
                                                  │    └─ InputManager.injectInputEvent, clipboard
                                                  ├─ screencap -p
                                                  ├─ uiautomator dump
                                                  └─ pm / am / monkey / settings
```

The agent is a small static Go binary started by the module's `service.sh` at boot. It runs as root, so it can use the same system tools adb shell would, but nothing on the device needs USB debugging or wireless debugging enabled, and no accessibility service is registered.

Touch, key and text input do not go through the `input` command. On the first input request the agent starts `lib/input-server.jar` with `app_process` (the same way scrcpy runs its server) and keeps it alive. It injects events directly through `InputManager.injectInputEvent`, so each tap costs a pipe round trip instead of a JVM start. The agent talks to it over its stdin/stdout only, so no other process on the device can reach it. If it dies or stops answering, the agent restarts it on the next request.

## Install

1. Build the module zip (needs Go 1.22+ and `zip`):

   ```bash
   ./android-root-module/build.sh
   # -> android-root-module/dist/mobile-mcp-agent-v0.1.0.zip
   ```

2. Copy the zip to the phone and install it from the Magisk / KernelSU / APatch app, then reboot.
3. The install log prints the access token. It is also stored in `/data/adb/mobile-mcp/token` (root only), for example `su -c cat /data/adb/mobile-mcp/token` in Termux.

## Use with mobile-mcp

Set these on the machine that runs mobile-mcp:

```json
{
  "mcpServers": {
    "mobile-mcp": {
      "command": "npx",
      "args": ["-y", "@mobilenext/mobile-mcp@latest"],
      "env": {
        "MOBILEMCP_ANDROID_ROOT_DEVICES": "192.168.1.20:8765",
        "MOBILEMCP_ANDROID_ROOT_TOKEN": "<token from the install log>"
      }
    }
  }
}
```

- `MOBILEMCP_ANDROID_ROOT_DEVICES` is a comma-separated list of `[token@]host[:port]` entries. The port defaults to 8765, and a per-device `token@` overrides `MOBILEMCP_ANDROID_ROOT_TOKEN`.
- Each device appears in `mobile_list_available_devices` as `root:<host>:<port>`. Unreachable devices are listed as `offline`.
- mobilecli and adb are not needed for these devices. If mobilecli is missing, only the root devices are listed.

## Configuration

`/data/adb/mobile-mcp/config` is read at boot:

```sh
LISTEN=0.0.0.0:8765
ALLOW_LOCAL=0
```

- `ALLOW_LOCAL=0` (default) drops connections that come from the phone itself, i.e. from `127.0.0.1` / `::1` or any of the phone's own interface addresses, so apps on the device cannot talk to the agent. Set it to `1` to allow them (for example to test with `curl` in Termux).

Reboot, or disable and re-enable the module, after changing it. The agent logs to `/data/adb/mobile-mcp/agent.log`.

## Supported tools

| Works | Not supported |
|---|---|
| screenshots, screen size, tap / double tap / long press, swipe, type text, buttons, list elements, list / launch / terminate / install / uninstall apps, foreground app, open URL, orientation | logs, crash reports, screen recording, location, clipboard, fold, tap-by-ref |

Text that the virtual keyboard map can type (ASCII) is sent as key events. Any other text (Chinese, emoji, ...) is put on the clipboard, pasted with `KEYCODE_PASTE` and the clipboard is cleared afterwards. No helper app such as devicekit is needed.

## Security

Anyone who can reach the port **and** knows the token gets root-level control of the device (installing apps is enough to own it). Keep that in mind:

- Traffic is plain HTTP. Use it only on a network you trust, or bind `LISTEN` to a VPN address (e.g. Tailscale / WireGuard), or tunnel it.
- The token is 48 random hex characters, generated on first start and compared in constant time. To rotate it, delete `/data/adb/mobile-mcp/token` and reboot.
- Connections from the device itself are dropped at accept time, before any HTTP is read (see `ALLOW_LOCAL`).
- The agent only exposes fixed operations. Every parameter is validated and passed as a separate argv entry to the system tool, never through a shell, so there is no generic command execution endpoint. The input server only accepts the fixed `tap` / `swipe` / `key` / `text` commands on its private stdin.

## HTTP API

All endpoints require `Authorization: Bearer <token>`. Errors are returned as `{"error": "..."}`, with status 400 for invalid input and 500 for command failures.

| Method | Path | Body / response |
|---|---|---|
| GET | `/v1/info` | `{agentVersion, manufacturer, model, version, sdk, deviceType, width, height, density}` |
| GET | `/v1/screenshot[?display=N]` | PNG |
| GET | `/v1/ui` | uiautomator XML |
| POST | `/v1/input/tap` | `{x, y}` |
| POST | `/v1/input/swipe` | `{x1, y1, x2, y2, duration}` (ms) |
| POST | `/v1/input/key` | `{key: "KEYCODE_BACK"}` |
| POST | `/v1/input/text` | `{text}` |
| GET | `/v1/apps` | `{packages: [...]}` (launcher apps) |
| GET | `/v1/apps/foreground` | `{packageName}` |
| POST | `/v1/apps/launch` | `{packageName, locale?}` |
| POST | `/v1/apps/terminate` | `{packageName}` |
| POST | `/v1/apps/install` | raw APK bytes |
| POST | `/v1/apps/uninstall` | `{packageName}` |
| POST | `/v1/url` | `{url}` |
| GET/POST | `/v1/orientation` | `{orientation: "portrait" \| "landscape"}` |

## Development

```bash
cd android-root-module/agent
go test ./...
```

The tests replace the Android binaries (and `app_process`) with fake scripts on `PATH`, so they run on any Linux or macOS machine.

`build.sh` compiles `input-server/` with `javac` against the Android API 16 stubs and dexes it with d8. Both jars are downloaded from Maven once into `.cache/` and checked against pinned SHA-256 sums. Set `ANDROID_JAR` / `R8_JAR` to use local copies instead.
