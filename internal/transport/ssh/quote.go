package ssh

import (
	"fmt"
	"sort"
	"strings"
)

// shellQuote wraps s in POSIX single quotes so that every byte it contains is
// treated as data by any conforming shell. Single quotes inside s are encoded
// as the three-step sequence '\” (close, escaped, reopen).
func shellQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// buildCommand renders an SSH exec command string that preserves the
// transport contract exactly: argv boundaries stay argument boundaries, the
// working directory is applied, and a non-nil environment replaces the
// login environment wholesale. Supplied values are data, never shell syntax.
// A failed cd exits 126 (start failure per the transport contract); '--'
// stops env from parsing supplied values as options.
func buildCommand(dir string, env map[string]string, argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", fmt.Errorf("argv must name a program")
	}
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" || exit 126\nexec")
	if env != nil {
		b.WriteString(" env -i --")
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteByte(' ')
			b.WriteString(shellQuote(k + "=" + env[k]))
		}
	}
	for _, a := range argv {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String(), nil
}
