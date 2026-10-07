/*
 * ● KanhaMusic
 * ○ YouTube platform with Shruti API downloader.
 */

package platforms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	state "KanhaMusic/kanha/core/models"
	td "github.com/Kanha/Meow"
)

const PlatformYouTube state.PlatformName = "YouTube"

type YouTubePlatform struct{}

func init() {
	Register(&YouTubePlatform{})
}

func (p *YouTubePlatform) Name() state.PlatformName { return PlatformYouTube }

// Keep YouTube above MeowApi so Shruti is tried first for YouTube downloads.
func (p *YouTubePlatform) Priority() int { return 90 }

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
	return source == PlatformYouTube
}

// Download uses the Shruti API instead of yt-dlp for the actual media file.
// Required Railway variables:
//
//	SHRUTI_API_URL=https://api.shrutibots.site
//	SHRUTI_API_KEY=<your Shruti API key>
func (p *YouTubePlatform) Download(ctx context.Context, track *state.Track, _ *td.Message) (string, error) {
	if track == nil {
		return "", errors.New("nil track")
	}

	if cached := findFile(track); cached != "" {
		return cached, nil
	}

	apiURL := strings.TrimRight(os.Getenv("SHRUTI_API_URL"), "/")
	if apiURL == "" {
		apiURL = "https://api.shrutibots.site"
	}
	apiKey := strings.TrimSpace(os.Getenv("SHRUTI_API_KEY"))
	if apiKey == "" {
		return "", errors.New("SHRUTI_API_KEY is not configured")
	}

	videoID := strings.TrimSpace(track.ID)
	if videoID == "" {
		videoID = extractYouTubeID(track.URL)
	}
	if videoID == "" {
		return "", errors.New("missing YouTube video id")
	}

	mediaType := "audio"
	ext := ".mp3"
	timeout := 5 * time.Minute
	if track.Video {
		mediaType = "video"
		ext = ".mp4"
		timeout = 10 * time.Minute
	}

	path := getPath(track, ext)
	if err := os.MkdirAll("downloads", 0755); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}

	values := url.Values{}
	values.Set("url", videoID)
	values.Set("type", mediaType)
	values.Set("api_key", apiKey)

	reqURL := apiURL + "/download?" + values.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("create Shruti request: %w", err)
	}
	req.Header.Set("User-Agent", "KanhaMusic/1.0")
	req.Header.Set("Accept", "*/*")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		os.Remove(path)
		if reqCtx.Err() != nil {
			return "", fmt.Errorf("Shruti download timeout/cancelled: %w", reqCtx.Err())
		}
		return "", fmt.Errorf("Shruti request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		os.Remove(path)
		return "", fmt.Errorf("Shruti API returned HTTP %d", resp.StatusCode)
	}

	file, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create output file: %w", err)
	}

	_, copyErr := copyResponse(file, resp)
	closeErr := file.Close()
	if copyErr != nil {
		os.Remove(path)
		return "", fmt.Errorf("Shruti download failed: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(path)
		return "", fmt.Errorf("close downloaded file: %w", closeErr)
	}

	if !fileExists(path) {
		os.Remove(path)
		return "", errors.New("Shruti API returned an empty file")
	}

	return path, nil
}

func copyResponse(dst *os.File, resp *http.Response) (int64, error) {
	var total int64
	buf := make([]byte, 128*1024)

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, err := dst.Write(buf[:n])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, errors.New("short write while saving Shruti download")
			}
		}
		if readErr != nil {
			if readErr.Error() == "EOF" {
				return total, nil
			}
			return total, readErr
		}
	}
}

// VideoSearch keeps compatibility with registry.go.
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
	Thumbnail   string  `json:"thumbnail"`
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
	if err == nil {
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
	}

	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])([A-Za-z0-9_-]{11})(?:$|[^A-Za-z0-9_-])`)
	if match := re.FindStringSubmatch(value); len(match) == 2 {
		return match[1]
	}
	return ""
}
