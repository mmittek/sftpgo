// Share previews (fork addition): media players, thumbnails and Open Graph metadata for
// single-file read shares. Previews are generated with ffmpeg into a ".cache" folder next to
// the shared file, regenerated when missing or when the source changes, and they never block
// the download. Preview requests do not count as share uses. Enabled with SHARE_PREVIEWS=true.

package httpd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // branding logo
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

	// version of the generated assets, bump it to regenerate every preview after changing a recipe
	sharePreviewAssetsVersion = "3"
	sharePreviewCacheDir      = ".cache"
	sharePreviewProbeSuffix   = ".probe.json"
	sharePreviewThumbSuffix   = ".thumb.jpg"
	sharePreviewPeaksSuffix   = ".peaks.json"
	sharePreviewAudioSuffix   = ".preview.mp3"
	sharePreviewPeaks         = 1800
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
	// content types of the original videos streamed to the page player
	sharePreviewMediaTypes = map[string]string{
		// Chrome refuses video/quicktime but plays H.264/HEVC .mov files announced as mp4
		".mov": "video/mp4", ".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
	}
	sharePreviewSuffixes = []string{sharePreviewProbeSuffix, sharePreviewThumbSuffix, sharePreviewPeaksSuffix,
		sharePreviewAudioSuffix}
	sharePreviews *sharePreviewCache
)

func init() {
	if enabled, _ := strconv.ParseBool(os.Getenv("SHARE_PREVIEWS")); !enabled {
		return
	}
	sharePreviews = &sharePreviewCache{
		sem:  make(chan struct{}, 2),
		jobs: make(map[string]*sharePreviewJob),
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
	// the source this probe and the other cached assets were made from
	SourceSize  int64  `json:"source_size"`
	SourceMtime int64  `json:"source_mtime"`
	Version     string `json:"version"`
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
	sem  chan struct{}
	mu   sync.Mutex
	jobs map[string]*sharePreviewJob
}

// path returns where an asset of the shared file lives: <dir>/.cache/<file name><suffix>
func (c *sharePreviewCache) path(src *sharePreviewSource, suffix string) string {
	return filepath.Join(filepath.Dir(src.resolved), sharePreviewCacheDir, filepath.Base(src.resolved)+suffix)
}

// dropStale removes the assets made from an older version of the source file, the next
// requests regenerate them
func (c *sharePreviewCache) dropStale(src *sharePreviewSource) {
	data, err := os.ReadFile(c.path(src, sharePreviewProbeSuffix))
	if err != nil {
		return
	}
	var probe sharePreviewProbe
	if json.Unmarshal(data, &probe) == nil && probe.SourceSize == src.info.Size() &&
		probe.SourceMtime == src.info.ModTime().UnixNano() && probe.Version == sharePreviewAssetsVersion {
		return
	}
	for _, suffix := range sharePreviewSuffixes {
		_ = os.Remove(c.path(src, suffix))
	}
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

// ensure returns the path of a cached asset, generating it if missing
func (c *sharePreviewCache) ensure(ctx context.Context, src *sharePreviewSource, suffix string,
	gen func(ctx context.Context, src *sharePreviewSource) error,
) (string, error) {
	p := c.path(src, suffix)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o775); err != nil {
		return "", err
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
	p := c.path(src, sharePreviewProbeSuffix)
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
		"-show_streams", "-show_format", "file:"+src.resolved).Output()
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
	probe.SourceSize, probe.SourceMtime = src.info.Size(), src.info.ModTime().UnixNano()
	probe.Version = sharePreviewAssetsVersion
	data, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path(src, sharePreviewProbeSuffix), data)
}

func (c *sharePreviewCache) genThumb(ctx context.Context, src *sharePreviewSource) error {
	if src.kind == sharePreviewAudio {
		// the waveform image comes out of the audio pass together with the peaks
		return c.genAudio(ctx, src)
	}
	args := []string{"-v", "error", "-y"}
	box := "1600"
	if src.kind == sharePreviewImage {
		// a "%" in the file name must not be read as an image sequence pattern
		args = append(args, "-pattern_type", "none")
	}
	if src.kind == sharePreviewVideo {
		probe, err := c.loadProbe(ctx, src)
		if err != nil {
			return err
		}
		box = "1280"
		args = append(args, "-ss", fmt.Sprintf("%.2f", math.Min(probe.Duration*0.1, 30)))
	}
	tmp := c.path(src, ".thumb.tmp.jpg")
	args = append(args, "-i", "file:"+src.resolved, "-frames:v", "1", "-vf",
		fmt.Sprintf("scale=w='min(%[1]s,iw)':h='min(%[1]s,ih)':force_original_aspect_ratio=decrease,format=yuvj420p", box),
		"-q:v", "3", "-update", "1", "file:"+tmp)
	if out, err := exec.CommandContext(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg thumbnail: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, c.path(src, sharePreviewThumbSuffix))
}

// genAudio makes, in one decoding pass, the 128 kbps mp3 the page plays, the waveform peaks
// and the waveform image used as thumbnail
func (c *sharePreviewCache) genAudio(ctx context.Context, src *sharePreviewSource) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if !probe.HasAudio || probe.Duration <= 0 {
		return errors.New("no audio stream")
	}
	tmpMedia := c.path(src, ".preview.tmp.mp3")
	// mp3 carries at most two channels, mono sources stay mono
	channels := strconv.Itoa(min(max(probe.Channels, 1), 2))
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-y", "-i", "file:"+src.resolved,
		"-map", "0:a:0", "-ac", channels, "-ar", "44100", "-c:a", "libmp3lame", "-b:a", "128k", "file:"+tmpMedia,
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
	if err := writeFileAtomic(c.path(src, sharePreviewPeaksSuffix), data); err != nil {
		return err
	}
	tmpThumb := c.path(src, ".thumb.tmp.jpg")
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
	if err := os.Rename(tmpThumb, c.path(src, sharePreviewThumbSuffix)); err != nil {
		return err
	}
	return os.Rename(tmpMedia, c.path(src, sharePreviewAudioSuffix))
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
	src := &sharePreviewSource{
		kind:     kind,
		name:     name,
		resolved: resolved,
		info:     info,
		key:      hex.EncodeToString(sum[:16]),
	}
	sharePreviews.dropStale(src)
	return src, nil
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
		_, err := sharePreviews.ensure(context.Background(), src, sharePreviewThumbSuffix, sharePreviews.genThumb)
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
		preview.MediaType = sharePreviewMediaTypes[strings.ToLower(path.Ext(src.name))]
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
	if src.kind == sharePreviewAudio {
		s.serveBrandedWaveform(w, r, src)
		return
	}
	s.serveSharePreviewAsset(w, r, src, sharePreviewThumbSuffix, "image/jpeg", sharePreviews.genThumb)
}

// serveBrandedWaveform stamps the current branding logo on the cached waveform at request
// time, so a branding change shows up in the link previews without regenerating anything
func (s *httpdServer) serveBrandedWaveform(w http.ResponseWriter, r *http.Request, src *sharePreviewSource) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	p, err := sharePreviews.ensure(ctx, src, sharePreviewThumbSuffix, sharePreviews.genThumb)
	if err != nil {
		logger.Warn(logSender, "", "unable to generate the waveform for %q: %v", src.name, err)
		sendAPIResponse(w, r, err, "Preview not available", http.StatusServiceUnavailable)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	waveform, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	img := image.NewRGBA(waveform.Bounds())
	draw.Draw(img, img.Bounds(), waveform, image.Point{}, draw.Src)
	modTime := src.info.ModTime()
	if configs, err := dataprovider.GetConfigs(); err == nil && configs.Branding != nil &&
		len(configs.Branding.WebClient.Logo) > 0 {
		if logo, _, err := image.Decode(bytes.NewReader(configs.Branding.WebClient.Logo)); err == nil {
			stampLogo(img, logo, img.Bounds().Dy()/7, img.Bounds().Dy()/18)
			if updated := util.GetTimeFromMsecSinceEpoch(configs.UpdatedAt); updated.After(modTime) {
				modTime = updated
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// revalidate every time, the logo may change while the waveform stays the same
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", modTime, bytes.NewReader(buf.Bytes()))
}

// stampLogo draws the logo in white in the bottom-right corner. The logo darkness becomes the
// opacity, so a black logo on a white square and a transparent one both come out as clean
// white marks on the dark waveform
func stampLogo(dst *image.RGBA, logo image.Image, height, margin int) {
	lb := logo.Bounds()
	if lb.Dx() == 0 || lb.Dy() == 0 || height <= 0 {
		return
	}
	width := lb.Dx() * height / lb.Dy()
	x0 := dst.Bounds().Max.X - margin - width
	y0 := dst.Bounds().Max.Y - margin - height
	for ty := 0; ty < height; ty++ {
		sy0, sy1 := lb.Min.Y+ty*lb.Dy()/height, lb.Min.Y+max((ty+1)*lb.Dy()/height, ty*lb.Dy()/height+1)
		for tx := 0; tx < width; tx++ {
			sx0, sx1 := lb.Min.X+tx*lb.Dx()/width, lb.Min.X+max((tx+1)*lb.Dx()/width, tx*lb.Dx()/width+1)
			// average the source pixels behind this target pixel
			var alpha float64
			n := 0
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, b, a := logo.At(sx, sy).RGBA()
					if a == 0 {
						n++
						continue
					}
					// RGBA is alpha premultiplied, un-premultiply before reading the darkness
					lum := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)) / float64(a)
					alpha += (1 - lum) * float64(a) / 0xffff
					n++
				}
			}
			alpha = alpha / float64(n) * 0.9
			px := dst.RGBAAt(x0+tx, y0+ty)
			blend := func(v uint8) uint8 { return uint8(float64(v)*(1-alpha) + 255*alpha) }
			dst.SetRGBA(x0+tx, y0+ty, color.RGBA{blend(px.R), blend(px.G), blend(px.B), 255})
		}
	}
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
	s.serveSharePreviewAsset(w, r, src, sharePreviewPeaksSuffix, "application/json", sharePreviews.genAudio)
}

// handleShareGetPreviewMedia serves what the page player loads: the 128 kbps mp3 for audio,
// the original file for video and the scaled image for images. It does not count as a share use, since players
// issue many range requests while seeking
func (s *httpdServer) handleShareGetPreviewMedia(w http.ResponseWriter, r *http.Request) {
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	switch src.kind {
	case sharePreviewAudio:
		s.serveSharePreviewAsset(w, r, src, sharePreviewAudioSuffix, "audio/mpeg", sharePreviews.genAudio)
	case sharePreviewImage:
		s.serveSharePreviewAsset(w, r, src, sharePreviewThumbSuffix, "image/jpeg", sharePreviews.genThumb)
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
	w.Header().Set("Content-Type", sharePreviewMediaTypes[strings.ToLower(path.Ext(src.name))])
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
