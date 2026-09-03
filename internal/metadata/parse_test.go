package metadata

import "testing"

func TestParseNameTV(t *testing.T) {
	cases := []struct {
		name        string
		wantTitle   string
		wantSeason  int
		wantEpisode int
	}{
		{"The.Expanse.S03E10.1080p.WEB.x264-GRP", "the expanse", 3, 10},
		{"Some Show 2x05 HDTV XviD", "some show", 2, 5},
		{"Cool.Series.Season.2.Complete.720p", "cool series", 2, 0},
	}
	for _, c := range cases {
		q := ParseName(c.name, true)
		if q.Title != c.wantTitle {
			t.Errorf("ParseName(%q).Title = %q, want %q", c.name, q.Title, c.wantTitle)
		}
		if q.Season != c.wantSeason || q.Episode != c.wantEpisode {
			t.Errorf("ParseName(%q) season/ep = %d/%d, want %d/%d", c.name, q.Season, q.Episode, c.wantSeason, c.wantEpisode)
		}
	}
}

func TestParseNameMovieYear(t *testing.T) {
	q := ParseName("Great.Movie.2024.1080p.BluRay.x264-GRP", false)
	if q.Title != "great movie" {
		t.Errorf("title = %q, want %q", q.Title, "great movie")
	}
	if q.Year != 2024 {
		t.Errorf("year = %d, want 2024", q.Year)
	}
	if q.Season != 0 || q.Episode != 0 {
		t.Errorf("movie should have no season/episode, got %d/%d", q.Season, q.Episode)
	}
}
