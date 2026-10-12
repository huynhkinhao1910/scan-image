// Package netshare finds and mounts the SMB shares of a server that the
// logged-in account can reach, using the macOS tools (mount, smbutil,
// osascript) and the credentials Finder saved in the Keychain.
package netshare

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner executes a command and returns its combined output. It is a seam
// so parsing can be tested without macOS.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Exec runs real commands.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Mount is one mounted network volume.
type Mount struct {
	User  string
	Host  string
	Share string
	Path  string
	FS    string
}

// Client talks to one host.
type Client struct {
	Run Runner
}

// New returns a client that runs real commands.
func New() *Client { return &Client{Run: Exec} }

// Mounted lists network volumes, optionally only those of host.
func (c *Client) Mounted(ctx context.Context, host string) ([]Mount, error) {
	out, err := c.Run(ctx, "mount")
	if err != nil {
		return nil, fmt.Errorf("mount: %w: %s", err, out)
	}
	var res []Mount
	for _, m := range ParseMount(string(out)) {
		if host == "" || strings.EqualFold(m.Host, host) {
			res = append(res, m)
		}
	}
	return res, nil
}

var mountLine = regexp.MustCompile(`^//(?:([^@/]+)@)?([^/]+)/(.+?) on (/.+) \(([a-z]+)[,)]`)

// ParseMount parses the output of `mount` into network volumes.
func ParseMount(out string) []Mount {
	var res []Mount
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		m := mountLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		switch m[5] {
		case "smbfs", "afpfs", "nfs", "webdav":
		default:
			continue
		}
		share, err := url.PathUnescape(m[3])
		if err != nil {
			share = m[3]
		}
		user, _ := url.PathUnescape(m[1])
		res = append(res, Mount{User: user, Host: m[2], Share: share, Path: m[4], FS: m[5]})
	}
	return res
}

// Shares lists the disk shares host offers to user, without admin shares
// such as C$ or IPC$. smbutil reads the password from the Keychain when
// Finder saved it; otherwise it fails and the caller falls back to the
// shares already mounted.
func (c *Client) Shares(ctx context.Context, user, host string) ([]string, error) {
	target := "//" + host
	if user != "" {
		target = "//" + url.PathEscape(user) + "@" + host
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := c.Run(ctx, "smbutil", "view", target)
	if err != nil {
		return nil, fmt.Errorf("smbutil view %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return ParseShares(string(out)), nil
}

var shareLine = regexp.MustCompile(`^(.*\S)\s+(Disk|Pipe|Printer|Device)(\s|$)`)

// ParseShares parses the table `smbutil view` prints.
func ParseShares(out string) []string {
	var res []string
	inTable := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if strings.HasPrefix(line, "---") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		m := shareLine.FindStringSubmatch(line)
		if m == nil || m[2] != "Disk" || strings.HasSuffix(m[1], "$") {
			continue
		}
		res = append(res, m[1])
	}
	return res
}

// MountShare mounts smb://user@host/share through Finder, which uses the
// Keychain, and returns where it is mounted.
func (c *Client) MountShare(ctx context.Context, user, host, share string) (string, error) {
	u := "smb://"
	if user != "" {
		u += url.PathEscape(user) + "@"
	}
	u += host + "/" + url.PathEscape(share)
	script := fmt.Sprintf(`mount volume %q`, u)
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if out, err := c.Run(ctx, "osascript", "-e", script); err != nil {
		return "", fmt.Errorf("mount %s: %w: %s", u, err, strings.TrimSpace(string(out)))
	}
	ms, err := c.Mounted(ctx, host)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if strings.EqualFold(m.Share, share) {
			return m.Path, nil
		}
	}
	return "", fmt.Errorf("mounted %s but cannot find its mount point", u)
}

// Servers lists the hosts that have network volumes mounted, as
// suggestions for the connect screen.
func (c *Client) Servers(ctx context.Context) ([]Mount, error) {
	ms, err := c.Mounted(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Mount
	for _, m := range ms {
		k := strings.ToLower(m.Host)
		if !seen[k] {
			seen[k] = true
			out = append(out, m)
		}
	}
	return out, nil
}

// Share is one share of a server, with where it is mounted ("" if not).
type Share struct {
	Name string
	Path string
}

// Discovery is what Discover found on a server.
type Discovery struct {
	User   string
	Shares []Share
	// ListErr is set when the server's share list could not be read (no
	// password in the Keychain, say); Shares then holds only those
	// already mounted.
	ListErr error
}

// Discover lists the shares of host the account can reach, marking the
// ones already mounted. An empty user takes the one of an existing mount.
func (c *Client) Discover(ctx context.Context, host, user string) (Discovery, error) {
	mounted, err := c.Mounted(ctx, host)
	if err != nil {
		return Discovery{}, err
	}
	have := map[string]string{}
	for _, m := range mounted {
		have[strings.ToLower(m.Share)] = m.Path
		if user == "" {
			user = m.User
		}
	}
	d := Discovery{User: user}
	names, err := c.Shares(ctx, user, host)
	if err != nil {
		d.ListErr = err
		for _, m := range mounted {
			d.Shares = append(d.Shares, Share{Name: m.Share, Path: m.Path})
		}
		return d, nil
	}
	for _, n := range names {
		d.Shares = append(d.Shares, Share{Name: n, Path: have[strings.ToLower(n)]})
	}
	return d, nil
}

// Ensure mounts the shares that are not mounted yet and returns the mount
// points of all of them, with the errors of those that failed.
func (c *Client) Ensure(ctx context.Context, host, user string, shares []Share, progress func(string)) ([]string, map[string]error) {
	var roots []string
	failed := map[string]error{}
	for _, s := range shares {
		if s.Path != "" {
			roots = append(roots, s.Path)
			continue
		}
		if progress != nil {
			progress("Đang mount " + s.Name + "…")
		}
		p, err := c.MountShare(ctx, user, host, s.Name)
		if err != nil {
			failed[s.Name] = err
			continue
		}
		roots = append(roots, p)
	}
	return roots, failed
}
