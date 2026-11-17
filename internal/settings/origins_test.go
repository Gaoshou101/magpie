package settings

import (
	"strings"
	"testing"
)

// An origin is saved as the browser sends it, whichever way the user wrote
// it, and a wildcard or anything that is not an origin is refused (#1051).
func TestCleanOrigins(t *testing.T) {
	got, err := CleanOrigins([]string{" http://LocalHost:3000/ ", "https://a.example:443", "http://[::1]:8080", "http://a.example:80", "", "http://localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://localhost:3000", "https://a.example", "http://[::1]:8080", "http://a.example"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%v, want %v", got, want)
	}
	for _, bad := range []string{"*", "https://*.example.com", "localhost:3000", "file:///x", "http://a.example/path", "http://u:p@a.example", "http://a.example?x=1", "null"} {
		if _, err := CleanOrigins([]string{bad}); err == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// A browser extension's page is listed by its origin, as the browser
// sends it (#1051, SkyAerope on Discord: chrome-extension://<id> couldn't
// be added): Chromium's id of 32 letters a–p, Firefox's and Safari's
// UUID, however the user cased it or with a trailing "/". A bare scheme,
// a wildcard, an id that browser never gives, a path or a port is refused.
func TestCleanExtensionOrigins(t *testing.T) {
	const chrome = "abcdefghijklmnopabcdefghijklmnop"
	const uuid = "e0b5d7c7-c079-484b-8825-44d261383cb6"
	got, err := CleanOrigins([]string{
		"chrome-extension://" + chrome,
		" Chrome-Extension://" + strings.ToUpper(chrome) + "/ ",
		"moz-extension://" + strings.ToUpper(uuid) + "/",
		"safari-web-extension://" + uuid,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chrome-extension://" + chrome, "moz-extension://" + uuid, "safari-web-extension://" + uuid}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%v, want %v", got, want)
	}
	for _, bad := range []string{
		"chrome-extension://", "chrome-extension:///", "chrome-extension://*", "chrome-extension://" + chrome[:31] + "*",
		"chrome-extension://" + chrome[:31],            // too short
		"chrome-extension://" + chrome + "a",           // too long
		"chrome-extension://" + chrome[:31] + "q",      // not a–p
		"chrome-extension://" + chrome + "/popup.html", // a page, not an origin
		"chrome-extension://" + chrome + ":80",
		"chrome-extension://u@" + chrome,
		"chrome-extension://" + chrome + "?x=1",
		"chrome-extension://" + uuid,     // Firefox's id under Chrome's scheme
		"moz-extension://" + chrome,      // and Chrome's under Firefox's
		"moz-extension://{" + uuid + "}", // braces are not in the origin
		"moz-extension://" + strings.ReplaceAll(uuid, "-", ""),
		"moz-extension://" + uuid[:35] + "g",
		"safari-web-extension://" + uuid + "/x",
		"moz-extension://*",
		"safari-web-extension://",
		"ms-browser-extension://" + chrome,
	} {
		if o, err := CleanOrigins([]string{bad}); err == nil {
			t.Errorf("%q taken as %v", bad, o)
		}
	}
}

// A desktop or mobile app's WebView that serves its UI from a custom URL
// scheme sends scheme://host as Origin (#5415: MarginNote's WKWebView sends
// mnpanel://panel, another WebView app canvas://app) and enforces CORS on
// fetch. Such an origin may be listed when it names an app scheme and host
// alone; browser built-in schemes (file, ws, wss, ftp, blob, data, about,
// javascript), single-letter drive names, unknown *-extension schemes, paths,
// queries, userinfo and bad ports are refused.
func TestCleanCustomSchemeOrigins(t *testing.T) {
	got, err := CleanOrigins([]string{
		"mnpanel://panel",
		" MNPANEL://Panel/ ",
		"canvas://app",
		"tauri://LocalHost/",
		"capacitor://localhost",
		"wails://wails.localhost:34115",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"mnpanel://panel",
		"canvas://app",
		"tauri://localhost",
		"capacitor://localhost",
		"wails://wails.localhost:34115",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("%v, want %v", got, want)
	}
	for _, bad := range []string{
		"mnpanel://",
		"mnpanel:///",
		"mnpanel://*",
		"mnpanel://*.panel",
		"mnpanel://panel/index.html",
		"mnpanel://panel?x=1",
		"mnpanel://panel#top",
		"mnpanel://u@panel",
		"mnpanel://panel:",
		"mnpanel://panel:0",
		"mnpanel://panel:70000",
		"mnpanel://panel:abc",
		"mnpanel://..panel",
		"file://localhost",
		"file:///Users/a/index.html",
		"ws://localhost:3000",
		"wss://app.example.com",
		"ftp://files.example.com",
		"blob://localhost",
		"data://text",
		"javascript://alert",
		"about://blank",
		"mailto://user",
		"view-source://localhost",
		"c://users",
		"edge-extension://abcdefghijklmnopabcdefghijklmnop",
	} {
		if o, err := CleanOrigins([]string{bad}); err == nil {
			t.Errorf("%q taken as %v", bad, o)
		}
	}
}
