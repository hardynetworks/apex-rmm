//go:build windows

package main

import (
	"fmt"
	"syscall"
)

// Windows keeps some shortcuts for itself before an app ever sees them: the Windows key,
// Alt+Tab, Alt+Esc, Ctrl+Esc, Alt+F4, Alt+Space. While a live remote-control window is in front,
// a low-level keyboard hook takes those keys and hands them to the page, which sends them to the
// remote computer. Everything else goes through WebView2 as normal key events.

var hook uintptr

// held tracks keys we swallowed on the way down (and which window got them), so the matching
// "up" is swallowed too and goes to the same remote session.
var held = map[uint32]uintptr{}

func installKeyboardHook() {
	h, _, _ := pSetWindowsHookExW.Call(whKeyboardLL, syscall.NewCallback(keyboardProc), moduleHandle(), 0)
	hook = h
}

func uninstallKeyboardHook() {
	if hook != 0 {
		pUnhookWindowsHookEx.Call(hook)
	}
}

func keyDown(vk uintptr) bool {
	r, _, _ := pGetAsyncKeyState.Call(vk)
	return r&0x8000 != 0
}

func keyboardProc(code int, wp, lp uintptr) uintptr {
	if code == 0 && a != nil {
		k := (*kbdLLHook)(ptr(lp))
		if k.Flags&llkhfInjected == 0 {
			if domCode, ok := captureTarget(k); ok {
				up := k.Flags&llkhfUp != 0
				target, _, _ := pGetForegroundWindow.Call()
				if up {
					target = held[k.VkCode]
				}
				if w := a.windows[target]; w != nil {
					kind := "kd"
					if up {
						kind = "ku"
					}
					w.eval(fmt.Sprintf("window.__apexKey&&window.__apexKey(%q,%q)", kind, domCode))
				}
				if up {
					delete(held, k.VkCode)
				} else {
					held[k.VkCode] = target
				}
				return 1 // swallow: don't let Windows act on it locally
			}
		}
	}
	r, _, _ := pCallNextHookEx.Call(hook, uintptr(code), wp, lp)
	return r
}

// captureTarget decides whether a key event should go to the remote computer.
func captureTarget(k *kbdLLHook) (string, bool) {
	up := k.Flags&llkhfUp != 0
	if _, ok := held[k.VkCode]; up && ok { // always finish what we started
		return domCodeFor(k.VkCode), true
	}
	if up || !a.cfg.captureKeys() {
		return "", false
	}
	fg, _, _ := pGetForegroundWindow.Call()
	w := a.windows[fg]
	if w == nil || w.kind != kindSession || !w.capture {
		return "", false
	}
	alt := k.Flags&llkhfAltDown != 0
	switch k.VkCode {
	case vkLWin, vkRWin:
		return domCodeFor(k.VkCode), true
	case vkTab, vkSpace, vkF4:
		if alt {
			return domCodeFor(k.VkCode), true
		}
	case vkEscape:
		if alt || keyDown(vkControl) {
			return domCodeFor(k.VkCode), true
		}
	}
	return "", false
}

func domCodeFor(vk uint32) string {
	switch vk {
	case vkLWin:
		return "MetaLeft"
	case vkRWin:
		return "MetaRight"
	case vkTab:
		return "Tab"
	case vkSpace:
		return "Space"
	case vkF4:
		return "F4"
	case vkEscape:
		return "Escape"
	}
	return ""
}
