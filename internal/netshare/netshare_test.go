package netshare

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const mountOut = `/dev/disk3s1s1 on / (apfs, sealed, local, read-only, journaled)
//sunny@192.168.101.251/Design%20Team on /Volumes/Design Team (smbfs, nodev, nosuid, mounted by sunny)
//sunny@192.168.101.251/Photos on /Volumes/Photos (smbfs, nodev, nosuid, mounted by sunny)
//guest@10.0.0.9/Public on /Volumes/Public (smbfs, nodev, nosuid, mounted by sunny)
map auto_home on /System/Volumes/Data/home (autofs, automounted, nobrowse)
`

const viewOut = `Share                                           Type    Comments
-------------------------------
Design Team                                     Disk
Photos                                          Disk    Ảnh sản phẩm
Archive                                         Disk
IPC$                                            Pipe    IPC Service
ADMIN$                                          Disk    Remote Admin
HP Printer                                      Printer
5 shares listed
`

func TestParseMount(t *testing.T) {
	ms := ParseMount(mountOut)
	if len(ms) != 3 {
		t.Fatalf("got %d mounts: %+v", len(ms), ms)
	}
	want := Mount{User: "sunny", Host: "192.168.101.251", Share: "Design Team", Path: "/Volumes/Design Team", FS: "smbfs"}
	if ms[0] != want {
		t.Errorf("got %+v, want %+v", ms[0], want)
	}
}

func TestParseShares(t *testing.T) {
	got := ParseShares(viewOut)
	want := []string{"Design Team", "Photos", "Archive"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

type fake struct {
	mounted string
	view    string
	viewErr error
	calls   []string
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "mount":
		return []byte(f.mounted), nil
	case "smbutil":
		return []byte(f.view), f.viewErr
	case "osascript":
		f.mounted += "//sunny@192.168.101.251/Archive on /Volumes/Archive (smbfs, nodev)\n"
		return nil, nil
	}
	return nil, errors.New("unexpected " + name)
}

func TestDiscoverAndEnsure(t *testing.T) {
	f := &fake{mounted: mountOut, view: viewOut}
	c := &Client{Run: f.run}
	host := "192.168.101.251"
	d, err := c.Discover(context.Background(), host, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []Share{{"Design Team", "/Volumes/Design Team"}, {"Photos", "/Volumes/Photos"}, {"Archive", ""}}
	if d.User != "sunny" || !reflect.DeepEqual(d.Shares, want) {
		t.Fatalf("discovery %+v", d)
	}
	roots, failed := c.Ensure(context.Background(), host, d.User, d.Shares, nil)
	if len(failed) != 0 || !reflect.DeepEqual(roots, []string{"/Volumes/Design Team", "/Volumes/Photos", "/Volumes/Archive"}) {
		t.Errorf("roots %q failed %v", roots, failed)
	}
}

func TestDiscoverFallsBackToMounted(t *testing.T) {
	f := &fake{mounted: mountOut, viewErr: errors.New("auth")}
	c := &Client{Run: f.run}
	d, err := c.Discover(context.Background(), "192.168.101.251", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.ListErr == nil || len(d.Shares) != 2 {
		t.Errorf("discovery %+v", d)
	}
}

func TestServers(t *testing.T) {
	c := &Client{Run: (&fake{mounted: mountOut}).run}
	ss, err := c.Servers(context.Background())
	if err != nil || len(ss) != 2 || ss[0].Host != "192.168.101.251" || ss[1].Host != "10.0.0.9" {
		t.Errorf("servers %+v, %v", ss, err)
	}
}
