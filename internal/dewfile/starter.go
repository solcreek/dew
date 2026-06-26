package dewfile

import (
	"fmt"
	"strings"
)

// Starter renders a starter dew.toml for a detected project. Only fields
// that were actually detected are pinned; the rest are shown as commented
// hints, including a [[service]] example for composing OCI services. The
// output round-trips through Load.
func Starter(profile, install, command string, port int) string {
	var b strings.Builder
	b.WriteString("# dew.toml — project descriptor for `dew up`.\n")
	b.WriteString("# Auto-detection still works without this file; dew.toml pins the\n")
	b.WriteString("# choices and lets you compose OCI services in the same VM.\n\n")

	b.WriteString("[project]\n")
	if profile != "" {
		fmt.Fprintf(&b, "profile = %q\n", profile)
	} else {
		b.WriteString("# profile = \"node\"   # minimal|node|python|standard\n")
	}
	b.WriteString("\n[dev]\n")
	writeOptStr(&b, "install", install, "npm ci")
	writeOptStr(&b, "command", command, "npm run dev")
	if port > 0 {
		fmt.Fprintf(&b, "port = %d\n", port)
	} else {
		b.WriteString("# port = 3000\n")
	}

	b.WriteString("\n# Services run in the same VM as the project (containers use the\n")
	b.WriteString("# VM network, so the dev server reaches them on localhost).\n")
	b.WriteString("# Uncomment and edit, then re-run `dew up`:\n")
	b.WriteString("#\n")
	b.WriteString("# [[service]]\n")
	b.WriteString("# name = \"redis\"\n")
	b.WriteString("# image = \"redis:7-alpine\"\n")
	b.WriteString("# port = 6379\n")
	b.WriteString("#\n")
	b.WriteString("# [[service]]\n")
	b.WriteString("# name = \"mailpit\"\n")
	b.WriteString("# image = \"axllent/mailpit:latest\"\n")
	b.WriteString("# port = 8025\n")
	b.WriteString("# env = [\"MP_SMTP_AUTH_ACCEPT_ANY=1\"]\n")
	return b.String()
}

// writeOptStr writes `key = "val"` when val is set, otherwise a commented
// `# key = "placeholder"` hint.
func writeOptStr(b *strings.Builder, key, val, placeholder string) {
	if val != "" {
		fmt.Fprintf(b, "%s = %q\n", key, val)
	} else {
		fmt.Fprintf(b, "# %s = %q\n", key, placeholder)
	}
}
