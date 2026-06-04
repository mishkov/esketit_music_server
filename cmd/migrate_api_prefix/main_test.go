package main

import "testing"

func TestMigrate(t *testing.T) {
	input := []byte(`{
  "tracks": [
    {"id": 1, "audioFilePath": "/songs/foo.mp3", "coverImagePath": "/album-covers/foo.jpg"},
    {"id": 2, "audioFilePath": "https://cdn.example.com/songs/used.mp3?token=abc", "coverImagePath": ""},
    {"id": 3, "audioFilePath": "/api/songs/already.mp3", "coverImagePath": "/api/album-covers/already.jpg"},
    {"id": 4, "audioFilePath": "/songs/escaped%20name.mp3"}
  ],
  "albums": [
    {"id": 1, "coverImagePath": "/album-covers/cover.jpg"}
  ]
}`)

	output, stats := migrate(input)

	if stats.audioMigrated != 2 {
		t.Errorf("audioMigrated = %d, want 2", stats.audioMigrated)
	}
	if stats.audioAlreadyPrefixed != 1 {
		t.Errorf("audioAlreadyPrefixed = %d, want 1", stats.audioAlreadyPrefixed)
	}
	if stats.audioSkipped != 1 {
		t.Errorf("audioSkipped = %d, want 1 (full URL)", stats.audioSkipped)
	}
	if stats.coverMigrated != 2 {
		t.Errorf("coverMigrated = %d, want 2", stats.coverMigrated)
	}
	if stats.coverAlreadyPrefixed != 1 {
		t.Errorf("coverAlreadyPrefixed = %d, want 1", stats.coverAlreadyPrefixed)
	}
	if stats.coverSkipped != 1 {
		t.Errorf("coverSkipped = %d, want 1 (empty string)", stats.coverSkipped)
	}

	s := string(output)
	wantSubs := []string{
		`"audioFilePath": "/api/songs/foo.mp3"`,
		`"audioFilePath": "https://cdn.example.com/songs/used.mp3?token=abc"`,
		`"audioFilePath": "/api/songs/already.mp3"`,
		`"audioFilePath": "/api/songs/escaped%20name.mp3"`,
		`"coverImagePath": "/api/album-covers/foo.jpg"`,
		`"coverImagePath": ""`,
		`"coverImagePath": "/api/album-covers/already.jpg"`,
		`"coverImagePath": "/api/album-covers/cover.jpg"`,
	}
	for _, w := range wantSubs {
		if !contains(s, w) {
			t.Errorf("output missing %q\nfull output:\n%s", w, s)
		}
	}

	// Idempotency: running migrate again is a no-op.
	output2, stats2 := migrate(output)
	if string(output2) != string(output) {
		t.Errorf("second run modified output (not idempotent)")
	}
	if stats2.audioMigrated != 0 || stats2.coverMigrated != 0 {
		t.Errorf("second run migrated rows: audio=%d cover=%d (want 0)", stats2.audioMigrated, stats2.coverMigrated)
	}
}

func TestHasScheme(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://example.com", true},
		{"https://example.com/foo", true},
		{"/songs/foo.mp3", false},
		{"/api/songs/foo.mp3", false},
		{"", false},
		{"foo", false},
		{"file:///tmp", true},
	}
	for _, c := range cases {
		if got := hasScheme(c.in); got != c.want {
			t.Errorf("hasScheme(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
