// Terminal input for the viewer: the bytes a terminal sends in raw mode, read
// as keys, mouse events, and pastes.
package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// keyPress is a key with modifiers. Key is a character or a key name that
// keyFor knows, such as Enter or ArrowLeft.
type keyPress struct {
	Key                    string
	Ctrl, Alt, Shift, Meta bool
}

// modifiers names the held modifiers the way Input expects.
func (k keyPress) modifiers() []string {
	var mods []string
	for _, m := range []struct {
		on   bool
		name string
	}{{k.Ctrl, "Control"}, {k.Alt, "Alt"}, {k.Shift, "Shift"}, {k.Meta, "Meta"}} {
		if m.on {
			mods = append(mods, m.name)
		}
	}
	return mods
}

// mouseEvent is an SGR mouse report. X and Y are 1-based cells, or pixels when
// the terminal reports pixels. Button is 0 left, 1 middle, 2 right; Motion is
// set for movement, with the button held if one is; Wheel is -1 up, +1 down.
type mouseEvent struct {
	X, Y    int
	Button  int
	Motion  bool
	Release bool
	Wheel   int
	Shift   bool
	Alt     bool
	Ctrl    bool
}

// pasteEvent is text the person pasted, when bracketed paste is on.
type pasteEvent struct{ Text string }

// inputParser turns terminal input into events. It keeps state between reads
// for a paste that spans several.
type inputParser struct {
	pasting bool
	paste   strings.Builder
}

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// csiKeys are the keys that CSI sequences end in, by final byte, and those
// that end in ~, by number.
var (
	csiKeys   = map[byte]string{'A': "ArrowUp", 'B': "ArrowDown", 'C': "ArrowRight", 'D': "ArrowLeft", 'H': "Home", 'F': "End"}
	tildeKeys = map[int]string{1: "Home", 2: "Insert", 3: "Delete", 4: "End", 5: "PageUp", 6: "PageDown", 7: "Home", 8: "End"}
)

// feed returns the events in b. A lone ESC at the end of a read is the Escape
// key, since terminals send each sequence in one write.
func (p *inputParser) feed(b []byte) []any {
	var events []any
	s := string(b)
	for len(s) > 0 {
		if p.pasting {
			end := strings.Index(s, pasteEnd)
			if end < 0 {
				p.paste.WriteString(s)
				return events
			}
			p.paste.WriteString(s[:end])
			events = append(events, pasteEvent{Text: p.paste.String()})
			p.paste.Reset()
			p.pasting = false
			s = s[end+len(pasteEnd):]
			continue
		}
		if strings.HasPrefix(s, pasteStart) {
			p.pasting = true
			s = s[len(pasteStart):]
			continue
		}

		var ev any
		var n int
		switch {
		case strings.HasPrefix(s, "\x1b[<"):
			ev, n = parseSGRMouse(s)
		case strings.HasPrefix(s, "\x1b["):
			ev, n = parseCSI(s)
		case strings.HasPrefix(s, "\x1bO") && len(s) >= 3:
			ev, n = keyPress{Key: csiKeys[s[2]]}, 3
			if csiKeys[s[2]] == "" {
				ev = nil
			}
		case s[0] == 0x1b && len(s) > 1:
			// Alt plus a key.
			k, size := plainKey(s[1:])
			k.Alt = true
			ev, n = k, 1+size
		default:
			ev, n = plainKey(s)
		}
		if ev != nil {
			events = append(events, ev)
		}
		s = s[max(n, 1):]
	}
	return events
}

// plainKey reads one key that is not an escape sequence.
func plainKey(s string) (keyPress, int) {
	c := s[0]
	switch {
	case c == 0x1b:
		return keyPress{Key: "Escape"}, 1
	case c == '\r' || c == '\n':
		return keyPress{Key: "Enter"}, 1
	case c == '\t':
		return keyPress{Key: "Tab"}, 1
	case c == 0x7f || c == 0x08:
		return keyPress{Key: "Backspace"}, 1
	case c == 0:
		return keyPress{Key: " ", Ctrl: true}, 1
	case c < 0x20:
		return keyPress{Key: string(rune('a' + c - 1)), Ctrl: true}, 1
	}
	r, size := utf8.DecodeRuneInString(s)
	return keyPress{Key: string(r)}, size
}

// modifierBits reads the xterm modifier parameter: 1 + shift(1) + alt(2) + ctrl(4) + meta(8).
func modifierBits(param int, k *keyPress) {
	bits := max(param-1, 0)
	k.Shift, k.Alt, k.Ctrl, k.Meta = bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
}

// parseCSI reads a CSI key sequence such as ESC [ 1 ; 5 C or ESC [ 5 ~.
// Unknown sequences, such as replies to queries, are skipped.
func parseCSI(s string) (any, int) {
	end := 2
	for end < len(s) && (s[end] < 0x40 || s[end] > 0x7e) {
		end++
	}
	if end == len(s) {
		return nil, len(s)
	}
	final := s[end]
	params := strings.Split(s[2:end], ";")
	num := func(i int) int {
		if i >= len(params) {
			return 0
		}
		n, _ := strconv.Atoi(params[i])
		return n
	}

	var k keyPress
	switch {
	case final == 'Z':
		k = keyPress{Key: "Tab", Shift: true}
	case final == '~' && tildeKeys[num(0)] != "":
		k.Key = tildeKeys[num(0)]
		modifierBits(num(1), &k)
	case csiKeys[final] != "":
		k.Key = csiKeys[final]
		modifierBits(num(1), &k)
	default:
		return nil, end + 1
	}
	return k, end + 1
}

// parseSGRMouse reads ESC [ < button ; x ; y M (press) or m (release).
func parseSGRMouse(s string) (any, int) {
	end := 3
	for end < len(s) && s[end] != 'M' && s[end] != 'm' {
		end++
	}
	if end == len(s) {
		return nil, len(s)
	}
	parts := strings.Split(s[3:end], ";")
	if len(parts) != 3 {
		return nil, end + 1
	}
	code, _ := strconv.Atoi(parts[0])
	x, _ := strconv.Atoi(parts[1])
	y, _ := strconv.Atoi(parts[2])
	ev := mouseEvent{
		X: x, Y: y,
		Button:  code & 3,
		Motion:  code&32 != 0,
		Release: s[end] == 'm',
		Shift:   code&4 != 0,
		Alt:     code&8 != 0,
		Ctrl:    code&16 != 0,
	}
	if code&64 != 0 {
		ev.Wheel = -1
		if code&1 != 0 {
			ev.Wheel = 1
		}
	}
	return ev, end + 1
}
