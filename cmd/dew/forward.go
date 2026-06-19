//go:build darwin

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/solcreek/dew/internal/daemon"
	"github.com/solcreek/dew/pkg/dewerr"
)

// cmdForward dispatches `dew forward {add,remove,list}`. All three
// reach the running daemon over its Unix socket so the host TCP
// listener gets spawned (or torn down) inside the dew start process
// where the vsock-to-guest proxy already lives. Adding a forward
// without restarting the VM is the load-bearing UX win — `grove
// install` calls this immediately after an app comes up.
func cmdForward(args []string) error {
	args, err := popNameFlag(args)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage, "usage: dew forward add|remove|list ...")
	}
	switch args[0] {
	case "add":
		return cmdForwardAdd(args[1:])
	case "remove", "rm":
		return cmdForwardRemove(args[1:])
	case "list", "ls":
		return cmdForwardList(args[1:])
	default:
		return dewerr.Newf(dewerr.CodeUsage, "dew forward: unknown subcommand %q", args[0])
	}
}

func cmdForwardAdd(args []string) error {
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage, "usage: dew forward add <hostPort:guestPort>")
	}
	host, guest, err := parseForwardPair(args[0])
	if err != nil {
		return err
	}

	resp, err := sendDaemonRequest(daemon.ExecRequest{
		Kind:      "forward-add",
		HostPort:  host,
		GuestPort: guest,
	})
	if err != nil {
		return err
	}
	if errStr, _ := resp["error"].(string); errStr != "" {
		return dewerr.Newf(dewerr.CodeGeneric, "forward add: %s", errStr)
	}

	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           resp,
		})
		return nil
	}
	// The daemon may have bound a different port if the requested one was
	// busy — report what was actually bound.
	actual := host
	if hp, ok := resp["host_port"].(float64); ok {
		actual = int(hp)
	}
	if actual != host {
		fmt.Printf("dew: 127.0.0.1:%d was busy → forwarding 127.0.0.1:%d → guest:%d\n", host, actual, guest)
	} else {
		fmt.Printf("dew: forwarding 127.0.0.1:%d → guest:%d\n", actual, guest)
	}
	return nil
}

func cmdForwardRemove(args []string) error {
	if len(args) == 0 {
		return dewerr.New(dewerr.CodeUsage, "usage: dew forward remove <hostPort:guestPort>")
	}
	host, guest, err := parseForwardPair(args[0])
	if err != nil {
		return err
	}
	resp, err := sendDaemonRequest(daemon.ExecRequest{
		Kind:      "forward-remove",
		HostPort:  host,
		GuestPort: guest,
	})
	if err != nil {
		return err
	}
	if errStr, _ := resp["error"].(string); errStr != "" {
		return dewerr.Newf(dewerr.CodeGeneric, "forward remove: %s", errStr)
	}
	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           resp,
		})
		return nil
	}
	fmt.Printf("dew: forward removed (host:%d ↛ guest:%d)\n", host, guest)
	return nil
}

func cmdForwardList(args []string) error {
	_ = args
	resp, err := sendDaemonRequest(daemon.ExecRequest{Kind: "forward-list"})
	if err != nil {
		return err
	}
	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"ok":             true,
			"schema_version": schemaVersion,
			"data":           resp,
		})
		return nil
	}

	raw, _ := resp["forwards"].([]any)
	type pair struct{ host, guest int }
	pairs := make([]pair, 0, len(raw))
	for _, item := range raw {
		obj, _ := item.(map[string]any)
		h, _ := obj["HostPort"].(float64)
		g, _ := obj["GuestPort"].(float64)
		pairs = append(pairs, pair{int(h), int(g)})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].host < pairs[j].host })

	if len(pairs) == 0 {
		fmt.Println("dew: no active forwards")
		return nil
	}
	fmt.Println("Active forwards:")
	for _, p := range pairs {
		fmt.Printf("  127.0.0.1:%d → guest:%d\n", p.host, p.guest)
	}
	return nil
}

// sendDaemonRequest opens the daemon socket, posts the request, and
// reads back a single JSON response. Returns CodeConflict when no
// VM is running so callers see the same "no running VM" hint shape
// as `dew exec`.
func sendDaemonRequest(req daemon.ExecRequest) (map[string]any, error) {
	sockPath := daemon.SocketPath(flagVMName)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, dewerr.Wrapf(err, dewerr.CodeConflict, "no running VM (socket %s)", sockPath)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, dewerr.Wrap(err, dewerr.CodeNetwork, "send")
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, dewerr.Wrap(err, dewerr.CodeNetwork, "recv")
	}
	return resp, nil
}

// parseForwardPair accepts "host:guest" (matches the --forward flag
// shape on `dew start`) so users don't have to learn two formats.
func parseForwardPair(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, dewerr.Newf(dewerr.CodeUsage, "forward: expected hostPort:guestPort, got %q", s)
	}
	host, err1 := strconv.Atoi(parts[0])
	guest, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || host < 1 || guest < 1 {
		return 0, 0, dewerr.Newf(dewerr.CodeUsage, "forward: invalid ports %q", s)
	}
	return host, guest, nil
}
