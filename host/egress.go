package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// What a manifest may declare, and where a declared request may actually go
// (TENG-3103, threat G9). The manifest format checks spelling. These checks
// ask whether a declaration is one a person could mean.

// declarationProblems names declarations the runner will not honor.
func declarationProblems(m *manifest) []string {
	var p []string
	for _, f := range m.Files {
		clean := filepath.Clean(f.Path)
		switch {
		case clean == string(filepath.Separator) || clean == filepath.VolumeName(clean)+string(filepath.Separator):
			p = append(p, fmt.Sprintf("files: %q is the root of the file system; name the directory the primitive needs", f.Path))
		case clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)):
			p = append(p, fmt.Sprintf("files: %q leaves the directory the primitive runs in", f.Path))
		}
	}
	for _, f := range m.Fetch {
		host := originHost(f.Origin)
		if host == "" {
			continue
		}
		ip := net.ParseIP(host)
		switch {
		case strings.HasPrefix(f.Origin, "http://") && !isLocalName(host, ip):
			p = append(p, fmt.Sprintf("fetch: %q is not https; plain http is allowed only for localhost", f.Origin))
		case ip != nil && alwaysBlocked(ip):
			p = append(p, fmt.Sprintf("fetch: %q is an address no primitive may reach (link-local, unspecified or multicast: cloud metadata services live there)", f.Origin))
		}
	}
	return p
}

// originHost is the host of a declared origin, without a port.
func originHost(origin string) string {
	u, err := url.Parse(strings.Replace(origin, "://*.", "://wildcard-label.", 1))
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// isLocalName reports whether a host is this machine, spelled out.
func isLocalName(host string, ip net.IP) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return ip != nil && ip.IsLoopback()
}

var metadataV6 = net.ParseIP("fd00:ec2::254")

// alwaysBlocked is an address a primitive never reaches, however it is named:
// link-local (169.254.0.0/16 holds cloud metadata services), unspecified,
// multicast.
func alwaysBlocked(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(metadataV6)
}

// reachable decides whether a connection to ip may be made for a request to
// host. Loopback and private addresses are for a host that is itself spelled
// as one, or localhost: a name in DNS that resolves to one is how a declared
// origin would be turned on the machine's own services.
func reachable(host string, ip net.IP) error {
	if alwaysBlocked(ip) {
		return fmt.Errorf("%s resolves to %s, an address no primitive may reach", host, ip)
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		if literal := net.ParseIP(host); literal != nil || strings.EqualFold(host, "localhost") {
			return nil
		}
		return fmt.Errorf("%s resolves to %s, an address on this machine or its network; declare it as an address if that is meant", host, ip)
	}
	return nil
}

// guardedTransport dials only addresses reachable allows. It resolves the name
// itself and dials the address it checked, so what was checked is what is
// connected to.
func guardedTransport() *http.Transport {
	d := &net.Dialer{Timeout: 30 * time.Second}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil // a proxy would be reached instead of the origin
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, a := range ips {
			if err := reachable(host, a.IP); err != nil {
				last = err
				continue
			}
			c, err := d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		if last == nil {
			last = fmt.Errorf("%s has no address", host)
		}
		return nil, last
	}
	return t
}
