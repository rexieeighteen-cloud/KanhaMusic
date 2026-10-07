package platforms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	td "github.com/Kanha/Meow"

	state "KanhaMusic/kanha/core/models"
)

const PlatformYouTube state.PlatformName = "YouTube"

// YouTubePlatform provides API-key-free YouTube search and playback
// using the yt-dlp binary already installed by the Dockerfile.
type YouTubePlatform struct{}

func init() {
	RegisterPlatform(&YouTubePlatform{})
}

func (p *YouTubePlatform) Name() state.PlatformName {
	return PlatformYouTube
}

func (p *YouTubePlatform) Priority() int {
	return 80
}

// CanGet reports whether this platform can handle the query.
func (p *YouTubePlatform) CanGet(query string) bool {
	query = strings.TrimSpace(query)

	if query == "" {
		return false
	}

	// YouTube URLs.
	if isYouTubeURL(query) {
		return true
	}

	// Explicit URLs belonging to another platform should not be
	// interpreted as YouTube searches.
	if strings.HasPrefix(query, "http://") ||
		strings.HasPrefix(query, "https://") {
		return false
	}

	// Normal text is treated as a YouTube search query.
	return true
}

// Get searches YouTube or extracts information from a YouTube URL.
func (p *YouTubePlatform) Get(query string, video bool) ([]*state.Track, error) {
	query = strings.TrimSpace(query)

	if query == "" {
		return nil, errors.New("empty YouTube query")
	}

	target := query

	// Plain text -> YouTube search.
	if !isYouTubeURL(query) {
		target = "ytsearch5:" + query
	}

	items, err := ytDlpJSON(target)
	if err != nil {
		return nil, err
	}

	tracks := make([]*state.Track, 0, len(items))

	for _, item := range items {
		if item.ID == "" {
			continue
		}

		// Ignore live streams because normal song playback expects
		// a finite duration.
		if item.IsLive {
			continue
		}

		track := p.toTrack(item)

		if track == nil {
			continue
		}

		if track.Duration <= 0 {
			continue
		}

		tracks = append(tracks, track)
	}

	if len(tracks) == 0 {
		return nil, errors.New("no playable YouTube result found")
	}

	if video && len(tracks) > 1 {
		return tracks[:1], nil
	}

	return tracks, nil
}

// CanDownload allows the existing yt-dlp backend to download
// YouTube tracks without requiring Meow API credentials.
func (p *YouTubePlatform) CanDownload(source state.PlatformName) bool {
	return source == PlatformYouTube ||
		source == PlatformYtDlp
}

// Download delegates the actual media download to the existing
// yt-dlp implementation.
func (p *YouTubePlatform) Download(
	ctx context.Context,
	track *state.Track,
	msg *td.Message,
) (string, error) {
	if track == nil {
		return "", errors.New("nil track")
	}

	downloader := &YtdlpPlatform{}

	return downloader.Download(ctx, track, msg)
}

// DownloadTrack is a helper for callers that only have a track.
func (p *YouTubePlatform) DownloadTrack(
	ctx context.Context,
	track *state.Track,
) (string, error) {
	if track == nil {
		return "", errors.New("nil track")
	}

	downloader := &YtdlpPlatform{}

	return downloader.Download(ctx, track, nil)
}

// VideoSearch is kept for compatibility with the registry/autoplay code.
func (p *YouTubePlatform) VideoSearch(
	query string,
) ([]*state.Track, error) {
	return p.Get(query, true)
}

// AutoplayCandidates returns YouTube tracks that can be used for
// automatic queue playback without requiring an API key.
func (p *YouTubePlatform) AutoplayCandidates(
	videoID string,
	title string,
	limit int,
) ([]*state.Track, error) {
	if limit <= 0 {
		limit = 5
	}

	if limit > 20 {
		limit = 20
	}

	var candidates []*state.Track

	seen := make(map[string]bool)

	addTrack := func(track *state.Track) {
		if track == nil {
			return
		}

		if track.ID == "" {
			return
		}

		if track.ID == videoID {
			return
		}

		if seen[track.ID] {
			return
		}

		if track.Duration <= 0 {
			return
		}

		seen[track.ID] = true
		track.MarkAutoplay()
		candidates = append(candidates, track)
	}

	// First try YouTube's webpage metadata/search context.
	if videoID != "" {
		target := "https://www.youtube.com/watch?v=" +
			url.QueryEscape(videoID)

		items, err := ytDlpJSON(target)
		if err == nil {
			for _, item := range items {
				addTrack(p.toTrack(item))

				if len(candidates) >= limit {
					return candidates[:limit], nil
				}
			}
		}
	}

	// API-key-free fallback: search for related songs using the
	// current title.
	if strings.TrimSpace(title) != "" {
		searchLimit := limit + 3

		if searchLimit > 20 {
			searchLimit = 20
		}

		target := "ytsearch" +
			strconv.Itoa(searchLimit) +
			":" +
			title +
			" related songs"

		items, err := ytDlpJSON(target)
		if err == nil {
			for _, item := range items {
				addTrack(p.toTrack(item))

				if len(candidates) >= limit {
					break
				}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, errors.New("no autoplay candidates found")
	}

	return candidates, nil
}

// ytInfo is the subset of yt-dlp JSON that this platform needs.
type ytInfo struct {
	ID string `json:"id"`

	Title string `json:"title"`

	Duration float64 `json:"duration"`

	WebpageURL string `json:"webpage_url"`

	URL string `json:"url"`

	Thumbnail string `json:"thumbnail"`

	Uploader string `json:"uploader"`

	Channel string `json:"channel"`

	LiveStatus string `json:"live_status"`

	IsLive bool `json:"is_live"`

	Extractor string `json:"extractor"`

	OriginalURL string `json:"original_url"`
}

// toTrack converts yt-dlp metadata to the project's Track model.
func (p *YouTubePlatform) toTrack(info ytInfo) *state.Track {
	if info.ID == "" {
		return nil
	}

	title := strings.TrimSpace(info.Title)

	if title == "" {
		title = "YouTube Track"
	}

	duration := int(info.Duration)

	if duration < 0 {
		duration = 0
	}

	trackURL := strings.TrimSpace(info.WebpageURL)

	if trackURL == "" {
		trackURL = strings.TrimSpace(info.OriginalURL)
	}

	if trackURL == "" {
		trackURL = "https://www.youtube.com/watch?v=" + info.ID
	}

	artwork := strings.TrimSpace(info.Thumbnail)

	if artwork == "" {
		artwork = fmt.Sprintf(
			"https://i.ytimg.com/vi/%s/hqdefault.jpg",
			info.ID,
		)
	}

	return &state.Track{
		ID:       info.ID,
		Title:    state.NormalizeTrackTitle(title),
		Duration: duration,
		Artwork:  artwork,
		URL:      trackURL,
		Video:    true,
		Source:   PlatformYouTube,
	}
}

// ytDlpJSON executes yt-dlp and returns metadata records.
//
// This intentionally does not use any Meow API or YouTube API key.
func ytDlpJSON(target string) ([]ytInfo, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("empty yt-dlp target")
	}

	args := []string{
		"-j",
		"--flat-playlist",
		"--no-warnings",
		"--no-check-certificate",
		"--skip-download",
		"--ignore-errors",
		"--no-playlist",
		"--socket-timeout",
		"20",
		"--retries",
		"2",
		"--",
		target,
	}

	output, err := runCommand(
		context.Background(),
		"yt-dlp",
		args...,
	)

	if err != nil {
		if strings.TrimSpace(output) == "" {
			return nil, fmt.Errorf(
				"yt-dlp failed: %w",
				err,
			)
		}

		return nil, fmt.Errorf(
			"yt-dlp failed: %w: %s",
			err,
			strings.TrimSpace(output),
		)
	}

	lines := strings.Split(output, "\n")

	items := make([]ytInfo, 0)

	for _, line := range lines {
		line = strings.TrimSpace(line)

		if line == "" {
			continue
		}

		var info ytInfo

		if err := json.Unmarshal(
			[]byte(line),
			&info,
		); err != nil {
			// yt-dlp may print non-JSON diagnostic lines.
			continue
		}

		if info.ID == "" {
			continue
		}

		if strings.EqualFold(
			info.LiveStatus,
			"live",
		) || info.IsLive {
			continue
		}

		items = append(items, info)
	}

	if len(items) == 0 {
		return nil, errors.New(
			"yt-dlp returned no usable YouTube metadata",
		)
	}

	return items, nil
}

// runCommand executes a command while respecting context cancellation.
func runCommand(
	ctx context.Context,
	name string,
	args ...string,
) (string, error) {
	cmd := exec.CommandContext(
		ctx,
		name,
		args...,
	)

	cmd.Env = append(
		os.Environ(),
		"LC_ALL=C",
		"LANG=C",
	)

	output, err := cmd.CombinedOutput()

	if err != nil {
		return string(output), err
	}

	return string(output), nil
}

// isYouTubeURL detects normal YouTube/watch, youtu.be and Shorts URLs.
func isYouTubeURL(value string) bool {
	value = strings.TrimSpace(value)

	if value == "" {
		return false
	}

	parsed, err := url.Parse(value)

	if err != nil {
		return false
	}

	host := strings.ToLower(
		strings.TrimPrefix(
			parsed.Hostname(),
			"www.",
		),
	)

	switch host {
	case "youtube.com",
		"m.youtube.com",
		"music.youtube.com",
		"youtu.be":
		return true
	default:
		return false
	}
}

// extractYouTubeID is kept as a small utility for compatibility with
// code that may need to extract a video ID from a URL.
func extractYouTubeID(value string) string {
	value = strings.TrimSpace(value)

	if value == "" {
		return ""
	}

	parsed, err := url.Parse(value)

	if err != nil {
		return ""
	}

	host := strings.ToLower(
		strings.TrimPrefix(
			parsed.Hostname(),
			"www.",
		),
	)

	if host == "youtu.be" {
		id := strings.Trim(
			parsed.Path,
			"/",
		)

		if id != "" {
			return id
		}
	}

	if strings.Contains(host, "youtube.com") {
		if id := parsed.Query().Get("v"); id != "" {
			return id
		}

		parts := strings.Split(
			strings.Trim(parsed.Path, "/"),
			"/",
		)

		if len(parts) >= 2 {
			switch parts[0] {
			case "shorts", "embed", "live":
				return parts[1]
			}
		}
	}

	// Last-resort 11-character YouTube ID extraction.
	re := regexp.MustCompile(
		`(?:^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{11})(?:$|[^A-Za-z0-9_-])`,
	)

	match := re.FindStringSubmatch(value)

	if len(match) == 2 {
		return match[1]
	}

	return ""
}

// Keep time imported for projects that build this file with older
// helper integrations which use this package's timing utilities.
var _ = time.Second
