package nmcache

import _ "embed"

//go:embed cache.sh
var setupScript string

//go:embed commit.sh
var commitScript string

// SetupScript returns the shell script the host runs inside the
// guest VM to set up the bind-mount and detect cache hit/miss.
// The script reads DEW_NM_KEY and DEW_NM_WANT from the environment;
// the caller is responsible for inlining them safely.
func SetupScript() string { return setupScript }

// CommitScript returns the shell script the host runs after a
// successful install to atomically commit the cache stamp.
func CommitScript() string { return commitScript }

// SetupCommand returns a single-line `env=val ... sh -c "..."` invocation
// suitable for passing to execVsockConnTimeout. The key and want_stamp
// are passed as env vars (single-quoted) rather than interpolated into
// the script body, so they cannot terminate quoting or inject commands.
func SetupCommand(key, wantStamp string) string {
	return shellEnv("DEW_NM_KEY", key) + " " +
		shellEnv("DEW_NM_WANT", wantStamp) + " " +
		"sh -c " + shellQuote(setupScript)
}

// CommitCommand returns the env-prefixed invocation for committing a
// successful install.
func CommitCommand(key string) string {
	return shellEnv("DEW_NM_KEY", key) + " " +
		"sh -c " + shellQuote(commitScript)
}

func shellEnv(name, value string) string {
	return name + "=" + shellQuote(value)
}

// shellQuote produces a single-quoted shell literal safe under POSIX
// sh. Embedded single quotes are escaped via the standard
// '...'\''...' trick.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}
