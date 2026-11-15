package xmlutil

import (
	"bytes"
	"strings"
	"testing"
)

func TestRequestedPropsAreParsed(t *testing.T) {
	body := `<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop></D:propfind>`
	req, err := ParsePropfindRequest(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	names := RequestedProps(req.Prop)
	if !HasProp(names, NSDAVUrl, "getetag") || !HasProp(names, NSCalDAVUrl, "calendar-data") || len(names) != 2 {
		t.Errorf("RequestedProps = %v", names)
	}
}

func TestLockResponseDepthWhitelisted(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteLockResponse(&buf, "t", "", "exclusive", "write", "<x/>", 60)
	if strings.Contains(buf.String(), "<x/>") || !strings.Contains(buf.String(), "<D:depth>0</D:depth>") {
		t.Errorf("depth not whitelisted: %s", buf.String())
	}
	buf.Reset()
	_ = WriteLockResponse(&buf, "t", "", "exclusive", "write", "Infinity", 60)
	if !strings.Contains(buf.String(), "<D:depth>infinity</D:depth>") {
		t.Errorf("infinity depth lost: %s", buf.String())
	}
}
