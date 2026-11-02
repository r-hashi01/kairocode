package plugins

import (
	"cmp"
	"fmt"
)

// Said is how the plugin commands tell what they did, the same in each
// front end but for its command (Prefix: "/plugin", "kairocode plugin").
type Said struct{ Prefix string }

// Installed: got is in place.
func (s Said) Installed(got Installed) string {
	at := ""
	if got.Version != "" {
		at = " " + got.Version
	}
	return fmt.Sprintf("Installed %s%s from %s.", got.Name, at, got.From)
}

// NeedsCredentials: name is installed, and is not connected yet.
func (s Said) NeedsCredentials(name string) string {
	return "It needs credentials: " + s.Prefix + " connect " + name
}

// Updated: name went from before to after.
func (s Said) Updated(name, before, after string) string {
	if before == after {
		return fmt.Sprintf("%s is up to date (%s).", name, cmp.Or(after, "no version"))
	}
	return fmt.Sprintf("Updated %s: %s → %s. Turns under way go on with what they started with.", name, cmp.Or(before, "?"), cmp.Or(after, "?"))
}

// Removed: name is gone; its credentials are not.
func (s Said) Removed(name string) string {
	return fmt.Sprintf("Removed %s. Its credentials are kept until %s disconnect %s.", name, s.Prefix, name)
}

// MarketAdded: m can be installed from.
func (s Said) MarketAdded(m Marketplace) string {
	return fmt.Sprintf("Added the marketplace %s (%d plugins). %s list shows them.", m.Name, len(m.Plugins), s.Prefix)
}

// MarketUpdated: name went from before to after.
func (s Said) MarketUpdated(name, before, after string) string {
	if before == after {
		return fmt.Sprintf("%s is up to date (%s).", name, after)
	}
	return fmt.Sprintf("Updated %s: %s → %s. %s update <name> brings its plugins up to it.", name, before, after, s.Prefix)
}

// MarketRemoved: name is gone; what was installed from it is not.
func (s Said) MarketRemoved(name string) string {
	return "Removed the marketplace " + name + ". The plugins installed from it stay."
}
