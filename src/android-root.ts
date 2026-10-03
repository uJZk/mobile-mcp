import fs from "node:fs";

import * as xml from "fast-xml-parser";

import { ActionableError, Button, InstalledApp, Orientation, Robot, ScreenElement, ScreenElementRect, ScreenSize, SwipeDirection } from "./robot";
import { validateLocale, validatePackageName } from "./utils";

// Android devices driven through the root module's on-device agent (see
// android-root-module/), i.e. without adb and without an accessibility service.
// Devices are configured with:
//   MOBILEMCP_ANDROID_ROOT_DEVICES  comma separated [token@]host[:port] entries
//   MOBILEMCP_ANDROID_ROOT_TOKEN    default token for entries without one

export const ANDROID_ROOT_DEVICE_PREFIX = "root:";

const DEFAULT_PORT = 8765;
const REQUEST_TIMEOUT = 30000;
const INSTALL_TIMEOUT = 5 * 60 * 1000;
const DISCOVERY_TIMEOUT = 3000;

const BUTTON_MAP: Record<Button, string> = {
	"BACK": "KEYCODE_BACK",
	"HOME": "KEYCODE_HOME",
	"VOLUME_UP": "KEYCODE_VOLUME_UP",
	"VOLUME_DOWN": "KEYCODE_VOLUME_DOWN",
	"ENTER": "KEYCODE_ENTER",
	"DPAD_CENTER": "KEYCODE_DPAD_CENTER",
	"DPAD_UP": "KEYCODE_DPAD_UP",
	"DPAD_DOWN": "KEYCODE_DPAD_DOWN",
	"DPAD_LEFT": "KEYCODE_DPAD_LEFT",
	"DPAD_RIGHT": "KEYCODE_DPAD_RIGHT",
};

export interface AndroidRootEndpoint {
	id: string;
	host: string;
	port: number;
	token: string;
}

export interface AndroidRootInfo {
	agentVersion: string;
	manufacturer: string;
	model: string;
	version: string;
	sdk: string;
	deviceType: "mobile" | "tv";
	width: number;
	height: number;
	density: number;
}

interface UiAutomatorXmlNode {
	node: UiAutomatorXmlNode[];
	class?: string;
	text?: string;
	bounds?: string;
	hint?: string;
	focused?: string;
	checkable?: string;
	"content-desc"?: string;
	"resource-id"?: string;
}

export const isAndroidRootDeviceId = (deviceId: string): boolean => deviceId.startsWith(ANDROID_ROOT_DEVICE_PREFIX);

export const parseAndroidRootEndpoints = (devices: string | undefined, defaultToken: string | undefined): AndroidRootEndpoint[] => {
	if (!devices) {
		return [];
	}

	return devices
		.split(",")
		.map(entry => entry.trim())
		.filter(entry => entry !== "")
		.map(entry => {
			let token = defaultToken || "";
			let address = entry;
			const at = entry.lastIndexOf("@");
			if (at !== -1) {
				token = entry.substring(0, at);
				address = entry.substring(at + 1);
			}

			let host = address;
			let port = DEFAULT_PORT;
			const colon = address.lastIndexOf(":");
			if (colon !== -1) {
				host = address.substring(0, colon);
				port = Number(address.substring(colon + 1));
			}

			if (host === "" || !Number.isInteger(port) || port <= 0 || port > 65535) {
				throw new ActionableError(`Invalid entry "${entry}" in MOBILEMCP_ANDROID_ROOT_DEVICES, expected [token@]host[:port]`);
			}

			if (token === "") {
				throw new ActionableError(`No token for "${address}", set MOBILEMCP_ANDROID_ROOT_TOKEN or use token@host:port`);
			}

			return { id: `${ANDROID_ROOT_DEVICE_PREFIX}${host}:${port}`, host, port, token };
		});
};

export class AndroidRootRobot implements Robot {

	private screenSize: ScreenSize | null = null;

	public constructor(private endpoint: AndroidRootEndpoint) {
	}

	private async request(method: "GET" | "POST", path: string, body?: object | Buffer, timeout = REQUEST_TIMEOUT): Promise<Response> {
		const url = `http://${this.endpoint.host}:${this.endpoint.port}${path}`;
		const headers: Record<string, string> = { "Authorization": `Bearer ${this.endpoint.token}` };

		let payload: BodyInit | undefined;
		if (Buffer.isBuffer(body)) {
			headers["Content-Type"] = "application/vnd.android.package-archive";
			payload = new Uint8Array(body);
		} else if (body !== undefined) {
			headers["Content-Type"] = "application/json";
			payload = JSON.stringify(body);
		}

		let response: Response;
		try {
			response = await fetch(url, { method, headers, body: payload, signal: AbortSignal.timeout(timeout) });
		} catch (error: any) {
			throw new ActionableError(`Could not reach the mobile-mcp root agent at ${this.endpoint.host}:${this.endpoint.port} (${error.cause?.message || error.message}). Make sure the device is on the network and the module is running`);
		}

		if (response.status === 401) {
			throw new ActionableError(`The mobile-mcp root agent at ${this.endpoint.host}:${this.endpoint.port} rejected the token, check MOBILEMCP_ANDROID_ROOT_TOKEN`);
		}

		if (!response.ok) {
			const text = await response.text();
			let message = text;
			try {
				message = JSON.parse(text).error || text;
			} catch {
				// not json, use the raw text
			}

			if (response.status === 400) {
				throw new ActionableError(message);
			}

			throw new Error(`root agent ${path} failed with ${response.status}: ${message}`);
		}

		return response;
	}

	private async post(path: string, body: object): Promise<void> {
		await this.request("POST", path, body);
	}

	public async getInfo(timeout = REQUEST_TIMEOUT): Promise<AndroidRootInfo> {
		const response = await this.request("GET", "/v1/info", undefined, timeout);
		return await response.json() as AndroidRootInfo;
	}

	public async getScreenSize(): Promise<ScreenSize> {
		if (this.screenSize === null) {
			const info = await this.getInfo();
			this.screenSize = { width: info.width, height: info.height, scale: info.density / 160 };
		}

		return this.screenSize;
	}

	public async swipe(direction: SwipeDirection): Promise<void> {
		const { width, height } = await this.getScreenSize();
		const centerX = width >> 1;

		let x0: number, y0: number, x1: number, y1: number;
		switch (direction) {
			case "up":
				x0 = x1 = centerX;
				y0 = Math.floor(height * 0.80);
				y1 = Math.floor(height * 0.20);
				break;
			case "down":
				x0 = x1 = centerX;
				y0 = Math.floor(height * 0.20);
				y1 = Math.floor(height * 0.80);
				break;
			case "left":
				x0 = Math.floor(width * 0.80);
				x1 = Math.floor(width * 0.20);
				y0 = y1 = Math.floor(height * 0.50);
				break;
			case "right":
				x0 = Math.floor(width * 0.20);
				x1 = Math.floor(width * 0.80);
				y0 = y1 = Math.floor(height * 0.50);
				break;
			default:
				throw new ActionableError(`Swipe direction "${direction}" is not supported`);
		}

		await this.swipeBetween(x0, y0, x1, y1, 1000);
	}

	public async swipeFromCoordinate(x: number, y: number, direction: SwipeDirection, distance?: number): Promise<void> {
		const { width, height } = await this.getScreenSize();
		const distanceY = distance || Math.floor(height * 0.3);
		const distanceX = distance || Math.floor(width * 0.3);

		let x1 = x, y1 = y;
		switch (direction) {
			case "up":
				y1 = Math.max(0, y - distanceY);
				break;
			case "down":
				y1 = Math.min(height, y + distanceY);
				break;
			case "left":
				x1 = Math.max(0, x - distanceX);
				break;
			case "right":
				x1 = Math.min(width, x + distanceX);
				break;
			default:
				throw new ActionableError(`Swipe direction "${direction}" is not supported`);
		}

		await this.swipeBetween(x, y, x1, y1, 1000);
	}

	private async swipeBetween(x1: number, y1: number, x2: number, y2: number, duration: number): Promise<void> {
		await this.post("/v1/input/swipe", {
			x1: Math.round(x1), y1: Math.round(y1),
			x2: Math.round(x2), y2: Math.round(y2),
			duration: Math.round(duration),
		});
	}

	public async getScreenshot(): Promise<Buffer> {
		const response = await this.request("GET", "/v1/screenshot");
		return Buffer.from(await response.arrayBuffer());
	}

	public async listApps(): Promise<InstalledApp[]> {
		const response = await this.request("GET", "/v1/apps");
		const { packages } = await response.json() as { packages: string[] };
		return packages.map(packageName => ({ packageName, appName: packageName }));
	}

	public async getForegroundApp(): Promise<InstalledApp> {
		const response = await this.request("GET", "/v1/apps/foreground");
		const { packageName } = await response.json() as { packageName: string };
		return { packageName, appName: packageName };
	}

	public async launchApp(packageName: string, locale?: string): Promise<void> {
		validatePackageName(packageName);
		if (locale) {
			validateLocale(locale);
		}

		await this.post("/v1/apps/launch", { packageName, locale });
	}

	public async terminateApp(packageName: string): Promise<void> {
		validatePackageName(packageName);
		await this.post("/v1/apps/terminate", { packageName });
	}

	public async installApp(path: string): Promise<void> {
		if (!path.toLowerCase().endsWith(".apk")) {
			throw new ActionableError("Only .apk files can be installed on Android");
		}

		await this.request("POST", "/v1/apps/install", fs.readFileSync(path), INSTALL_TIMEOUT);
	}

	public async uninstallApp(bundleId: string): Promise<void> {
		validatePackageName(bundleId);
		await this.post("/v1/apps/uninstall", { packageName: bundleId });
	}

	public async openUrl(url: string): Promise<void> {
		await this.post("/v1/url", { url });
	}

	public async sendKeys(text: string): Promise<void> {
		if (text === "") {
			return;
		}

		await this.post("/v1/input/text", { text });
	}

	public async pressButton(button: Button): Promise<void> {
		const key = BUTTON_MAP[button];
		if (!key) {
			throw new ActionableError(`Button "${button}" is not supported`);
		}

		await this.post("/v1/input/key", { key });
	}

	public async tap(x: number, y: number): Promise<void> {
		await this.post("/v1/input/tap", { x: Math.round(x), y: Math.round(y) });
	}

	public async doubleTap(x: number, y: number): Promise<void> {
		await this.tap(x, y);
		await new Promise(r => setTimeout(r, 100));
		await this.tap(x, y);
	}

	public async longPress(x: number, y: number, duration: number): Promise<void> {
		// a long press is a swipe with no movement and a long duration
		await this.swipeBetween(x, y, x, y, duration);
	}

	public async getElementsOnScreen(): Promise<ScreenElement[]> {
		const response = await this.request("GET", "/v1/ui");
		const dump = await response.text();
		const parser = new xml.XMLParser({ ignoreAttributes: false, attributeNamePrefix: "" });
		const parsed = parser.parse(dump.substring(dump.indexOf("<?xml")));
		if (!parsed?.hierarchy?.node) {
			throw new ActionableError("No UI elements returned by uiautomator, the screen may be locked or showing secure content");
		}

		return collectElements(parsed.hierarchy.node);
	}

	public async setOrientation(orientation: Orientation): Promise<void> {
		await this.post("/v1/orientation", { orientation });
		// the reported screen size does not change with rotation, but drop it to be safe
		this.screenSize = null;
	}

	public async getOrientation(): Promise<Orientation> {
		const response = await this.request("GET", "/v1/orientation");
		const { orientation } = await response.json() as { orientation: Orientation };
		return orientation;
	}
}

const getScreenElementRect = (node: UiAutomatorXmlNode): ScreenElementRect => {
	const bounds = String(node.bounds);
	const [, left, top, right, bottom] = bounds.match(/^\[(\d+),(\d+)\]\[(\d+),(\d+)\]$/)?.map(Number) || [];
	return {
		x: left,
		y: top,
		width: right - left,
		height: bottom - top,
	};
};

export const collectElements = (node: UiAutomatorXmlNode): ScreenElement[] => {
	const elements: ScreenElement[] = [];

	if (node.node) {
		const children = Array.isArray(node.node) ? node.node : [node.node];
		for (const child of children) {
			elements.push(...collectElements(child));
		}
	}

	if (node.text || node["content-desc"] || node.hint || node["resource-id"] || node.checkable === "true") {
		const element: ScreenElement = {
			type: node.class || "text",
			text: node.text,
			label: node["content-desc"] || node.hint || "",
			rect: getScreenElementRect(node),
		};

		if (node.focused === "true") {
			// only provide it if it's true, otherwise don't confuse llm
			element.focused = true;
		}

		const resourceId = node["resource-id"];
		if (resourceId) {
			element.identifier = resourceId;
		}

		if (element.rect.width > 0 && element.rect.height > 0) {
			elements.push(element);
		}
	}

	return elements;
};

export interface AndroidRootDevice {
	id: string;
	name: string;
	platform: "android";
	type: "real";
	version: string;
	state: "online" | "offline";
}

export class AndroidRootManager {

	public getEndpoints(): AndroidRootEndpoint[] {
		return parseAndroidRootEndpoints(process.env.MOBILEMCP_ANDROID_ROOT_DEVICES, process.env.MOBILEMCP_ANDROID_ROOT_TOKEN);
	}

	public isConfigured(): boolean {
		return !!process.env.MOBILEMCP_ANDROID_ROOT_DEVICES?.trim();
	}

	public getRobot(deviceId: string): AndroidRootRobot {
		const endpoint = this.getEndpoints().find(e => e.id === deviceId);
		if (!endpoint) {
			throw new ActionableError(`Device "${deviceId}" is not listed in MOBILEMCP_ANDROID_ROOT_DEVICES. Use the mobile_list_available_devices tool to see available devices.`);
		}

		return new AndroidRootRobot(endpoint);
	}

	public async listDevices(): Promise<AndroidRootDevice[]> {
		return Promise.all(this.getEndpoints().map(async endpoint => {
			try {
				const info = await new AndroidRootRobot(endpoint).getInfo(DISCOVERY_TIMEOUT);
				const name = [info.manufacturer, info.model].filter(s => s).join(" ") || endpoint.host;
				return { id: endpoint.id, name, platform: "android" as const, type: "real" as const, version: info.version, state: "online" as const };
			} catch {
				return { id: endpoint.id, name: endpoint.host, platform: "android" as const, type: "real" as const, version: "unknown", state: "offline" as const };
			}
		}));
	}
}
