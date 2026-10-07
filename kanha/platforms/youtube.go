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

	state "KanhaMusic/kanha/core/models"
	td "github.com/Kanha/Meow"
)

const PlatformYouTube state.PlatformName = "YouTube"

type YouTubePlatform struct{}

func init() {
	Register(&YouTubePlatform{})
}

func (p *YouTubePlatform) Name() state.PlatformName { return PlatformYouTube }
func (p *YouTubePlatform) Priority() int            { return 80 }

func (p *YouTubePlatform) CanGet(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	if isYouTubeURL(query) {
		return true
	}
	if strings.HasPrefix(query, "http://") || strings.HasPrefix(query, "https://") {
		return false
	}
	return true
}

func (p *YouTubePlatform) Get(query string, video bool) ([]*state.Track, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty YouTube query")
	}

	target := query
	if !isYouTubeURL(query) {
		target = "ytsearch5:" + query
	}

	items, err := ytDlpJSON(target)
	if err != nil {
		return nil, err
	}

	tracks := make([]*state.Track, 0, len(items))
	for _, item := range items {
		if item.ID == "" || item.IsLive || strings.EqualFold(item.LiveStatus, "live") {
			continue
		}
		track := p.toTrack(item)
		if track == nil || track.Duration <= 0 {
			continue
		}
		track.Video = video
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

func (p *YouTubePlatform) CanDownload(source state.PlatformName) bool {
	return source == PlatformYouTube || source == PlatformYtDlp
}

func (p *YouTubePlatform) Download(ctx context.Context, track *state.Track, msg *td.Message) (string, error) {
	if track == nil {
		return "", errors.New("nil track")
	}
	return (&YtdlpPlatform{}).Download(ctx, track, msg)
}

func (p *YouTubePlatform) DownloadTrack(ctx context.Context, track *state.Track) (string, error) {
	return p.Download(ctx, track, nil)
}

// VideoSearch keeps compatibility with registry.go, which passes the video flag.
func (p *YouTubePlatform) VideoSearch(query string, video bool) ([]*state.Track, error) {
	return p.Get(query, video)
}

func (p *YouTubePlatform) AutoplayCandidates(videoID, title string, limit int) ([]*state.Track, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 20 {
		limit = 20
	}

	candidates := make([]*state.Track, 0, limit)
	seen := make(map[string]bool)

	add := func(track *state.Track) {
		if track == nil || track.ID == "" || track.ID == videoID || track.Duration <= 0 || seen[track.ID] {
			return
		}
		seen[track.ID] = true
		track.MarkAutoplay("", "")
		candidates = append(candidates, track)
	}

	if strings.TrimSpace(title) != "" {
		searchLimit := limit + 5
		if searchLimit > 20 {
			searchLimit = 20
		}
		target := "ytsearch" + strconv.Itoa(searchLimit) + ":" + title + " related songs"
		if items, err := ytDlpJSON(target); err == nil {
			for _, item := range items {
				if item.IsLive || strings.EqualFold(item.LiveStatus, "live") {
					continue
				}
				add(p.toTrack(item))
				if len(candidates) >= limit {
					break
				}
			}
		}
	}

	if len(candidates) == 0 && videoID != "" {
		target := "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
		if items, err := ytDlpJSON(target); err == nil {
			for _, item := range items {
				add(p.toTrack(item))
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

type ytInfo struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Duration    float64 `json:"duration"`
	WebpageURL  string  `json:"webpage_url"`
	URL         string  `json:"url"`
	Thumbnail   string  `json:"thumbnail"`
	Uploader    string  `json:"uploader"`
	Channel     string  `json:"channel"`
	LiveStatus  string  `json:"live_status"`
	IsLive      bool    `json:"is_live"`
	OriginalURL string  `json:"original_url"`
}

func (p *YouTubePlatform) toTrack(info ytInfo) *state.Track {
	if info.ID == "" {
		return nil
	}
	title := strings.TrimSpace(info.Title)
	if title == "" {
		title = "YouTube Track"
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
		artwork = fmt.Sprintf("https://i.ytimg.com/vi/%s/hqdefault.jpg", info.ID)
	}

	return &state.Track{
		ID:       info.ID,
		Title:    state.NormalizeTrackTitle(title),
		Duration: int(info.Duration),
		Artwork:  artwork,
		URL:      trackURL,
		Video:    true,
		Source:   PlatformYouTube,
	}
}

// withVideo applies the requested playback mode to a list of tracks.
func withVideo(tracks []*state.Track, video bool) []*state.Track {
	for _, track := range tracks {
		if track != nil {
			track.Video = video
		}
	}
	return tracks
}

func ytDlpJSON(target string) ([]ytInfo, error) {
	if strings.TrimSpace(target) == "" {
		return nil, errors.New("empty yt-dlp target")
	}

	args := []string{
		"-j", "--flat-playlist", "--no-warnings", "--no-check-certificate",
		"--skip-download", "--ignore-errors", "--no-playlist",
		"--socket-timeout", "20", "--retries", "2", "--", target,
	}
	output, err := runCommand(context.Background(), "yt-dlp", args...)
	if err != nil {
		if strings.TrimSpace(output) == "" {
			return nil, fmt.Errorf("yt-dlp failed: %w", err)
		}
		return nil, fmt.Errorf("yt-dlp failed: %w: %s", err, strings.TrimSpace(output))
	}

	items := make([]ytInfo, 0)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var info ytInfo
		if err := json.Unmarshal([]byte(line), &info); err != nil {
			continue
		}
		if info.ID == "" || info.IsLive || strings.EqualFold(info.LiveStatus, "live") {
			continue
		}
		items = append(items, info)
	}
	if len(items) == 0 {
		return nil, errors.New("yt-dlp returned no usable YouTube metadata")
	}
	return items, nil
}

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func isYouTubeURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	switch host {
	case "youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be":
		return true
	default:
		return false
	}
}

func extractYouTubeID(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	if host == "youtu.be" {
		if id := strings.Trim(parsed.Path, "/"); id != "" {
			return id
		}
	}
	if strings.Contains(host, "youtube.com") {
		if id := parsed.Query().Get("v"); id != "" {
			return id
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) >= 2 && (parts[0] == "shorts" || parts[0] == "embed" || parts[0] == "live") {
			return parts[1]
		}
	}
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{11})(?:$|[^A-Za-z0-9_-])`)
	if match := re.FindStringSubmatch(value); len(match) == 2 {
		return match[1]
	}
	return ""
}
