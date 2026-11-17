package scheduling

import "testing"

func TestMeetingURL(t *testing.T) {
	cases := []struct{ location, description, want string }{
		{"Room 4 · https://meet.google.com/abc-defg-hij", "", "https://meet.google.com/abc-defg-hij"},
		{"https://us02web.zoom.us/j/123?pwd=x.", "", "https://us02web.zoom.us/j/123?pwd=x"},
		{"Room 4", "Join: https://teams.microsoft.com/l/meetup-join/abc", "https://teams.microsoft.com/l/meetup-join/abc"},
		{"Room 4", "Agenda at https://example.com/doc", ""},
		{"", "", ""},
		{"https://custom.example.org/room", "", "https://custom.example.org/room"},
	}
	for _, c := range cases {
		if got := meetingURL(c.location, c.description); got != c.want {
			t.Errorf("meetingURL(%q, %q) = %q, want %q", c.location, c.description, got, c.want)
		}
	}
}
