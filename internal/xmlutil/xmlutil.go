package xmlutil

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Common WebDAV/CalDAV XML namespaces.
const (
	NSDAVUrl    = "DAV:"
	NSCalDAVUrl = "urn:ietf:params:xml:ns:caldav"
	NSCSUrl     = "http://calendarserver.org/ns/"
	NSAppleUrl  = "http://apple.com/ns/ical/"
)

// ============================================================
// Propfind Request Parsing
// ============================================================

// PropfindRequest represents a PROPFIND XML body.
type PropfindRequest struct {
	XMLName  xml.Name  `xml:"DAV: propfind"`
	AllProp  *struct{} `xml:"DAV: allprop,omitempty"`
	PropName *struct{} `xml:"DAV: propname,omitempty"`
	Prop     *Prop     `xml:"DAV: prop,omitempty"`
}

// Prop contains a list of requested properties.
type Prop struct {
	XMLName xml.Name
	Props   []xml.Name `xml:",any"`
}

// ParsePropfindRequest parses a PROPFIND request body.
func ParsePropfindRequest(body io.Reader) (*PropfindRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	// Empty body means allprop per RFC 4918
	if len(strings.TrimSpace(string(data))) == 0 {
		return &PropfindRequest{AllProp: &struct{}{}}, nil
	}

	// Sanitize to prevent XML entity expansion attacks
	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req PropfindRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal propfind: %w", err)
	}

	return &req, nil
}

// ============================================================
// Calendar Query (REPORT) Request Parsing
// ============================================================

// CalendarQueryRequest represents a calendar-query REPORT body.
type CalendarQueryRequest struct {
	XMLName xml.Name    `xml:"urn:ietf:params:xml:ns:caldav calendar-query"`
	Prop    *Prop       `xml:"DAV: prop,omitempty"`
	Filter  *CompFilter `xml:"urn:ietf:params:xml:ns:caldav filter>comp-filter"`
}

// CompFilter represents a component filter in calendar-query.
type CompFilter struct {
	Name       string      `xml:"name,attr"`
	TimeRange  *TimeRange  `xml:"urn:ietf:params:xml:ns:caldav time-range,omitempty"`
	PropFilter *PropFilter `xml:"urn:ietf:params:xml:ns:caldav prop-filter,omitempty"`
	CompFilter *CompFilter `xml:"urn:ietf:params:xml:ns:caldav comp-filter,omitempty"`
}

// TimeRange represents a time-range filter.
type TimeRange struct {
	Start string `xml:"start,attr,omitempty"`
	End   string `xml:"end,attr,omitempty"`
}

// PropFilter represents a property filter in calendar-query.
type PropFilter struct {
	Name      string     `xml:"name,attr"`
	TextMatch *TextMatch `xml:"urn:ietf:params:xml:ns:caldav text-match,omitempty"`
}

// TextMatch represents a text match filter.
type TextMatch struct {
	Collation string `xml:"collation,attr,omitempty"`
	NegateArg string `xml:"negate-condition,attr,omitempty"`
	MatchType string `xml:"match-type,attr,omitempty"`
	Value     string `xml:",chardata"`
}

// CalendarMultigetRequest represents a calendar-multiget REPORT body.
type CalendarMultigetRequest struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav calendar-multiget"`
	Prop    *Prop    `xml:"DAV: prop,omitempty"`
	Hrefs   []string `xml:"DAV: href"`
}

// SyncCollectionRequest represents a sync-collection REPORT body.
type SyncCollectionRequest struct {
	XMLName   xml.Name `xml:"DAV: sync-collection"`
	SyncToken string   `xml:"DAV: sync-token"`
	SyncLevel string   `xml:"DAV: sync-level"`
	Prop      *Prop    `xml:"DAV: prop,omitempty"`
}

// ParseCalendarQuery parses a calendar-query REPORT body.
func ParseCalendarQuery(body io.Reader) (*CalendarQueryRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req CalendarQueryRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal calendar-query: %w", err)
	}

	return &req, nil
}

// ParseCalendarMultiget parses a calendar-multiget REPORT body.
func ParseCalendarMultiget(body io.Reader) (*CalendarMultigetRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req CalendarMultigetRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal calendar-multiget: %w", err)
	}

	return &req, nil
}

// ParseSyncCollection parses a sync-collection REPORT body.
func ParseSyncCollection(body io.Reader) (*SyncCollectionRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req SyncCollectionRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal sync-collection: %w", err)
	}

	return &req, nil
}

// ============================================================
// Proppatch Parsing
// ============================================================

// ProppatchRequest represents a PROPPATCH XML body.
type ProppatchRequest struct {
	XMLName xml.Name           `xml:"DAV: propertyupdate"`
	Set     []ProppatchSetProp `xml:"DAV: set"`
	Remove  []ProppatchSetProp `xml:"DAV: remove"`
}

// ProppatchSetProp is a set/remove element.
type ProppatchSetProp struct {
	Prop ProppatchPropValues `xml:"DAV: prop"`
}

// ProppatchPropValues holds raw property values for set/remove.
type ProppatchPropValues struct {
	InnerXML string `xml:",innerxml"`
}

// ParseProppatch parses a PROPPATCH request body.
func ParseProppatch(body io.Reader) (*ProppatchRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req ProppatchRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal proppatch: %w", err)
	}

	return &req, nil
}

// ============================================================
// MKCALENDAR Parsing
// ============================================================

// MkcalendarRequest represents a MKCALENDAR XML body.
type MkcalendarRequest struct {
	XMLName xml.Name `xml:"urn:ietf:params:xml:ns:caldav mkcalendar"`
	Set     *struct {
		Prop MkcalendarProps `xml:"DAV: prop"`
	} `xml:"DAV: set,omitempty"`
}

// MkcalendarProps holds new calendar properties.
type MkcalendarProps struct {
	DisplayName         string `xml:"DAV: displayname,omitempty"`
	CalendarDescription string `xml:"urn:ietf:params:xml:ns:caldav calendar-description,omitempty"`
	CalendarTimezone    string `xml:"urn:ietf:params:xml:ns:caldav calendar-timezone,omitempty"`
	CalendarColor       string `xml:"http://apple.com/ns/ical/ calendar-color,omitempty"`
}

// ParseMkcalendar parses a MKCALENDAR request body.
func ParseMkcalendar(body io.Reader) (*MkcalendarRequest, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("xmlutil: read body: %w", err)
	}

	if len(strings.TrimSpace(string(data))) == 0 {
		return &MkcalendarRequest{}, nil
	}

	if err := validateXMLSafety(string(data)); err != nil {
		return nil, err
	}

	var req MkcalendarRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("xmlutil: unmarshal mkcalendar: %w", err)
	}

	return &req, nil
}

// ============================================================
// Multi-Status Response Generation
// ============================================================

// MultiStatusResponse represents a 207 Multi-Status response.
type MultiStatusResponse struct {
	Responses []DAVResponse
}

// DAVResponse represents a single response in a multistatus.
type DAVResponse struct {
	Href       string
	PropStats  []PropStat
	Status     string
	StatusCode int
}

// PropStat groups properties with their status.
type PropStat struct {
	Props  map[string]string
	Status int
}

// WriteMultiStatus generates a 207 Multi-Status XML response.
func WriteMultiStatus(w io.Writer, responses []DAVResponse) error {
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CS="http://calendarserver.org/ns/" xmlns:A="http://apple.com/ns/ical/">`)

	for _, resp := range responses {
		fmt.Fprint(w, `<D:response>`)
		fmt.Fprintf(w, `<D:href>%s</D:href>`, xmlEscape(resp.Href))

		if resp.StatusCode > 0 && len(resp.PropStats) == 0 {
			fmt.Fprintf(w, `<D:status>HTTP/1.1 %d %s</D:status>`, resp.StatusCode, statusText(resp.StatusCode))
		}

		for _, ps := range resp.PropStats {
			fmt.Fprint(w, `<D:propstat>`)
			fmt.Fprint(w, `<D:prop>`)
			for k, v := range ps.Props {
				fmt.Fprint(w, v)
				_ = k
			}
			fmt.Fprint(w, `</D:prop>`)
			fmt.Fprintf(w, `<D:status>HTTP/1.1 %d %s</D:status>`, ps.Status, statusText(ps.Status))
			fmt.Fprint(w, `</D:propstat>`)
		}

		fmt.Fprint(w, `</D:response>`)
	}

	fmt.Fprint(w, `</D:multistatus>`)
	return nil
}

// WriteSyncResponse writes a sync-collection response with a new sync token.
func WriteSyncResponse(w io.Writer, responses []DAVResponse, newSyncToken string) error {
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprint(w, `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:CS="http://calendarserver.org/ns/">`)

	for _, resp := range responses {
		fmt.Fprint(w, `<D:response>`)
		fmt.Fprintf(w, `<D:href>%s</D:href>`, xmlEscape(resp.Href))

		if resp.StatusCode > 0 && len(resp.PropStats) == 0 {
			fmt.Fprintf(w, `<D:status>HTTP/1.1 %d %s</D:status>`, resp.StatusCode, statusText(resp.StatusCode))
		}

		for _, ps := range resp.PropStats {
			fmt.Fprint(w, `<D:propstat>`)
			fmt.Fprint(w, `<D:prop>`)
			for _, v := range ps.Props {
				fmt.Fprint(w, v)
			}
			fmt.Fprint(w, `</D:prop>`)
			fmt.Fprintf(w, `<D:status>HTTP/1.1 %d %s</D:status>`, ps.Status, statusText(ps.Status))
			fmt.Fprint(w, `</D:propstat>`)
		}

		fmt.Fprint(w, `</D:response>`)
	}

	fmt.Fprintf(w, `<D:sync-token>%s</D:sync-token>`, xmlEscape(newSyncToken))
	fmt.Fprint(w, `</D:multistatus>`)
	return nil
}

// ============================================================
// Lock Response
// ============================================================

// WriteLockResponse writes a LOCK response body.
func WriteLockResponse(w io.Writer, lockToken, owner, scope, lockType, depth string, timeout int) error {
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprint(w, `<D:prop xmlns:D="DAV:">`)
	fmt.Fprint(w, `<D:lockdiscovery>`)
	fmt.Fprint(w, `<D:activelock>`)
	fmt.Fprintf(w, `<D:locktype><D:%s/></D:locktype>`, lockType)
	fmt.Fprintf(w, `<D:lockscope><D:%s/></D:lockscope>`, scope)
	fmt.Fprintf(w, `<D:depth>%s</D:depth>`, depth)
	if owner != "" {
		fmt.Fprintf(w, `<D:owner><D:href>%s</D:href></D:owner>`, xmlEscape(owner))
	}
	fmt.Fprintf(w, `<D:timeout>Second-%d</D:timeout>`, timeout)
	fmt.Fprintf(w, `<D:locktoken><D:href>urn:uuid:%s</D:href></D:locktoken>`, lockToken)
	fmt.Fprint(w, `</D:activelock>`)
	fmt.Fprint(w, `</D:lockdiscovery>`)
	fmt.Fprint(w, `</D:prop>`)
	return nil
}

// ============================================================
// Helper Functions
// ============================================================

// validateXMLSafety checks for XML entity expansion attacks.
func validateXMLSafety(data string) error {
	// Prevent XML entity expansion attacks (billion laughs, etc.)
	if strings.Contains(data, "<!ENTITY") || strings.Contains(data, "<!DOCTYPE") {
		return fmt.Errorf("xmlutil: DOCTYPE/ENTITY declarations not allowed for security")
	}
	// Size limit to prevent memory exhaustion
	if len(data) > 10*1024*1024 { // 10MB limit
		return fmt.Errorf("xmlutil: request body too large")
	}
	return nil
}

// xmlEscape escapes special XML characters.
func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 207:
		return "Multi-Status"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 412:
		return "Precondition Failed"
	case 423:
		return "Locked"
	case 424:
		return "Failed Dependency"
	case 507:
		return "Insufficient Storage"
	default:
		return "Unknown"
	}
}

// RequestedProps extracts the list of property names from a Prop.
func RequestedProps(p *Prop) []xml.Name {
	if p == nil {
		return nil
	}
	return p.Props
}

// HasProp checks if a specific property was requested.
func HasProp(props []xml.Name, space, local string) bool {
	for _, p := range props {
		if p.Space == space && p.Local == local {
			return true
		}
	}
	return false
}

// IsAllProp returns true if allprop is requested.
func IsAllProp(req *PropfindRequest) bool {
	return req.AllProp != nil
}
