package cli

import (
	"path/filepath"

	"github.com/openclaw/crawlkit/control"
	"github.com/openclaw/wacrawl/internal/backup"
)

func controlManifest() control.Manifest {
	m := control.NewManifest("wacrawl", "WhatsApp Crawl", "wacrawl")
	m.Description = "Local-first WhatsApp Desktop archive crawler."
	m.Branding = control.Branding{SymbolName: "message.fill", AccentColor: "#25d366", BundleIdentifier: "net.whatsapp.WhatsApp"}
	m.Paths = control.Paths{
		DefaultConfig:   backup.DefaultConfigPath(),
		DefaultDatabase: defaultDBPath(),
		DefaultCache:    filepath.Join(filepath.Dir(defaultDBPath()), "cache"),
		DefaultLogs:     filepath.Join(filepath.Dir(defaultDBPath()), "logs"),
	}
	m.Capabilities = []string{"metadata", "doctor", "status", "sync", "search", "sql", "web", "backup"}
	m.Privacy = control.Privacy{ContainsPrivateMessages: true, ExportsSecrets: false, LocalOnlyScopes: []string{"whatsapp-desktop", "sqlite", "encrypted-git-backup"}}
	m.Commands = map[string]control.Command{
		"doctor":         {Title: "Doctor", Argv: []string{"wacrawl", "--json", "doctor"}, JSON: true},
		"status":         {Title: "Status", Argv: []string{"wacrawl", "--json", "--sync", "never", "status"}, JSON: true},
		"sync":           {Title: "Sync", Argv: []string{"wacrawl", "--json", "sync"}, JSON: true, Mutates: true},
		"search":         {Title: "Search", Argv: []string{"wacrawl", "--json", "--sync", "never", "search"}, JSON: true},
		"sql":            {Title: "SQL", Argv: []string{"wacrawl", "--json", "--sync", "never", "sql"}, JSON: true},
		"web":            {Title: "Web viewer", Argv: []string{"wacrawl", "--sync", "never", "web"}},
		"contact-export": {Title: "Export contacts", Argv: []string{"wacrawl", "--json", "--sync", "never", "contacts", "export"}, JSON: true},
	}
	return m
}
