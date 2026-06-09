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
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// managedStream groups a running pipeline with its LiveKit publication SID.
type managedStream struct {
	pipeline *utils.GStreamerPipeline
	pubSID   string
}

// StreamManagerOptions configures the quality limits used when starting streams.
// All values are read once at startup from environment variables.
type StreamManagerOptions struct {
	// Video
	VideoBitrate int // bits/s

	// Audio
	AudioBitrate int // bits/s

	// Behaviour flags (mirrors the CLI flags)
	NoCam bool
	NoMic bool
	Fake  bool
}

// StreamManager detects cameras and microphones, starts their streams, and
// handles hot-plug events so devices can be added or removed at any time
// without restarting the process.
//
// Thread-safe: all public methods may be called concurrently.
type StreamManager struct {
	mu           sync.Mutex
	room         *lksdk.Room
	opts         StreamManagerOptions
	devCfg       utils.DevicesConfig       // parsed devices.yaml (empty = accept all)
	videoStreams  map[string]*managedStream // key: /dev/videoN
	audioStreams  map[string]*managedStream // key: hw:X,Y
	pendingVideo  map[string]struct{}       // devices being initialised
	pendingAudio  map[string]struct{}
	stopCh       chan struct{}
	wg           sync.WaitGroup
}

// NewStreamManager creates a StreamManager bound to the given LiveKit room.
func NewStreamManager(room *lksdk.Room, opts StreamManagerOptions) *StreamManager {
	return &StreamManager{
		room:         room,
		opts:         opts,
		videoStreams:  make(map[string]*managedStream),
		audioStreams:  make(map[string]*managedStream),
		pendingVideo:  make(map[string]struct{}),
		pendingAudio:  make(map[string]struct{}),
		stopCh:       make(chan struct{}),
	}
}

// Start scans for already-connected devices, starts their streams, and then
// spawns a goroutine that watches /dev and /dev/snd for hot-plug events.
// In fake mode only one synthetic video + audio stream is started.
func (sm *StreamManager) Start() {
	cfg, err := utils.LoadDevicesConfig()
	if err != nil {
		utils.Log.Warnw("Failed to load devices.yaml; no devices will be started.", "error", err)
	} else {
		sm.devCfg = cfg
		utils.Log.Infow("Device config loaded.",
			"cameras", len(cfg.Cameras), "microphones", len(cfg.Microphones))
	}

	if sm.opts.Fake {
		if !sm.opts.NoCam {
			sm.startFakeVideo()
		}
		if !sm.opts.NoMic {
			sm.startFakeAudio()
		}
		return
	}

	sm.startFromConfig()

	sm.wg.Add(1)
	go sm.watchHotplug()
}

// Stop terminates the hot-plug watcher and tears down all active streams.
func (sm *StreamManager) Stop() {
	close(sm.stopCh)
	sm.wg.Wait() // wait for watchHotplug to exit

	sm.mu.Lock()
	defer sm.mu.Unlock()

	for dev, ms := range sm.videoStreams {
		sm.stopStream(ms)
		delete(sm.videoStreams, dev)
	}
	for dev, ms := range sm.audioStreams {
		sm.stopStream(ms)
		delete(sm.audioStreams, dev)
	}
}

// stopStream stops and frees a pipeline and unpublishes its LiveKit track.
// Must be called with sm.mu held or from a context where the entry has already
// been removed from the map.
func (sm *StreamManager) stopStream(ms *managedStream) {
	ms.pipeline.Stop()
	ms.pipeline.Wait()
	ms.pipeline.Free()
	if ms.pubSID != "" {
		if err := sm.room.LocalParticipant.UnpublishTrack(ms.pubSID); err != nil {
			utils.Log.Warnw("Failed to unpublish track.", "sid", ms.pubSID, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Hot-plug watcher
// ---------------------------------------------------------------------------

// watchHotplug monitors /dev and /dev/snd via inotify and reacts to devices
// being added or removed.
func (sm *StreamManager) watchHotplug() {
	defer sm.wg.Done()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		utils.Log.Errorw("Failed to create fsnotify watcher; hot-plug disabled.", "error", err)
		return
	}
	defer watcher.Close()

	if !sm.opts.NoCam {
		if err := watcher.Add("/dev"); err != nil {
			utils.Log.Warnw("Cannot watch /dev for video hot-plug events.", "error", err)
		}
	}
	if !sm.opts.NoMic {
		if err := watcher.Add("/dev/snd"); err != nil {
			utils.Log.Warnw("Cannot watch /dev/snd for audio hot-plug events.", "error", err)
		}
	}

	utils.Log.Infow("Hot-plug watcher started.")

	for {
		select {
		case <-sm.stopCh:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			sm.handleHotplugEvent(event)
		case watchErr, ok := <-watcher.Errors:
			if !ok {
				return
			}
			utils.Log.Warnw("Hot-plug watcher error.", "error", watchErr)
		}
	}
}

var (
	// /dev/video0, /dev/video1, …
	videoNodeRe = regexp.MustCompile(`^/dev/video\d+$`)
	// /dev/snd/pcmC2D0c  →  card=2, device=0, capture
	audioNodeRe = regexp.MustCompile(`^/dev/snd/pcmC(\d+)D(\d+)c$`)
)

func (sm *StreamManager) handleHotplugEvent(event fsnotify.Event) {
	name := event.Name

	if videoNodeRe.MatchString(name) {
		switch {
		case event.Has(fsnotify.Create):
			utils.Log.Infow("Video device connected.", "device", name)
			go sm.retryAddVideo(name)
		case event.Has(fsnotify.Remove):
			utils.Log.Infow("Video device disconnected.", "device", name)
			sm.removeVideoDevice(name)
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
			go sm.retryAddAudio(alsaDev)
		case event.Has(fsnotify.Remove):
			utils.Log.Infow("Audio device disconnected.", "device", alsaDev)
			sm.removeAudioDevice(alsaDev)
		}
	}
}

// ---------------------------------------------------------------------------
// Retry helpers (back-off loop for newly connected devices)
// ---------------------------------------------------------------------------

func (sm *StreamManager) retryAddVideo(device string) {
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-sm.stopCh:
			return
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
		if sm.addVideoDevice(device) {
			return
		}
		utils.Log.Warnw("Retrying video device initialisation.", "device", device, "attempt", attempt)
	}
	utils.Log.Errorw("Giving up on video device after 3 attempts.", "device", device)
}

func (sm *StreamManager) retryAddAudio(device string) {
	for attempt := 1; attempt <= 3; attempt++ {
		select {
		case <-sm.stopCh:
			return
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
		if sm.addAudioDevice(device, "") {
			return
		}
		utils.Log.Warnw("Retrying audio device initialisation.", "device", device, "attempt", attempt)
	}
	utils.Log.Errorw("Giving up on audio device after 3 attempts.", "device", device)
}

// ---------------------------------------------------------------------------
// Video stream lifecycle
// ---------------------------------------------------------------------------

// startFromConfig starts streams for all devices explicitly listed in
// devices.yaml, resolving each entry by UID (stable) or by card-name match.
// Devices not mentioned in the file are never started.
func (sm *StreamManager) startFromConfig() {
	if !sm.opts.NoCam {
		for _, entry := range sm.devCfg.Cameras {
			if entry.Disabled {
				utils.Log.Infow("Camera disabled in devices.yaml.", "uid", entry.UID)
				continue
			}
			dev, ok := utils.VideoDeviceByUID(entry.UID)
			if !ok {
				utils.Log.Infow("Camera UID not found at startup.", "uid", entry.UID)
				continue
			}
			sm.addVideoDevice(dev)
		}
	}

	if !sm.opts.NoMic {
		for _, entry := range sm.devCfg.Microphones {
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
					sm.addAudioDevice(info.Device, info.Name)
				}
			}
		}
	}
}

// cameraConfigFor returns the CameraEntry whose UID matches the device.
// Devices not listed in devices.yaml are always rejected.
func (sm *StreamManager) cameraConfigFor(device string) (utils.CameraEntry, bool) {
	if len(sm.devCfg.Cameras) == 0 {
		return utils.CameraEntry{}, false
	}
	uid := utils.VideoDeviceUID(device)
	for _, entry := range sm.devCfg.Cameras {
		if uid != "" && entry.UID == uid {
			if entry.Disabled {
				return utils.CameraEntry{}, false
			}
			return entry, true
		}
	}
	return utils.CameraEntry{}, false
}

// micConfigFor returns the MicrophoneEntry whose UID matches the ALSA device.
// Devices not listed in devices.yaml are always rejected.
func (sm *StreamManager) micConfigFor(device string) (utils.MicrophoneEntry, bool) {
	if len(sm.devCfg.Microphones) == 0 {
		return utils.MicrophoneEntry{}, false
	}
	var cardNum int
	fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(device, "plughw:"), "hw:"), "%d", &cardNum)
	uid := utils.AudioDeviceUID(cardNum)
	for _, entry := range sm.devCfg.Microphones {
		if uid != "" && entry.UID == uid {
			if entry.Disabled {
				return utils.MicrophoneEntry{}, false
			}
			return entry, true
		}
	}
	return utils.MicrophoneEntry{}, false
}

// addVideoDevice starts a stream for device if one is not already running or
// pending. Returns true on success.
func (sm *StreamManager) addVideoDevice(device string) bool {
	entry, ok := sm.cameraConfigFor(device)
	if !ok {
		utils.Log.Infow("Video device skipped (no matching entry in devices.yaml).", "device", device)
		return false
	}

	sm.mu.Lock()
	if _, running := sm.videoStreams[device]; running {
		sm.mu.Unlock()
		return true
	}
	if _, pending := sm.pendingVideo[device]; pending {
		sm.mu.Unlock()
		return false
	}
	sm.pendingVideo[device] = struct{}{}
	sm.mu.Unlock()

	pipeline, pubSID, startErr := sm.buildVideoStream(device, entry)

	sm.mu.Lock()
	delete(sm.pendingVideo, device)
	if startErr == nil {
		sm.videoStreams[device] = &managedStream{pipeline: pipeline, pubSID: pubSID}
	}
	sm.mu.Unlock()

	if startErr != nil {
		utils.Log.Errorw("Failed to start video stream.", "device", device, "error", startErr)
		return false
	}

	// Register error callback so a later device disconnect triggers cleanup.
	pipeline.SetErrorCallback(func() {
		utils.Log.Warnw("Video pipeline error; cleaning up stream.", "device", device)
		sm.removeVideoDevice(device)
	})

	utils.Log.Infow("Video stream started.", "device", device, "pub_sid", pubSID)
	return true
}

func (sm *StreamManager) buildVideoStream(device string, entry utils.CameraEntry) (*utils.GStreamerPipeline, string, error) {
	name := utils.GetVideoDeviceName(device)
	if entry.Name != "" {
		name = entry.Name
	}
	cfg := utils.VideoPipelineConfig{
		Name:           name,
		Device:         device,
		Width:          entry.Width,
		Height:         entry.Height,
		Framerate:      entry.Framerate,
		Bitrate:        sm.opts.VideoBitrate,
		VerticalFlip:   entry.VerticalFlip,
		HorizontalFlip: entry.HorizontalFlip,
	}
	utils.Log.Infow("Starting video stream.",
		"device", device, "name", name, "uid", utils.VideoDeviceUID(device),
		"width", entry.Width, "height", entry.Height, "fps", entry.Framerate,
		"v_flip", entry.VerticalFlip, "h_flip", entry.HorizontalFlip)

	return sm.launchVideoStream(cfg, false)
}

// removeVideoDevice stops and unpublishes the stream for device (if any).
func (sm *StreamManager) removeVideoDevice(device string) {
	sm.mu.Lock()
	ms, ok := sm.videoStreams[device]
	if ok {
		delete(sm.videoStreams, device)
	}
	sm.mu.Unlock()

	if !ok {
		return
	}
	sm.stopStream(ms)
	utils.Log.Infow("Video stream removed.", "device", device)
}

// ---------------------------------------------------------------------------
// Audio stream lifecycle
// ---------------------------------------------------------------------------

// addAudioDevice starts a stream for device if one is not already running or
// pending. alsaName is the ALSA card name (may be empty; resolved if needed).
// Returns true on success.
func (sm *StreamManager) addAudioDevice(device, alsaName string) bool {
	entry, ok := sm.micConfigFor(device)
	if !ok {
		utils.Log.Infow("Audio device skipped (no matching entry in devices.yaml).", "device", device)
		return false
	}

	sm.mu.Lock()
	if _, running := sm.audioStreams[device]; running {
		sm.mu.Unlock()
		return true
	}
	if _, pending := sm.pendingAudio[device]; pending {
		sm.mu.Unlock()
		return false
	}
	sm.pendingAudio[device] = struct{}{}
	sm.mu.Unlock()

	pipeline, pubSID, startErr := sm.buildAudioStream(device, alsaName, entry)

	sm.mu.Lock()
	delete(sm.pendingAudio, device)
	if startErr == nil {
		sm.audioStreams[device] = &managedStream{pipeline: pipeline, pubSID: pubSID}
	}
	sm.mu.Unlock()

	if startErr != nil {
		utils.Log.Errorw("Failed to start audio stream.", "device", device, "error", startErr)
		return false
	}

	pipeline.SetErrorCallback(func() {
		utils.Log.Warnw("Audio pipeline error; cleaning up stream.", "device", device)
		sm.removeAudioDevice(device)
	})

	utils.Log.Infow("Audio stream started.", "device", device, "pub_sid", pubSID)
	return true
}

func (sm *StreamManager) buildAudioStream(device, alsaName string, entry utils.MicrophoneEntry) (*utils.GStreamerPipeline, string, error) {
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

	cfg := utils.AudioPipelineConfig{
		Name:       name,
		Device:     device,
		SampleRate: rate,
		Channels:   channels,
		Bitrate:    sm.opts.AudioBitrate,
	}
	utils.Log.Infow("Selected audio mode.",
		"device", device, "name", name, "rate", rate, "channels", channels)

	return sm.launchAudioStream(cfg, false)
}

// removeAudioDevice stops and unpublishes the stream for device (if any).
func (sm *StreamManager) removeAudioDevice(device string) {
	sm.mu.Lock()
	ms, ok := sm.audioStreams[device]
	if ok {
		delete(sm.audioStreams, device)
	}
	sm.mu.Unlock()

	if !ok {
		return
	}
	sm.stopStream(ms)
	utils.Log.Infow("Audio stream removed.", "device", device)
}

// ---------------------------------------------------------------------------
// Fake stream helpers
// ---------------------------------------------------------------------------

func (sm *StreamManager) startFakeVideo() {
	cfg := utils.VideoPipelineConfig{
		Name:      "Fake Camera",
		Width:     1920,
		Height:    1080,
		Framerate: 30,
		Bitrate:   sm.opts.VideoBitrate,
	}
	pipeline, pubSID, err := sm.launchVideoStream(cfg, true)
	if err != nil {
		utils.Log.Errorw("Failed to start fake video stream.", "error", err)
		return
	}
	sm.mu.Lock()
	sm.videoStreams["fake-video"] = &managedStream{pipeline: pipeline, pubSID: pubSID}
	sm.mu.Unlock()
	utils.Log.Infow("Fake video stream started.")
}

func (sm *StreamManager) startFakeAudio() {
	cfg := utils.AudioPipelineConfig{
		Name:       "Fake Microphone",
		SampleRate: 48000,
		Channels:   2,
		Bitrate:    sm.opts.AudioBitrate,
	}
	pipeline, pubSID, err := sm.launchAudioStream(cfg, true)
	if err != nil {
		utils.Log.Errorw("Failed to start fake audio stream.", "error", err)
		return
	}
	sm.mu.Lock()
	sm.audioStreams["fake-audio"] = &managedStream{pipeline: pipeline, pubSID: pubSID}
	sm.mu.Unlock()
	utils.Log.Infow("Fake audio stream started.")
}

// ---------------------------------------------------------------------------
// Stream launchers
// ---------------------------------------------------------------------------

// launchVideoStream creates, publishes, attaches and starts a video pipeline.
func (sm *StreamManager) launchVideoStream(cfg utils.VideoPipelineConfig, fake bool) (*utils.GStreamerPipeline, string, error) {
	if fake {
		utils.Log.Infow("Video stream: using SMPTE test pattern.")
	} else {
		utils.Log.Infow("Video stream: using webcam.", "device", cfg.Device)
	}
	pipeline, err := utils.NewVideoPipeline(cfg, fake)
	if err != nil {
		return nil, "", fmt.Errorf("create video pipeline: %w", err)
	}
	track, pubSID, err := utils.PublishVideoTrack(sm.room, cfg.Name)
	if err != nil {
		pipeline.Free()
		return nil, "", fmt.Errorf("publish video track: %w", err)
	}
	pipeline.AttachTrack(track)
	track.OnBind(func() { pipeline.ForceKeyframe() })
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		if unpubErr := sm.room.LocalParticipant.UnpublishTrack(pubSID); unpubErr != nil {
			utils.Log.Warnw("Failed to unpublish track after pipeline start failure.", "error", unpubErr)
		}
		return nil, "", fmt.Errorf("start video pipeline: %w", err)
	}
	return pipeline, pubSID, nil
}

// launchAudioStream creates, publishes, attaches and starts an audio pipeline.
func (sm *StreamManager) launchAudioStream(cfg utils.AudioPipelineConfig, fake bool) (*utils.GStreamerPipeline, string, error) {
	if fake {
		utils.Log.Infow("Audio stream: using sine-wave test source.")
	} else {
		utils.Log.Infow("Audio stream: using ALSA device.", "device", cfg.Device)
	}
	pipeline, err := utils.NewAudioPipeline(cfg, fake)
	if err != nil {
		return nil, "", fmt.Errorf("create audio pipeline: %w", err)
	}
	track, pubSID, err := utils.PublishAudioTrack(sm.room, cfg)
	if err != nil {
		pipeline.Free()
		return nil, "", fmt.Errorf("publish audio track: %w", err)
	}
	pipeline.AttachTrack(track)
	if err := pipeline.Start(); err != nil {
		pipeline.Free()
		if unpubErr := sm.room.LocalParticipant.UnpublishTrack(pubSID); unpubErr != nil {
			utils.Log.Warnw("Failed to unpublish track after pipeline start failure.", "error", unpubErr)
		}
		return nil, "", fmt.Errorf("start audio pipeline: %w", err)
	}
	return pipeline, pubSID, nil
}
