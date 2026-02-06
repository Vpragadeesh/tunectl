package youtube

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"audictl/internal/provider"
)

type YouTubeProvider struct{}

func New() *YouTubeProvider { return &YouTubeProvider{} }

func (y *YouTubeProvider) Name() string { return "youtube" }

// getYtDlpCmd returns an exec.Cmd for yt-dlp with proper PATH including deno
func getYtDlpCmd(args ...string) *exec.Cmd {
	cmd := exec.Command("yt-dlp", args...)
	// Ensure deno is in PATH for yt-dlp's JavaScript runtime
	home, _ := os.UserHomeDir()
	denoPath := filepath.Join(home, ".deno", "bin")
	currentPath := os.Getenv("PATH")
	cmd.Env = append(os.Environ(), fmt.Sprintf("PATH=%s:%s", denoPath, currentPath))
	return cmd
}

// Search uses yt-dlp's JSON output for multiple results
func (y *YouTubeProvider) Search(query string, kind provider.SearchKind, limit int) ([]provider.Track, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 20 {
		limit = 20
	}

	// use ytsearch to get multiple results
	q := fmt.Sprintf("ytsearch%d:%s", limit, query)
	cmd := getYtDlpCmd("-j", "--flat-playlist", q)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp search failed: %w", err)
	}

	// yt-dlp outputs one JSON object per line
	var tracks []provider.Track
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(line), &meta); err != nil {
			continue
		}
		title := safeString(meta["title"])
		uploader := safeString(meta["uploader"])
		if uploader == "" {
			uploader = safeString(meta["channel"])
		}
		duration := int(safeFloat64(meta["duration"]))
		id := safeString(meta["id"])
		if id == "" {
			id = safeString(meta["url"])
		}

		t := provider.Track{
			ID:       "youtube:" + id,
			Provider: y.Name(),
			Title:    title,
			Artist:   uploader,
			Duration: duration,
			Links:    map[string]string{"youtube": fmt.Sprintf("https://www.youtube.com/watch?v=%s", id)},
		}
		tracks = append(tracks, t)
	}

	if len(tracks) == 0 {
		return nil, fmt.Errorf("no results found")
	}
	return tracks, nil
}

func (y *YouTubeProvider) GetTrack(id string) (provider.Track, error) {
	// accept either raw id or youtube: prefix
	if strings.HasPrefix(id, "youtube:") {
		id = strings.TrimPrefix(id, "youtube:")
	}
	url := "https://www.youtube.com/watch?v=" + id
	cmd := getYtDlpCmd("-j", url)
	out, err := cmd.Output()
	if err != nil {
		return provider.Track{}, err
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(out, &meta); err != nil {
		return provider.Track{}, err
	}
	title := safeString(meta["title"])
	uploader := safeString(meta["uploader"])
	duration := int(safeFloat64(meta["duration"]))

	t := provider.Track{
		ID:       "youtube:" + id,
		Provider: y.Name(),
		Title:    title,
		Artist:   uploader,
		Duration: duration,
		Links:    map[string]string{"youtube": url},
	}
	return t, nil
}

func (y *YouTubeProvider) ResolveStream(track provider.Track, qualityPreference provider.QualityPref) (provider.Stream, error) {
	// prefer best audio. Resolve target URL or search query
	target := track.Links["youtube"]
	if target == "" {
		if strings.HasPrefix(track.ID, "youtube:") {
			id := strings.TrimPrefix(track.ID, "youtube:")
			target = "https://www.youtube.com/watch?v=" + id
		} else {
			target = "ytsearch1:" + track.Artist + " - " + track.Title
		}
	}

	// Try JSON extraction to get formats and direct URLs
	jcmd := getYtDlpCmd("-f", "bestaudio[ext=webm+opus]/bestaudio/best", "-j", target)
	jout, err := jcmd.Output()
	if err != nil {
		// If yt-dlp JSON extraction fails, fall back to returning the page URL so mpv can handle it.
		// This avoids hard failure when yt-dlp lacks a JS runtime or SABR formats.
		return provider.Stream{URL: target, Meta: map[string]string{"note": "fallback to page URL"}}, nil
	}

	var meta map[string]interface{}
	if err := json.Unmarshal(jout, &meta); err != nil {
		return provider.Stream{}, err
	}

	// Find best audio format with a direct URL
	var chosenURL, chosenExt, chosenCodec string
	var chosenAbr float64
	if fmts, ok := meta["formats"]; ok {
		if arr, ok := fmts.([]interface{}); ok {
			for _, fi := range arr {
				if m, ok := fi.(map[string]interface{}); ok {
					urlv := safeString(m["url"])
					if urlv == "" {
						continue
					}
					acodec := safeString(m["acodec"])
					if acodec == "none" {
						continue
					}
					abr := safeFloat64(m["abr"])
					ext := safeString(m["ext"])
					if chosenURL == "" || abr > chosenAbr {
						chosenURL = urlv
						chosenAbr = abr
						chosenExt = ext
						chosenCodec = acodec
					}
				}
			}
		}
	}
	if chosenURL == "" {
		// Many YouTube formats may use SABR or lack a direct URL in formats; fall back to the page URL
		// so mpv (which supports youtube URLs) can resolve it itself.
		return provider.Stream{URL: target, Meta: map[string]string{"note": "fallback to page URL"}}, nil
	}

	// Some direct format URLs (googlevideo/videoplayback) are short-lived or require
	// specific headers/cookies; trying to pass them directly to mpv may result in
	// HTTP 403. Prefer letting mpv resolve the original YouTube page URL so it can
	// use its internal extractor (youtube.lua/yt-dlp) which handles required headers.
	if strings.Contains(chosenURL, "googlevideo.com") || strings.Contains(chosenURL, "rr") {
		return provider.Stream{URL: target, Meta: map[string]string{"note": "fallback to page URL (direct googlevideo URL skipped)"}}, nil
	}

	s := provider.Stream{
		URL:        chosenURL,
		Container:  chosenExt,
		Codec:      chosenCodec,
		Bitrate:    int(chosenAbr),
		SampleRate: func() int { return 0 }(),
		Lossless:   false,
		Meta:       map[string]string{"orig": target},
	}
	return s, nil
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
		f, _ := strconv.ParseFloat(t, 64)
		return f
	default:
		return 0
	}
}

// FetchTracksFromURL accepts a YouTube video or playlist URL and returns one or more tracks.
// If the URL points to a single video, a single-track slice is returned. For playlists the
// function returns all entries found by yt-dlp's --flat-playlist JSON output. A limit <= 0
// will use a sensible default (all entries up to 100).
func (y *YouTubeProvider) FetchTracksFromURL(url string, limit int) ([]provider.Track, error) {
	if limit <= 0 {
		limit = 0 // yt-dlp will return all by default for playlists
	}
	cmd := getYtDlpCmd("-j", "--flat-playlist", url)
	out, err := cmd.Output()
	if err != nil {
		// Try falling back to single JSON output for video URLs
		cmd2 := getYtDlpCmd("-j", url)
		out, err = cmd2.Output()
		if err != nil {
			return nil, fmt.Errorf("yt-dlp extraction failed: %w", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var tracks []provider.Track
	for _, line := range lines {
		if line == "" {
			continue
		}
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(line), &meta); err != nil {
			continue
		}
		title := safeString(meta["title"])
		uploader := safeString(meta["uploader"])
		if uploader == "" {
			uploader = safeString(meta["channel"])
		}
		duration := int(safeFloat64(meta["duration"]))
		id := safeString(meta["id"])
		if id == "" {
			id = safeString(meta["url"])
		}
		if id == "" {
			continue
		}

		t := provider.Track{
			ID:       "youtube:" + id,
			Provider: y.Name(),
			Title:    title,
			Artist:   uploader,
			Duration: duration,
			Links:    map[string]string{"youtube": fmt.Sprintf("https://www.youtube.com/watch?v=%s", id)},
		}
		tracks = append(tracks, t)
	}

	if len(tracks) == 0 {
		return nil, fmt.Errorf("no tracks found for url")
	}
	return tracks, nil
}
