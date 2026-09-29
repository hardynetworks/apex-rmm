package desktop

// evdev maps browser KeyboardEvent.code values to Linux evdev key codes.
// These are layout independent (physical keys). Windows scan codes and X11
// keycodes are derived from them; macOS uses its own table.
var evdev = map[string]int{
	"Escape": 1, "Digit1": 2, "Digit2": 3, "Digit3": 4, "Digit4": 5, "Digit5": 6, "Digit6": 7, "Digit7": 8, "Digit8": 9, "Digit9": 10, "Digit0": 11,
	"Minus": 12, "Equal": 13, "Backspace": 14, "Tab": 15,
	"KeyQ": 16, "KeyW": 17, "KeyE": 18, "KeyR": 19, "KeyT": 20, "KeyY": 21, "KeyU": 22, "KeyI": 23, "KeyO": 24, "KeyP": 25,
	"BracketLeft": 26, "BracketRight": 27, "Enter": 28, "ControlLeft": 29,
	"KeyA": 30, "KeyS": 31, "KeyD": 32, "KeyF": 33, "KeyG": 34, "KeyH": 35, "KeyJ": 36, "KeyK": 37, "KeyL": 38,
	"Semicolon": 39, "Quote": 40, "Backquote": 41, "ShiftLeft": 42, "Backslash": 43,
	"KeyZ": 44, "KeyX": 45, "KeyC": 46, "KeyV": 47, "KeyB": 48, "KeyN": 49, "KeyM": 50,
	"Comma": 51, "Period": 52, "Slash": 53, "ShiftRight": 54, "NumpadMultiply": 55, "AltLeft": 56, "Space": 57, "CapsLock": 58,
	"F1": 59, "F2": 60, "F3": 61, "F4": 62, "F5": 63, "F6": 64, "F7": 65, "F8": 66, "F9": 67, "F10": 68,
	"NumLock": 69, "ScrollLock": 70, "Numpad7": 71, "Numpad8": 72, "Numpad9": 73, "NumpadSubtract": 74,
	"Numpad4": 75, "Numpad5": 76, "Numpad6": 77, "NumpadAdd": 78, "Numpad1": 79, "Numpad2": 80, "Numpad3": 81,
	"Numpad0": 82, "NumpadDecimal": 83, "IntlBackslash": 86, "F11": 87, "F12": 88,
	"NumpadEnter": 96, "ControlRight": 97, "NumpadDivide": 98, "PrintScreen": 99, "AltRight": 100,
	"Home": 102, "ArrowUp": 103, "PageUp": 104, "ArrowLeft": 105, "ArrowRight": 106, "End": 107, "ArrowDown": 108,
	"PageDown": 109, "Insert": 110, "Delete": 111, "Pause": 119, "MetaLeft": 125, "MetaRight": 126, "ContextMenu": 127,
	"OSLeft": 125, "OSRight": 126,
}

// winExtended lists evdev codes whose Windows scan code carries the E0 prefix.
var winExtended = map[int]int{
	96: 0x1C, 97: 0x1D, 98: 0x35, 99: 0x37, 100: 0x38, 102: 0x47, 103: 0x48, 104: 0x49, 105: 0x4B,
	106: 0x4D, 107: 0x4F, 108: 0x50, 109: 0x51, 110: 0x52, 111: 0x53, 125: 0x5B, 126: 0x5C, 127: 0x5D,
}

// usASCII maps printable ASCII to (evdev code, shift) on a US layout, used to
// "type" pasted text on X11.
var usASCII = func() map[rune][2]int {
	m := map[rune][2]int{}
	plain := map[rune]string{
		'1': "Digit1", '2': "Digit2", '3': "Digit3", '4': "Digit4", '5': "Digit5", '6': "Digit6", '7': "Digit7", '8': "Digit8", '9': "Digit9", '0': "Digit0",
		'-': "Minus", '=': "Equal", '[': "BracketLeft", ']': "BracketRight", ';': "Semicolon", '\'': "Quote", '`': "Backquote",
		'\\': "Backslash", ',': "Comma", '.': "Period", '/': "Slash", ' ': "Space", '\n': "Enter", '\t': "Tab",
	}
	shifted := map[rune]string{
		'!': "Digit1", '@': "Digit2", '#': "Digit3", '$': "Digit4", '%': "Digit5", '^': "Digit6", '&': "Digit7", '*': "Digit8", '(': "Digit9", ')': "Digit0",
		'_': "Minus", '+': "Equal", '{': "BracketLeft", '}': "BracketRight", ':': "Semicolon", '"': "Quote", '~': "Backquote",
		'|': "Backslash", '<': "Comma", '>': "Period", '?': "Slash",
	}
	for r, c := range plain {
		m[r] = [2]int{evdev[c], 0}
	}
	for r, c := range shifted {
		m[r] = [2]int{evdev[c], 1}
	}
	for c := 'a'; c <= 'z'; c++ {
		code := evdev["Key"+string(c-32)]
		m[c] = [2]int{code, 0}
		m[c-32] = [2]int{code, 1}
	}
	return m
}()

// macKeys maps KeyboardEvent.code to macOS virtual key codes (kVK_*).
var macKeys = map[string]uint16{
	"KeyA": 0x00, "KeyS": 0x01, "KeyD": 0x02, "KeyF": 0x03, "KeyH": 0x04, "KeyG": 0x05, "KeyZ": 0x06, "KeyX": 0x07,
	"KeyC": 0x08, "KeyV": 0x09, "IntlBackslash": 0x0A, "KeyB": 0x0B, "KeyQ": 0x0C, "KeyW": 0x0D, "KeyE": 0x0E, "KeyR": 0x0F,
	"KeyY": 0x10, "KeyT": 0x11, "Digit1": 0x12, "Digit2": 0x13, "Digit3": 0x14, "Digit4": 0x15, "Digit6": 0x16, "Digit5": 0x17,
	"Equal": 0x18, "Digit9": 0x19, "Digit7": 0x1A, "Minus": 0x1B, "Digit8": 0x1C, "Digit0": 0x1D, "BracketRight": 0x1E,
	"KeyO": 0x1F, "KeyU": 0x20, "BracketLeft": 0x21, "KeyI": 0x22, "KeyP": 0x23, "Enter": 0x24, "KeyL": 0x25, "KeyJ": 0x26,
	"Quote": 0x27, "KeyK": 0x28, "Semicolon": 0x29, "Backslash": 0x2A, "Comma": 0x2B, "Slash": 0x2C, "KeyN": 0x2D,
	"KeyM": 0x2E, "Period": 0x2F, "Tab": 0x30, "Space": 0x31, "Backquote": 0x32, "Backspace": 0x33, "Escape": 0x35,
	"MetaRight": 0x36, "MetaLeft": 0x37, "OSRight": 0x36, "OSLeft": 0x37, "ShiftLeft": 0x38, "CapsLock": 0x39, "AltLeft": 0x3A,
	"ControlLeft": 0x3B, "ShiftRight": 0x3C, "AltRight": 0x3D, "ControlRight": 0x3E, "NumpadDecimal": 0x41,
	"NumpadMultiply": 0x43, "NumpadAdd": 0x45, "NumLock": 0x47, "NumpadDivide": 0x4B, "NumpadEnter": 0x4C,
	"NumpadSubtract": 0x4E, "NumpadEqual": 0x51, "Numpad0": 0x52, "Numpad1": 0x53, "Numpad2": 0x54, "Numpad3": 0x55,
	"Numpad4": 0x56, "Numpad5": 0x57, "Numpad6": 0x58, "Numpad7": 0x59, "Numpad8": 0x5B, "Numpad9": 0x5C,
	"F5": 0x60, "F6": 0x61, "F7": 0x62, "F3": 0x63, "F8": 0x64, "F9": 0x65, "F11": 0x67, "F10": 0x6D, "F12": 0x6F,
	"Insert": 0x72, "Home": 0x73, "PageUp": 0x74, "Delete": 0x75, "F4": 0x76, "End": 0x77, "F2": 0x78, "PageDown": 0x79,
	"F1": 0x7A, "ArrowLeft": 0x7B, "ArrowRight": 0x7C, "ArrowDown": 0x7D, "ArrowUp": 0x7E,
}
