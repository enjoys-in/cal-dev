package caldav

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/enjoys-in/cal-dev/internal/xmlutil"
)

func TestExtractEventUIDDecodesOnce(t *testing.T) {
	cases := map[string]string{
		"/dav/1/2/calendars/c/abc.ics":                       "abc",
		"/dav/1/2/calendars/c/a%20b.ics":                     "a b",
		"/dav/1/2/calendars/c/a%2520b.ics":                   "a%20b", // decoded once, not twice
		"https://h.example/dav/1/2/calendars/c/x%40y.ics":    "x@y",
		"https://h.example/dav/1/2/calendars/c/q%3Fz%23.ics": "q?z#",
	}
	for in, want := range cases {
		if got := extractEventUID(in); got != want {
			t.Errorf("extractEventUID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEtagMatches(t *testing.T) {
	if !etagMatches("*", `"a"`) || !etagMatches(`"x", "a"`, `"a"`) || etagMatches(`"x","y"`, `"a"`) {
		t.Error("etagMatches mismatch")
	}
}

func TestXMLEscapeStrStripsInvalidChars(t *testing.T) {
	got := xmlEscapeStr("a\x00b\x0bc\td<&>\"￾")
	if got != "abc\td&lt;&amp;&gt;&quot;" {
		t.Errorf("xmlEscapeStr = %q", got)
	}
}

func TestSelectProps(t *testing.T) {
	resp := xmlutil.DAVResponse{Href: "/x", PropStats: []xmlutil.PropStat{{
		Props:  map[string]string{"getetag": "<D:getetag>e</D:getetag>", "displayname": "<D:displayname>n</D:displayname>"},
		Status: 200,
	}}}
	got := selectProps(resp, []xml.Name{{Space: "DAV:", Local: "getetag"}, {Space: "urn:x", Local: "nope"}})
	if len(got.PropStats) != 2 || got.PropStats[0].Status != 200 || len(got.PropStats[0].Props) != 1 ||
		got.PropStats[1].Status != 404 || !strings.Contains(got.PropStats[1].Props["urn:x nope"], `<nope xmlns="urn:x"/>`) {
		t.Errorf("selectProps = %+v", got.PropStats)
	}
	if all := selectProps(resp, nil); len(all.PropStats[0].Props) != 2 {
		t.Error("allprop must keep every property")
	}
}

func TestLockPathNormalizes(t *testing.T) {
	if lockPath("/dav/1/2/calendars/c/a%20b.ics") != lockPath("/dav/1/2/calendars/c/a b.ics") {
		t.Error("encoded and decoded paths must share one lock key")
	}
}
