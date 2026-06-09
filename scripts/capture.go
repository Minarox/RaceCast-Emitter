package scripts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"racecast-emitter/utils"

	"github.com/fsnotify/fsnotify"
)

// StreamKind identifies whether a capture stream carries video or audio.
type StreamKind int

const (
	VideoStream StreamKind = iota
	AudioStream
)

// StreamInfo describes a running capture stream exposed by CaptureManager.
type StreamInfo struct {
	Key      string                   // "/dev/videoN" or "hw:X,Y" or "fake-*"
	Name     string                   // track name for LiveKit
	Kind     StreamKind
	Frames   <-chan utils.Frame       // encoded frames; closed when pipeline stops
	Pipeline *utils.GStreamerPipeline // for ForceKeyframe on new subscribers
	Channels int                      // audio: channel count (0 for video)
}

// CaptureOptions configures the CaptureManager at creation time.
type CaptureOptions struct {
	NoCam bool
	NoMic bool
	Fake  bool
}

// CaptureManager starts and monitors GStreamer pipelines for cameras and
// microphones declared in devices.yaml.  It has no dependency on LiveKit:
// consumers subscribe via OnStreamAdded/OnStreamRemoved callbacks and read
// encoded frames from StreamInfo.Frames.
//
// Thread-safe.
type CaptureManager struct {
	mu        sync.Mutex
	opts      CaptureOptions
	devCfg    utils.DevicesConfig
	pipelines map[string]*utils.GStreamerPipeline // key → pipeline
	pending   map[string]struct{}                 // devices being initialised
	stopCh    chan struct{}
	wg        sync.WaitGroup
	onAdded   func(StreamInfo)
	onRemoved func(string)
}

// NewCaptureManager creates a CaptureManager with the given options.
func NewCaptureManager(opts CaptureOptions) *CaptureManager {
	return &CaptureManager{
		pipelines: make(map[string]*utils.GStreamerPipeline),
		pending:   make(map[string]struct{}),
		stopCh:    make(chan struct{}),
		opts:      opts,
	}
}

// OnStreamAdded registers a callback invoked whenever a new capture stream
// becomes available.  Must be called before Start.
func (cm *CaptureManager) OnStreamAdded(fn func(StreamInfo)) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.onAdded = fn
}

// OnStreamRemoved registers a callback invoked whenever a capture stream stops
// (device disconnected or pipeline error).  Must be called before Start.
func (cm *CaptureManager) OnStreamRemoved(fn func(string)) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.onRemoved = fn
}

// Start loads devices.yaml, starts pipelines for currently connected devices,
// and launches the hot-plug watcher goroutine.
func (cm *CaptureManager) Start() {
	cfg, err := utils.LoadDevicesConfig()
	if err != nil {
		utils.Log.Warnw("Failed to load devices.yaml; no devices will be started.", "error", err)
	} else {
		cm.devCfg = cfg
		utils.Log.Infow("Device config loaded.",
			"cameras", len(cfg.Cameras), "microphones", len(cfg.Microphones))
	}

	if cm.opts.Fake {
		if !cm.opts.NoCam {
			cm.startFakeVideo()
		}
		if !cm.opts.NoMic {
			cm.startFakeAudio()
		}
		return
	}

	cm.startFromConfig()

	cm.wg.Add(1)
	go cm.watchHotplug()
}

// Stop shuts down all running pipelines and the hot-plug watcher.
func (cm *CaptureManager) Stop() {
	close(cm.stopCh)
	cm.wg.Wait()

	cm.mu.Lock()
	defer cm.mu.Unlock()
	for key, p := range cm.pipelines {
		p.Stop()
		p.Wait()
		p.Free()
		delete(cm.pipelines, key)
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func (cm *CaptureManager) notifyAdded(info StreamInfo) {
	cm.mu.Lock()
	fn := cm.onAdded
	cm.mu.Unlock()
	if fn != nil {
		fn(info)
	}
}

func (cm *CaptureManager) notifyRemoved(key string) {
	cm.mu.Lock()
	fn := cm.onRemoved
	cm.mu.Unlock()
	if fn != nil {
		fn(key)
	}
}

// ---------------------------------------------------------------------------
// Config matching
// ---------------------------------------------------------------------------

func (cm *CaptureManager) cameraConfigFor(device string) (utils.CameraEntry, bool) {
	if len(cm.devCfg.Cameras) == 0 {
		return utils.CameraEntry{}, false
	}
	uid := utils.VideoDeviceUID(device)
	for _, entry := range cm.devCfg.Cameras {
		if uid != "" && entry.UID == uid {
			if entry.Disabled {
				return utils.CameraEntry{}, false
			}
			return entry, true
		}
	}
	return utils.CameraEntry{}, false
}

func (cm *CaptureManager) micConfigFor(device string) (utils.MicrophoneEntry, bool) {
	if len(cm.devCfg.Microphones) == 0 {
		return utils.MicrophoneEntry{}, false
	}
	var cardNum int
	fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(device, "plughw:"), "hw:"), "%d", &cardNum)
	uid := utils.AudioDeviceUID(cardNum)
	for _, entry := range cm.devCfg.Microphones {
		if uid != "" && entry.UID == uid {
			if entry.Disabled {
				return utils.MicrophoneEntry{}, false
			}
			return entry, true
		}
	}
	return utils.MicrophoneEntry{}, false
}

// ---------------------------------------------------------------------------
// Startup from config
// ---------------------------------------------------------------------------

func (cm *CaptureManager) startFromConfig() {
	if !cm.opts.NoCam {
		for _, entry := range cm.devCfg.Cameras {
			if entry.Disabled {
				utils.Log.Infow("Camera disabled in devices.yaml.", "uid", entry.UID)
				continue
			}
			dev, ok := utils.VideoDeviceByUID(entry.UID)
			if !ok {
				utils.Log.Infow("Camera UID not found at startup.", "uid", entry.UID)
				continue
			}
			cm.addVideoDevice(dev)
		}
	}

	if !cm.opts.NoMic {
		for _, entry := range cm.devCfg.Microphones {
			if entry.Disabled {
				utils.Log.Infow("Microphone disabled in devices.yaml.", "uid", entry.UID)
				continue
			}
			cardNum, ok := utils.AudioCardByUID(entry.UID)
			if !ok {
				utils.Log.Infow("Microphone UID not found at startup.", "uid", entry.UID)
				continue
			}
			for _, info := range utils.ListAudioDevices() {
				if info.CardNum == cardNum {
					cm.addAudioDevice(info.Device, info.Name)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Hot-plug watcher
// ---------------------------------------------------------------------------

var (
	videoNodeRe = regexp.MustCompile(`^/dev/video\d+$`)
	audioNodeRe = regexp.MustCompile(`^/dev/snd/pcmC(\d+)D(\d+)c$`)
)

func (cm *CaptureManager) watchHotplug() {
	defer cm.wg.Done()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		utils.Log.Errorw("Failed to create fsnotify watcher; hot-plug disabled.", "error", err)
		return
	}
	defer watcher.Close()

	if !cm.opts.NoCam {
		if err := watcher.Add("/dev"); err != nil {
			utils.Log.Warnw("Cannot watch /dev for video hot-plug events.", "error", err)
		}
	}
	if !cm.opts.NoMic {
		if err := watcher.Add("/dev/snd"); err != nil {
			utils.Log.Warnw("Cannot watch /dev/snd for audio hot-plug events.", "error", err)
		}
	}

	utils.Log.Infow("Hot-plug watcher started.")

	for {
		select {
		case <-cm.stopCh:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			cm.handleHotplugEvent(event)
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return
			}
			utils.Log.Warnw("Hot-plug watcher error.", "error", watchErr)
		}
	}
}

func (cm *CaptureManager) handleHotplugEvent(event fsnotify.Event) {
	name := event.Name

	if videoNodeRe.MatchString(name) {
		switch {
		case event.Has(fsnotify.Create):
			utils.Log.Infow("Video device connected.", "device", name)
			go cm.retryAddVideo(name)
		case event.Has(fsnotify.Remove):
			utils.Log.Infow("Video device disconnected.", "device", name)
			cm.removeVideoDevice(name)
		}
		return
	}

	if m := audioNodeRe.FindStringSubmatch(name); m != nil {
		cardNum, _ := strconv.Atoi(m[1])
		devNum, _ := strconv.Atoi(m[2])
		alsaDev := fmt.Sprintf("hw:%d,%d", cardNum, devNum)
		switch {
		case event.Has(fsnotify.Create):
			utils.Log.Infow("Audio device connected.", "device", alsaDev)
			go cm.retryAddAudio(alsaDev)
		case event.Has(fsnotify.Remove):
			utils.Log.Infow("Audio device disconnected.", "device", alsaDev)
			cm.removeAudioDevice(alsaDev)
		}
	}
}

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

func (cm *CaptureManager) retryAddVideo(device string) {
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-cm.stopCh:
			return
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
		if cm.addVideoDevice(device) {
			return
		}
		utils.Log.Warnw("Retrying video device initialisation.", "device", device, "attempt", attempt)
	}
	utils.Log.Errorw("Giving up on video device after 3 attempts.", "device", device)
}

func (cm *CaptureManager) retryAddAudio(device string) {
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-cm.stopCh:
			return
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
		if cm.addAudioDevice(device, "") {
			return
		}
		utils.Log.Warnw("Retrying audio device initialisation.", "device", device, "attempt", attempt)
	}
	utils.Log.Errorw("Giving up on audio device after 3 attempts.", "device", device)
}

// ---------------------------------------------------------------------------
// Video lifecycle
// ---------------------------------------------------------------------------

func (cm *CaptureManager) addVideoDevice(device string) bool {
	entry, ok := cm.cameraConfigFor(device)
	if !ok {
		utils.Log.Infow("Video device skipped (no matching entry in devices.yaml).", "device", device)
		return false
	}

	cm.mu.Lock()
	if _, running := cm.pipelines[device]; running {
		cm.mu.Unlock()
		return true
	}
	if _, pending := cm.pending[device]; pending {
		cm.mu.Unlock()
		return false
	}
	cm.pending[device] = struct{}{}
	cm.mu.Unlock()

	pipeline, info, err := cm.buildVideoCapture(device, entry)

	cm.mu.Lock()
	delete(cm.pending, device)
	if err == nil {
		cm.pipelines[device] = pipeline
	}
	cm.mu.Unlock()

	if err != nil {
		utils.Log.Errorw("Failed to start video capture.", "device", device, "error", err)
		return false
	}

	pipeline.SetErrorCallback(func() {
		utils.Log.Warnw("Video pipeline error; cleaning up.", "device", device)
		cm.removeVideoDevice(device)
	})

	utils.Log.Infow("Video capture started.", "device", device)
	cm.notifyAdded(info)
	return true
}

func (cm *CaptureManager) buildVideoCapture(device string, entry utils.CameraEntry) (*utils.GStreamerPipeline, StreamInfo, error) {
	name := utils.GetVideoDeviceName(device)
	if entry.Name != "" {
		name = entry.Name
	}

	// Stream-specific overrides fall back to capture values when zero.
	streamWidth := entry.Stream.Width
	if streamWidth == 0 {
		streamWidth = entry.Width
	}
	streamHeight := entry.Stream.Height
	if streamHeight == 0 {
		streamHeight = entry.Height
	}
	streamFPS := entry.Stream.Framerate
	if streamFPS == 0 {
		streamFPS = entry.Framerate
	}
	bitrate := entry.Stream.Bitrate
	if bitrate <= 0 {
		utils.Log.Errorw("Video stream skipped: 'stream.bitrate' is not set in devices.yaml.", "uid", entry.UID)
		return nil, StreamInfo{}, fmt.Errorf("stream.bitrate not configured for camera uid=%s", entry.UID)
	}

	cfg := utils.VideoPipelineConfig{
		Name:             name,
		Device:           device,
		CaptureWidth:     entry.Width,
		CaptureHeight:    entry.Height,
		CaptureFramerate: entry.Framerate,
		Width:            streamWidth,
		Height:           streamHeight,
		Framerate:        streamFPS,
		Bitrate:          bitrate,
		VerticalFlip:     entry.VerticalFlip,
		HorizontalFlip:   entry.HorizontalFlip,
	}
	utils.Log.Infow("Starting video capture.",
		"device", device, "name", name, "uid", utils.VideoDeviceUID(device),
		"capture", fmt.Sprintf("%dx%d@%dfps", entry.Width, entry.Height, entry.Framerate),
		"stream", fmt.Sprintf("%dx%d@%dfps", streamWidth, streamHeight, streamFPS),
		"bitrate", bitrate, "v_flip", entry.VerticalFlip, "h_flip", entry.HorizontalFlip)

	pipeline, err := utils.NewVideoPipeline(cfg, false)
	if err != nil {
		return nil, StreamInfo{}, fmt.Errorf("create video pipeline: %w", err)
	}
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		return nil, StreamInfo{}, fmt.Errorf("start video pipeline: %w", err)
	}

	info := StreamInfo{
		Key:      device,
		Name:     name,
		Kind:     VideoStream,
		Frames:   pipeline.Frames(),
		Pipeline: pipeline,
	}
	return pipeline, info, nil
}

func (cm *CaptureManager) removeVideoDevice(device string) {
	cm.mu.Lock()
	p, ok := cm.pipelines[device]
	if ok {
		delete(cm.pipelines, device)
	}
	cm.mu.Unlock()

	if !ok {
		return
	}
	p.Stop()
	p.Wait()
	p.Free()
	utils.Log.Infow("Video capture removed.", "device", device)
	cm.notifyRemoved(device)
}

// ---------------------------------------------------------------------------
// Audio lifecycle
// ---------------------------------------------------------------------------

func (cm *CaptureManager) addAudioDevice(device, alsaName string) bool {
	entry, ok := cm.micConfigFor(device)
	if !ok {
		utils.Log.Infow("Audio device skipped (no matching entry in devices.yaml).", "device", device)
		return false
	}

	cm.mu.Lock()
	if _, running := cm.pipelines[device]; running {
		cm.mu.Unlock()
		return true
	}
	if _, pending := cm.pending[device]; pending {
		cm.mu.Unlock()
		return false
	}
	cm.pending[device] = struct{}{}
	cm.mu.Unlock()

	pipeline, info, err := cm.buildAudioCapture(device, alsaName, entry)

	cm.mu.Lock()
	delete(cm.pending, device)
	if err == nil {
		cm.pipelines[device] = pipeline
	}
	cm.mu.Unlock()

	if err != nil {
		utils.Log.Errorw("Failed to start audio capture.", "device", device, "error", err)
		return false
	}

	pipeline.SetErrorCallback(func() {
		utils.Log.Warnw("Audio pipeline error; cleaning up.", "device", device)
		cm.removeAudioDevice(device)
	})

	utils.Log.Infow("Audio capture started.", "device", device)
	cm.notifyAdded(info)
	return true
}

func (cm *CaptureManager) buildAudioCapture(device, alsaName string, entry utils.MicrophoneEntry) (*utils.GStreamerPipeline, StreamInfo, error) {
	deviceMaxRate, deviceMaxCh := utils.ProbeAudioCapabilities(device)
	rate := utils.SelectBestAudioRate(deviceMaxRate, entry.SampleRate)
	channels := utils.SelectBestAudioChannels(deviceMaxCh, entry.Channels)

	name := alsaName
	if name == "" {
		for _, info := range utils.ListAudioDevices() {
			if info.Device == device {
				name = info.Name
				break
			}
		}
		if name == "" {
			name = device
		}
	}
	if entry.Name != "" {
		name = entry.Name
	}

	bitrate := entry.Stream.Bitrate
	if bitrate <= 0 {
		utils.Log.Errorw("Audio stream skipped: 'stream.bitrate' is not set in devices.yaml.", "uid", entry.UID)
		return nil, StreamInfo{}, fmt.Errorf("stream.bitrate not configured for microphone uid=%s", entry.UID)
	}
	cfg := utils.AudioPipelineConfig{
		Name:       name,
		Device:     device,
		SampleRate: rate,
		Channels:   channels,
		Bitrate:    bitrate,
	}
	utils.Log.Infow("Starting audio capture.",
		"device", device, "name", name, "rate", rate, "channels", channels, "bitrate", bitrate)

	pipeline, err := utils.NewAudioPipeline(cfg, false)
	if err != nil {
		return nil, StreamInfo{}, fmt.Errorf("create audio pipeline: %w", err)
	}
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		return nil, StreamInfo{}, fmt.Errorf("start audio pipeline: %w", err)
	}

	info := StreamInfo{
		Key:      device,
		Name:     name,
		Kind:     AudioStream,
		Frames:   pipeline.Frames(),
		Pipeline: pipeline,
		Channels: channels,
	}
	return pipeline, info, nil
}

func (cm *CaptureManager) removeAudioDevice(device string) {
	cm.mu.Lock()
	p, ok := cm.pipelines[device]
	if ok {
		delete(cm.pipelines, device)
	}
	cm.mu.Unlock()

	if !ok {
		return
	}
	p.Stop()
	p.Wait()
	p.Free()
	utils.Log.Infow("Audio capture removed.", "device", device)
	cm.notifyRemoved(device)
}

// ---------------------------------------------------------------------------
// Fake stream helpers
// ---------------------------------------------------------------------------

func (cm *CaptureManager) startFakeVideo() {
	cfg := utils.VideoPipelineConfig{
		Name:      "Fake Camera",
		Width:     1920,
		Height:    1080,
		Framerate: 30,
		Bitrate:   2_000_000,
	}
	pipeline, err := utils.NewVideoPipeline(cfg, true)
	if err != nil {
		utils.Log.Errorw("Failed to create fake video pipeline.", "error", err)
		return
	}
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		utils.Log.Errorw("Failed to start fake video pipeline.", "error", err)
		return
	}
	cm.mu.Lock()
	cm.pipelines["fake-video"] = pipeline
	cm.mu.Unlock()

	info := StreamInfo{
		Key:      "fake-video",
		Name:     cfg.Name,
		Kind:     VideoStream,
		Frames:   pipeline.Frames(),
		Pipeline: pipeline,
	}
	utils.Log.Infow("Fake video capture started.")
	cm.notifyAdded(info)
}

func (cm *CaptureManager) startFakeAudio() {
	cfg := utils.AudioPipelineConfig{
		Name:       "Fake Microphone",
		SampleRate: 48000,
		Channels:   2,
		Bitrate:    96_000,
	}
	pipeline, err := utils.NewAudioPipeline(cfg, true)
	if err != nil {
		utils.Log.Errorw("Failed to create fake audio pipeline.", "error", err)
		return
	}
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		utils.Log.Errorw("Failed to start fake audio pipeline.", "error", err)
		return
	}
	cm.mu.Lock()
	cm.pipelines["fake-audio"] = pipeline
	cm.mu.Unlock()

	info := StreamInfo{
		Key:      "fake-audio",
		Name:     cfg.Name,
		Kind:     AudioStream,
		Frames:   pipeline.Frames(),
		Pipeline: pipeline,
		Channels: cfg.Channels,
	}
	utils.Log.Infow("Fake audio capture started.")
	cm.notifyAdded(info)
}
