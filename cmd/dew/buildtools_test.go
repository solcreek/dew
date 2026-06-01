//go:build darwin

package main

import "testing"

func TestSummariseApkFailure(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"empty output",
			"",
			"apk install failed (no output)",
		},
		{
			"DNS error",
			"fetch https://dl-cdn.alpinelinux.org/alpine/v3.21/main/x86_64/APKINDEX.tar.gz: temporary failure in name resolution\nWARNING: opening from cache https://...: No such file or directory",
			"apk install failed — DNS/network unreachable",
		},
		{
			"could-not-resolve",
			"fetch https://dl-cdn.alpinelinux.org: Could not resolve host: dl-cdn.alpinelinux.org",
			"apk install failed — DNS/network unreachable",
		},
		{
			"unknown package",
			"ERROR: unable to select packages:\n  build-base (no such package):\n    required by: world[build-base]",
			"apk install failed — package name or repo error",
		},
		{
			"permission denied",
			"ERROR: Permission denied opening apkdb",
			"apk install failed — permission denied (unexpected; report this)",
		},
		{
			"generic — last meaningful line",
			"fetching APKINDEX\nsome warning\nERROR: post-install hook failed\n",
			"apk install failed — ERROR: post-install hook failed",
		},
		{
			"single line",
			"network unreachable",
			"apk install failed — network unreachable",
		},
		{
			"long last line gets truncated",
			"\n" + repeatChar("x", 200),
			"apk install failed — " + repeatChar("x", 97) + "...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summariseApkFailure(tc.in)
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func repeatChar(c string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += c
	}
	return out
}
