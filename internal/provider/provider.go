package provider

import "time"

type Track struct {
	ID       string            `json:"id"`
	Provider string            `json:"provider"`
	Title    string            `json:"title"`
	Artist   string            `json:"artist"`
	Album    string            `json:"album"`
	Duration int               `json:"duration"`
	Links    map[string]string `json:"links"`
	IsStream bool              `json:"is_stream"`
	DRM      bool              `json:"drm"`
	Tags     map[string]string `json:"tags"`
}

type Stream struct {
	URL        string            `json:"url"`
	Container  string            `json:"container"`
	Codec      string            `json:"codec"`
	Bitrate    int               `json:"bitrate_kbps"`
	SampleRate int               `json:"sample_rate"`
	BitDepth   int               `json:"bit_depth"`
	Channels   int               `json:"channels"`
	Lossless   bool              `json:"lossless"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Meta       map[string]string `json:"meta"`
}

type SearchKind int

const (
	SearchKindTrack SearchKind = iota
	SearchKindAlbum
	SearchKindPlaylist
)

type QualityPref int

const (
	QualityAny QualityPref = iota
	QualityLosslessFirst
)

type Provider interface {
	Name() string
	Search(query string, kind SearchKind, limit int) ([]Track, error)
	GetTrack(id string) (Track, error)
	ResolveStream(track Track, qualityPreference QualityPref) (Stream, error)
}
