package collect

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/morass/hostdiff/internal/snapshot"
)

var domainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,200}$`)

// Names of the system hotkeys in com.apple.symbolichotkeys. Numbers without a
// name are shown as "hotkey N".
var hotkeyNames = map[int]string{
	7: "Move focus to the menu bar", 8: "Move focus to the Dock", 9: "Move focus to active or next window",
	10: "Move focus to window toolbar", 11: "Move focus to floating window", 12: "Turn keyboard access on or off",
	13: "Change the way Tab moves focus", 15: "Turn zoom on or off", 17: "Zoom in", 19: "Zoom out",
	21: "Invert colours", 23: "Turn image smoothing on or off", 25: "Increase contrast", 26: "Decrease contrast",
	27: "Move focus to next window", 28: "Save picture of screen as a file", 29: "Copy picture of screen to the clipboard",
	30: "Save picture of selected area as a file", 31: "Copy picture of selected area to the clipboard",
	32: "Mission Control", 33: "Application windows", 36: "Show Desktop", 52: "Turn Dock hiding on or off",
	57: "Move focus to status menus", 59: "Turn VoiceOver on or off", 60: "Select the previous input source",
	61: "Select next source in Input menu", 64: "Show Spotlight search", 65: "Show Finder search window",
	79: "Move left a space", 81: "Move right a space",
	118: "Switch to Desktop 1", 119: "Switch to Desktop 2", 120: "Switch to Desktop 3", 121: "Switch to Desktop 4",
	122: "Switch to Desktop 5", 123: "Switch to Desktop 6", 124: "Switch to Desktop 7", 125: "Switch to Desktop 8",
	126: "Switch to Desktop 9", 160: "Show Launchpad", 162: "Show Accessibility controls",
	163: "Show Notification Centre", 164: "Turn Do Not Disturb on or off", 184: "Screenshot and recording options",
}

var keyCodes = map[int64]string{
	36: "return", 48: "tab", 49: "space", 51: "delete", 53: "escape", 117: "forward-delete",
	115: "home", 119: "end", 116: "page-up", 121: "page-down",
	123: "left", 124: "right", 125: "down", 126: "up",
	122: "F1", 120: "F2", 99: "F3", 118: "F4", 96: "F5", 97: "F6", 98: "F7", 100: "F8",
	101: "F9", 109: "F10", 103: "F11", 111: "F12",
	18: "1", 19: "2", 20: "3", 21: "4", 23: "5", 22: "6", 26: "7", 28: "8", 25: "9", 29: "0",
	27: "-", 24: "=", 33: "[", 30: "]", 41: ";", 39: "'", 43: ",", 47: ".", 44: "/", 42: "\\", 50: "`",
}

// hotkeyCombo renders [ascii, keycode, modifier flags] as "ctrl+opt+space".
func hotkeyCombo(params []any) string {
	if len(params) < 3 {
		return ""
	}
	ascii, _ := toInt(params[0])
	code, _ := toInt(params[1])
	flags, _ := toInt(params[2])
	var mods []string
	for _, m := range []struct {
		bit  int64
		name string
	}{{0x800000, "fn"}, {0x40000, "ctrl"}, {0x80000, "opt"}, {0x20000, "shift"}, {0x100000, "cmd"}} {
		if flags&m.bit != 0 {
			mods = append(mods, m.name)
		}
	}
	key := keyCodes[code]
	if key == "" && ascii > 32 && ascii < 127 {
		key = string(rune(ascii))
	}
	if key == "" {
		key = "key" + strconv.FormatInt(code, 10)
	}
	return strings.Join(append(mods, key), "+")
}

// menuCombo renders an NSUserKeyEquivalents string ("@~k") as "cmd+opt+k".
func menuCombo(s string) string {
	var mods []string
	i := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '@':
			mods = append(mods, "cmd")
		case '~':
			mods = append(mods, "opt")
		case '^':
			mods = append(mods, "ctrl")
		case '$':
			mods = append(mods, "shift")
		default:
			goto done
		}
	}
done:
	key := s[i:]
	if key == "" {
		return "(none)"
	}
	return strings.Join(append(mods, strings.ToLower(key)), "+")
}

func keyboard(e *Env, s *snapshot.Section) {
	if m, err := e.exportDomain("com.apple.symbolichotkeys"); err == nil {
		hk, _ := m["AppleSymbolicHotKeys"].(map[string]any)
		ids := make([]int, 0, len(hk))
		for k := range hk {
			if n, err := strconv.Atoi(k); err == nil {
				ids = append(ids, n)
			}
		}
		sort.Ints(ids)
		for _, id := range ids {
			entry, _ := hk[strconv.Itoa(id)].(map[string]any)
			name := hotkeyNames[id]
			if name == "" {
				name = "hotkey " + strconv.Itoa(id)
			}
			state := "off"
			if on, ok := toInt(entry["enabled"]); ok && on != 0 {
				state = "on"
			}
			if val, ok := entry["value"].(map[string]any); ok {
				if params, ok := val["parameters"].([]any); ok {
					if c := hotkeyCombo(params); c != "" {
						state += " " + c
					}
				}
			}
			s.Add("system › "+name, state, "")
		}
	}

	// App menu shortcuts set in System Settings › Keyboard › App Shortcuts are
	// stored per app as NSUserKeyEquivalents, and System Settings keeps the
	// list of apps that have any in com.apple.universalaccess. Only those
	// domains are read, through `defaults`. Opening arbitrary files in
	// ~/Library/Preferences (or `defaults find`) can block on a macOS privacy
	// prompt for another app's data.
	domains := []string{"NSGlobalDomain"}
	if ua, err := e.exportDomain("com.apple.universalaccess"); err == nil {
		if list, ok := ua["com.apple.custommenu.apps"].([]any); ok {
			for _, v := range list {
				if d := str(v); domainName.MatchString(d) && d != "NSGlobalDomain" {
					domains = append(domains, d)
				}
			}
		}
	}
	for _, d := range domains {
		m, err := e.exportDomain(d)
		if err != nil {
			continue
		}
		eq, _ := m["NSUserKeyEquivalents"].(map[string]any)
		app := d
		if d == "NSGlobalDomain" {
			app = "all apps"
		}
		for title, v := range eq {
			s.Add("app › "+app+" › "+title, menuCombo(str(v)), "")
		}
	}

	if m, err := e.exportDomain("pbs"); err == nil {
		st, _ := m["NSServicesStatus"].(map[string]any)
		for id, v := range st {
			entry, _ := v.(map[string]any)
			var parts []string
			if b, ok := entry["enabled_context_menu"].(bool); ok {
				parts = append(parts, "context menu "+onOff(b))
			}
			if b, ok := entry["enabled_services_menu"].(bool); ok {
				parts = append(parts, "services menu "+onOff(b))
			}
			if k := str(entry["key_equivalent"]); k != "" {
				parts = append(parts, "key "+menuCombo(k))
			}
			s.Add("service › "+id, strings.Join(parts, ", "), "")
		}
	}
	if entries, err := os.ReadDir(e.HomePath("Library", "Services")); err == nil {
		for _, en := range entries {
			if !strings.HasPrefix(en.Name(), ".") {
				s.Add("quick action › "+en.Name(), "installed", "")
			}
		}
	}

	if m, err := e.exportDomain("com.apple.HIToolbox"); err == nil {
		srcs, _ := m["AppleEnabledInputSources"].([]any)
		for _, v := range srcs {
			src, _ := v.(map[string]any)
			name := str(src["KeyboardLayout Name"])
			if name == "" {
				name = str(src["Input Mode"])
			}
			if name == "" {
				name = str(src["Bundle ID"])
			}
			if name != "" {
				s.Add("input source › "+name, "enabled", "")
			}
		}
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
