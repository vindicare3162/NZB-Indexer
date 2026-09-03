package release

import "testing"

func TestIsObfuscated(t *testing.T) {
	obfuscated := []string{
		"b6534bac9d3149e5bcd657b08345c075",
		"4Mg2PERuop6Dzy1Vzu1JupP9fg83J1",
		"abc123xyz-9f8e7d6c5b4a3210fedcba98",
		"Ef9UyY9ZpxkXPkQy",
		"",
	}
	for _, n := range obfuscated {
		if !IsObfuscated(n) {
			t.Errorf("expected %q to be obfuscated", n)
		}
	}

	readable := []string{
		"Great.Movie.2024.1080p.BluRay.x264-GRP",
		"Some.Show.S01E01.HDTV.x264",
		"Artist - Album (2021) [FLAC]",
		"National Geographic Documentary",
		"Saving.Grace.S03E10.HDTV.XviD",
	}
	for _, n := range readable {
		if IsObfuscated(n) {
			t.Errorf("expected %q to be treated as readable (not obfuscated)", n)
		}
	}
}
