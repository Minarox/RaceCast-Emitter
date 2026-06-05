package scripts

import (
	"fmt"
	"regexp"
	"strconv"
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

// StreamManagerOptions configures the quality limits used when auto-detecting
// devices. All values are read once at startup from environment variables.
type StreamManagerOptions struct {
	// Video limits
	MaxVideoWidth  int
	MaxVideoHeight int
	MaxVideoFPS    int
	VideoBitrate   int // bits/s

	// Audio limits
	MaxAudioRate     int // Hz
	MaxAudioChannels int
	AudioBitrate     int // bits/s

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
	if sm.opts.Fake {
		if !sm.opts.NoCam {
			sm.startFakeVideo()
		}
		if !sm.opts.NoMic {
			sm.startFakeAudio()
		}
		return
	}

	if !sm.opts.NoCam {
		for _, dev := range utils.ListVideoDevices() {
			sm.addVideoDevice(dev)
		}
	}
	if !sm.opts.NoMic {
		for _, info := range utils.ListAudioDevices() {
			sm.addAudioDevice(info.Device, info.Name)
		}
	}

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

// addVideoDevice starts a stream for device if one is not already running or
// pending. Returns true on success.
func (sm *StreamManager) addVideoDevice(device string) bool {
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

	pipeline, pubSID, startErr := sm.buildVideoStream(device)

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

func (sm *StreamManager) buildVideoStream(device string) (*utils.GStreamerPipeline, string, error) {
	modes, err := utils.ListVideoModes(device)
	if err != nil || len(modes) == 0 {
		return nil, "", fmt.Errorf("cannot query video modes for %s: %w", device, err)
	}

	mode, ok := utils.SelectBestVideoMode(modes, sm.opts.MaxVideoWidth, sm.opts.MaxVideoHeight, sm.opts.MaxVideoFPS)
	if !ok {
		return nil, "", fmt.Errorf("no mode for %s fits within %dx%d@%dfps",
			device, sm.opts.MaxVideoWidth, sm.opts.MaxVideoHeight, sm.opts.MaxVideoFPS)
	}

	name := utils.GetVideoDeviceName(device)
	cfg := utils.VideoPipelineConfig{
		Name:      name,
		Device:    device,
		Width:     mode.Width,
		Height:    mode.Height,
		Framerate: mode.Framerate,
		Bitrate:   sm.opts.VideoBitrate,
	}
	utils.Log.Infow("Selected video mode.",
		"device", device, "name", name,
		"format", mode.Format, "width", mode.Width, "height", mode.Height, "fps", mode.Framerate)

	return AddVideoStream(sm.room, cfg, false)
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
// pending. name is optional; it will be resolved from arecord if empty.
// Returns true on success.
func (sm *StreamManager) addAudioDevice(device, name string) bool {
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

	pipeline, pubSID, startErr := sm.buildAudioStream(device, name)

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

func (sm *StreamManager) buildAudioStream(device, name string) (*utils.GStreamerPipeline, string, error) {
	deviceMaxRate, deviceMaxCh := utils.ProbeAudioCapabilities(device)
	rate := utils.SelectBestAudioRate(deviceMaxRate, sm.opts.MaxAudioRate)
	channels := utils.SelectBestAudioChannels(deviceMaxCh, sm.opts.MaxAudioChannels)

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

	cfg := utils.AudioPipelineConfig{
		Name:       name,
		Device:     device,
		SampleRate: rate,
		Channels:   channels,
		Bitrate:    sm.opts.AudioBitrate,
	}
	utils.Log.Infow("Selected audio mode.",
		"device", device, "name", name, "rate", rate, "channels", channels)

	return AddAudioStream(sm.room, cfg, false)
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
		Width:     sm.opts.MaxVideoWidth,
		Height:    sm.opts.MaxVideoHeight,
		Framerate: sm.opts.MaxVideoFPS,
		Bitrate:   sm.opts.VideoBitrate,
	}
	pipeline, pubSID, err := AddVideoStream(sm.room, cfg, true)
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
		SampleRate: sm.opts.MaxAudioRate,
		Channels:   sm.opts.MaxAudioChannels,
		Bitrate:    sm.opts.AudioBitrate,
	}
	pipeline, pubSID, err := AddAudioStream(sm.room, cfg, true)
	if err != nil {
		utils.Log.Errorw("Failed to start fake audio stream.", "error", err)
		return
	}
	sm.mu.Lock()
	sm.audioStreams["fake-audio"] = &managedStream{pipeline: pipeline, pubSID: pubSID}
	sm.mu.Unlock()
	utils.Log.Infow("Fake audio stream started.")
}
