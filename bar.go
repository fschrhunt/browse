// The bar and the address box. The bar is barRows tall: back, forward, and
// reload on the left, the tabs in the middle, centered, with new tab right
// after the last one while the mouse is over the bar, and the agent sign on
// the right. A grey rounded rectangle sits behind the active tab and behind
// whatever the mouse is over. The address box is a rounded rectangle at eye
// level, the one place an address is typed: on a blank tab's start screen,
// and over the page when editing. Every color comes from the terminal's own,
// so the bar fits light and dark themes alike.
package main

import (
	"fmt"
	"image/color"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	barRows    = 3
	barRow     = 2 // the row the bar's buttons and tabs are on
	buttonCols = 4 // a button's width, with its icon in the middle
	iconCols   = 2
	tabPad     = 2  // columns in a tab on each side of its label
	tabMax     = 22 // the widest a tab's label gets
	agentSign  = "● agent"

	loadingCycle = 1400                    // milliseconds for the loading band to cross the bar
	loadingBlue  = "\x1b[38;2;59;130;246m" // the loading band's color
)

// The bar's buttons, in the order of their image ids after the page's.
const (
	backButton = iota
	forwardButton
	reloadButton
	newTabButton
	buttonCount
)

var buttonIcons = [buttonCount]string{"back", "forward", "reload", "new-tab"}

// The ids of the images behind text, after the buttons'.
var (
	boxImage    = pageImage + 1 + buttonCount // the address box
	activeImage = boxImage + 1                // behind the active tab
	hoverImage  = boxImage + 2                // behind what the mouse is over
)

// barItem is a place on the bar the mouse can point at: a button, or a tab.
type barItem struct {
	button     int // the button, or -1 for a tab
	tab        TabInfo
	label      string // a tab's label
	left, cols int
}

// items lays out the bar: the three buttons from the second column, the tabs
// centered on the terminal, and new tab after the last tab. Tabs share the
// room between the buttons and the agent sign, each as wide as its label.
func (v *viewer) items() []barItem {
	var out []barItem
	for b := backButton; b <= reloadButton; b++ {
		out = append(out, barItem{button: b, left: 2 + b*buttonCols, cols: buttonCols})
	}
	first := 2 + 3*buttonCols + 1
	room := v.lay.cols - first - utf8.RuneCountInString(agentSign) - 2 - buttonCols - 1
	tabs := v.frame.Tabs
	if len(tabs) == 0 {
		return out
	}
	widest := min(tabMax, max((room-(len(tabs)-1))/len(tabs)-2*tabPad, 3))
	total := len(tabs) - 1
	labels := make([]string, len(tabs))
	for i, t := range tabs {
		labels[i] = fit(tabLabel(t), widest)
		total += utf8.RuneCountInString(labels[i]) + 2*tabPad
	}
	left := max((v.lay.cols-total)/2+1, first)
	for i, t := range tabs {
		cols := utf8.RuneCountInString(labels[i]) + 2*tabPad
		out = append(out, barItem{button: -1, tab: t, label: labels[i], left: left, cols: cols})
		left += cols + 1
	}
	return append(out, barItem{button: newTabButton, left: left, cols: buttonCols})
}

// tabLabel is what a tab shows: its site, its title when it has no site, or
// New tab when it is blank.
func tabLabel(t TabInfo) string {
	if t.URL == "about:blank" || t.URL == "" {
		return "New tab"
	}
	if u, err := url.Parse(t.URL); err == nil && u.Hostname() != "" {
		return strings.TrimPrefix(u.Hostname(), "www.")
	}
	if t.Title != "" {
		return t.Title
	}
	return t.URL
}

// hovered returns the item under the mouse, if the mouse is over the bar.
func (v *viewer) hovered(items []barItem) (barItem, bool) {
	for _, s := range items {
		if v.hoverCol >= s.left && v.hoverCol < s.left+s.cols {
			return s, true
		}
	}
	return barItem{}, false
}

// enabled reports whether a button does anything now: back and forward only
// when the tab's history goes that way.
func (v *viewer) enabled(b int) bool {
	return (b != backButton || v.frame.Back) && (b != forwardButton || v.frame.Forward)
}

// barClick acts on a click at col on the bar: it presses a button, closes a
// tab at its ×, edits the active tab's address, or switches to another tab.
func (v *viewer) barClick(col int) {
	s, ok := v.hovered(v.items())
	switch {
	case !ok || !v.enabled(s.button):
	case s.button == reloadButton && v.frame.Loading:
		v.do(Request{Cmd: "stop"})
	case s.button == backButton, s.button == forwardButton, s.button == reloadButton:
		v.do(Request{Cmd: buttonIcons[s.button]})
	case s.button == newTabButton:
		v.newTab()
	case col == s.left+1:
		v.do(Request{Cmd: "close", Tab: s.tab.Tab})
	case s.tab.Active:
		v.startEdit(s.tab.URL)
	default:
		v.do(Request{Cmd: "switch", Tab: s.tab.Tab})
	}
}

// bar draws the bar: its middle row of tab labels, the agent sign, and any
// note, then the buttons and grey rectangles that changed. A hovered tab
// shows × to close it. The row is written around new tab's icon, never over it.
func (v *viewer) bar() string {
	items := v.items()
	hover, hovering := v.hovered(items)
	row := make([]string, v.lay.cols+1) // by column, from 1
	for i := range row {
		row[i] = " "
	}
	var plusCol int
	for _, s := range items {
		switch {
		case s.button == newTabButton && v.hoverCol > 0:
			plusCol = s.left + (buttonCols-iconCols)/2
		case s.button < 0:
			for i, r := range []rune(s.label) {
				if c := s.left + tabPad + i; c < v.lay.cols {
					row[c] = string(r)
				}
			}
			if hovering && hover.button < 0 && hover.tab.Tab == s.tab.Tab {
				row[s.left+1] = dim + "×" + reset
			}
			if v.p.tmux && (s.tab.Active || hovering && hover.left == s.left) {
				// tmux cannot show the grey rectangles, so the tab's cells take the grey.
				shade := v.shade(0.08)
				if s.tab.Active {
					shade = v.shade(0.16)
				}
				bg := fmt.Sprintf("\x1b[48;2;%d;%d;%dm", shade.R, shade.G, shade.B)
				for c := s.left; c < s.left+s.cols; c++ {
					row[c] = bg + row[c] + reset
				}
			}
		}
	}
	if v.frame.Agent {
		for i, r := range []rune(agentSign) {
			row[v.lay.cols-len([]rune(agentSign))+i] = accent + string(r) + reset
		}
	}

	// Write the row from just past the buttons, skipping new tab's icon.
	from := 2 + 3*buttonCols
	var b strings.Builder
	for c := from; c <= v.lay.cols; c++ {
		if c == plusCol {
			c += iconCols - 1
			continue
		}
		if c == from || c == plusCol+iconCols {
			fmt.Fprintf(&b, "\x1b[%d;%dH", barRow, c)
		}
		b.WriteString(row[c])
	}

	// The bottom row shows notes, such as errors, for a few seconds, and while
	// the page loads, a blue band sliding along its bottom edge.
	bottom := make([]string, v.lay.cols+1)
	for i := range bottom {
		bottom[i] = " "
	}
	if v.frame.Loading {
		width := max(v.lay.cols/4, 8)
		phase := float64(time.Now().UnixMilli()%loadingCycle) / loadingCycle
		first := int(phase*float64(v.lay.cols+width)) - width + 1
		for c := max(first, 1); c < first+width && c <= v.lay.cols; c++ {
			bottom[c] = loadingBlue + "▁" + reset
		}
	}
	if v.note != "" && time.Now().Before(v.noteUntil) {
		note := []rune(fit(v.note, v.lay.cols-2))
		for i, r := range note {
			bottom[(v.lay.cols-len(note))/2+1+i] = accent + string(r) + reset
		}
	}
	fmt.Fprintf(&b, "\x1b[%d;1H%s", barRows, strings.Join(bottom[1:], ""))

	var active, under barItem
	for _, s := range items {
		if s.button < 0 && s.tab.Active {
			active = s
		}
	}
	if hovering && v.enabled(hover.button) && !(hover.button < 0 && hover.tab.Active) {
		under = hover
	}
	return b.String() + v.pill(activeImage, active, 0.16) + v.pill(hoverImage, under, 0.08) + v.buttons(items)
}

// pill places a grey rounded rectangle behind item s as image id, shade of
// the way from the terminal's background to its text color, or removes it
// when s is empty. It only changes what is on screen when the item or shade
// does. tmux cannot show it.
func (v *viewer) pill(id uint32, s barItem, shade float64) string {
	key := ""
	if s.cols > 0 && !v.p.tmux {
		key = fmt.Sprint(s.left, s.cols, shade)
	}
	if key == v.pills[id] {
		return ""
	}
	v.pills[id] = key
	if key == "" {
		return v.p.remove(id)
	}
	h := barRows * v.lay.cellH
	png, err := drawBox(int(float64(s.cols)*v.lay.cellW+0.5), int(h+0.5), h*0.2, h*0.17, v.shade(shade), v.shade(shade))
	if err != nil {
		v.say(err.Error())
		return ""
	}
	return fmt.Sprintf("\x1b[1;%dH", s.left) + v.p.under(id, png, s.cols, barRows)
}

// buttons draws the buttons whose look changed since they were last drawn.
// Back and forward are dimmed when the tab's history does not go that way,
// reload turns into stop while the page loads, and new tab only shows while
// the mouse is over the bar.
func (v *viewer) buttons(items []barItem) string {
	var b strings.Builder
	for _, s := range items {
		if s.button < 0 {
			continue
		}
		i := s.button
		look := buttonIcons[i]
		switch {
		case !v.enabled(i):
			look += " dim"
		case i == reloadButton && v.frame.Loading:
			look = "stop"
		case i == newTabButton && v.hoverCol == 0:
			look = ""
		}
		col := s.left + (buttonCols-iconCols)/2
		key := fmt.Sprint(look, col)
		if look == "" {
			key = ""
		}
		if key == v.drawn[i] {
			continue
		}
		v.drawn[i] = key
		id := pageImage + 1 + uint32(i)
		if look == "" {
			b.WriteString(v.p.remove(id))
			continue
		}
		png := v.icon(strings.CutSuffix(look, " dim"))
		if png == nil {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH", barRow, col)
		b.WriteString(v.p.image(id, png, iconCols, 1))
		if v.p.tmux {
			b.WriteString(placeholders(id, barRow, col, iconCols, 1))
		}
	}
	return b.String()
}

// icon returns a button's icon as a PNG that fills its cells, in the
// terminal's text color, or halfway to its background when dimmed.
func (v *viewer) icon(name string, dimmed bool) []byte {
	key := fmt.Sprint(name, dimmed)
	if png, ok := v.icons[key]; ok {
		return png
	}
	c := v.caps.fg
	if dimmed {
		c = v.shade(0.5)
	}
	png, err := drawIcon(name, int(iconCols*v.lay.cellW+0.5), int(v.lay.cellH+0.5), c)
	if err != nil {
		v.say(err.Error())
	}
	v.icons[key] = png
	return png
}

// shade is the color amount of the way from the terminal's background to its
// text color: a light grey on a dark theme, and a dark grey on a light one.
func (v *viewer) shade(amount float64) color.NRGBA {
	mix := func(bg, fg uint8) uint8 { return uint8(float64(bg) + (float64(fg)-float64(bg))*amount) }
	fg, bg := v.caps.fg, v.caps.bg
	return color.NRGBA{mix(bg.R, fg.R), mix(bg.G, fg.G), mix(bg.B, fg.B), 255}
}

// placeholder is the hint shown in the address box before anything is typed.
const placeholder = "Search or enter address"

// boxRect is where the address box goes: three rows from row top, width
// columns from column left, centered across and at eye level, a third of the
// way down the page area.
func (v *viewer) boxRect() (top, left, width int) {
	width = min(max(v.lay.cols*2/5, 40), 96, v.lay.cols-4)
	left = (v.lay.cols-width)/2 + 1
	top = barRows + 1 + (v.lay.rows-barRows-3)/3
	return top, left, width
}

// box draws the address box while it shows, which is on the start screen and
// while editing, and clears it away when it stops showing. The box takes the
// keyboard, so the cursor goes to its caret. Inside tmux, where text cannot
// sit on an image, it is drawn with rounded line characters instead.
func (v *viewer) box() string {
	top, left, width := v.boxRect()
	show := v.onStart() || v.editing
	var out string
	switch {
	case show && !v.boxShown && !v.p.tmux:
		h := 3 * v.lay.cellH
		png, err := drawBox(int(float64(width)*v.lay.cellW+0.5), int(h+0.5), h*0.15, h*0.2, v.shade(0.08), v.shade(0.22))
		if err != nil {
			v.say(err.Error())
			break
		}
		out = fmt.Sprintf("\x1b[%d;%dH", top, left) + v.p.under(boxImage, png, width, 3)
	case !show && v.boxShown:
		out = v.p.remove(boxImage)
		for r := top; r < top+3; r++ {
			out += fmt.Sprintf("\x1b[%d;%dH%s", r, left, strings.Repeat(" ", width))
		}
		if v.p.tmux {
			out += placeholders(pageImage, barRows+1, 1, v.lay.cols, v.lay.rows-barRows)
		}
	}
	v.boxShown = show
	if !show {
		return out
	}

	inner := width - 4 // two columns of padding on each side
	field := dim + fit(placeholder, inner) + reset + strings.Repeat(" ", max(inner-len(placeholder), 0))
	v.cursorRow, v.cursorCol = top+1, left+2
	if v.editing && len(v.edit) > 0 {
		var caret int
		field, caret = v.editField(inner)
		v.cursorCol += caret
	}
	if !v.p.tmux {
		return out + fmt.Sprintf("\x1b[%d;%dH", top+1, left+2) + field
	}
	line := strings.Repeat("─", width-2)
	return out + fmt.Sprintf("\x1b[%d;%dH%s╭%s╮%s", top, left, dim, line, reset) +
		fmt.Sprintf("\x1b[%d;%dH%s│%s %s %s│%s", top+1, left, dim, reset, field, dim, reset) +
		fmt.Sprintf("\x1b[%d;%dH%s╰%s╯%s", top+2, left, dim, line, reset)
}

// inBox reports whether a cell is inside the address box.
func (v *viewer) inBox(col, row int) bool {
	top, left, width := v.boxRect()
	return row >= top && row < top+3 && col >= left && col < left+width
}

// screen draws what changes as the person types and moves: the bar, the
// address box, and the terminal's cursor, a thin bar at the box's caret
// while the box shows.
func (v *viewer) screen() string {
	v.cursorRow = 0
	out := v.bar() + v.box()
	if v.cursorRow == 0 {
		return out + "\x1b[?25l"
	}
	return out + fmt.Sprintf("\x1b[%d;%dH\x1b[6 q\x1b[?25h", v.cursorRow, v.cursorCol)
}
