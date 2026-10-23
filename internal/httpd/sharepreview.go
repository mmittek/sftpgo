// Share previews (fork addition): media players, thumbnails and Open Graph metadata for
// single-file read shares. Previews are generated with ffmpeg into a size-capped cache and
// do not count as share uses. The feature is enabled by setting SHARE_PREVIEWS_DIR.

package httpd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/common"
	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

const (
	sharePreviewImage = "image"
	sharePreviewAudio = "audio"
	sharePreviewVideo = "video"

	// version of the generated assets, bump it to regenerate the cache after changing a recipe
	sharePreviewAssetsVersion = "2"
	// probe results live in their own versioned file, adding fields must not regenerate the media
	sharePreviewProbeSuffix = ".probe-v2.json"
	sharePreviewPeaks       = 1800
	// sample rate of the decimated stream used to compute the waveform peaks
	sharePreviewPeaksRate = 4000
	// chat apps refuse to play large videos inline, above this size only the poster is advertised
	sharePreviewOGVideoMaxSize = 100 * 1024 * 1024
)

var (
	sharePreviewExts = map[string]string{
		".jpg": sharePreviewImage, ".jpeg": sharePreviewImage, ".png": sharePreviewImage,
		".webp": sharePreviewImage, ".gif": sharePreviewImage,
		".wav": sharePreviewAudio, ".mp3": sharePreviewAudio, ".m4a": sharePreviewAudio,
		".flac": sharePreviewAudio, ".aif": sharePreviewAudio, ".aiff": sharePreviewAudio,
		".ogg": sharePreviewAudio, ".opus": sharePreviewAudio,
		".mov": sharePreviewVideo, ".mp4": sharePreviewVideo, ".m4v": sharePreviewVideo,
		".webm": sharePreviewVideo,
	}
	sharePreviewVideoTypes = map[string]string{
		// Chrome refuses video/quicktime but plays H.264/HEVC .mov files announced as mp4
		".mov": "video/mp4", ".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	}
	sharePreviews *sharePreviewCache
)

func init() {
	dir := os.Getenv("SHARE_PREVIEWS_DIR")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logger.WarnToConsole("share previews disabled, unable to create %q: %v", dir, err)
		return
	}
	maxSize := int64(4096)
	if v, err := strconv.ParseInt(os.Getenv("SHARE_PREVIEWS_MAX_SIZE_MB"), 10, 64); err == nil && v > 0 {
		maxSize = v
	}
	sharePreviews = &sharePreviewCache{
		dir:     dir,
		maxSize: maxSize * 1024 * 1024,
		sem:     make(chan struct{}, 2),
		jobs:    make(map[string]*sharePreviewJob),
	}
}

// sharePreview is the template data for a previewable share
type sharePreview struct {
	Kind        string
	Title       string
	Description string
	// Details are the facts behind Description, shown as chips on the page
	Details     []string
	FileType    string
	Size        string
	SiteName    string
	PageURL     string
	DownloadURL string
	MediaURL    string
	MediaType   string
	ThumbURL    string
	PeaksURL    string
	Width       int
	Height      int
	OGVideo     bool
}

type sharePreviewProbe struct {
	Duration   float64 `json:"duration"`
	VideoCodec string  `json:"video_codec"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	HasAudio   bool    `json:"has_audio"`
	Channels   int     `json:"channels"`
	SampleRate int     `json:"sample_rate"`
	// BitDepth is set for lossless audio only, lossy codecs have no meaningful bit depth
	BitDepth   int  `json:"bit_depth"`
	FloatAudio bool `json:"float_audio"`
}

// sharePreviewSource is a validated single shared file that can be read locally by ffmpeg
type sharePreviewSource struct {
	kind     string
	name     string // virtual path
	resolved string // local filesystem path
	info     os.FileInfo
	key      string
}

type sharePreviewJob struct {
	done chan struct{}
	err  error
}

type sharePreviewCache struct {
	dir     string
	maxSize int64
	sem     chan struct{}
	mu      sync.Mutex
	jobs    map[string]*sharePreviewJob
}

func (c *sharePreviewCache) path(key, suffix string) string {
	return filepath.Join(c.dir, key+suffix)
}

// run executes fn once per name while concurrent callers wait for the same result
func (c *sharePreviewCache) run(ctx context.Context, name string, fn func(context.Context) error) error {
	c.mu.Lock()
	job, ok := c.jobs[name]
	if !ok {
		job = &sharePreviewJob{done: make(chan struct{})}
		c.jobs[name] = job
		go func() {
			c.sem <- struct{}{}
			// the job outlives the request that started it, so a crawler that gives up
			// does not waste the work for the next one
			jobCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			job.err = fn(jobCtx)
			cancel()
			<-c.sem
			c.mu.Lock()
			delete(c.jobs, name)
			c.mu.Unlock()
			close(job.done)
			if job.err == nil {
				c.prune()
			}
		}()
	}
	c.mu.Unlock()
	select {
	case <-job.done:
		return job.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// prune removes the least recently used assets above the size cap
func (c *sharePreviewCache) prune() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type cached struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []cached
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, cached{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= c.maxSize {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mtime.Before(files[j].mtime) })
	for _, f := range files {
		if total <= c.maxSize {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// ensure returns the path of a cached asset, generating it if missing
func (c *sharePreviewCache) ensure(ctx context.Context, src *sharePreviewSource, suffix string,
	gen func(ctx context.Context, src *sharePreviewSource) error,
) (string, error) {
	p := c.path(src.key, suffix)
	if _, err := os.Stat(p); err == nil {
		now := time.Now()
		_ = os.Chtimes(p, now, now) // keep recently used assets out of the pruning
		return p, nil
	}
	jobName := src.key + suffix
	if src.kind == sharePreviewAudio && suffix != sharePreviewProbeSuffix {
		// one ffmpeg pass writes every audio asset
		jobName = src.key + ".audio"
	}
	if err := c.run(ctx, jobName, func(ctx context.Context) error { return gen(ctx, src) }); err != nil {
		return "", err
	}
	return p, nil
}

func (c *sharePreviewCache) probe(ctx context.Context, src *sharePreviewSource) (sharePreviewProbe, error) {
	var probe sharePreviewProbe
	p, err := c.ensure(ctx, src, sharePreviewProbeSuffix, c.genProbe)
	if err != nil {
		return probe, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return probe, err
	}
	err = json.Unmarshal(data, &probe)
	return probe, err
}

// loadProbe is for use inside a job: it probes directly instead of queueing another job,
// which could deadlock on the job slots
func (c *sharePreviewCache) loadProbe(ctx context.Context, src *sharePreviewSource) (sharePreviewProbe, error) {
	var probe sharePreviewProbe
	p := c.path(src.key, sharePreviewProbeSuffix)
	if _, err := os.Stat(p); err != nil {
		if err := c.genProbe(ctx, src); err != nil {
			return probe, err
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return probe, err
	}
	err = json.Unmarshal(data, &probe)
	return probe, err
}

func (c *sharePreviewCache) genProbe(ctx context.Context, src *sharePreviewSource) error {
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", src.resolved).Output()
	if err != nil {
		return fmt.Errorf("ffprobe: %w", err)
	}
	var res struct {
		Streams []struct {
			CodecType     string `json:"codec_type"`
			CodecName     string `json:"codec_name"`
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			Channels      int    `json:"channels"`
			SampleRate    string `json:"sample_rate"`
			BitsPerSample int    `json:"bits_per_sample"`
			BitsPerRaw    string `json:"bits_per_raw_sample"`
			Disposition   struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return err
	}
	var probe sharePreviewProbe
	probe.Duration, _ = strconv.ParseFloat(res.Format.Duration, 64)
	for _, s := range res.Streams {
		switch s.CodecType {
		case "video":
			// the first real video stream, embedded cover art and previews come later
			if probe.VideoCodec == "" && s.Disposition.AttachedPic == 0 {
				probe.VideoCodec, probe.Width, probe.Height = s.CodecName, s.Width, s.Height
			}
		case "audio":
			if probe.HasAudio {
				continue
			}
			probe.HasAudio = true
			probe.Channels = s.Channels
			probe.SampleRate, _ = strconv.Atoi(s.SampleRate)
			if strings.HasPrefix(s.CodecName, "pcm_") || s.CodecName == "flac" || s.CodecName == "alac" {
				probe.BitDepth, _ = strconv.Atoi(s.BitsPerRaw)
				if probe.BitDepth == 0 {
					probe.BitDepth = s.BitsPerSample
				}
				probe.FloatAudio = strings.HasPrefix(s.CodecName, "pcm_f")
			}
		}
	}
	data, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path(src.key, sharePreviewProbeSuffix), data)
}

func (c *sharePreviewCache) genThumb(ctx context.Context, src *sharePreviewSource) error {
	if src.kind == sharePreviewAudio {
		// the waveform image comes out of the audio pass together with the peaks
		return c.genAudio(ctx, src)
	}
	args := []string{"-v", "error", "-y"}
	box := "1600"
	if src.kind == sharePreviewVideo {
		probe, err := c.loadProbe(ctx, src)
		if err != nil {
			return err
		}
		box = "1280"
		args = append(args, "-ss", fmt.Sprintf("%.2f", math.Min(probe.Duration*0.1, 30)))
	}
	tmp := c.path(src.key, ".thumb.tmp.jpg")
	args = append(args, "-i", src.resolved, "-frames:v", "1", "-vf",
		fmt.Sprintf("scale=w='min(%[1]s,iw)':h='min(%[1]s,ih)':force_original_aspect_ratio=decrease,format=yuvj420p", box),
		"-q:v", "3", tmp)
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg thumbnail: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, c.path(src.key, ".thumb.jpg"))
}

// genAudio makes, in one decoding pass, the light mp3 the page plays, the waveform peaks and
// the waveform image used as thumbnail
func (c *sharePreviewCache) genAudio(ctx context.Context, src *sharePreviewSource) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if !probe.HasAudio || probe.Duration <= 0 {
		return errors.New("no audio stream")
	}
	tmpMedia := c.path(src.key, ".media.tmp.mp3")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-i", src.resolved,
		"-map", "0:a:0", "-ac", "1", "-ar", "22050", "-c:a", "libmp3lame", "-b:a", "64k", tmpMedia,
		"-map", "0:a:0", "-ac", "1", "-ar", strconv.Itoa(sharePreviewPeaksRate), "-f", "s16le", "pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	peaks := computePeaks(bufio.NewReaderSize(stdout, 1<<16), probe.Duration*sharePreviewPeaksRate, sharePreviewPeaks)
	if err := cmd.Wait(); err != nil {
		os.Remove(tmpMedia)
		return fmt.Errorf("ffmpeg audio preview: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	data, err := json.Marshal(peaks)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(c.path(src.key, ".peaks.json"), data); err != nil {
		return err
	}
	tmpThumb := c.path(src.key, ".thumb.tmp.jpg")
	f, err := os.Create(tmpThumb)
	if err != nil {
		return err
	}
	err = jpeg.Encode(f, renderWaveform(peaks, 1200, 630), &jpeg.Options{Quality: 88})
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmpThumb)
		return err
	}
	if err := os.Rename(tmpThumb, c.path(src.key, ".thumb.jpg")); err != nil {
		return err
	}
	return os.Rename(tmpMedia, c.path(src.key, ".media.mp3"))
}

// computePeaks reads mono s16le samples and returns the normalized loudness (RMS) of each
// bucket: on mastered material the plain peak is near full scale everywhere and the
// waveform turns into a block that hides the song structure
func computePeaks(r io.Reader, expectedSamples float64, buckets int) []float64 {
	perBucket := int(math.Ceil(expectedSamples / float64(buckets)))
	if perBucket < 1 {
		perBucket = 1
	}
	peaks := make([]float64, 0, buckets)
	var current, maxPeak float64
	count := 0
	buf := make([]byte, 2)
	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		v := float64(int16(binary.LittleEndian.Uint16(buf)))
		current += v * v
		count++
		if count == perBucket {
			rms := math.Sqrt(current / float64(count))
			peaks = append(peaks, rms)
			maxPeak = math.Max(maxPeak, rms)
			current, count = 0, 0
		}
	}
	if count > 0 {
		rms := math.Sqrt(current / float64(count))
		peaks = append(peaks, rms)
		maxPeak = math.Max(maxPeak, rms)
	}
	_, _ = io.Copy(io.Discard, r) // let ffmpeg finish writing if the duration was underestimated
	for i := range peaks {
		if maxPeak > 0 {
			peaks[i] = math.Round(peaks[i]/maxPeak*1000) / 1000
		}
	}
	return peaks
}

// renderWaveform draws mirrored waveform bars on a dark gradient, sized for link previews
func renderWaveform(peaks []float64, width, height int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		t := float64(y) / float64(height)
		bg := color.RGBA{uint8(18 + 10*t), uint8(16 + 4*t), uint8(30 + 18*t), 255}
		for x := 0; x < width; x++ {
			img.SetRGBA(x, y, bg)
		}
	}
	if len(peaks) == 0 {
		return img
	}
	const bar, gap, margin = 4, 2, 60
	mid := height / 2
	bars := (width - 2*margin) / (bar + gap)
	for i := 0; i < bars; i++ {
		// the loudest peak among the samples behind this bar
		from := i * len(peaks) / bars
		to := max((i+1)*len(peaks)/bars, from+1)
		v := 0.0
		for _, p := range peaks[from:min(to, len(peaks))] {
			v = math.Max(v, p)
		}
		h := int(math.Max(2, v*float64(height/2-margin)))
		t := float64(i) / float64(bars)
		// magenta to amber across the timeline
		c := color.RGBA{uint8(236 + 14*t), uint8(64 + 120*t), uint8(140 - 100*t), 255}
		x0 := margin + i*(bar+gap)
		for x := x0; x < x0+bar; x++ {
			for y := mid - h; y <= mid+h; y++ {
				img.SetRGBA(x, y, c)
			}
		}
	}
	return img
}

func writeFileAtomic(name string, data []byte) error {
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, name)
}

// getSharePreviewSource validates a single-file share for previews, nil means no preview
func getSharePreviewSource(share *dataprovider.Share, connection *Connection) (*sharePreviewSource, error) {
	if sharePreviews == nil || len(share.Paths) != 1 {
		return nil, nil
	}
	name := share.Paths[0]
	kind, ok := sharePreviewExts[strings.ToLower(path.Ext(name))]
	if !ok {
		return nil, nil
	}
	// the same checks a download does, ffmpeg reads the file outside of the vfs layer
	if !connection.User.HasPerm(dataprovider.PermDownload, path.Dir(name)) {
		return nil, connection.GetPermissionDeniedError()
	}
	if ok, policy := connection.User.IsFileAllowed(name); !ok {
		return nil, connection.GetErrorForDeniedFile(policy)
	}
	info, err := connection.Stat(name, 1)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	fs, resolved, err := connection.GetFsAndResolvedPath(name)
	if err != nil {
		return nil, err
	}
	if !vfs.IsLocalOsFs(fs) {
		return nil, nil
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s", resolved, info.Size(), info.ModTime().UnixNano(),
		sharePreviewAssetsVersion)))
	return &sharePreviewSource{
		kind:     kind,
		name:     name,
		resolved: resolved,
		info:     info,
		key:      hex.EncodeToString(sum[:16]),
	}, nil
}

func getRequestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// formatPreviewAudio describes the source audio, for example "Stereo · 48 kHz · 24-bit"
func formatPreviewAudio(probe sharePreviewProbe) []string {
	var parts []string
	switch probe.Channels {
	case 0:
	case 1:
		parts = append(parts, "Mono")
	case 2:
		parts = append(parts, "Stereo")
	default:
		parts = append(parts, fmt.Sprintf("%d channels", probe.Channels))
	}
	if probe.SampleRate > 0 {
		khz := strconv.FormatFloat(float64(probe.SampleRate)/1000, 'f', -1, 64)
		parts = append(parts, khz+" kHz")
	}
	if probe.BitDepth > 0 {
		depth := fmt.Sprintf("%d-bit", probe.BitDepth)
		if probe.FloatAudio {
			depth += " float"
		}
		parts = append(parts, depth)
	}
	return parts
}

// formatPreviewDuration rounds down like the browser players do, so page and player agree
func formatPreviewDuration(seconds float64) string {
	s := int(seconds)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// getSharePreview returns the page data for a previewable share and starts generating its
// assets in background, so crawlers that fetch the thumbnail next find it ready or in progress
func (s *httpdServer) getSharePreview(r *http.Request, share *dataprovider.Share, connection *Connection,
	siteName string,
) *sharePreview {
	src, err := getSharePreviewSource(share, connection)
	if err != nil || src == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	probe, err := sharePreviews.probe(ctx, src)
	if err != nil {
		connection.Log(logger.LevelWarn, "unable to probe %q for share preview: %v", src.name, err)
		return nil
	}
	if src.kind == sharePreviewAudio && !probe.HasAudio {
		return nil
	}
	go func() {
		_, err := sharePreviews.ensure(context.Background(), src, ".thumb.jpg", sharePreviews.genThumb)
		if err != nil {
			logger.Warn(logSender, "", "unable to generate share preview for %q: %v", src.name, err)
		}
	}()

	base := getRequestBaseURL(r) + path.Join(webClientPubSharesPath, share.ShareID)
	preview := &sharePreview{
		Kind:     src.kind,
		Title:    share.Name,
		SiteName: siteName,
		PageURL:  base + "/download?compress=false",
		// the uncompressed download of the original file, what the Download button points to
		DownloadURL: base + "?compress=false",
		MediaURL:    base + "/preview/media",
		ThumbURL:    base + "/preview/thumb",
		Width:       probe.Width,
		Height:      probe.Height,
	}
	if preview.Title == "" {
		preview.Title = path.Base(src.name)
	}
	details := []string{util.ByteCountIEC(src.info.Size())}
	switch src.kind {
	case sharePreviewImage:
		details = append([]string{"Image", fmt.Sprintf("%dx%d", probe.Width, probe.Height)}, details...)
	case sharePreviewAudio:
		preview.MediaType = "audio/mpeg"
		preview.PeaksURL = base + "/preview/peaks"
		details = append(append([]string{"Audio", formatPreviewDuration(probe.Duration)}, formatPreviewAudio(probe)...),
			details...)
		preview.Width, preview.Height = 1200, 630
	case sharePreviewVideo:
		preview.MediaType = sharePreviewVideoTypes[strings.ToLower(path.Ext(src.name))]
		preview.OGVideo = probe.VideoCodec == "h264" && src.info.Size() <= sharePreviewOGVideoMaxSize
		details = append([]string{"Video", formatPreviewDuration(probe.Duration),
			fmt.Sprintf("%dx%d", probe.Width, probe.Height)}, details...)
	}
	preview.Details = details[1:] // the page shows the kind as the file type chip instead
	preview.FileType = strings.ToUpper(strings.TrimPrefix(path.Ext(src.name), "."))
	preview.Size = util.ByteCountIEC(src.info.Size())
	preview.Description = strings.Join(details, " · ")
	return preview
}

func (s *httpdServer) getSharePreviewForRequest(w http.ResponseWriter, r *http.Request,
) (*sharePreviewSource, *dataprovider.Share, *Connection, bool) {
	validScopes := []dataprovider.ShareScope{dataprovider.ShareScopeRead, dataprovider.ShareScopeReadWrite}
	share, connection, err := s.checkPublicShare(w, r, validScopes)
	if err != nil {
		return nil, nil, nil, false
	}
	src, err := getSharePreviewSource(&share, connection)
	if err != nil || src == nil {
		connection.CloseFS()
		if err == nil {
			err = util.NewRecordNotFoundError("no preview available for this share")
		}
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return nil, nil, nil, false
	}
	return src, &share, connection, true
}

func (s *httpdServer) serveSharePreviewAsset(w http.ResponseWriter, r *http.Request, src *sharePreviewSource,
	suffix, contentType string, gen func(context.Context, *sharePreviewSource) error,
) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	p, err := sharePreviews.ensure(ctx, src, suffix, gen)
	if err != nil {
		logger.Warn(logSender, "", "unable to generate share preview %q for %q: %v", suffix, src.name, err)
		sendAPIResponse(w, r, err, "Preview not available", http.StatusServiceUnavailable)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeContent(w, r, "", src.info.ModTime(), f)
}

func (s *httpdServer) handleShareGetPreviewThumb(w http.ResponseWriter, r *http.Request) {
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	s.serveSharePreviewAsset(w, r, src, ".thumb.jpg", "image/jpeg", sharePreviews.genThumb)
}

func (s *httpdServer) handleShareGetPreviewPeaks(w http.ResponseWriter, r *http.Request) {
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	if src.kind != sharePreviewAudio {
		sendAPIResponse(w, r, nil, "Peaks are only available for audio", http.StatusNotFound)
		return
	}
	s.serveSharePreviewAsset(w, r, src, ".peaks.json", "application/json", sharePreviews.genAudio)
}

// handleShareGetPreviewMedia serves what the page player loads: the light mp3 for audio,
// the original file for video and the scaled image for images. It does not count as a
// share use, since players issue many range requests while seeking
func (s *httpdServer) handleShareGetPreviewMedia(w http.ResponseWriter, r *http.Request) {
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	switch src.kind {
	case sharePreviewAudio:
		s.serveSharePreviewAsset(w, r, src, ".media.mp3", "audio/mpeg", sharePreviews.genAudio)
	case sharePreviewImage:
		s.serveSharePreviewAsset(w, r, src, ".thumb.jpg", "image/jpeg", sharePreviews.genThumb)
	default:
		if err := common.Connections.Add(connection); err != nil {
			sendAPIResponse(w, r, err, "Unable to add connection", http.StatusTooManyRequests)
			return
		}
		defer common.Connections.Remove(connection.GetID())
		if status, err := serveInlineMedia(w, r, connection, src); err != nil && status != 0 {
			sendAPIResponse(w, r, err, "", status)
		}
	}
}

// serveInlineMedia streams the original file through the vfs layer, with range support and a
// fixed media content type, so the browser plays it instead of downloading it
func serveInlineMedia(w http.ResponseWriter, r *http.Request, connection *Connection, src *sharePreviewSource) (int, error) {
	info := src.info
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && checkIfRange(r, info.ModTime()) == condFalse {
		rangeHeader = ""
	}
	offset, size := int64(0), info.Size()
	status := http.StatusOK
	if strings.HasPrefix(rangeHeader, "bytes=") {
		if strings.Contains(rangeHeader, ",") {
			return http.StatusRequestedRangeNotSatisfiable, fmt.Errorf("unsupported range %q", rangeHeader)
		}
		var err error
		offset, size, err = parseRangeRequest(rangeHeader[6:], size)
		if err != nil {
			return http.StatusRequestedRangeNotSatisfiable, err
		}
		status = http.StatusPartialContent
	}
	reader, err := connection.getFileReader(src.name, offset, r.Method)
	if err != nil {
		return getMappedStatusCode(err), err
	}
	defer reader.Close()

	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	if checkPreconditions(w, r, info.ModTime()) {
		return 0, errors.New(http.StatusText(http.StatusPreconditionFailed))
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+size-1, info.Size()))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", sharePreviewVideoTypes[strings.ToLower(path.Ext(src.name))])
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if _, err := io.CopyN(w, reader, size); err != nil {
			// players abort range requests all the time while seeking
			connection.Log(logger.LevelDebug, "share preview stream interrupted: %v", err)
			panic(http.ErrAbortHandler)
		}
	}
	return 0, nil
}
