package imagecache

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jonahgcarpenter/oswald-ai/internal/media"
)

var blockedRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.Zone() != "" || ip.Is4In6() {
		return false
	}
	for _, p := range blockedRanges {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func validImageURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) || u.Opaque != "" || u.User != nil || strings.Contains(raw, "#") || u.Hostname() == "" || u.Hostname() == "." {
		return nil, errors.New("invalid image URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid image URL port")
		}
	}
	return u, nil
}

// ImportHTTPS downloads a bounded image over HTTPS with public-IP-only,
// pinned dialing. Redirects undergo independent validation and DNS resolution.
func (c *Cache) ImportHTTPS(ctx context.Context, userID, rawURL string) (string, []byte, string, error) {
	return c.importHTTPS(ctx, userID, rawURL, net.DefaultResolver.LookupNetIP, nil, nil)
}

// The optional dial and TLS configuration are test seams; production always
// dials the resolved IP and verifies TLS against the original hostname.
func (c *Cache) importHTTPS(ctx context.Context, userID, rawURL string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error), tlsConfig *tls.Config) (string, []byte, string, error) {
	if !userName.MatchString(userID) {
		return "", nil, "", errors.New("invalid user ID")
	}
	// The existing import deadline includes both download and cache publication.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	data, mime, err := downloadImage(ctx, rawURL, false, lookup, dial, tlsConfig)
	if err != nil {
		return "", nil, "", err
	}
	path, err := c.Save(ctx, userID, data, mime)
	if err != nil {
		return "", nil, "", err
	}
	return path, data, mime, nil
}

// DownloadPublicImage reads a bounded HTTP/HTTPS image without caching it.
// It pins public-IP dialing, disables proxies, and revalidates redirects.
func DownloadPublicImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	return downloadImage(ctx, rawURL, true, net.DefaultResolver.LookupNetIP, nil, nil)
}

func downloadImage(ctx context.Context, rawURL string, allowHTTP bool, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error), tlsConfig *tls.Config) ([]byte, string, error) {
	if _, err := validImageURL(rawURL, allowHTTP); err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, TLSClientConfig: tlsConfig,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	defer transport.CloseIdleConnections()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, errors.New("image hostname resolution failed")
		}
		for _, ip := range ips {
			if !publicIP(ip) {
				return nil, errors.New("image destination is not public")
			}
		}
		pinned := net.JoinHostPort(ips[0].String(), port)
		if dial != nil {
			return dial(ctx, network, pinned)
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", pinned)
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many image redirects")
		}
		// net/http otherwise forwards the previous URL as Referer, including its query.
		req.Header.Del("Referer")
		if !allowHTTP && req.URL.Scheme != "https" {
			return errors.New("image redirect requires HTTPS")
		}
		if req.URL.Scheme == "http" && via[len(via)-1].URL.Scheme == "https" {
			return errors.New("image redirect cannot downgrade HTTPS")
		}
		_, err := validImageURL(req.URL.String(), allowHTTP)
		return err
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > media.MaxOutputAttachmentBytes {
		return nil, "", errors.New("image download rejected")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, media.MaxOutputAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	mime, _, err := validate(data, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", err
	}
	return data, mime, nil
}
