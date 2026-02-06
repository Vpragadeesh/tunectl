package spotify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"audictl/internal/provider"
	yprov "audictl/providers/youtube"
)

type SpotifyProvider struct {
	yt provider.Provider
}

func New() *SpotifyProvider {
	return &SpotifyProvider{
		yt: yprov.New(),
	}
}

func (s *SpotifyProvider) Name() string { return "spotify" }

// parseSpotifyURL extracts the type (track/playlist/album) and ID from a Spotify URL
func parseSpotifyURL(rawURL string) (idType, id string, err error) {
	trackRe := regexp.MustCompile(`/track/([a-zA-Z0-9]+)`)
	if match := trackRe.FindStringSubmatch(rawURL); match != nil {
		return "track", match[1], nil
	}
	playlistRe := regexp.MustCompile(`/playlist/([a-zA-Z0-9]+)`)
	if match := playlistRe.FindStringSubmatch(rawURL); match != nil {
		return "playlist", match[1], nil
	}
	albumRe := regexp.MustCompile(`/album/([a-zA-Z0-9]+)`)
	if match := albumRe.FindStringSubmatch(rawURL); match != nil {
		return "album", match[1], nil
	}
	return "", "", fmt.Errorf("invalid spotify url format")
}

// spotifyOEmbed calls Spotify's public oEmbed API to get the title of a track/playlist/album.
// No authentication required.
// API: https://open.spotify.com/oembed?url=<spotify_url>
// Returns JSON with "title" field like "Never Gonna Give You Up"
func spotifyOEmbed(spotifyURL string) (title string, err error) {
	apiURL := "https://open.spotify.com/oembed?url=" + url.QueryEscape(spotifyURL)
	resp, err := http.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("oembed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("oembed returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return "", fmt.Errorf("failed to read oembed response: %w", err)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("failed to parse oembed json: %w", err)
	}

	t, ok := data["title"]
	if !ok || t == nil {
		return "", fmt.Errorf("oembed response has no title")
	}

	titleStr, ok := t.(string)
	if !ok || titleStr == "" {
		return "", fmt.Errorf("oembed title is empty")
	}

	return titleStr, nil
}

// Search falls back to YouTube search
func (s *SpotifyProvider) Search(query string, kind provider.SearchKind, limit int) ([]provider.Track, error) {
	return s.yt.Search(query, kind, limit)
}

// GetTrack uses oEmbed to get the real track name, then searches YouTube
func (s *SpotifyProvider) GetTrack(id string) (provider.Track, error) {
	spotifyURL := fmt.Sprintf("https://open.spotify.com/track/%s", id)
	title, err := spotifyOEmbed(spotifyURL)
	if err != nil {
		return provider.Track{}, fmt.Errorf("could not get spotify track info: %w", err)
	}

	results, err := s.yt.Search(title, provider.SearchKindTrack, 1)
	if err != nil {
		return provider.Track{}, fmt.Errorf("youtube search failed for '%s': %w", title, err)
	}
	if len(results) == 0 {
		return provider.Track{}, fmt.Errorf("no youtube results for '%s'", title)
	}
	return results[0], nil
}

// ResolveStream uses YouTube provider to resolve the actual playable stream
func (s *SpotifyProvider) ResolveStream(track provider.Track, qualityPreference provider.QualityPref) (provider.Stream, error) {
	return s.yt.ResolveStream(track, qualityPreference)
}

// FetchTracksFromURL uses Spotify's oEmbed API to get the real song/playlist name,
// then searches YouTube for playable results. No Spotify auth required.
func (s *SpotifyProvider) FetchTracksFromURL(spotifyURL string) ([]provider.Track, error) {
	idType, id, err := parseSpotifyURL(spotifyURL)
	if err != nil {
		return nil, err
	}

	// Build canonical Spotify URL
	var pageURL string
	switch idType {
	case "track":
		pageURL = fmt.Sprintf("https://open.spotify.com/track/%s", id)
	case "playlist":
		pageURL = fmt.Sprintf("https://open.spotify.com/playlist/%s", id)
	case "album":
		pageURL = fmt.Sprintf("https://open.spotify.com/album/%s", id)
	default:
		return nil, fmt.Errorf("unknown spotify type: %s", idType)
	}

	// Get real title via oEmbed API (public, no auth)
	title, err := spotifyOEmbed(pageURL)
	if err != nil {
		return nil, fmt.Errorf("could not get spotify info: %w", err)
	}

	// Clean up title for better YouTube search
	// Remove common suffixes like "(feat. ...)" for cleaner results
	query := strings.TrimSpace(title)

	// Search YouTube with the real song name
	results, err := s.yt.Search(query, provider.SearchKindTrack, 10)
	if err != nil {
		return nil, fmt.Errorf("youtube search failed for '%s': %w", query, err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no youtube results for '%s'", query)
	}

	return results, nil
}

// FetchPlaylistTracks is an alias for FetchTracksFromURL
func (s *SpotifyProvider) FetchPlaylistTracks(spotifyURL string) ([]provider.Track, error) {
	return s.FetchTracksFromURL(spotifyURL)
}

func safeFloat64(v interface{}) float64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		var f float64
		fmt.Sscanf(t, "%f", &f)
		return f
	default:
		return 0
	}
}
