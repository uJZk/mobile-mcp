import { test, expect } from "@playwright/test";
import http from "node:http";
import { AddressInfo } from "node:net";

import { AndroidRootManager, AndroidRootRobot, parseAndroidRootEndpoints } from "../src/android-root";
import { ActionableError } from "../src/robot";

const TOKEN = "0123456789abcdef0123456789abcdef";

interface Call {
	method: string;
	path: string;
	body: any;
}

const UI_DUMP = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0">` +
	`<node class="android.widget.FrameLayout" bounds="[0,0][1080,2400]">` +
	`<node class="android.widget.Button" text="OK" resource-id="com.example:id/ok" bounds="[10,20][110,70]" focused="true" />` +
	`<node class="android.widget.TextView" text="hidden" bounds="[0,0][0,0]" />` +
	`</node></hierarchy>`;

async function startMockAgent(): Promise<{ server: http.Server; port: number; calls: Call[] }> {
	const calls: Call[] = [];
	const server = http.createServer((req, res) => {
		const chunks: Buffer[] = [];
		req.on("data", chunk => chunks.push(chunk));
		req.on("end", () => {
			if (req.headers.authorization !== `Bearer ${TOKEN}`) {
				res.writeHead(401).end(JSON.stringify({ error: "invalid or missing token" }));
				return;
			}

			const raw = Buffer.concat(chunks);
			const isJson = req.headers["content-type"] === "application/json";
			calls.push({ method: req.method!, path: req.url!, body: isJson ? JSON.parse(raw.toString()) : raw.length });

			const json = (v: any) => res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(v));
			switch (req.url) {
				case "/v1/info":
					return json({ agentVersion: "0.1.0", manufacturer: "Google", model: "Pixel 8", version: "14", sdk: "34", deviceType: "mobile", width: 1080, height: 2400, density: 420 });
				case "/v1/apps":
					return json({ packages: ["com.android.settings", "com.example"] });
				case "/v1/ui":
					return res.writeHead(200).end(UI_DUMP);
				case "/v1/screenshot":
					return res.writeHead(200, { "Content-Type": "image/png" }).end(Buffer.from([0x89, 0x50, 0x4e, 0x47]));
				case "/v1/apps/launch":
					return res.writeHead(400).end(JSON.stringify({ error: "Failed launching app" }));
				default:
					return json({ status: "ok" });
			}
		});
	});

	await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
	return { server, port: (server.address() as AddressInfo).port, calls };
}

test.describe("parseAndroidRootEndpoints", () => {

	test("parses host, port and token", () => {
		expect(parseAndroidRootEndpoints("192.168.1.5, other@10.0.0.2:9000", "default-token")).toEqual([
			{ id: "root:192.168.1.5:8765", host: "192.168.1.5", port: 8765, token: "default-token" },
			{ id: "root:10.0.0.2:9000", host: "10.0.0.2", port: 9000, token: "other" },
		]);
	});

	test("returns nothing when unset", () => {
		expect(parseAndroidRootEndpoints(undefined, "t")).toEqual([]);
		expect(parseAndroidRootEndpoints("", "t")).toEqual([]);
	});

	test("rejects entries without token or with a bad port", () => {
		expect(() => parseAndroidRootEndpoints("192.168.1.5", undefined)).toThrow(ActionableError);
		expect(() => parseAndroidRootEndpoints("192.168.1.5:abc", "t")).toThrow(ActionableError);
	});
});

test.describe("AndroidRootRobot", () => {

	let agent: Awaited<ReturnType<typeof startMockAgent>>;

	test.beforeEach(async () => {
		agent = await startMockAgent();
	});

	test.afterEach(async () => {
		await new Promise(resolve => agent.server.close(resolve));
	});

	const robot = (token = TOKEN) => new AndroidRootRobot({ id: "root:127.0.0.1", host: "127.0.0.1", port: agent.port, token });

	test("reports screen size from the agent", async () => {
		expect(await robot().getScreenSize()).toEqual({ width: 1080, height: 2400, scale: 420 / 160 });
	});

	test("rounds tap coordinates", async () => {
		await robot().tap(10.4, 20.6);
		expect(agent.calls).toEqual([{ method: "POST", path: "/v1/input/tap", body: { x: 10, y: 21 } }]);
	});

	test("swipes up from the center of the screen", async () => {
		await robot().swipe("up");
		expect(agent.calls[1]).toEqual({ method: "POST", path: "/v1/input/swipe", body: { x1: 540, y1: 1920, x2: 540, y2: 480, duration: 1000 } });
	});

	test("long press is a swipe without movement", async () => {
		await robot().longPress(5, 6, 800);
		expect(agent.calls[0].body).toEqual({ x1: 5, y1: 6, x2: 5, y2: 6, duration: 800 });
	});

	test("maps buttons to keycodes", async () => {
		await robot().pressButton("BACK");
		expect(agent.calls[0]).toEqual({ method: "POST", path: "/v1/input/key", body: { key: "KEYCODE_BACK" } });
	});

	test("skips empty text", async () => {
		await robot().sendKeys("");
		expect(agent.calls).toEqual([]);
	});

	test("parses ui elements", async () => {
		const elements = await robot().getElementsOnScreen();
		expect(elements).toEqual([{
			type: "android.widget.Button",
			text: "OK",
			label: "",
			identifier: "com.example:id/ok",
			focused: true,
			rect: { x: 10, y: 20, width: 100, height: 50 },
		}]);
	});

	test("lists apps", async () => {
		expect(await robot().listApps()).toEqual([
			{ packageName: "com.android.settings", appName: "com.android.settings" },
			{ packageName: "com.example", appName: "com.example" },
		]);
	});

	test("returns the screenshot bytes", async () => {
		const screenshot = await robot().getScreenshot();
		expect(screenshot[0]).toBe(0x89);
	});

	test("turns agent 400s into actionable errors", async () => {
		await expect(robot().launchApp("com.example")).rejects.toThrow(ActionableError);
	});

	test("validates package names before calling the agent", async () => {
		await expect(robot().terminateApp("com.example;reboot")).rejects.toThrow(ActionableError);
		expect(agent.calls).toEqual([]);
	});

	test("reports a wrong token", async () => {
		await expect(robot("wrong").tap(1, 1)).rejects.toThrow(/rejected the token/);
	});

	test("manager lists online and offline devices", async () => {
		process.env.MOBILEMCP_ANDROID_ROOT_DEVICES = `127.0.0.1:${agent.port},127.0.0.1:1`;
		process.env.MOBILEMCP_ANDROID_ROOT_TOKEN = TOKEN;
		try {
			const devices = await new AndroidRootManager().listDevices();
			expect(devices).toEqual([
				{ id: `root:127.0.0.1:${agent.port}`, name: "Google Pixel 8", platform: "android", type: "real", version: "14", state: "online" },
				{ id: "root:127.0.0.1:1", name: "127.0.0.1", platform: "android", type: "real", version: "unknown", state: "offline" },
			]);
		} finally {
			delete process.env.MOBILEMCP_ANDROID_ROOT_DEVICES;
			delete process.env.MOBILEMCP_ANDROID_ROOT_TOKEN;
		}
	});
});
