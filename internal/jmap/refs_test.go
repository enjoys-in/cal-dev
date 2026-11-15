package jmap

import (
	"encoding/json"
	"testing"
	"time"
)

func TestResultReferencesResolveIdsFromEarlierResponse(t *testing.T) {
	prev := []interface{}{
		[]interface{}{"CalendarEvent/query", map[string]interface{}{"ids": []string{"a", "b"}}, "q"},
		[]interface{}{"CalendarEvent/get", map[string]interface{}{"list": []map[string]interface{}{{"id": "x"}, {"id": "y"}}}, "g"},
	}
	for path, callID := range map[string]string{"/ids": "q", "/list/*/id": "g"} {
		name := map[string]string{"q": "CalendarEvent/query", "g": "CalendarEvent/get"}[callID]
		raw, _ := json.Marshal(map[string]interface{}{"#ids": map[string]string{"resultOf": callID, "name": name, "path": path}})
		call := &JMAPMethodCall{MethodName: "CalendarEvent/get", Args: raw, CallID: "c"}
		if err := resolveResultReferences(call, prev); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var args calendarEventGetArgs
		if err := json.Unmarshal(call.Args, &args); err != nil || len(args.IDs) != 2 {
			t.Fatalf("%s: ids not resolved: %s", path, call.Args)
		}
	}

	raw, _ := json.Marshal(map[string]interface{}{"#ids": map[string]string{"resultOf": "nope", "name": "X", "path": "/ids"}})
	if err := resolveResultReferences(&JMAPMethodCall{Args: raw}, prev); err == nil {
		t.Fatal("a reference to a missing response must fail")
	}
}

func TestLocalDateTimeStartUsesTimeZone(t *testing.T) {
	evt, err := jmapToEvent(map[string]interface{}{"start": "2026-11-20T09:00:00", "timeZone": "America/New_York"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC); !evt.DTStart.Equal(want) {
		t.Fatalf("start = %v, want %v", evt.DTStart.UTC(), want)
	}
	if _, err := jmapToEvent(map[string]interface{}{"start": "tomorrow"}, 1, 1); err == nil {
		t.Fatal("an unparseable start must be rejected")
	}
	if _, err := jmapToEvent(map[string]interface{}{"title": "no start"}, 1, 1); err == nil {
		t.Fatal("a missing start must be rejected")
	}
}
