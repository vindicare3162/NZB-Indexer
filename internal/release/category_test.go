package release

import "testing"

func TestCategorize(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		// TV, refined to SD/HD/UHD by resolution.
		{"Some.Show.S01E01.1080p.WEB.x264-GRP", CatTVHD},
		{"Some.Show.S01E01.2160p.WEB.x265-GRP", CatTVUHD},
		{"Another Show 2x05 HDTV XviD", CatTVSD},
		{"Cool.Series.Season.2.Complete.720p", CatTVHD},
		{"Plain.Show.S03E04-GRP", CatTVHD}, // no resolution -> HD default
		// Movies, refined to SD/HD/UHD by resolution.
		{"Great.Movie.2024.1080p.BluRay.x264-GRP", CatMoviesHD},
		{"Huge.Movie.2024.2160p.UHD.BluRay.x265", CatMoviesUHD},
		{"Old Film 1998 DVDRip XviD", CatMoviesSD},
		{"Artist - Album (2021) [FLAC]", CatAudioLoss},
		{"Various - Hits 2020 MP3 320kbps", CatAudioMP3},
		{"Some Author - Novel Title (epub)", CatBooksEbook},
		{"National Geographic Magazine 2023", CatBooksMags},
		{"Big.Game.2024-CODEX", CatPCGames},
		{"Utility.Suite.v3.2.x64.Keygen", CatPC},
		{"Racing.Game.PS5", CatConsole},
		{"Totally Random Junk Post", CatOther},
	}
	for _, tt := range tests {
		if got := Categorize(tt.name); got != tt.want {
			t.Errorf("Categorize(%q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestReleaseHashStableAndDedups(t *testing.T) {
	// Same content, minor size drift, same group -> same hash (dedup).
	h1 := ReleaseHash("Great.Movie.2024.1080p.BluRay.x264-GRP", 1_000_000_000, 5)
	h2 := ReleaseHash("Great.Movie.2024.1080p.BluRay.x264-GRP", 1_000_500_000, 5) // +0.05%
	if h1 != h2 {
		t.Errorf("expected near-identical sizes to hash equally:\n h1=%s\n h2=%s", h1, h2)
	}

	// Different group -> different hash.
	h3 := ReleaseHash("Great.Movie.2024.1080p.BluRay.x264-GRP", 1_000_000_000, 6)
	if h1 == h3 {
		t.Error("different group should produce a different hash")
	}

	// Different name -> different hash.
	h4 := ReleaseHash("Other.Movie.2024.1080p", 1_000_000_000, 5)
	if h1 == h4 {
		t.Error("different name should produce a different hash")
	}

	// Deterministic across calls.
	if ReleaseHash("x", 100, 1) != ReleaseHash("x", 100, 1) {
		t.Error("hash must be deterministic")
	}
}
