package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// What a manifest may declare, and where a declared request may actually go
// The manifest format checks spelling. These checks
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
// connected to. A proxy from the environment is honored, as it is for every
// other program on the machine: the connection to the proxy is allowed, and
// the origin is checked as far as this machine can see it, because the proxy
// resolves the name where it runs (TENG-3099, found testing a proxy).
func guardedTransport() *http.Transport { return guardedTransportVia(http.ProxyFromEnvironment) }

func guardedTransportVia(proxyFor func(*http.Request) (*url.URL, error)) *http.Transport {
	d := &net.Dialer{Timeout: 30 * time.Second}
	t := http.DefaultTransport.(*http.Transport).Clone()
	var proxies sync.Map // hosts this transport was told to reach as proxies
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		pu, err := proxyFor(req)
		if err != nil || pu == nil {
			return pu, err
		}
		proxies.Store(strings.ToLower(pu.Hostname()), true)
		// The proxy will resolve the origin. Check what this machine resolves it
		// to; a name this machine cannot resolve is left to the proxy.
		host := req.URL.Hostname()
		if net.ParseIP(host) == nil {
			if ips, lerr := net.LookupIP(host); lerr == nil {
				for _, ip := range ips {
					if rerr := reachable(host, ip); rerr != nil {
						return nil, rerr
					}
				}
			}
		} else if rerr := reachable(host, net.ParseIP(host)); rerr != nil {
			return nil, rerr
		}
		return pu, nil
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if _, isProxy := proxies.Load(strings.ToLower(host)); isProxy {
			return d.DialContext(ctx, network, addr)
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

// logURL is an address as it is written to the log: without its query string
// or fragment, which is where a token or something the program read tends to
// ride. The record of the run keeps the full address.
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(not a URL)"
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u.String()
}

// forPrompt makes text a program wrote safe to show a person who is deciding
// whether to allow it: control characters, which could redraw a terminal or
// start a new line that looks like part of the question, become spaces, and
// it is cut to a length that can be read.
func forPrompt(s string) string {
	const max = 300
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			fmt.Fprintf(&b, " ... (%d more characters)", len([]rune(s))-max)
			break
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029 {
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// logCommand is a command as it is written to the log: the program and its
// first two arguments (a subcommand), and how many more there were. The rest
// is where a token or something the program read tends to ride; the record of
// the run keeps the whole command line.
func logCommand(name string, args []string) string {
	shown := args
	if len(shown) > 2 {
		shown = shown[:2]
	}
	line := strings.TrimSpace(name + " " + strings.Join(shown, " "))
	if len(args) > 2 {
		line += fmt.Sprintf(" (+%d more arguments)", len(args)-2)
	}
	return forPrompt(line)
}
