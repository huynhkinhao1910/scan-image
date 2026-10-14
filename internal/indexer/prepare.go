package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/isuccesscorp/scan-image/internal/search"
)

// target is one root a scan covers. Mounted is false for a share that could
// not be mounted: its root is kept from the old index and never listed.
type target struct {
	Key, Path string
	Mounted   bool
}

// plan is what prepare decided: which roots to scan, or that the host is
// unreachable and the scan must be skipped.
type plan struct {
	targets []target
	offline bool
	// warn holds the shares that could not be mounted.
	warn error
	// unmounted lists the keys of those shares, whether or not they have old
	// data: none of them was examined.
	unmounted []string
}

// mountedTimeout bounds the lookup of mounted volumes.
var mountedTimeout = 10 * time.Second

// prepare turns the session into scan roots. For a server source it checks
// the host first and mounts nothing, so no dialog appears when the VPN is off.
func (s *Service) prepare(ctx context.Context, old *search.Index, background bool) (plan, error) {
	if s.sess.Mode != ModeServer {
		abs, err := filepath.Abs(s.sess.Source)
		if err != nil {
			return plan{}, fmt.Errorf("resolve %s: %w", s.sess.Source, err)
		}
		return plan{targets: []target{{Key: abs, Path: abs, Mounted: true}}}, nil
	}
	if !s.opt.Reachable(ctx, s.sess.Host) {
		return plan{offline: true}, nil
	}
	mctx, cancel := context.WithTimeout(ctx, mountedTimeout)
	mounts, err := s.opt.Net.Mounted(mctx, s.sess.Host)
	cancel()
	if err != nil {
		return plan{}, fmt.Errorf("list mounts: %w", err)
	}
	var p plan
	var warns []error
	oldPaths := rootPaths(old)
	for _, sh := range s.sess.Shares {
		path := ""
		for _, m := range mounts {
			if strings.EqualFold(m.Share, sh.Share) {
				path = m.Path
				break
			}
		}
		if path != "" {
			s.clearMountFailure(sh.Key)
			p.targets = append(p.targets, target{Key: sh.Key, Path: path, Mounted: true})
			continue
		}
		path, err = s.mount(ctx, sh, background)
		if err == nil {
			p.targets = append(p.targets, target{Key: sh.Key, Path: path, Mounted: true})
			continue
		}
		warns = append(warns, err)
		p.unmounted = append(p.unmounted, sh.Key)
		// A share without old data has nothing to keep, so it gets no root.
		if kept, ok := oldPaths[sh.Key]; ok {
			p.targets = append(p.targets, target{Key: sh.Key, Path: kept})
		}
	}
	p.warn = errors.Join(warns...)
	return p, nil
}

// mount mounts one share. A background scan does not retry a share whose
// mount already failed, so a password dialog does not reappear every
// interval; a user-initiated scan always tries.
func (s *Service) mount(ctx context.Context, sh SessionShare, background bool) (string, error) {
	s.mu.Lock()
	_, failed := s.mountFailed[sh.Key]
	s.mu.Unlock()
	if background && failed {
		return "", fmt.Errorf("share %s: not mounted, background scans do not retry", sh.Share)
	}
	path, err := s.opt.Net.MountShare(ctx, s.sess.User, s.sess.Host, sh.Share)
	if err != nil {
		s.mu.Lock()
		s.mountFailed[sh.Key] = struct{}{}
		s.mu.Unlock()
		return "", fmt.Errorf("share %s: %w", sh.Share, err)
	}
	s.clearMountFailure(sh.Key)
	return path, nil
}

func (s *Service) clearMountFailure(key string) {
	s.mu.Lock()
	delete(s.mountFailed, key)
	s.mu.Unlock()
}

func rootPaths(ix *search.Index) map[string]string {
	m := map[string]string{}
	if ix == nil {
		return m
	}
	for _, r := range ix.RootList() {
		m[r.Key] = r.Path
	}
	return m
}
