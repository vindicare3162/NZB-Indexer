package release

import "testing"

func TestCleanName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`[075/111] - "Some.Show.S01E01.1080p.WEB.x264-GRP.mkv" yEnc`, "Some.Show.S01E01.1080p.WEB.x264-GRP"},
		{`"Great.Movie.2024.1080p.BluRay.x264-GRP.rar" (1/50)`, "Great.Movie.2024.1080p.BluRay.x264-GRP"},
		{`Plain.Release.Name.2023.720p.WEB-DL`, "Plain.Release.Name.2023.720p.WEB-DL"},
		{`"artist - album (2021) [FLAC]" yEnc (1/120) - 524288000 bytes`, "artist - album (2021) [FLAC]"},
	}
	for _, tt := range tests {
		if got := CleanName(tt.in); got != tt.want {
			t.Errorf("CleanName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSearchName(t *testing.T) {
	got := SearchName("Some.Show-S01E01_1080p")
	want := "some show s01e01 1080p"
	if got != want {
		t.Errorf("SearchName = %q, want %q", got, want)
	}
}
