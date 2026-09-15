package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/morass/hostdiff/internal/redact"
	"github.com/morass/hostdiff/internal/snapshot"
)

// decodePlist reads XML or binary plist data into generic values.
func decodePlist(b []byte) (map[string]any, error) {
	var v map[string]any
	if _, err := plist.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// exportDomain returns a defaults domain as a map. It uses `defaults export`,
// which reads through cfprefsd and so sees unsaved changes, and never touches
// sandboxed app containers.
func (e *Env) exportDomain(domain string) (map[string]any, error) {
	if e.Root != "" {
		// Tests: read a fixture plist instead of the live defaults system.
		b, ok := ReadFile(filepath.Join(e.Root, "defaults", domain+".plist"), 8<<20)
		if !ok {
			return nil, errNotFound
		}
		return decodePlist(b)
	}
	out, err := e.Out("defaults", "export", domain, "-")
	if err != nil {
		return nil, err
	}
	return decodePlist([]byte(out))
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case uint64:
		return strconv.FormatUint(x, 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(x))
	case []any:
		parts := make([]string, len(x))
		for i, p := range x {
			parts[i] = str(p)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + str(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case uint64:
		return int64(x), true
	case int64:
		return x, true
	case float64:
		return int64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func mas(e *Env, s *snapshot.Section) {
	if e.Look("mas") == "" {
		s.Status, s.Note = snapshot.Absent, "mas is not installed (App Store apps still appear under Applications)"
		return
	}
	out, err := e.Out("mas", "list")
	if err != nil {
		s.Status, s.Note = snapshot.Failed, "mas list: "+err.Error()
		return
	}
	re := regexp.MustCompile(`^(\d+)\s+(.+?)\s+\(([^)]*)\)$`)
	for _, l := range Lines(out) {
		if m := re.FindStringSubmatch(l); m != nil {
			s.AddTag(m[2], m[3], "id "+m[1])
		}
	}
}

func apps(e *Env, s *snapshot.Section) {
	add := func(bundle, key string) {
		if e.pathProtected(bundle) {
			s.Add(key, "not read: linked into a privacy-protected folder", "")
			return
		}
		b, ok := ReadFile(filepath.Join(bundle, "Contents", "Info.plist"), 4<<20)
		if !ok {
			s.Add(key, "?", "")
			return
		}
		info, err := decodePlist(b)
		if err != nil {
			s.Add(key, "?", "")
			return
		}
		version := str(info["CFBundleShortVersionString"])
		if version == "" {
			version = str(info["CFBundleVersion"])
		}
		detail := "id " + str(info["CFBundleIdentifier"])
		if build := str(info["CFBundleVersion"]); build != "" && build != version {
			detail += "\nbuild " + build
		}
		s.Add(key, version, detail)
	}
	scan := func(dir, prefix string) {
		if e.pathProtected(dir) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, en := range entries {
			name := en.Name()
			p := filepath.Join(dir, name)
			if strings.HasSuffix(name, ".app") {
				add(p, prefix+name)
				continue
			}
			if !en.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			// One folder level (Utilities, vendor folders).
			sub, _ := os.ReadDir(p)
			for _, se := range sub {
				if strings.HasSuffix(se.Name(), ".app") {
					add(filepath.Join(p, se.Name()), prefix+name+"/"+se.Name())
				}
			}
		}
	}
	scan(e.Sys("/Applications"), "")
	scan(e.HomePath("Applications"), "~/Applications/")
}

func shortcuts(e *Env, s *snapshot.Section) {
	if e.Look("shortcuts") == "" {
		s.Status, s.Note = snapshot.Absent, "the shortcuts command is not available (macOS 12 or later)"
		return
	}
	// ~/Library/Shortcuts is never touched: opening it can raise a privacy
	// prompt, and over ssh it can block forever in the kernel. The shortcuts
	// tool returns an empty list, not an error, when the session may not
	// read them (over ssh, from some terminals), so empty means unavailable.
	out, err := e.Out("shortcuts", "list")
	if err != nil {
		s.Status, s.Note = snapshot.Failed, "shortcuts list: "+err.Error()
		return
	}
	names := Lines(out)
	if len(names) == 0 {
		s.Status, s.Note = snapshot.Unavailable, "the shortcuts tool listed nothing: no shortcuts, or this session (ssh, a terminal without access) may not read them"
		return
	}
	folderOf := map[string]string{}
	if fo, err := e.Out("shortcuts", "list", "--folders"); err == nil {
		for i, f := range Lines(fo) {
			if i >= 100 {
				break
			}
			in, err := e.Out("shortcuts", "list", "--folder-name", f)
			if err != nil {
				continue
			}
			for _, n := range Lines(in) {
				folderOf[n] = f
			}
		}
	}
	for _, n := range names {
		folder := folderOf[n]
		if folder == "" {
			folder = "(no folder)"
		}
		// Names are free text: give them the full redaction values get.
		name, _ := redact.Secrets(n)
		s.Add(name, folder, "")
	}
}

// A curated set of settings people actually change. Keys missing from a
// domain are left out: they are at their default.
var defaultsTable = []struct {
	domain, label string
	keys          []string
}{
	{"NSGlobalDomain", "global", []string{"AppleInterfaceStyle", "AppleInterfaceStyleSwitchesAutomatically", "AppleAccentColor", "AppleHighlightColor", "AppleShowAllExtensions", "AppleShowScrollBars", "KeyRepeat", "InitialKeyRepeat", "ApplePressAndHoldEnabled", "AppleKeyboardUIMode", "com.apple.keyboard.fnState", "NSAutomaticSpellingCorrectionEnabled", "NSAutomaticCapitalizationEnabled", "NSAutomaticPeriodSubstitutionEnabled", "NSAutomaticQuoteSubstitutionEnabled", "NSAutomaticDashSubstitutionEnabled", "NSAutomaticInlinePredictionEnabled", "WebAutomaticSpellingCorrectionEnabled", "com.apple.swipescrolldirection", "com.apple.mouse.scaling", "com.apple.trackpad.scaling", "AppleLanguages", "AppleLocale", "AppleMeasurementUnits", "AppleMetricUnits", "AppleTemperatureUnit", "AppleICUForce24HourTime", "NSDocumentSaveNewDocumentsToCloud", "NSTableViewDefaultSizeMode", "_HIHideMenuBar", "AppleWindowTabbingMode", "AppleActionOnDoubleClick", "NSQuitAlwaysKeepsWindows"}},
	{"com.apple.dock", "dock", []string{"autohide", "autohide-delay", "autohide-time-modifier", "tilesize", "magnification", "largesize", "orientation", "mineffect", "minimize-to-application", "show-recents", "show-process-indicators", "mru-spaces", "static-only", "launchanim", "expose-group-apps", "wvous-tl-corner", "wvous-tr-corner", "wvous-bl-corner", "wvous-br-corner", "persistent-apps", "persistent-others"}},
	{"com.apple.finder", "finder", []string{"AppleShowAllFiles", "ShowPathbar", "ShowStatusBar", "ShowTabView", "ShowSidebar", "FXPreferredViewStyle", "FXDefaultSearchScope", "FXEnableExtensionChangeWarning", "FXRemoveOldTrashItems", "_FXShowPosixPathInTitle", "_FXSortFoldersFirst", "NewWindowTarget", "NewWindowTargetPath", "ShowExternalHardDrivesOnDesktop", "ShowHardDrivesOnDesktop", "ShowRemovableMediaOnDesktop", "ShowMountedServersOnDesktop", "CreateDesktop", "QuitMenuItem"}},
	{"com.apple.screencapture", "screenshots", []string{"location", "type", "disable-shadow", "show-thumbnail", "include-date", "target"}},
	{"com.apple.AppleMultitouchTrackpad", "trackpad", []string{"Clicking", "TrackpadThreeFingerDrag", "TrackpadRightClick", "TrackpadCornerSecondaryClick", "FirstClickThreshold", "ActuationStrength", "Dragging", "DragLock"}},
	{"com.apple.driver.AppleBluetoothMultitouch.trackpad", "bt-trackpad", []string{"Clicking", "TrackpadThreeFingerDrag", "TrackpadRightClick"}},
	{"com.apple.menuextra.clock", "clock", []string{"DateFormat", "ShowSeconds", "ShowDate", "ShowDayOfWeek", "IsAnalog", "Show24Hour"}},
	{"com.apple.WindowManager", "windows", []string{"GloballyEnabled", "EnableStandardClickToShowDesktop", "EnableTilingByEdgeDrag", "EnableTopTilingByEdgeDrag", "EnableTilingOptionAccelerator", "EnableTiledWindowMargins", "HideDesktop", "StandardHideWidgets", "AutoHide"}},
	{"com.apple.controlcenter", "menu bar", []string{"NSStatusItem Visible Battery", "NSStatusItem Visible Bluetooth", "NSStatusItem Visible WiFi", "NSStatusItem Visible Sound", "NSStatusItem Visible NowPlaying", "NSStatusItem Visible FocusModes", "NSStatusItem Visible Display"}},
	{"com.apple.Terminal", "terminal", []string{"Default Window Settings", "Startup Window Settings", "SecureKeyboardEntry", "ShowLineMarks"}},
	{"com.apple.TextEdit", "textedit", []string{"RichText", "PlainTextEncoding"}},
	{"com.apple.desktopservices", "desktopservices", []string{"DSDontWriteNetworkStores", "DSDontWriteUSBStores"}},
	{"com.apple.LaunchServices", "launchservices", []string{"LSQuarantine"}},
	{"com.apple.universalaccess", "accessibility", []string{"reduceMotion", "reduceTransparency", "increaseContrast", "mouseDriverCursorSize", "closeViewScrollWheelToggle"}},
	{"com.apple.Siri", "siri", []string{"StatusMenuVisible", "VoiceTriggerUserEnabled"}},
}

func macDefaults(e *Env, s *snapshot.Section) {
	read := 0
	for _, d := range defaultsTable {
		m, err := e.exportDomain(d.domain)
		if err != nil {
			continue
		}
		read++
		for _, k := range d.keys {
			v, ok := m[k]
			if !ok {
				continue
			}
			value := defaultsValue(k, v)
			s.AddTag(d.label+" › "+k, value, defaultsType(d.domain, k, v))
		}
	}
	if read == 0 {
		s.Status, s.Note = snapshot.Failed, "defaults export returned nothing"
	}
}

// defaultsValue renders a setting. Dock tiles become their app names.
func defaultsValue(key string, v any) string {
	if key == "persistent-apps" || key == "persistent-others" {
		if arr, ok := v.([]any); ok {
			var names []string
			for _, t := range arr {
				tile, _ := t.(map[string]any)
				data, _ := tile["tile-data"].(map[string]any)
				label := str(data["file-label"])
				if label == "" {
					label = "(spacer)"
				}
				names = append(names, label)
			}
			return strings.Join(names, ", ")
		}
	}
	return str(v)
}

// defaultsType records the type of scalar settings so a fix script can write
// them back; compound values get no type and are never written.
func defaultsType(domain, key string, v any) string {
	t := ""
	switch v.(type) {
	case bool:
		t = "bool"
	case uint64, int64:
		t = "int"
	case float64, float32:
		t = "float"
	case string:
		t = "string"
	default:
		return ""
	}
	return "defaults " + domain + "\ntype " + t
}

func launchd(e *Env, s *snapshot.Section) {
	dirs := []struct{ dir, scope string }{
		{e.HomePath("Library", "LaunchAgents"), "user agent"},
		{e.Sys("/Library/LaunchAgents"), "agent"},
		{e.Sys("/Library/LaunchDaemons"), "daemon"},
	}
	for _, d := range dirs {
		if e.pathProtected(d.dir) {
			continue
		}
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			continue
		}
		for _, en := range entries {
			if !strings.HasSuffix(en.Name(), ".plist") {
				continue
			}
			key := d.scope + " › " + strings.TrimSuffix(en.Name(), ".plist")
			b, ok := ReadFile(filepath.Join(d.dir, en.Name()), 1<<20)
			if !ok {
				s.Add(key, "unreadable", "")
				continue
			}
			m, err := decodePlist(b)
			if err != nil {
				s.Add(key, "invalid plist", "")
				continue
			}
			xml, _ := plist.MarshalIndent(m, plist.XMLFormat, "  ")
			s.Add(key, launchdSummary(m), string(xml))
		}
	}
	if e.Root == "" && e.User != "" {
		// Overrides made with launchctl enable/disable live outside the plists.
		if out, err := e.Out("id", "-u"); err == nil {
			uid := strings.TrimSpace(out)
			if dis, err := e.Out("launchctl", "print-disabled", "gui/"+uid); err == nil {
				re := regexp.MustCompile(`"([^"]+)"\s*=>\s*(\w+)`)
				for _, m := range re.FindAllStringSubmatch(dis, -1) {
					if strings.HasPrefix(m[1], "com.apple.") {
						continue
					}
					state := "enabled"
					if m[2] == "true" || m[2] == "disabled" {
						state = "disabled"
					}
					s.Add("override › "+m[1], state, "")
				}
			}
		}
	}
}

func launchdSummary(m map[string]any) string {
	prog := str(m["Program"])
	if prog == "" {
		if args, ok := m["ProgramArguments"].([]any); ok && len(args) > 0 {
			prog = str(args[0])
		}
	}
	var when []string
	if n, ok := toInt(m["StartInterval"]); ok {
		when = append(when, fmt.Sprintf("every %ds", n))
	}
	if cal, ok := m["StartCalendarInterval"]; ok {
		when = append(when, "calendar "+str(cal))
	}
	if b, ok := m["RunAtLoad"].(bool); ok && b {
		when = append(when, "at load")
	}
	if _, ok := m["KeepAlive"]; ok {
		when = append(when, "keep alive")
	}
	if b, ok := m["Disabled"].(bool); ok && b {
		when = append(when, "disabled")
	}
	out := filepath.Base(prog)
	if prog == "" {
		out = "(no program)"
	}
	if len(when) > 0 {
		out += " (" + strings.Join(when, ", ") + ")"
	}
	return out
}

func power(e *Env, s *snapshot.Section) {
	out, err := e.Out("pmset", "-g", "custom")
	if err != nil {
		s.Status, s.Note = snapshot.Failed, "pmset: "+err.Error()
		return
	}
	group := ""
	for _, raw := range strings.Split(out, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		if strings.HasSuffix(l, ":") {
			group = strings.TrimSuffix(l, ":")
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		s.Add(group+" › "+strings.Join(f[:len(f)-1], " "), f[len(f)-1], "")
	}
}
