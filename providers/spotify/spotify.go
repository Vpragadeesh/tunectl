package spotify

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"audictl/internal/provider"
)

type SpotifyProvider struct{}

func New() *SpotifyProvider { return &SpotifyProvider{} }

func (s *SpotifyProvider) Name() string { return "spotify" }

// Search uses spodtl to fetch track metadata. This is metadata-only; DRM is honored by marking DRM=true.
func (s *SpotifyProvider) Search(query string, kind provider.SearchKind, limit int) ([]provider.Track, error) {
	// spodtl must be installed and configured. We shell out and attempt to parse basic output.
	cmd := exec.Command("spodtl", "search", "track", query, "--limit", "1", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("spodtl search failed: %w", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		// try to fall back to plain text parsing
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 0 || lines[0] == "" {
			return nil, fmt.Errorf("spodtl returned no results")
		}
		t := provider.Track{
			ID:       "spotify:unknown",
			Provider: s.Name(),
			Title:    lines[0],
			DRM:      true,
		}
		return []provider.Track{t}, nil
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("no results")
	}
	m := arr[0]
	id := safeString(m["id"])
	title := safeString(m["title"])
	artist := safeString(m["artist"])
	album := safeString(m["album"])

	t := provider.Track{
		ID:       id,
		Provider: s.Name(),
		Title:    title,
		Artist:   artist,
		Album:    album,
		DRM:      true, // metadata only
	}
	return []provider.Track{t}, nil
}

func (s *SpotifyProvider) GetTrack(id string) (provider.Track, error) {
	cmd := exec.Command("spodtl", "track", id, "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return provider.Track{}, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		return provider.Track{}, err
	}
	t := provider.Track{
		ID:       safeString(m["id"]),
		Provider: s.Name(),
		Title:    safeString(m["title"]),
		Artist:   safeString(m["artist"]),
		Album:    safeString(m["album"]),
		DRM:      true,
	}
	return t, nil
}

func (s *SpotifyProvider) ResolveStream(track provider.Track, qualityPreference provider.QualityPref) (provider.Stream, error) {
	// Spotify is metadata-only; do not attempt downloading
	return provider.Stream{}, fmt.Errorf("spotify provider does not supply playable audio")
}

func safeString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
