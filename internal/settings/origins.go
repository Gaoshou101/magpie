package settings

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/lazyre"
)

// CleanOrigins is the web origins a user typed for CORSOrigins, as a
// browser's or WebView's Origin header spells them: scheme://host[:port],
// lower-cased, with no path and an http(s) scheme's default port left out.
// http and https are origins, so is a browser extension's page by its id
// (#1051, SkyAerope: an extension without host permissions is held to CORS
// like a page) as extensionIDs spells them, and so is a desktop or mobile
// app's custom WebView scheme (#5415, PapainTea: MarginNote's WKWebView sends
// mnpanel://panel, another WebView app canvas://app); "*" is refused, since it
// would let every web page or extension the user has spend their subscriptions.
func CleanOrigins(in []string) ([]string, error) {
	var out []string
	for _, raw := range in {
		o, err := cleanOrigin(raw)
		if err != nil {
			return nil, err
		}
		if o != "" && !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	return out, nil
}

func notOriginErr(raw string) error {
	return fmt.Errorf("%q is not an origin: write it as http://localhost:3000, https://app.example.com, chrome-extension://<its ID>, or an app's custom scheme such as mnpanel://panel", raw)
}

func cleanOrigin(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.Contains(raw, "*") {
		return "", fmt.Errorf("%q: name each origin; a wildcard would let every web page use magpie", raw)
	}
	if o, ok, err := extensionOrigin(raw); ok {
		return o, err
	}
	if !strings.Contains(raw, "://") {
		return "", notOriginErr(raw)
	}
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", notOriginErr(raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return customSchemeOrigin(raw, scheme, u.Host)
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return scheme + "://" + host, nil
}

var (
	customSchemeRE = lazyre.New(`^[a-z][a-z0-9+.-]+$`)
	customHostRE   = lazyre.New(`^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$`)
	nonAppSchemes  = []string{
		"about", "blob", "data", "file", "ftp", "ftps", "jar",
		"javascript", "mailto", "tel", "urn", "vbscript", "view-source", "ws", "wss",
	}
)

// customSchemeOrigin cleans an app WebView's custom scheme://host[:port]
// origin (#5415, e.g. mnpanel://panel or canvas://app), refusing browser
// built-in schemes, single-letter drive names, and unrecognised *-extension
// schemes.
func customSchemeOrigin(raw, scheme, rawHost string) (string, error) {
	if !customSchemeRE.MatchString(scheme) || slices.Contains(nonAppSchemes, scheme) || strings.HasSuffix(scheme, "-extension") {
		return "", notOriginErr(raw)
	}
	host := rawHost
	port := ""
	if strings.Contains(rawHost, ":") {
		h, p, err := net.SplitHostPort(rawHost)
		if err != nil || p == "" {
			return "", notOriginErr(raw)
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return "", notOriginErr(raw)
		}
		host, port = h, p
	}
	host = strings.ToLower(host)
	if !customHostRE.MatchString(host) || strings.Contains(host, "..") {
		return "", notOriginErr(raw)
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port), nil
	}
	return scheme + "://" + host, nil
}

// extensionIDs is the host each browser's extension origin has, as it
// sends it in Origin for a fetch from the extension's own pages, popup or
// service worker (a content script's fetch carries the page's origin):
// Chromium's chrome-extension://<id>, the id 32 letters a–p; Firefox's
// moz-extension://<uuid>, its per-install Internal UUID; Safari's
// safari-web-extension://<uuid>, which Safari picks anew on each launch.
var extensionIDs = map[string]*lazyre.RE{
	"chrome-extension":     lazyre.New(`^[a-p]{32}$`),
	"moz-extension":        lazyre.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
	"safari-web-extension": lazyre.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`),
}

// extensionOrigin is raw as an extension's origin, lower-cased, with a
// trailing "/" let go; ok says raw names an extension scheme at all, and
// then err says when its id isn't one that browser gives.
func extensionOrigin(raw string) (o string, ok bool, err error) {
	scheme, rest, found := strings.Cut(strings.ToLower(raw), "://")
	id := extensionIDs[scheme]
	if !found || id == nil {
		return "", false, nil
	}
	rest = strings.TrimSuffix(rest, "/")
	if !id.MatchString(rest) {
		return "", true, fmt.Errorf("%q is not an extension's origin: write it as %s://<its ID>, the ID as the browser's extensions page shows it", raw, scheme)
	}
	return scheme + "://" + rest, true, nil
}
