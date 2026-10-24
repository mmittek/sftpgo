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
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png" // also decodes the branding logo
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/common"
	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
	"github.com/rs/xid"
)

const (
	sharePreviewImage = "image"
	sharePreviewAudio = "audio"
	sharePreviewVideo = "video"

	// version of the generated assets, bump it to regenerate every preview after changing a recipe
	sharePreviewAssetsVersion = "4"
	// version of the probe alone: adding details redoes only the small probe files
	sharePreviewProbeVersion = "5"
	// version of the video poster choice: changing it redoes only the video posters
	sharePreviewPosterVersion = "2"
	sharePreviewCacheDir      = ".cache"
	sharePreviewProbeSuffix   = ".probe.json"
	sharePreviewThumbSuffix   = ".thumb.jpg"
	sharePreviewPeaksSuffix   = ".peaks.json"
	sharePreviewAudioSuffix   = ".preview.mp3"
	// audio as a video, the branded waveform over the sound, for chats that only play videos inline
	sharePreviewCardVideoSuffix = ".card.mp4"
	// version of what is stamped on the audio waveform card when it is served
	sharePreviewCardStyle = "3"
	// tiny thumbnail for file lists
	sharePreviewSmallSuffix = ".thumb-sm.jpg"
	sharePreviewSmallBox    = 256
	sharePreviewPeaks       = 1800
	// sample rate of the decimated stream used to compute the waveform peaks
	sharePreviewPeaksRate = 4000
	// chat apps refuse to play large videos inline, above this size only the poster is advertised
	sharePreviewOGVideoMaxSize = 100 * 1024 * 1024
	// preview pages and assets of the logged-in file browser
	webClientPreviewPath      = "/web/client/preview"
	webClientPreviewThumbPath = "/web/client/preview/thumb"
	webClientPreviewMediaPath = "/web/client/preview/media"
	webClientPreviewPeaksPath = "/web/client/preview/peaks"
)

var (
	sharePreviewExts = map[string]string{
		".jpg": sharePreviewImage, ".jpeg": sharePreviewImage, ".png": sharePreviewImage,
		".webp": sharePreviewImage, ".gif": sharePreviewImage,
		".dng": sharePreviewImage, ".arw": sharePreviewImage, ".cr2": sharePreviewImage, ".cr3": sharePreviewImage,
		".nef": sharePreviewImage, ".raf": sharePreviewImage, ".orf": sharePreviewImage, ".rw2": sharePreviewImage,
		".wav": sharePreviewAudio, ".mp3": sharePreviewAudio, ".m4a": sharePreviewAudio,
		".flac": sharePreviewAudio, ".aif": sharePreviewAudio, ".aiff": sharePreviewAudio,
		".ogg": sharePreviewAudio, ".opus": sharePreviewAudio,
		".mov": sharePreviewVideo, ".mp4": sharePreviewVideo, ".m4v": sharePreviewVideo,
		".webm": sharePreviewVideo,
	}
	// camera raw formats: the preview is the JPEG rendering embedded in the file
	sharePreviewRawExts = map[string]bool{".dng": true, ".arw": true, ".cr2": true, ".cr3": true, ".nef": true,
		".raf": true, ".orf": true, ".rw2": true}
	// EXIF orientation to ffmpeg filters, the embedded raw preview carries no orientation of its own
	sharePreviewOrientations = map[int]string{2: "hflip", 3: "hflip,vflip", 4: "vflip", 5: "transpose=0",
		6: "transpose=1", 7: "transpose=3", 8: "transpose=2"}
	// content types of the originals streamed to the page player: videos, and mp3 files that
	// every browser plays as they are
	sharePreviewMediaTypes = map[string]string{
		// Chrome refuses video/quicktime but plays H.264/HEVC .mov files announced as mp4
		".mov": "video/mp4", ".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm",
		".mp3": "audio/mpeg",
	}
	sharePreviewSuffixes = []string{sharePreviewProbeSuffix, sharePreviewThumbSuffix, sharePreviewPeaksSuffix,
		sharePreviewAudioSuffix, sharePreviewSmallSuffix, sharePreviewCardVideoSuffix}
	sharePreviews *sharePreviewCache
	// shareCardLogo returns the current branding logo, set by the server that serves shares
	shareCardLogo func() (image.Image, time.Time)
)

func init() {
	if enabled, _ := strconv.ParseBool(os.Getenv("SHARE_PREVIEWS")); !enabled {
		return
	}
	sharePreviews = &sharePreviewCache{}
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
	// VideoURL and VideoType are the og:video of link previews: the original video, or for
	// audio the waveform card video
	VideoURL  string
	VideoType string
	// the size of the og:video, the copy is smaller than the original
	VideoWidth  int
	VideoHeight int
	ThumbURL    string
	PeaksURL    string
	// the size of the thumbnail, link previews show the picture on first share when they know it
	ThumbWidth  int
	ThumbHeight int
	Width       int
	Height      int
	OGVideo     bool
	// OriginalAudio: the audio player plays the original file (mp3), not a preview copy
	OriginalAudio bool
	// VideoCopy: the page player plays the small preview copy of the video, not the original;
	// PreparingVideo: the copy is not ready yet, the page shows the poster meanwhile
	VideoCopy      bool
	PreparingVideo bool
	// PreparingURL is where the page waits for the copy
	PreparingURL string
}

type sharePreviewProbe struct {
	Duration   float64 `json:"duration"`
	VideoCodec string  `json:"video_codec"`
	// VideoBitRate is the video stream bit rate, or the whole file one when the stream has none
	VideoBitRate int64 `json:"video_bit_rate"`
	Width        int   `json:"width"`
	Height       int   `json:"height"`
	HasAudio     bool  `json:"has_audio"`
	Channels     int   `json:"channels"`
	SampleRate   int   `json:"sample_rate"`
	// BitDepth is set for lossless audio only, lossy codecs have no meaningful bit depth
	BitDepth   int  `json:"bit_depth"`
	FloatAudio bool `json:"float_audio"`
	// photo metadata, empty when the file does not carry it
	Camera      string `json:"camera"`
	Lens        string `json:"lens"`
	Orientation int    `json:"orientation"`
	// video only: frames per second and what wrote the file (camera, editor, ffmpeg)
	FrameRate float64 `json:"frame_rate"`
	Source    string  `json:"source"`
	// the moment chosen for the video poster, by the poster choice of PosterVersion
	PosterTime    float64 `json:"poster_time,omitempty"`
	PosterVersion string  `json:"poster_version,omitempty"`
	// the source this probe and the other cached assets were made from
	SourceSize   int64  `json:"source_size"`
	SourceMtime  int64  `json:"source_mtime"`
	Version      string `json:"version"`
	ProbeVersion string `json:"probe_version"`
}

// sharePreviewSource is a validated single shared file that can be read locally by ffmpeg
type sharePreviewSource struct {
	kind     string
	name     string // virtual path
	resolved string // local filesystem path
	info     os.FileInfo
	key      string
}

type sharePreviewCache struct{}

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
		if probe.ProbeVersion != sharePreviewProbeVersion {
			_ = os.Remove(c.path(src, sharePreviewProbeSuffix))
		}
		return
	}
	for _, suffix := range sharePreviewSuffixes {
		_ = os.Remove(c.path(src, suffix))
	}
}

// run executes fn once per name while concurrent callers wait for the same result, at the
// priority of the request (share or browse): the job outlives the request that started it, so
// a crawler that gives up does not waste the work for the next one
func (c *sharePreviewCache) run(ctx context.Context, src *sharePreviewSource, name string,
	fn func(context.Context) error,
) error {
	return tasks.run(ctx, "preview", name, []string{src.resolved}, taskPriorityFrom(ctx), 10*time.Minute, fn)
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
	if err := c.run(ctx, src, previewJobName(src, suffix), func(ctx context.Context) error { return gen(ctx, src) }); err != nil {
		return "", err
	}
	return p, nil
}

// previewJobName groups the assets one generator writes: for audio, the waveform pass writes
// the peaks and the waveform image, the mp3 has its own slower pass
func previewJobName(src *sharePreviewSource, suffix string) string {
	if src.kind == sharePreviewAudio {
		switch suffix {
		case sharePreviewThumbSuffix, sharePreviewPeaksSuffix:
			return src.key + ".waveform"
		case sharePreviewAudioSuffix:
			return src.key + ".mp3"
		case sharePreviewCardVideoSuffix:
			return src.key + ".video"
		}
	}
	return src.key + suffix
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
	if src.kind == sharePreviewImage {
		return c.genImageProbe(ctx, src)
	}
	out, err := mediaCommand(ctx, "ffprobe", "-v", "error", "-print_format", "json",
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
			BitRate       string `json:"bit_rate"`
			FrameRate     string `json:"r_frame_rate"`
			AvgFrameRate  string `json:"avg_frame_rate"`
			Disposition   struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			Tags map[string]string `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			BitRate  string            `json:"bit_rate"`
			Tags     map[string]string `json:"tags"`
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
				probe.VideoBitRate, _ = strconv.ParseInt(s.BitRate, 10, 64)
				if probe.VideoBitRate == 0 {
					probe.VideoBitRate, _ = strconv.ParseInt(res.Format.BitRate, 10, 64)
				}
				probe.FrameRate = parseFrameRate(s.FrameRate)
				if probe.FrameRate == 0 {
					probe.FrameRate = parseFrameRate(s.AvgFrameRate)
				}
				if src.kind == sharePreviewVideo {
					probe.Camera, probe.Lens, probe.Source = describeVideoOrigin(src.resolved, res.Format.Tags, s.Tags)
				}
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
	return c.writeProbe(src, probe)
}

func (c *sharePreviewCache) writeProbe(src *sharePreviewSource, probe sharePreviewProbe) error {
	probe.SourceSize, probe.SourceMtime = src.info.Size(), src.info.ModTime().UnixNano()
	probe.Version, probe.ProbeVersion = sharePreviewAssetsVersion, sharePreviewProbeVersion
	data, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path(src, sharePreviewProbeSuffix), data)
}

// parseFrameRate reads ffprobe rates like "24000/1001"
func parseFrameRate(rate string) float64 {
	num, den, ok := strings.Cut(rate, "/")
	n, errN := strconv.ParseFloat(num, 64)
	if !ok || errN != nil {
		return 0
	}
	d, errD := strconv.ParseFloat(den, 64)
	if errD != nil || d == 0 {
		return 0
	}
	return n / d
}

// describeVideoOrigin finds the camera, the lens and what wrote a video file: Sony keeps the
// camera in the XML sidecar next to the clip, DJI and phones in the container tags, editors
// and ffmpeg sign the container as its encoder
func describeVideoOrigin(file string, formatTags, streamTags map[string]string) (camera, lens, source string) {
	tag := func(tags map[string]string, key string) string {
		for k, v := range tags {
			if strings.EqualFold(k, key) {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	encoder := tag(formatTags, "encoder")
	streamEncoder := tag(streamTags, "encoder")
	lowerEncoder := strings.ToLower(encoder)
	camera, lens = readSonySidecar(file)
	if camera == "" {
		if appleModel := tag(formatTags, "com.apple.quicktime.model"); appleModel != "" {
			camera = formatCamera(tag(formatTags, "com.apple.quicktime.make"), appleModel)
		} else if strings.HasPrefix(lowerEncoder, "dji") {
			camera = strings.ToUpper(encoder)
		}
	}
	switch {
	case strings.Contains(lowerEncoder, "davinci resolve"):
		source = strings.TrimSpace(strings.TrimPrefix(encoder, "Blackmagic Design")) + " render"
	case strings.HasPrefix(lowerEncoder, "lavf"):
		source = "ffmpeg output"
		if strings.HasPrefix(strings.ToLower(streamEncoder), "lavc") {
			// "Lavc61.3.100 libx265": the video was encoded by ffmpeg, not only copied
			if _, lib, ok := strings.Cut(streamEncoder, " "); ok {
				source = "ffmpeg encode (" + lib + ")"
			}
		}
	case strings.HasPrefix(lowerEncoder, "dji"),
		strings.EqualFold(tag(formatTags, "major_brand"), "XAVC"),
		encoder == "" && tag(formatTags, "com.apple.quicktime.model") != "":
		source = "Camera original"
	case encoder != "":
		source = encoder
	}
	return camera, lens, source
}

// readSonySidecar reads the camera and lens from the XML Sony cameras write next to each clip
// (C0001M01.XML, renamed with the clip as "<clip name>.XML")
func readSonySidecar(file string) (camera, lens string) {
	base := strings.TrimSuffix(file, filepath.Ext(file))
	for _, ext := range []string{".XML", ".xml"} {
		f, err := os.Open(base + ext)
		if err != nil {
			continue
		}
		defer f.Close()
		decoder := xml.NewDecoder(io.LimitReader(f, 1<<20))
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			el, ok := token.(xml.StartElement)
			if !ok {
				continue
			}
			attr := func(name string) string {
				for _, a := range el.Attr {
					if a.Name.Local == name {
						return strings.TrimSpace(a.Value)
					}
				}
				return ""
			}
			switch el.Name.Local {
			case "Device":
				camera = formatCamera(attr("manufacturer"), attr("modelName"))
			case "Lens":
				lens = attr("modelName")
			}
		}
		return camera, lens
	}
	return "", ""
}

// fitInBox is the size of a picture scaled down to fit box x box, never enlarged
func fitInBox(width, height, box int) (int, int) {
	if width <= 0 || height <= 0 || (width <= box && height <= box) {
		return width, height
	}
	if width >= height {
		return box, int(math.Round(float64(height) * float64(box) / float64(width)))
	}
	return int(math.Round(float64(width) * float64(box) / float64(height))), box
}

// formatFrameRate prints 23.976, 29.97, 59.94 or 24 frames per second
func formatFrameRate(rate float64) string {
	if rate <= 0 {
		return ""
	}
	return strconv.FormatFloat(math.Round(rate*1000)/1000, 'f', -1, 64) + " fps"
}

// genImageProbe reads size, camera and lens with exiftool, which understands camera raw files
func (c *sharePreviewCache) genImageProbe(ctx context.Context, src *sharePreviewSource) error {
	out, err := mediaCommand(ctx, "exiftool", "-j", "-Make", "-Model", "-LensID", "-LensModel",
		"-ImageSize#", "-DefaultCropSize#", "-Orientation#", src.resolved).Output()
	if err != nil {
		return fmt.Errorf("exiftool: %w", err)
	}
	var res []struct {
		Make            string `json:"Make"`
		Model           string `json:"Model"`
		LensID          any    `json:"LensID"`
		LensModel       any    `json:"LensModel"`
		ImageSize       any    `json:"ImageSize"`
		DefaultCropSize any    `json:"DefaultCropSize"`
		Orientation     int    `json:"Orientation"`
	}
	if err := json.Unmarshal(out, &res); err != nil || len(res) == 0 {
		return fmt.Errorf("exiftool output: %w", err)
	}
	r := res[0]
	var probe sharePreviewProbe
	// raw files: the default crop is the size of the developed image, without the sensor margins
	probe.Width, probe.Height = parseImageSize(r.DefaultCropSize)
	if probe.Width == 0 {
		probe.Width, probe.Height = parseImageSize(r.ImageSize)
	}
	probe.Orientation = r.Orientation
	if probe.Orientation >= 5 && probe.Orientation <= 8 {
		probe.Width, probe.Height = probe.Height, probe.Width
	}
	probe.Camera = formatCamera(r.Make, r.Model)
	probe.Lens = formatLens(r.LensID)
	if probe.Lens == "" {
		probe.Lens = formatLens(r.LensModel)
	}
	return c.writeProbe(src, probe)
}

// parseImageSize reads "7008 4672" or "7008x4672"
func parseImageSize(v any) (int, int) {
	s, ok := v.(string)
	if !ok {
		return 0, 0
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == 'x' })
	if len(parts) != 2 {
		return 0, 0
	}
	w, errW := strconv.ParseFloat(parts[0], 64)
	h, errH := strconv.ParseFloat(parts[1], 64)
	if errW != nil || errH != nil {
		return 0, 0
	}
	return int(w), int(h)
}

// formatCamera returns the camera in capitals, so names read the same whatever case the
// maker writes ("SONY ILCE-7M5", "CANON EOS R5"), without repeating the brand when the model
// already carries it
func formatCamera(brand, model string) string {
	brand, model = strings.TrimSpace(brand), strings.TrimSpace(model)
	if fields := strings.Fields(brand); len(fields) > 0 {
		brand = fields[0] // "NIKON CORPORATION", "OLYMPUS IMAGING CORP."
	}
	result := brand + " " + model
	switch {
	case model == "":
		result = brand
	case brand == "" || strings.HasPrefix(strings.ToLower(model), strings.ToLower(brand)):
		result = model
	}
	return strings.ToUpper(result)
}

func formatLens(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "Unknown") || strings.Trim(s, "-") == "" {
		return ""
	}
	return s
}

func (c *sharePreviewCache) genThumb(ctx context.Context, src *sharePreviewSource) error {
	if src.kind == sharePreviewAudio {
		// the waveform image comes out of the waveform pass together with the peaks
		return c.genWaveform(ctx, src)
	}
	box := 1600
	if src.kind == sharePreviewVideo {
		box = 1280
	}
	return c.renderStill(ctx, src, scaleFilter(box), sharePreviewThumbSuffix)
}

// genSmallThumb makes the tiny thumbnail of file lists straight from the source, so a folder
// view does not also produce the large page previews. It is a square center crop, so lists
// line up whatever the image proportions
func (c *sharePreviewCache) genSmallThumb(ctx context.Context, src *sharePreviewSource) error {
	if src.kind == sharePreviewAudio {
		return errors.New("audio files have no list thumbnail")
	}
	return c.renderStill(ctx, src, squareCropFilter(sharePreviewSmallBox), sharePreviewSmallSuffix)
}

// renderStill writes a JPEG of the image itself, of the embedded rendering of a camera raw
// file or of a video frame (the middle one, then the one pickPosterTime chose), through the
// given scale filter
func (c *sharePreviewCache) renderStill(ctx context.Context, src *sharePreviewSource, filter, suffix string) error {
	if sharePreviewRawExts[strings.ToLower(filepath.Ext(src.resolved))] {
		return c.genRawThumb(ctx, src, filter, suffix)
	}
	args := []string{"-v", "error", "-y"}
	if src.kind == sharePreviewImage && strings.Contains(filepath.Base(src.resolved), "%") {
		// a "%" in the file name must not be read as an image sequence pattern; the option exists
		// only in the image2 reader, ffmpeg picks other readers for png and webp files
		args = append(args, "-f", "image2", "-pattern_type", "none")
	}
	if src.kind == sharePreviewVideo {
		probe, err := c.loadProbe(ctx, src)
		if err != nil {
			return err
		}
		// the chosen moment once picked, the middle of the video until then (fast: one seek,
		// one frame) while the pick runs in background
		posterTime := probe.Duration / 2
		if probe.PosterVersion == sharePreviewPosterVersion {
			posterTime = probe.PosterTime
		} else {
			defer c.refinePosterLater(ctx, src)
		}
		args = append(args, "-ss", fmt.Sprintf("%.3f", posterTime))
	}
	tmp := c.path(src, suffix+".tmp.jpg")
	args = append(args, "-i", "file:"+src.resolved, "-frames:v", "1", "-vf", filter,
		"-q:v", "3", "-update", "1", "file:"+tmp)
	if out, err := mediaCommand(ctx, "ffmpeg", args...).CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg thumbnail: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, c.path(src, suffix))
}

// scaleFilter fits the picture in box x box, keeping its proportions
func scaleFilter(box int) string {
	return fmt.Sprintf("scale=w='min(%[1]d,iw)':h='min(%[1]d,ih)':force_original_aspect_ratio=decrease,format=yuvj420p", box)
}

// squareCropFilter fills box x box and crops the center
func squareCropFilter(box int) string {
	return fmt.Sprintf("scale=%[1]d:%[1]d:force_original_aspect_ratio=increase,crop=%[1]d:%[1]d,format=yuvj420p", box)
}

// genRawThumb scales the JPEG rendering embedded in a camera raw file, it is what the camera
// or the raw converter developed (edits included), decoding the sensor data would not be
func (c *sharePreviewCache) genRawThumb(ctx context.Context, src *sharePreviewSource, filter, suffix string) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	var embedded []byte
	for _, tag := range []string{"-JpgFromRaw", "-PreviewImage"} {
		out, err := mediaCommand(ctx, "exiftool", "-b", tag, src.resolved).Output()
		if err == nil && len(out) > 2 && out[0] == 0xff && out[1] == 0xd8 {
			embedded = out
			break
		}
	}
	if embedded == nil {
		return errors.New("no embedded preview in the raw file")
	}
	filters := filter
	if rotate, ok := sharePreviewOrientations[probe.Orientation]; ok {
		filters = rotate + "," + filters
	}
	tmp := c.path(src, suffix+".tmp.jpg")
	cmd := mediaCommand(ctx, "ffmpeg", "-v", "error", "-y", "-f", "jpeg_pipe", "-i", "pipe:0",
		"-frames:v", "1", "-vf", filters, "-q:v", "3", "-update", "1", "file:"+tmp)
	cmd.Stdin = bytes.NewReader(embedded)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg raw preview: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, c.path(src, suffix))
}

// genWaveform decodes the audio once into the waveform peaks and the waveform image, the
// picture link previews show: it only reads the file, so it is ready long before the mp3
func (c *sharePreviewCache) genWaveform(ctx context.Context, src *sharePreviewSource) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if !probe.HasAudio || probe.Duration <= 0 {
		return errors.New("no audio stream")
	}
	cmd := mediaCommand(ctx, "ffmpeg", "-v", "error", "-i", "file:"+src.resolved,
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
		return fmt.Errorf("ffmpeg waveform: %w: %s", err, strings.TrimSpace(stderr.String()))
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
	return os.Rename(tmpThumb, c.path(src, sharePreviewThumbSuffix))
}

// genAudioPreview encodes the mp3 the page plays and the card video carries: 192 kbps average
// (variable) bitrate, stereo, 44.1 kHz
func (c *sharePreviewCache) genAudioPreview(ctx context.Context, src *sharePreviewSource) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if !probe.HasAudio {
		return errors.New("no audio stream")
	}
	tmpMedia := c.path(src, ".preview.tmp.mp3")
	out, err := mediaCommand(ctx, "ffmpeg", "-v", "error", "-y", "-i", "file:"+src.resolved,
		"-map", "0:a:0", "-ac", "2", "-ar", "44100", "-c:a", "libmp3lame", "-abr", "1", "-b:a", "192k",
		"file:"+tmpMedia).CombinedOutput()
	if err != nil {
		os.Remove(tmpMedia)
		return fmt.Errorf("ffmpeg audio preview: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmpMedia, c.path(src, sharePreviewAudioSuffix))
}

// cardVideoReady: the card video exists and is newer than the branding logo it shows, an older
// one is removed so the next share warm-up or fetch renders it again
func cardVideoReady(src *sharePreviewSource) bool {
	p := sharePreviews.path(src, sharePreviewCardVideoSuffix)
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	if src.kind == sharePreviewAudio && shareCardLogo != nil {
		if logo, updated := shareCardLogo(); logo != nil && updated.After(info.ModTime()) {
			_ = os.Remove(p)
			return false
		}
	}
	return true
}

// cardVideoURL is the address of the card video, versioned like the picture: chat apps cache
// videos by address
func cardVideoURL(src *sharePreviewSource, base string) string {
	if info, err := os.Stat(sharePreviews.path(src, sharePreviewCardVideoSuffix)); err == nil {
		return base + "?v=" + strconv.FormatInt(info.ModTime().Unix(), 10)
	}
	return base
}

// queueCardVideo renders the card video in the background at share priority, for the next
// look at the share page
func queueCardVideo(src *sharePreviewSource) {
	p := sharePreviews.path(src, sharePreviewCardVideoSuffix)
	timeout := 10 * time.Minute
	if src.kind == sharePreviewVideo {
		timeout = time.Hour // a long 4K source decodes on the CPU
	}
	_, _ = tasks.submit("preview", previewJobName(src, sharePreviewCardVideoSuffix), []string{src.resolved},
		priorityShare, 0, timeout, func(ctx context.Context) error {
			if _, err := os.Stat(p); err == nil {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o775); err != nil {
				return err
			}
			return sharePreviews.genCardVideo(ctx, src)
		})
}

const (
	// card videos stay under 100 MB, chat apps refuse to play larger videos inline: the
	// bitrate is planned for the budget and the output is cut at the hard limit regardless
	cardVideoBudget     = 90 * 1024 * 1024 * 8 // bits
	cardVideoHardLimit  = "95M"
	videoPreviewMaxRate = 2_500_000
	videoPreviewMinRate = 400_000
	videoPreviewBox     = 720
)

// genVideoPreview renders the card video of a video that chat apps cannot play as it is (not
// H.264 or above 100 MB): H.264 with the long side at most 720 pixels, never cropped, 128 kbps AAC, with the
// bitrate chosen to stay under the size budget. The fastest encoder preset, quality comes
// second. Videos too long for the lowest bitrate are cut
func (c *sharePreviewCache) genVideoPreview(ctx context.Context, src *sharePreviewSource) error {
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if probe.Width == 0 || probe.Height == 0 {
		return errors.New("no video stream")
	}
	if probe.VideoCodec == "h264" && src.info.Size() <= sharePreviewOGVideoMaxSize {
		return nil // the original is offered as it is
	}
	duration := math.Max(probe.Duration, 1)
	limit := duration
	videoRate := min(float64(cardVideoBudget)/duration-128_000, videoPreviewMaxRate)
	if videoRate < videoPreviewMinRate {
		videoRate = videoPreviewMinRate
		limit = cardVideoBudget / (videoPreviewMinRate + 128_000)
	}
	rate := strconv.Itoa(int(videoRate))
	box := strconv.Itoa(videoPreviewBox)
	tmpVideo := c.path(src, ".card.tmp.mp4")
	// labels: the file name on top, PREVIEW at the bottom left
	outWidth, _ := fitInBox(probe.Width, probe.Height, videoPreviewBox)
	const labelMargin, labelSize = 16, 16
	labels := []string{c.path(src, ".card-title.tmp.png"), c.path(src, ".card-mark.tmp.png")}
	for idx, text := range []string{path.Base(src.name), "PREVIEW"} {
		img := renderTextLabel(text, labelSize, idx == 0, outWidth-2*labelMargin)
		if img == nil {
			return errors.New("unable to load the label font")
		}
		defer os.Remove(labels[idx])
		f, err := os.Create(labels[idx])
		if err != nil {
			return err
		}
		err = png.Encode(f, img)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	margin := strconv.Itoa(labelMargin)
	filter := "[0:v:0]scale='if(gt(iw,ih),min(" + box + ",iw),-2)':'if(gt(iw,ih),-2,min(" + box + ",ih))'[v0];" +
		"[v0][1:v]overlay=" + margin + ":" + margin + "[v1];" +
		"[v1][2:v]overlay=" + margin + ":main_h-overlay_h-" + margin + ",format=yuv420p[v]"
	out, err := mediaCommand(ctx, "ffmpeg", "-v", "error", "-y", "-i", "file:"+src.resolved,
		"-i", "file:"+labels[0], "-i", "file:"+labels[1], "-filter_complex", filter,
		"-map", "[v]", "-map", "0:a:0?", "-t", strconv.FormatFloat(limit, 'f', 3, 64),
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", rate, "-maxrate", rate,
		"-bufsize", strconv.Itoa(int(videoRate*2)), "-c:a", "aac", "-b:a", "128k", "-ac", "2",
		"-dn", "-write_tmcd", "0", "-map_metadata", "-1",
		"-fs", cardVideoHardLimit, "-movflags", "+faststart", "-f", "mp4", "file:"+tmpVideo).CombinedOutput()
	if err != nil {
		os.Remove(tmpVideo)
		return fmt.Errorf("ffmpeg video preview: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmpVideo, c.path(src, sharePreviewCardVideoSuffix))
}

// stampWaveformText writes the file name on top of a 1200x630 waveform card and PREVIEW at the
// bottom left, clear of the logo: what plays is a preview copy, not the original quality
func stampWaveformText(img *image.RGBA, src *sharePreviewSource) {
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	drawCardText(img, path.Base(src.name), 24, true, waveformMargin, 44, width-2*waveformMargin,
		color.RGBA{0xf4, 0xf1, 0xff, 255})
	drawCardText(img, "PREVIEW", 16, false, waveformMargin, height-24, width-2*waveformMargin-200,
		color.RGBA{0xa9, 0xa3, 0xc4, 255})
}

// genCardVideo renders the audio as a small mp4, the branded waveform as a still picture at one
// frame per second over the page player's 192 kbps mp3, so chats that play only videos inline (Discord) can
// play the share in the link card. About 1 MB per minute
func (c *sharePreviewCache) genCardVideo(ctx context.Context, src *sharePreviewSource) error {
	if src.kind == sharePreviewVideo {
		return c.genVideoPreview(ctx, src)
	}
	probe, err := c.loadProbe(ctx, src)
	if err != nil {
		return err
	}
	if !probe.HasAudio {
		return errors.New("no audio stream")
	}
	peaksFile := c.path(src, sharePreviewPeaksSuffix)
	if _, err := os.Stat(peaksFile); err != nil {
		// the job holds the file, so the waveform is made here instead of in a job of its own
		if err := c.genWaveform(ctx, src); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(peaksFile)
	if err != nil {
		return err
	}
	var peaks []float64
	if err := json.Unmarshal(data, &peaks); err != nil {
		return err
	}
	// two stills like the page player: the played part in color, the rest in gray
	const width, height = 1200, 630
	played := renderWaveformWith(peaks, width, height, func(t float64) color.RGBA {
		return color.RGBA{uint8(236 + 14*t), uint8(64 + 120*t), uint8(140 - 100*t), 255}
	})
	unplayed := renderWaveformWith(peaks, width, height, func(_ float64) color.RGBA {
		return color.RGBA{0x5b, 0x57, 0x70, 255}
	})
	stills := []string{c.path(src, ".card.tmp.jpg"), c.path(src, ".card-gray.tmp.jpg")}
	tmpVideo := c.path(src, ".card.tmp.mp4")
	for idx, img := range []*image.RGBA{played, unplayed} {
		defer os.Remove(stills[idx])
		stampWaveformText(img, src)
		if shareCardLogo != nil {
			if logo, _ := shareCardLogo(); logo != nil {
				stampLogo(img, logo, height/7, height/18)
			}
		}
		f, err := os.Create(stills[idx])
		if err != nil {
			return err
		}
		err = jpeg.Encode(f, img, &jpeg.Options{Quality: 90})
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	// the played color sweeps over the bars with the playback time; short files get more
	// frames so the sweep does not jump
	duration := math.Max(probe.Duration, 1)
	fps := min(10, max(1, int(600/duration)))
	barsWidth := float64((width - 2*waveformMargin) / (waveformBar + waveformGap) * (waveformBar + waveformGap))
	progress := fmt.Sprintf("(%d+T/%f*%f)/%d", waveformMargin, duration, barsWidth, width)
	filter := fmt.Sprintf("[0:v][1:v]blend=all_expr='if(lt(X/W\\,%s)\\,A\\,B)',format=yuv420p[v]", progress)
	rate := strconv.Itoa(fps)
	// the audio is the page player's mp3, copied as it is (an mp3 original plays as it is
	// too); a set too long for the size budget gets a lower bitrate copy instead, the still
	// picture costs little
	audioFile := src.resolved
	if !streamsOriginal(src) {
		audioFile = c.path(src, sharePreviewAudioSuffix)
		if _, err := os.Stat(audioFile); err != nil {
			// the job holds the file, so the mp3 is made here instead of in a job of its own
			if err := c.genAudioPreview(ctx, src); err != nil {
				return err
			}
		}
	}
	limit := duration
	audioArgs := []string{"-c:a", "copy"}
	if info, err := os.Stat(audioFile); err != nil || float64(info.Size())*8+16_000*duration > cardVideoBudget {
		audioRate := min(192_000, float64(cardVideoBudget)/duration-16_000)
		if audioRate < 64_000 {
			audioRate = 64_000
			limit = cardVideoBudget / (64_000 + 16_000)
		}
		audioArgs = []string{"-c:a", "libmp3lame", "-b:a", strconv.Itoa(int(audioRate)), "-ac", "2", "-ar", "44100"}
	}
	args := append([]string{}, "-v", "error", "-y",
		"-loop", "1", "-framerate", rate, "-i", "file:"+stills[0],
		"-loop", "1", "-framerate", rate, "-i", "file:"+stills[1],
		"-i", "file:"+audioFile, "-filter_complex", filter, "-map", "[v]", "-map", "2:a:0",
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "stillimage", "-crf", "30", "-r", rate,
		// keyframes are most of the size of a still picture: one a minute
		"-g", strconv.Itoa(60*fps),
	)
	args = append(args, audioArgs...)
	args = append(args, "-shortest", "-t", strconv.FormatFloat(limit, 'f', 3, 64), "-fs", cardVideoHardLimit,
		"-movflags", "+faststart", "-f", "mp4", "file:"+tmpVideo)
	out, err := mediaCommand(ctx, "ffmpeg", args...).CombinedOutput()
	if err != nil {
		os.Remove(tmpVideo)
		return fmt.Errorf("ffmpeg card video: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmpVideo, c.path(src, sharePreviewCardVideoSuffix))
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
	return renderWaveformWith(peaks, width, height, func(t float64) color.RGBA {
		// magenta to amber across the timeline
		return color.RGBA{uint8(236 + 14*t), uint8(64 + 120*t), uint8(140 - 100*t), 255}
	})
}

// the bar layout of renderWaveform: bars start at this margin and step by bar + gap
const waveformBar, waveformGap, waveformMargin = 4, 2, 60

// renderWaveformWith draws the waveform with the bar color picked by the position on the
// timeline, from 0 to 1
func renderWaveformWith(peaks []float64, width, height int, barColor func(t float64) color.RGBA) *image.RGBA {
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
	const bar, gap, margin = waveformBar, waveformGap, waveformMargin
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
		c := barColor(float64(i) / float64(bars))
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
	if len(share.Paths) != 1 {
		return nil, nil
	}
	return getPreviewSource(connection, share.Paths[0])
}

// hasListThumb tells the file lists which entries get a tiny thumbnail: images and videos.
// Audio would need a full decode for its waveform, too slow for a folder of stems
func hasListThumb(name string) bool {
	if sharePreviews == nil {
		return false
	}
	kind := sharePreviewExts[strings.ToLower(path.Ext(name))]
	return kind == sharePreviewImage || kind == sharePreviewVideo
}

// getListThumbURL versions the URL with the file modification time, so browsers keep the
// thumbnail until the file changes
func getListThumbURL(baseWebPath, dir string, info os.FileInfo) string {
	return fmt.Sprintf("%s?path=%s&v=%d", baseWebPath, url.QueryEscape(path.Join(dir, info.Name())), info.ModTime().Unix())
}

// getPreviewSource validates a file the connection may read for previews, nil means no preview
func getPreviewSource(connection *Connection, name string) (*sharePreviewSource, error) {
	if sharePreviews == nil {
		return nil, nil
	}
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

// formatPreviewAudio describes the source audio, for example "Stereo | 48 kHz | 24-bit"
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
// formatPreviewVideo describes the video stream, for example "H.264 | 109 Mbps"
func formatPreviewVideo(probe sharePreviewProbe) []string {
	var parts []string
	if probe.VideoCodec != "" {
		names := map[string]string{"h264": "H.264", "hevc": "HEVC", "prores": "ProRes", "av1": "AV1",
			"vp9": "VP9", "vp8": "VP8", "mpeg4": "MPEG-4", "dnxhd": "DNxHD", "mjpeg": "MJPEG"}
		name, ok := names[probe.VideoCodec]
		if !ok {
			name = strings.ToUpper(probe.VideoCodec)
		}
		parts = append(parts, name)
	}
	switch rate := float64(probe.VideoBitRate); {
	case rate >= 10e6:
		parts = append(parts, fmt.Sprintf("%.0f Mbps", rate/1e6))
	case rate >= 1e6:
		parts = append(parts, fmt.Sprintf("%.1f Mbps", rate/1e6))
	case rate > 0:
		parts = append(parts, fmt.Sprintf("%.0f kbps", rate/1e3))
	}
	return parts
}

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
	base := getRequestBaseURL(r) + path.Join(webClientPubSharesPath, share.ShareID)
	title := share.Name
	if title == "" {
		title = path.Base(src.name)
	}
	return buildPreview(r, src, connection, title, siteName, previewURLs{
		Page: base + "/download",
		// the original file, what the Download button points to
		Download: base,
		Media:    base + "/preview/media",
		Thumb:    base + "/preview/thumb",
		Peaks:    base + "/preview/peaks",
		Video:    base + "/preview/video",
	})
}

// previewURLs are the absolute addresses a preview page uses: shares, files inside folder shares
// and the logged-in file browser each have their own
type previewURLs struct {
	Page     string
	Download string
	Media    string
	Thumb    string
	Peaks    string
	// Video is the waveform card video of audio files, only for shares
	Video string
}

// buildPreview probes a file and returns its preview page data, starting the thumbnail in the
// background so crawlers that fetch it next find it ready or in progress
func buildPreview(r *http.Request, src *sharePreviewSource, connection *Connection, title, siteName string,
	urls previewURLs,
) *sharePreview {
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
	enqueueCacheSweep(filepath.Dir(src.resolved))
	// the thumbnail is made at the priority of the page (share or browse), without its deadline
	warmCtx := context.WithoutCancel(r.Context())
	go func() {
		_, err := sharePreviews.ensure(warmCtx, src, sharePreviewThumbSuffix, sharePreviews.genThumb)
		if err != nil {
			logger.Warn(logSender, "", "unable to generate share preview for %q: %v", src.name, err)
		}
	}()

	preview := &sharePreview{
		Kind:        src.kind,
		Title:       title,
		SiteName:    siteName,
		PageURL:     urls.Page,
		DownloadURL: urls.Download,
		MediaURL:    urls.Media,
		ThumbURL:    urls.Thumb,
		Width:       probe.Width,
		Height:      probe.Height,
	}
	details := []string{util.ByteCountIEC(src.info.Size())}
	switch src.kind {
	case sharePreviewImage:
		details = append([]string{"Image", fmt.Sprintf("%dx%d", probe.Width, probe.Height)}, details...)
		for _, v := range []string{probe.Lens, probe.Camera} {
			if v != "" {
				details = append(details[:2], append([]string{v}, details[2:]...)...)
			}
		}
	case sharePreviewAudio:
		preview.MediaType = "audio/mpeg"
		preview.OriginalAudio = streamsOriginal(src)
		preview.PeaksURL = urls.Peaks
		details = append(append([]string{"Audio", formatPreviewDuration(probe.Duration)}, formatPreviewAudio(probe)...),
			details...)
		preview.Width, preview.Height = 1200, 630
		// advertised once rendered: a link preview that finds no video keeps the plain card
		if urls.Video != "" {
			if cardVideoReady(src) {
				preview.OGVideo = true
				preview.VideoURL, preview.VideoType = cardVideoURL(src, urls.Video), "video/mp4"
			} else {
				queueCardVideo(src)
			}
		}
	case sharePreviewVideo:
		preview.MediaType = sharePreviewMediaTypes[strings.ToLower(path.Ext(src.name))]
		switch {
		case probe.VideoCodec == "h264" && src.info.Size() <= sharePreviewOGVideoMaxSize:
			preview.OGVideo = true
			preview.VideoURL, preview.VideoType = urls.Media, preview.MediaType
		case urls.Video != "" && cardVideoReady(src):
			// too large or a codec chat apps do not play: the small H.264 copy
			preview.OGVideo = true
			preview.VideoURL, preview.VideoType = cardVideoURL(src, urls.Video), "video/mp4"
			// ffmpeg rounds the scaled side to an even number
			preview.VideoWidth, preview.VideoHeight = fitInBox(probe.Width, probe.Height, videoPreviewBox)
			preview.VideoWidth, preview.VideoHeight = preview.VideoWidth&^1, preview.VideoHeight&^1
			// share pages play the copy too, the original only comes with Download
			preview.MediaURL, preview.MediaType, preview.VideoCopy = preview.VideoURL, "video/mp4", true
		case urls.Video != "":
			queueCardVideo(src)
			preview.PreparingVideo, preview.PreparingURL = true, urls.Video
		}
		videoDetails := []string{"Video", formatPreviewDuration(probe.Duration), fmt.Sprintf("%dx%d", probe.Width, probe.Height)}
		if fps := formatFrameRate(probe.FrameRate); fps != "" {
			videoDetails = append(videoDetails, fps)
		}
		videoDetails = append(videoDetails, formatPreviewVideo(probe)...)
		for _, v := range []string{probe.Camera, probe.Lens, probe.Source} {
			if v != "" {
				videoDetails = append(videoDetails, v)
			}
		}
		details = append(videoDetails, details...)
	}
	preview.Details = details[1:] // the page shows the kind as the file type chip instead
	preview.FileType = strings.ToUpper(strings.TrimPrefix(path.Ext(src.name), "."))
	preview.Size = util.ByteCountIEC(src.info.Size())
	preview.Description = strings.Join(details, " | ")
	switch src.kind {
	case sharePreviewAudio:
		preview.ThumbWidth, preview.ThumbHeight = 1200, 630
	case sharePreviewVideo:
		preview.ThumbWidth, preview.ThumbHeight = fitInBox(probe.Width, probe.Height, 1280)
	default:
		preview.ThumbWidth, preview.ThumbHeight = fitInBox(probe.Width, probe.Height, 1600)
	}
	// link previews cache pictures by address: the share card is "pending", the real picture
	// is versioned by its file, so a later look at the page finds the real one
	version := "pending"
	if info, err := os.Stat(sharePreviews.path(src, sharePreviewThumbSuffix)); err == nil {
		version = strconv.FormatInt(info.ModTime().Unix(), 10)
		if src.kind == sharePreviewAudio {
			// the text and logo are stamped when served: a new card style is a new address
			version += "-" + sharePreviewCardStyle
		}
	} else {
		preview.ThumbWidth, preview.ThumbHeight = 1200, 630
	}
	separator := "?"
	if strings.Contains(preview.ThumbURL, "?") {
		separator = "&"
	}
	preview.ThumbURL += separator + "v=" + version
	return preview
}

func (s *httpdServer) getSharePreviewForRequest(w http.ResponseWriter, r *http.Request,
) (*sharePreviewSource, *dataprovider.Share, *Connection, bool) {
	validScopes := []dataprovider.ShareScope{dataprovider.ShareScopeRead, dataprovider.ShareScopeReadWrite}
	share, connection, err := s.checkPublicShare(w, r, validScopes)
	if err != nil {
		return nil, nil, nil, false
	}
	var src *sharePreviewSource
	if r.URL.Query().Has("path") {
		// a file inside a folder share, for the thumbnails of its file list
		if err = validateBrowsableShare(share, connection); err == nil {
			var name string
			if name, err = getBrowsableSharedPath(share.Paths[0], r); err == nil {
				src, err = getPreviewSource(connection, name)
			}
		}
	} else {
		src, err = getSharePreviewSource(&share, connection)
	}
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
	modTime := src.info.ModTime()
	if info, err := f.Stat(); err == nil {
		// the asset time: a poster replaced by a better pick is a new version
		modTime = info.ModTime()
	}
	if suffix == sharePreviewThumbSuffix || suffix == sharePreviewSmallSuffix {
		sharePreviews.refineExistingPoster(r.Context(), src)
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// revalidated every time (a cheap 304 when unchanged), posters are refined in place
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", modTime, f)
}

func (s *httpdServer) handleShareGetPreviewThumb(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withTaskPriority(r.Context(), priorityShare))
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	if r.URL.Query().Get("size") == "small" {
		s.serveListThumb(w, r, src)
		return
	}
	s.serveSharePicture(w, r, src)
}

// serveSharePicture is the picture of a share page and of its link previews. Crawlers give up
// after a few seconds: a picture not ready within 2 seconds is answered with the share card
// while it keeps generating, the page then points to the real picture under a new address
func (s *httpdServer) serveSharePicture(w http.ResponseWriter, r *http.Request, src *sharePreviewSource) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := sharePreviews.ensure(ctx, src, sharePreviewThumbSuffix, sharePreviews.genThumb); err != nil {
		s.serveShareCard(w, r, src.kind)
		return
	}
	if src.kind == sharePreviewAudio {
		s.serveBrandedWaveform(w, r, src)
		return
	}
	s.serveSharePreviewAsset(w, r, src, sharePreviewThumbSuffix, "image/jpeg", sharePreviews.genThumb)
}

// brandingLogo is the logo the web client pages show, resolved the same way they resolve it:
// the logo of the branding settings (database), else the logo_path of the configuration, else
// the default one. Read at every use, so a branding change applies at once. nil when the logo
// cannot be decoded (for example an SVG)
func (s *httpdServer) brandingLogo() (image.Image, time.Time) {
	branding := s.binding.webClientBranding()
	if branding.LogoPath == path.Join("/", "branding", "webclient", "logo.png") {
		if data := dbBrandingConfig.getWebClientLogo(); len(data) > 0 {
			logo, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				return nil, time.Time{}
			}
			var updated time.Time
			if configs, err := dataprovider.GetConfigs(); err == nil {
				updated = util.GetTimeFromMsecSinceEpoch(configs.UpdatedAt)
			}
			return logo, updated
		}
	}
	file := filepath.Join(s.staticFilesPath, filepath.FromSlash(strings.TrimPrefix(branding.LogoPath, "/")))
	f, err := os.Open(file)
	if err != nil {
		return nil, time.Time{}
	}
	defer f.Close()
	logo, _, err := image.Decode(f)
	if err != nil {
		return nil, time.Time{}
	}
	var updated time.Time
	if info, err := f.Stat(); err == nil {
		updated = info.ModTime()
	}
	return logo, updated
}

// serveShareCard answers with the placeholder link preview, never cached by browsers
func (s *httpdServer) serveShareCard(w http.ResponseWriter, r *http.Request, kind string) {
	logo, _ := s.brandingLogo()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, renderShareCard(kind, logo), &jpeg.Options{Quality: 88}); err != nil {
		sendAPIResponse(w, r, err, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// renderShareCard draws the placeholder picture of a share: the branding logo on the dark
// gradient of the waveforms, with a play button for videos and soft bars for audio
func renderShareCard(kind string, logo image.Image) *image.RGBA {
	const width, height = 1200, 630
	img := renderWaveform(nil, width, height).(*image.RGBA)
	switch kind {
	case sharePreviewAudio:
		// decorative bars along the bottom, deterministic
		bar, gap := 6, 4
		for i := 0; (i+1)*(bar+gap) < width-80; i++ {
			v := 0.35 + 0.3*math.Abs(math.Sin(float64(i)*0.37)) + 0.2*math.Abs(math.Sin(float64(i)*0.11))
			h := int(v * 70)
			x0 := 40 + i*(bar+gap)
			for x := x0; x < x0+bar; x++ {
				for y := height - 60 - h; y < height-60; y++ {
					img.SetRGBA(x, y, color.RGBA{64, 58, 92, 255})
				}
			}
		}
	case sharePreviewVideo:
		// a play button under the logo
		cx, cy, radius := width/2, 470, 56
		for y := cy - radius; y <= cy+radius; y++ {
			for x := cx - radius; x <= cx+radius; x++ {
				dx, dy := x-cx, y-cy
				if dx*dx+dy*dy > radius*radius {
					continue
				}
				// triangle pointing right, inside the circle
				inTriangle := dx >= -18 && dx <= 26 && float64(absInt(dy)) <= float64(26-dx)*22.0/44.0
				if inTriangle {
					img.SetRGBA(x, y, color.RGBA{18, 16, 30, 255})
				} else {
					img.SetRGBA(x, y, color.RGBA{236, 64, 140, 255})
				}
			}
		}
	}
	if logo != nil {
		logoHeight := 200
		lb := logo.Bounds()
		logoWidth := lb.Dx() * logoHeight / max(lb.Dy(), 1)
		top := (height - logoHeight) / 2
		if kind == sharePreviewVideo {
			top = 110
		}
		stampLogoAt(img, logo, logoHeight, (width-logoWidth)/2, top)
	}
	return img
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func (s *httpdServer) serveListThumb(w http.ResponseWriter, r *http.Request, src *sharePreviewSource) {
	if src.kind == sharePreviewAudio {
		sendAPIResponse(w, r, nil, "Audio files have no list thumbnail", http.StatusNotFound)
		return
	}
	s.serveSharePreviewAsset(w, r, src, sharePreviewSmallSuffix, "image/jpeg", sharePreviews.genSmallThumb)
}

// getClientPreviewSource is the file of a logged-in preview request, the caller removes the
// connection
func getClientPreviewSource(w http.ResponseWriter, r *http.Request) (*sharePreviewSource, *Connection, bool) {
	connection, err := getUserConnection(w, r)
	if err != nil {
		return nil, nil, false
	}
	name := connection.User.GetCleanedPath(r.URL.Query().Get("path"))
	src, err := getPreviewSource(connection, name)
	if err != nil || src == nil {
		common.Connections.Remove(connection.GetID())
		if err == nil {
			err = util.NewRecordNotFoundError("no preview for this file")
		}
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return nil, nil, false
	}
	return src, connection, true
}

// handleClientGetPreviewThumb serves the thumbnail of a file of the logged-in file browser: the
// tiny square with size=small, the large preview image otherwise
func (s *httpdServer) handleClientGetPreviewThumb(w http.ResponseWriter, r *http.Request) {
	src, connection, ok := getClientPreviewSource(w, r)
	if !ok {
		return
	}
	defer common.Connections.Remove(connection.GetID())
	switch {
	case r.URL.Query().Get("size") == "small":
		s.serveListThumb(w, r, src)
	case src.kind == sharePreviewAudio:
		s.serveBrandedWaveform(w, r, src)
	default:
		s.serveSharePreviewAsset(w, r, src, sharePreviewThumbSuffix, "image/jpeg", sharePreviews.genThumb)
	}
}

func (s *httpdServer) handleClientGetPreviewMedia(w http.ResponseWriter, r *http.Request) {
	src, connection, ok := getClientPreviewSource(w, r)
	if !ok {
		return
	}
	defer common.Connections.Remove(connection.GetID())
	s.servePreviewMedia(w, r, src, connection, false)
}

func (s *httpdServer) handleClientGetPreviewPeaks(w http.ResponseWriter, r *http.Request) {
	src, connection, ok := getClientPreviewSource(w, r)
	if !ok {
		return
	}
	defer common.Connections.Remove(connection.GetID())
	if src.kind != sharePreviewAudio {
		sendAPIResponse(w, r, nil, "Peaks are only available for audio", http.StatusNotFound)
		return
	}
	s.serveSharePreviewAsset(w, r, src, sharePreviewPeaksSuffix, "application/json", sharePreviews.genWaveform)
}

// handleClientPreviewPage opens a file of the logged-in file browser in the preview page, files
// without a preview are downloaded as before
func (s *httpdServer) handleClientPreviewPage(w http.ResponseWriter, r *http.Request) {
	connection, err := getUserConnection(w, r)
	if err != nil {
		return
	}
	defer common.Connections.Remove(connection.GetID())
	name := connection.User.GetCleanedPath(r.URL.Query().Get("path"))
	base := getRequestBaseURL(r)
	q := "?path=" + url.QueryEscape(name)
	download := base + webClientFilesPath + q
	var preview *sharePreview
	if src, err := getPreviewSource(connection, name); err == nil && src != nil {
		data := s.getBaseClientPageData(util.I18nShareDownloadTitle, "", w, r)
		preview = buildPreview(r, src, connection, path.Base(name), data.Branding.Name, previewURLs{
			Page:     base + webClientPreviewPath + q,
			Download: download,
			Media:    base + webClientPreviewMediaPath + q,
			Thumb:    base + webClientPreviewThumbPath + q,
			Peaks:    base + webClientPreviewPeaksPath + q,
		})
		if preview != nil {
			data.LogoutURL = ""
			renderClientTemplate(w, templateShareDownload, shareDownloadPage{
				baseClientPage: data,
				DownloadLink:   download,
				Preview:        preview,
				BackURL:        base + webClientFilesPath + "?path=" + url.QueryEscape(path.Dir(name)),
			})
			return
		}
	}
	http.Redirect(w, r, download, http.StatusFound)
}

// handleShareViewFile opens a file inside a folder share in the preview page, files without a
// preview are downloaded as before
func (s *httpdServer) handleShareViewFile(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withTaskPriority(r.Context(), priorityShare))
	validScopes := []dataprovider.ShareScope{dataprovider.ShareScopeRead, dataprovider.ShareScopeReadWrite}
	share, connection, err := s.checkPublicShare(w, r, validScopes)
	if err != nil {
		return
	}
	defer connection.CloseFS()
	if err := validateBrowsableShare(share, connection); err != nil {
		s.renderClientMessagePage(w, r, util.I18nShareAccessErrorTitle, getRespStatus(err), err, "")
		return
	}
	name, err := getBrowsableSharedPath(share.Paths[0], r)
	if err != nil {
		s.renderClientMessagePage(w, r, util.I18nShareAccessErrorTitle, getRespStatus(err), err, "")
		return
	}
	rel := share.GetRelativePath(name)
	base := getRequestBaseURL(r) + path.Join(webClientPubSharesPath, share.ShareID)
	q := "?path=" + url.QueryEscape(rel)
	download := base + "/browse" + q
	if src, err := getPreviewSource(connection, name); err == nil && src != nil {
		data := s.getBaseClientPageData(util.I18nShareDownloadTitle, "", w, r)
		preview := buildPreview(r, src, connection, path.Base(name), data.Branding.Name, previewURLs{
			Page:     base + "/view" + q,
			Download: download,
			Media:    base + "/preview/media" + q,
			Thumb:    base + "/preview/thumb" + q,
			Peaks:    base + "/preview/peaks" + q,
		})
		if preview != nil {
			data.LogoutURL = ""
			if share.Password != "" {
				data.LogoutURL = path.Join(webClientPubSharesPath, share.ShareID, "logout")
			}
			renderClientTemplate(w, templateShareDownload, shareDownloadPage{
				baseClientPage: data,
				DownloadLink:   download,
				Preview:        preview,
				BackURL:        base + "/browse?path=" + url.QueryEscape(path.Dir(rel)),
			})
			return
		}
	}
	http.Redirect(w, r, download, http.StatusFound)
}

// getPreviewPageURL is where a file of a list opens: its preview page when it has one
func getPreviewPageURL(baseWebPath, dir, name string) string {
	if sharePreviews == nil {
		return ""
	}
	if _, ok := sharePreviewExts[strings.ToLower(path.Ext(name))]; !ok {
		return ""
	}
	return baseWebPath + "?path=" + url.QueryEscape(path.Join(dir, name))
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
	stampWaveformText(img, src)
	modTime := src.info.ModTime()
	if logo, updated := s.brandingLogo(); logo != nil {
		stampLogo(img, logo, img.Bounds().Dy()/7, img.Bounds().Dy()/18)
		if updated.After(modTime) {
			modTime = updated
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
	stampLogoAt(dst, logo, height, dst.Bounds().Max.X-margin-width, dst.Bounds().Max.Y-margin-height)
}

// stampLogoAt draws the logo in white with its top left corner at x0, y0
func stampLogoAt(dst *image.RGBA, logo image.Image, height, x0, y0 int) {
	lb := logo.Bounds()
	if lb.Dx() == 0 || lb.Dy() == 0 || height <= 0 {
		return
	}
	width := lb.Dx() * height / lb.Dy()
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
	r = r.WithContext(withTaskPriority(r.Context(), priorityShare))
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	if src.kind != sharePreviewAudio {
		sendAPIResponse(w, r, nil, "Peaks are only available for audio", http.StatusNotFound)
		return
	}
	s.serveSharePreviewAsset(w, r, src, sharePreviewPeaksSuffix, "application/json", sharePreviews.genWaveform)
}

// handleShareGetPreviewVideo serves the card video, the og:video of link previews: the waveform
// video of audio shares, the small H.264 copy of video shares. Not a share use, like the other preview assets
func (s *httpdServer) handleShareGetPreviewVideo(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withTaskPriority(r.Context(), priorityShare))
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	if src.kind != sharePreviewAudio && src.kind != sharePreviewVideo {
		sendAPIResponse(w, r, nil, "The card video is only available for audio and video", http.StatusNotFound)
		return
	}
	cardVideoReady(src) // drops a video showing an old logo
	s.serveSharePreviewAsset(w, r, src, sharePreviewCardVideoSuffix, "video/mp4", sharePreviews.genCardVideo)
}

// handleShareGetPreviewMedia serves what the page player loads: the 192 kbps mp3 for audio,
// the original file for video and the scaled image for images. It does not count as a share use, since players
// issue many range requests while seeking
func (s *httpdServer) handleShareGetPreviewMedia(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withTaskPriority(r.Context(), priorityShare))
	src, _, connection, ok := s.getSharePreviewForRequest(w, r)
	if !ok {
		return
	}
	defer connection.CloseFS()
	s.servePreviewMedia(w, r, src, connection, true)
}

// servePreviewMedia serves what the page player loads: the 192 kbps mp3 for audio, the original
// file for video and the scaled image for images. Share connections are registered here, the
// logged-in ones already are
func (s *httpdServer) servePreviewMedia(w http.ResponseWriter, r *http.Request, src *sharePreviewSource,
	connection *Connection, register bool,
) {
	switch {
	case src.kind == sharePreviewAudio && !streamsOriginal(src):
		s.serveSharePreviewAsset(w, r, src, sharePreviewAudioSuffix, "audio/mpeg", sharePreviews.genAudioPreview)
	case src.kind == sharePreviewImage:
		s.serveSharePreviewAsset(w, r, src, sharePreviewThumbSuffix, "image/jpeg", sharePreviews.genThumb)
	default:
		if register {
			if err := common.Connections.Add(connection); err != nil {
				sendAPIResponse(w, r, err, "Unable to add connection", http.StatusTooManyRequests)
				return
			}
			defer common.Connections.Remove(connection.GetID())
		}
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

// warmSharePreview queues the preview of a single-file share at share priority as soon as the
// share is created or edited, so the picture link previews fetch is ready before the link is
// posted: crawlers give up after a few seconds. Dead shares are skipped
func warmSharePreview(share dataprovider.Share) {
	if sharePreviews == nil || len(share.Paths) != 1 || isDeadShare(&share) {
		return
	}
	if _, ok := sharePreviewExts[strings.ToLower(path.Ext(share.Paths[0]))]; !ok {
		return
	}
	user, err := dataprovider.GetUserWithGroupSettings(share.Username, "")
	if err != nil {
		return
	}
	user.Filters.FilePatterns = hideDotEntriesInShares(user.Filters.FilePatterns)
	conn := newConnection(common.NewBaseConnection(xid.New().String(), common.ProtocolHTTPShare, "", "", user), nil, nil)
	src, err := getPreviewSource(conn, share.Paths[0])
	if err != nil || src == nil {
		return
	}
	assets := []struct {
		suffix string
		gen    func(context.Context, *sharePreviewSource) error
	}{{sharePreviewProbeSuffix, sharePreviews.genProbe}, {sharePreviewThumbSuffix, sharePreviews.genThumb}}
	if src.kind == sharePreviewAudio && !streamsOriginal(src) {
		assets = append(assets, struct {
			suffix string
			gen    func(context.Context, *sharePreviewSource) error
		}{sharePreviewAudioSuffix, sharePreviews.genAudioPreview})
	}
	cardVideo := (src.kind == sharePreviewAudio || src.kind == sharePreviewVideo) && !cardVideoReady(src)
	for _, asset := range assets {
		p := sharePreviews.path(src, asset.suffix)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		gen := asset.gen
		_, _ = tasks.submit("preview", previewJobName(src, asset.suffix), []string{src.resolved}, priorityShare, 0,
			10*time.Minute,
			func(ctx context.Context) error {
				if _, err := os.Stat(p); err == nil {
					return nil
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o775); err != nil {
					return err
				}
				return gen(ctx, src)
			})
	}
	if cardVideo {
		queueCardVideo(src)
	}
}

const (
	posterCandidates = 9
	posterSampleSize = 160 // candidates are scored on tiny frames
)

// pickPosterTime picks the moment for a video poster the way YouTube does, sample, filter,
// score, with cheap measures instead of a neural network: 9 moments between 10% and 90% of the
// video, 3 consecutive tiny frames each; near-black frames and frames on a cut or a flash are
// dropped, the rest scored on stillness (concert lights fool sharpness measures, motion between
// frames is what blurs), contrast, colorfulness and exposure
func (c *sharePreviewCache) pickPosterTime(ctx context.Context, src *sharePreviewSource, probe sharePreviewProbe) float64 {
	if probe.Duration <= 0 {
		return 0
	}
	best, bestScore := probe.Duration/2, math.Inf(-1)
	for i := range posterCandidates {
		t := probe.Duration * (0.1 + 0.8*float64(i)/float64(posterCandidates-1))
		frames, err := readTinyFrames(ctx, src.resolved, t, 3)
		if err != nil || len(frames) < 3 {
			continue
		}
		if score := scorePosterFrames(frames); score > bestScore {
			// the middle frame of the three
			best, bestScore = t+1/math.Max(probe.FrameRate, 1), score
		}
	}
	return best
}

// readTinyFrames decodes count frames from t, scaled to posterSampleSize squared, as RGB
func readTinyFrames(ctx context.Context, file string, t float64, count int) ([][]byte, error) {
	out, err := mediaCommand(ctx, "ffmpeg", "-v", "error", "-ss", fmt.Sprintf("%.3f", t), "-i", "file:"+file,
		"-an", "-sn", "-frames:v", strconv.Itoa(count),
		"-vf", fmt.Sprintf("scale=%[1]d:%[1]d", posterSampleSize), "-pix_fmt", "rgb24", "-f", "rawvideo", "pipe:1").Output()
	if err != nil {
		return nil, err
	}
	size := posterSampleSize * posterSampleSize * 3
	var frames [][]byte
	for len(out) >= size {
		frames = append(frames, out[:size])
		out = out[size:]
	}
	return frames, nil
}

// scorePosterFrames scores three consecutive RGB frames, the middle one being the candidate
func scorePosterFrames(frames [][]byte) float64 {
	mid := frames[1]
	n := float64(len(mid) / 3)
	var sumL, sumL2, sumRG, sumRG2, sumYB, sumYB2, motion float64
	for i := 0; i+2 < len(mid); i += 3 {
		r, g, b := float64(mid[i]), float64(mid[i+1]), float64(mid[i+2])
		l := (0.299*r + 0.587*g + 0.114*b) / 255
		sumL += l
		sumL2 += l * l
		rg, yb := r-g, 0.5*(r+g)-b
		sumRG += rg
		sumRG2 += rg * rg
		sumYB += yb
		sumYB2 += yb * yb
		before := (0.299*float64(frames[0][i]) + 0.587*float64(frames[0][i+1]) + 0.114*float64(frames[0][i+2])) / 255
		after := (0.299*float64(frames[2][i]) + 0.587*float64(frames[2][i+1]) + 0.114*float64(frames[2][i+2])) / 255
		motion += math.Abs(after - before)
	}
	meanL := sumL / n
	stdL := math.Sqrt(math.Max(sumL2/n-meanL*meanL, 0))
	meanRG, meanYB := sumRG/n, sumYB/n
	stdRG := math.Sqrt(math.Max(sumRG2/n-meanRG*meanRG, 0))
	stdYB := math.Sqrt(math.Max(sumYB2/n-meanYB*meanYB, 0))
	// Hasler and Suesstrunk colorfulness
	colorfulness := math.Sqrt(stdRG*stdRG+stdYB*stdYB) + 0.3*math.Sqrt(meanRG*meanRG+meanYB*meanYB)
	motion /= n
	switch {
	case meanL < 0.06:
		return -2 // black or nearly
	case motion > 0.25:
		return -1 // a cut, a flash or a whip pan
	}
	// dark venues: aim at a moderately dark mean rather than mid grey
	exposure := 1 - math.Min(1, math.Abs(meanL-0.38)/0.38)
	return 0.35*math.Min(1, stdL/0.25) + 0.25*math.Min(1, colorfulness/60) +
		0.25*(1-math.Min(1, motion/0.12)) + 0.15*exposure
}

// refinePosterLater queues the poster pick of a video one priority below the request that
// needed the poster: the middle frame is shown meanwhile, then replaced in place
func (c *sharePreviewCache) refinePosterLater(ctx context.Context, src *sharePreviewSource) {
	if src.kind != sharePreviewVideo {
		return
	}
	priority := min(taskPriorityFrom(ctx)+1, priorityMaintenance)
	_, _ = tasks.submit("poster", src.key, []string{src.resolved}, priority, time.Hour, 10*time.Minute,
		func(ctx context.Context) error {
			probe, err := c.loadProbe(ctx, src)
			if err != nil || probe.PosterVersion == sharePreviewPosterVersion {
				return err
			}
			probe.PosterTime = c.pickPosterTime(ctx, src, probe)
			probe.PosterVersion = sharePreviewPosterVersion
			if err := c.writeProbe(src, probe); err != nil {
				return err
			}
			// redo the posters that exist, at the chosen moment
			for _, poster := range []struct{ suffix, filter string }{
				{sharePreviewThumbSuffix, scaleFilter(1280)},
				{sharePreviewSmallSuffix, squareCropFilter(sharePreviewSmallBox)},
			} {
				if _, err := os.Stat(c.path(src, poster.suffix)); err != nil {
					continue
				}
				if err := c.renderStill(ctx, src, poster.filter, poster.suffix); err != nil {
					return err
				}
			}
			return nil
		})
}

// refineExistingPoster queues the pick for a video poster made before it existed
func (c *sharePreviewCache) refineExistingPoster(ctx context.Context, src *sharePreviewSource) {
	if src.kind != sharePreviewVideo {
		return
	}
	data, err := os.ReadFile(c.path(src, sharePreviewProbeSuffix))
	if err != nil {
		return
	}
	var probe sharePreviewProbe
	if json.Unmarshal(data, &probe) == nil && probe.PosterVersion != sharePreviewPosterVersion {
		c.refinePosterLater(ctx, src)
	}
}

// mediaCommand runs ffmpeg, ffprobe or exiftool at the CPU and disk priority of the task that
// needs it: share work at normal priority, browsing a little lower, maintenance at idle disk
// priority. The kernel then hands share work the CPU and the disks the moment it needs them,
// even while other tasks keep running
func mediaCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	switch taskPriorityFrom(ctx) {
	case priorityBrowse:
		return exec.CommandContext(ctx, "nice", append([]string{"-n", "5", name}, args...)...)
	case priorityMaintenance:
		return exec.CommandContext(ctx, "nice", append([]string{"-n", "15", "ionice", "-c", "3", name}, args...)...)
	default:
		return exec.CommandContext(ctx, name, args...)
	}
}

// streamsOriginal: the player plays the original file, no preview copy is made (videos, mp3)
func streamsOriginal(src *sharePreviewSource) bool {
	if src.kind == sharePreviewVideo {
		return true
	}
	return src.kind == sharePreviewAudio && strings.EqualFold(filepath.Ext(src.resolved), ".mp3")
}
