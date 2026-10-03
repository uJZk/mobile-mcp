package com.mobilenext.mcp;

import android.content.ClipData;
import android.os.IBinder;
import android.os.Looper;
import android.os.SystemClock;
import android.util.Base64;
import android.view.InputDevice;
import android.view.InputEvent;
import android.view.KeyCharacterMap;
import android.view.KeyEvent;
import android.view.MotionEvent;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.PrintStream;
import java.lang.reflect.InvocationTargetException;
import java.lang.reflect.Method;
import java.nio.charset.Charset;

/**
 * Long-running input injector started by mobile-mcp-agent through app_process
 * (as root). It injects events straight into InputManager, the same way the
 * `input` command does, but stays alive so each request skips the JVM start.
 *
 * Protocol: one command per line on stdin, one reply per line on stdout,
 * either "ok" or "error <message>". Text arguments are base64 encoded.
 *
 *   tap X Y
 *   swipe X1 Y1 X2 Y2 DURATION_MS
 *   key KEYCODE_NAME
 *   text BASE64_UTF8
 */
public final class InputServer {

	// InputManager.INJECT_INPUT_EVENT_MODE_WAIT_FOR_FINISH
	private static final int WAIT_FOR_FINISH = 2;
	private static final String SHELL_PACKAGE = "com.android.shell";
	private static final long SWIPE_STEP_MS = 16;
	// KeyEvent.KEYCODE_PASTE (API 24), not in the API 16 stubs we compile against
	private static final int KEYCODE_PASTE = 279;

	private final Object inputManager;
	private final Method injectInputEvent;
	private final KeyCharacterMap keyCharacterMap = KeyCharacterMap.load(KeyCharacterMap.VIRTUAL_KEYBOARD);
	private Object clipboard;

	private InputServer() throws ReflectiveOperationException {
		Class<?> cls;
		try {
			// Android 14 moved injectInputEvent to InputManagerGlobal
			cls = Class.forName("android.hardware.input.InputManagerGlobal");
		} catch (ClassNotFoundException e) {
			cls = Class.forName("android.hardware.input.InputManager");
		}
		inputManager = cls.getDeclaredMethod("getInstance").invoke(null);
		injectInputEvent = cls.getMethod("injectInputEvent", InputEvent.class, int.class);
	}

	public static void main(String[] args) throws Exception {
		// some system services expect a looper on the calling thread
		Looper.prepareMainLooper();

		PrintStream out = new PrintStream(System.out, true, "UTF-8");
		InputServer server;
		try {
			server = new InputServer();
		} catch (Throwable t) {
			out.println("error " + describe(t));
			System.exit(1);
			return;
		}
		out.println("ready");

		BufferedReader in = new BufferedReader(new InputStreamReader(System.in, Charset.forName("UTF-8")));
		String line;
		while ((line = in.readLine()) != null) {
			if (line.isEmpty()) {
				continue;
			}
			try {
				server.handle(line.split(" "));
				out.println("ok");
			} catch (Throwable t) {
				out.println("error " + describe(t));
			}
		}
	}

	private static String describe(Throwable t) {
		if (t instanceof InvocationTargetException && t.getCause() != null) {
			t = t.getCause();
		}
		String message = t.getMessage();
		String text = message == null ? t.getClass().getName() : t.getClass().getSimpleName() + ": " + message;
		return text.replace('\n', ' ').replace('\r', ' ');
	}

	private void handle(String[] cmd) throws Exception {
		switch (cmd[0]) {
			case "tap":
				expectArgs(cmd, 2);
				tap(Integer.parseInt(cmd[1]), Integer.parseInt(cmd[2]));
				break;
			case "swipe":
				expectArgs(cmd, 5);
				swipe(Integer.parseInt(cmd[1]), Integer.parseInt(cmd[2]),
						Integer.parseInt(cmd[3]), Integer.parseInt(cmd[4]), Long.parseLong(cmd[5]));
				break;
			case "key":
				expectArgs(cmd, 1);
				key(cmd[1]);
				break;
			case "text":
				expectArgs(cmd, 1);
				text(new String(Base64.decode(cmd[1], Base64.DEFAULT), "UTF-8"));
				break;
			default:
				throw new IllegalArgumentException("unknown command " + cmd[0]);
		}
	}

	private static void expectArgs(String[] cmd, int count) {
		if (cmd.length != count + 1) {
			throw new IllegalArgumentException(cmd[0] + " expects " + count + " arguments");
		}
	}

	private void inject(InputEvent event) throws ReflectiveOperationException {
		Object injected = injectInputEvent.invoke(inputManager, event, WAIT_FOR_FINISH);
		if (!Boolean.TRUE.equals(injected)) {
			throw new IllegalStateException("InputManager rejected the event");
		}
	}

	private void touch(int action, long downTime, float x, float y) throws ReflectiveOperationException {
		MotionEvent event = MotionEvent.obtain(downTime, SystemClock.uptimeMillis(), action, x, y, 1.0f, 1.0f, 0, 1.0f, 1.0f, 0, 0);
		event.setSource(InputDevice.SOURCE_TOUCHSCREEN);
		try {
			inject(event);
		} finally {
			event.recycle();
		}
	}

	private void tap(int x, int y) throws ReflectiveOperationException {
		long downTime = SystemClock.uptimeMillis();
		touch(MotionEvent.ACTION_DOWN, downTime, x, y);
		touch(MotionEvent.ACTION_UP, downTime, x, y);
	}

	private void swipe(int x1, int y1, int x2, int y2, long duration) throws ReflectiveOperationException, InterruptedException {
		long downTime = SystemClock.uptimeMillis();
		long endTime = downTime + duration;
		touch(MotionEvent.ACTION_DOWN, downTime, x1, y1);

		long now;
		while ((now = SystemClock.uptimeMillis()) < endTime) {
			float alpha = (float) (now - downTime) / duration;
			touch(MotionEvent.ACTION_MOVE, downTime, lerp(x1, x2, alpha), lerp(y1, y2, alpha));
			Thread.sleep(Math.min(SWIPE_STEP_MS, Math.max(1, endTime - SystemClock.uptimeMillis())));
		}

		touch(MotionEvent.ACTION_MOVE, downTime, x2, y2);
		touch(MotionEvent.ACTION_UP, downTime, x2, y2);
	}

	private static float lerp(float a, float b, float alpha) {
		return a + (b - a) * alpha;
	}

	private void key(String name) throws ReflectiveOperationException {
		int code = KeyEvent.keyCodeFromString(name);
		if (code == KeyEvent.KEYCODE_UNKNOWN) {
			throw new IllegalArgumentException("unknown key " + name);
		}
		pressKey(code);
	}

	private void pressKey(int code) throws ReflectiveOperationException {
		long now = SystemClock.uptimeMillis();
		inject(keyEvent(now, KeyEvent.ACTION_DOWN, code));
		inject(keyEvent(now, KeyEvent.ACTION_UP, code));
	}

	private static KeyEvent keyEvent(long downTime, int action, int code) {
		return new KeyEvent(downTime, SystemClock.uptimeMillis(), action, code, 0, 0,
				KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0, InputDevice.SOURCE_KEYBOARD);
	}

	private void text(String text) throws Exception {
		if (text.isEmpty()) {
			return;
		}

		// characters the virtual keyboard can produce are typed as key events,
		// which leaves the user's clipboard alone
		KeyEvent[] events = keyCharacterMap.getEvents(text.toCharArray());
		if (events != null) {
			for (KeyEvent event : events) {
				inject(KeyEvent.changeTimeRepeat(event, SystemClock.uptimeMillis(), 0));
			}
			return;
		}

		// anything else (CJK, emoji, ...) goes through the clipboard; the paste is
		// handled synchronously by the focused view since we wait for it to finish
		setClipboard(ClipData.newPlainText("mobile-mcp", text));
		try {
			pressKey(KEYCODE_PASTE);
		} finally {
			clearClipboard();
		}
	}

	private Object clipboard() throws ReflectiveOperationException {
		if (clipboard == null) {
			Class<?> serviceManager = Class.forName("android.os.ServiceManager");
			IBinder binder = (IBinder) serviceManager.getMethod("getService", String.class).invoke(null, "clipboard");
			if (binder == null) {
				throw new IllegalStateException("clipboard service not available");
			}
			Class<?> stub = Class.forName("android.content.IClipboard$Stub");
			clipboard = stub.getMethod("asInterface", IBinder.class).invoke(null, binder);
		}
		return clipboard;
	}

	// IClipboard has changed signature across Android releases; try the newest first.
	private static final Class<?>[][] SET_CLIP_SIGNATURES = {
			{ClipData.class, String.class, String.class, int.class, int.class}, // 14+: + deviceId
			{ClipData.class, String.class, String.class, int.class},            // 11+: + attributionTag
			{ClipData.class, String.class, int.class},                          // 10: + userId
			{ClipData.class, String.class},                                     // 9 and older
	};

	private static final Class<?>[][] CLEAR_CLIP_SIGNATURES = {
			{String.class, String.class, int.class, int.class},
			{String.class, String.class, int.class},
			{String.class, int.class},
			{String.class},
	};

	private void setClipboard(ClipData clip) throws ReflectiveOperationException {
		callClipboard("setPrimaryClip", SET_CLIP_SIGNATURES, clip);
	}

	private void clearClipboard() {
		try {
			callClipboard("clearPrimaryClip", CLEAR_CLIP_SIGNATURES, null);
		} catch (Throwable ignored) {
			// clearPrimaryClip only exists on Android 9+, best effort
		}
	}

	private void callClipboard(String name, Class<?>[][] signatures, ClipData clip) throws ReflectiveOperationException {
		Object service = clipboard();
		for (Class<?>[] params : signatures) {
			Method method;
			try {
				method = service.getClass().getMethod(name, params);
			} catch (NoSuchMethodException e) {
				continue;
			}

			Object[] args = new Object[params.length];
			int i = 0;
			if (clip != null) {
				args[i++] = clip;
			}
			args[i++] = SHELL_PACKAGE;
			if (i < params.length && params[i] == String.class) {
				args[i++] = null; // attributionTag
			}
			while (i < params.length) {
				args[i++] = 0; // userId, deviceId
			}
			method.invoke(service, args);
			return;
		}
		throw new NoSuchMethodException("IClipboard." + name);
	}
}
