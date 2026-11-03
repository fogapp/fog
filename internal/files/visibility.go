// What a file browser shows by default is a judgement, not a fact. The tree
// on disk also holds the dotfiles tools leave behind and the bookkeeping
// other systems scatter through every folder they touch — @eaDir beside each
// file a Synology has indexed, Thumbs.db from Windows, .DS_Store from macOS.
// None of it is fog's to delete and none of it is what someone opening Drive
// came to see, so it is hidden by default and two settings put it back.
package files

import "strings"

const (
	// SettingShowHidden shows dotfiles. "on" or "" (off).
	SettingShowHidden = "files_show_hidden"
	// SettingShowSystem shows the bookkeeping other systems leave behind.
	SettingShowSystem = "files_show_system"
)

// systemNames is matched against the whole name, lowercased. Dot-prefixed
// entries are listed here as well as being hidden: someone who turns on
// dotfiles to find a config file has not asked for macOS's thumbnail cache.
var systemNames = []string{
	"@eadir",
	"thumbs.db",
	"desktop.ini",
	"$recycle.bin",
	"#recycle",
	"@recycle",
	"system volume information",
	".ds_store",
	".trashes",
	".spotlight-v100",
	".fseventsd",
	".temporaryitems",
	".documentrevisions-v100",
	".apdisk",
}

// visible reports the two settings, defaulting both to off.
func (m *Manager) visible() (hidden, system bool) {
	on := func(key string) bool {
		v, _ := m.Store.GetSetting(key)
		return v == "on"
	}
	return on(SettingShowHidden), on(SettingShowSystem)
}

// ValidShowSetting is the settings-table validator for both keys.
func ValidShowSetting(v string) bool { return v == "" || v == "on" || v == "off" }

// visibilityFilter is the SQL that hides what the settings say to hide, over
// the name column the caller names. It goes on the page query and its count
// alike: a total counting rows the page will not show is a lie about how
// much is in the folder.
func visibilityFilter(o ListOpts, col string) (string, []any) {
	var sql strings.Builder
	var args []any
	if !o.ShowHidden {
		sql.WriteString(" AND " + col + " NOT LIKE '.%'")
	}
	if !o.ShowSystem {
		sql.WriteString(" AND lower(" + col + ") NOT IN (")
		for i, n := range systemNames {
			if i > 0 {
				sql.WriteString(", ")
			}
			sql.WriteString("?")
			args = append(args, n)
		}
		sql.WriteString(")")
	}
	return sql.String(), args
}
