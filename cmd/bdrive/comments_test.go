package main

import "testing"

func TestLocateQuote(t *testing.T) {
	md := "# Title\n\nSome *intro* text.\n\nRevenue grew by **12%** this quarter.\n\nCosts grew by 12% too.\n"
	htmlSrc := "<html>\n<body>\n<h1>Pricing</h1>\n<p>Tom &amp; Jerry\n  <b>plan</b></p>\n</body>\n</html>\n"
	for _, tc := range []struct {
		name, src, quote, prefix, suffix string
		line                             int
		want                             int
		found                            bool
	}{
		{"markdown emphasis", md, "grew by 12% this", "", "", 0, 5, true},
		{"rendered whitespace", md, "Some intro\n text.", "", "", 0, 3, true},
		{"context picks the second occurrence", md, "grew by 12%", "Costs ", " too.", 0, 7, true},
		{"context picks the first occurrence", md, "grew by 12%", "Revenue ", " this", 0, 5, true},
		{"html tags and entities", htmlSrc, "Tom & Jerry plan", "", "", 0, 4, true},
		{"case folded", htmlSrc, "PRICING", "", "", 0, 3, true},
		{"long quote falls back to its start", md, "Revenue grew by 12% this quarter, as everyone agreed in the meeting", "", "", 0, 5, true},
		{"no quote uses the viewer's line", md, "", "", "", 2, 2, true},
		{"line past the end is ignored", md, "", "", "", 99, 0, false},
		{"not found", md, "nowhere in this file", "", "", 0, 0, false},
		{"not found keeps the recorded line as a hint", md, "nowhere in this file", "", "", 3, 3, false},
	} {
		got, found := locateQuote(tc.src, tc.quote, tc.prefix, tc.suffix, tc.line)
		if got != tc.want || found != tc.found {
			t.Errorf("%s: locateQuote = %d,%v; want %d,%v", tc.name, got, found, tc.want, tc.found)
		}
	}
}
