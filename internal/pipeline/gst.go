package pipeline

// #cgo pkg-config: gstreamer-1.0 gstreamer-app-1.0
// #include <gst/gst.h>
// #include <gst/app/gstappsink.h>
// #include <stdlib.h>
// #include <fcntl.h>
// #include <unistd.h>
//
// // Passe le pipeline en PLAYING et attend la fin du changement d'état asynchrone.
// // La suppression des sorties (dup2 vers /dev/null) est gérée par l'appelant Go
// // via StartAll/Start afin d'éviter toute race condition entre goroutines.
// //
// // Retourne 1 si le pipeline a atteint PLAYING, 0 sinon.
// static int start_pipeline_inner(GstElement *pipeline) {
//     GstStateChangeReturn ret = gst_element_set_state(pipeline, GST_STATE_PLAYING);
//     if (ret == GST_STATE_CHANGE_ASYNC) {
//         GstState state;
//         ret = gst_element_get_state(pipeline, &state, NULL, 10 * GST_SECOND);
//     }
//     return ret != GST_STATE_CHANGE_FAILURE ? 1 : 0;
// }
//
// // Redirige stdout/stderr vers /dev/null. Stocke les fds originaux dans *out/*err.
// // Appeler silence_end(*out, *err) pour restaurer. Ne fait rien si une erreur survient.
// static void silence_begin(int *out, int *err) {
//     *out = dup(1);
//     *err = dup(2);
//     int nul = open("/dev/null", O_WRONLY);
//     if (*out < 0 || *err < 0 || nul < 0) {
//         if (nul >= 0) close(nul);
//         if (*out >= 0) { close(*out); *out = -1; }
//         if (*err >= 0) { close(*err); *err = -1; }
//         return;
//     }
//     dup2(nul, 1);
//     dup2(nul, 2);
//     close(nul);
// }
//
// static void silence_end(int out, int err) {
//     if (out >= 0) { dup2(out, 1); close(out); }
//     if (err >= 0) { dup2(err, 2); close(err); }
// }
//
// // Pull one sample from appsink with a 1-second timeout.
// // Returns GST_FLOW_OK, GST_FLOW_EOS or GST_FLOW_CUSTOM_ERROR (timeout).
// static GstFlowReturn pull_sample(GstElement *sink, GstSample **sample) {
//     *sample = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), GST_SECOND);
//     if (*sample == NULL) {
//         if (gst_app_sink_is_eos(GST_APP_SINK(sink))) return GST_FLOW_EOS;
//         return GST_FLOW_CUSTOM_ERROR;
//     }
//     return GST_FLOW_OK;
// }
//
// static GstClockTime buf_duration(GstBuffer *buf) {
//     return GST_BUFFER_DURATION_IS_VALID(buf) ? GST_BUFFER_DURATION(buf) : 0;
// }
//
// static GstBus* get_bus(GstElement *pipeline) {
//     return gst_element_get_bus(pipeline);
// }
//
// // Poll bus for ERROR/WARNING/EOS with 100 ms timeout.
// // Returns 1=error, 2=warning, 3=eos, 0=nothing.
// // msg and dbg must be g_free'd by caller.
// static int pop_bus_message(GstBus *bus, char **msg, char **dbg) {
//     *msg = NULL; *dbg = NULL;
//     GstMessage *m = gst_bus_timed_pop_filtered(bus, 100 * GST_MSECOND,
//         GST_MESSAGE_ERROR | GST_MESSAGE_WARNING | GST_MESSAGE_EOS);
//     if (m == NULL) return 0;
//     int ret = 0;
//     GstMessageType t = GST_MESSAGE_TYPE(m);
//     if (t == GST_MESSAGE_ERROR) {
//         ret = 1;
//         GError *err = NULL; gchar *d = NULL;
//         gst_message_parse_error(m, &err, &d);
//         if (err) { *msg = g_strdup(err->message); g_error_free(err); }
//         if (d)   { *dbg = g_strdup(d); g_free(d); }
//     } else if (t == GST_MESSAGE_WARNING) {
//         ret = 2;
//         GError *err = NULL; gchar *d = NULL;
//         gst_message_parse_warning(m, &err, &d);
//         if (err) { *msg = g_strdup(err->message); g_error_free(err); }
//         if (d)   { *dbg = g_strdup(d); g_free(d); }
//     } else if (t == GST_MESSAGE_EOS) {
//         ret = 3;
//     }
//     gst_message_unref(m);
//     return ret;
// }
//
// static void send_eos(GstElement *pipeline) {
//     gst_element_send_event(pipeline, gst_event_new_eos());
// }
//
// static void force_keyunit(GstElement *sink) {
//     GstEvent *ev = gst_event_new_custom(
//         GST_EVENT_CUSTOM_UPSTREAM,
//         gst_structure_new("GstForceKeyUnit",
//             "timestamp",    G_TYPE_UINT64,  (guint64)GST_CLOCK_TIME_NONE,
//             "stream-time",  G_TYPE_UINT64,  (guint64)GST_CLOCK_TIME_NONE,
//             "running-time", G_TYPE_UINT64,  (guint64)GST_CLOCK_TIME_NONE,
//             "all-headers",  G_TYPE_BOOLEAN, TRUE,
//             "count",        G_TYPE_UINT,    (guint)0,
//             NULL));
//     gst_element_send_event(sink, ev);
// }
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"racecast-emitter/internal/logger"
)

func init() {
	C.gst_init(nil, nil)
	// Désactive entièrement le système de debug GStreamer.
	// Sans cela, même GST_DEBUG=0 laisse passer certains messages.
	C.gst_debug_set_active(C.FALSE)
}

// silenceMu sérialise toutes les opérations silence_begin/silence_end du package.
//
// Sans ce mutex, deux goroutines qui appellent SetNull ou Free simultanément
// créent une race condition :
//
//	1. Goroutine A : silence_begin → fd 1 → /dev/null, original sauvegardé dans fd 5
//	2. Goroutine B : silence_begin → fd 1 est déjà /dev/null → sauvegarde /dev/null dans fd 8
//	3. Goroutine A : silence_end → restaure fd 1 depuis fd 5 (correct)
//	4. Goroutine B : silence_end → restaure fd 1 depuis fd 8 = /dev/null (← bug)
//
// Résultat : tous les logs Go subséquents disparaissent (ils vont dans /dev/null).
var silenceMu sync.Mutex

// Frame contient un buffer encodé produit par un appsink GStreamer.
type Frame struct {
	Data     []byte
	Duration time.Duration
}

// GstPipeline gère un pipeline GStreamer unique avec un appsink de diffusion.
// La branche d'enregistrement (si activée) partage le même pipeline via un tee.
type GstPipeline struct {
	mu          sync.Mutex
	pipeline    *C.GstElement
	appsink     *C.GstElement // nil si diffusion désactivée (enregistrement seul)
	frames      chan Frame     // fermé quand le pipeline s'arrête
	pipelineStr string
	ctx         context.Context
	cancel      context.CancelFunc
	running     bool
	onError     func()    // appelé sur erreur irrécupérable (déconnexion périphérique)
	wg          sync.WaitGroup
}

// newGstPipeline crée et démarre un pipeline GStreamer depuis une chaîne de description.
// hasAppsink indique si le pipeline contient un élément appsink nommé "sink" pour la diffusion.
func newGstPipeline(pipelineStr string, hasAppsink bool) (*GstPipeline, error) {
	cStr := C.CString(pipelineStr)
	defer C.free(unsafe.Pointer(cStr))

	var gerr *C.GError
	gp := C.gst_parse_launch(cStr, &gerr)
	if gerr != nil {
		msg := C.GoString((*C.char)(unsafe.Pointer(gerr.message)))
		C.g_error_free(gerr)
		return nil, fmt.Errorf("gst_parse_launch : %s", msg)
	}

	var appsink *C.GstElement
	if hasAppsink {
		name := C.CString("sink")
		defer C.free(unsafe.Pointer(name))
		appsink = C.gst_bin_get_by_name((*C.GstBin)(unsafe.Pointer(gp)), name)
		if appsink == nil {
			C.gst_object_unref(C.gpointer(gp))
			return nil, fmt.Errorf("appsink 'sink' introuvable dans le pipeline")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	p := &GstPipeline{
		pipeline:    gp,
		appsink:     appsink,
		pipelineStr: pipelineStr,
		ctx:         ctx,
		cancel:      cancel,
	}
	if hasAppsink {
		p.frames = make(chan Frame, 8)
	}
	return p, nil
}

// Frames retourne le canal de frames encodées (nil si diffusion désactivée).
func (p *GstPipeline) Frames() <-chan Frame {
	return p.frames
}

// ForceKeyframe envoie un événement GstForceKeyUnit en amont pour demander
// un IDR immédiat à l'encodeur — à appeler lors de la connexion d'un nouveau
// subscriber LiveKit pour réduire le délai avant première image.
func (p *GstPipeline) ForceKeyframe() {
	p.mu.Lock()
	sink := p.appsink
	running := p.running
	p.mu.Unlock()
	if running && sink != nil {
		C.force_keyunit(sink)
	}
}

// SetOnError enregistre une fonction appelée (dans une goroutine) en cas d'erreur
// irrécupérable du pipeline (ex : déconnexion physique du périphérique).
func (p *GstPipeline) SetOnError(fn func()) {
	p.mu.Lock()
	p.onError = fn
	p.mu.Unlock()
}

// Start place le pipeline en PLAYING et démarre les goroutines internes.
// Pour démarrer plusieurs pipelines en parallèle sans bruit Nvidia, utiliser StartAll.
func (p *GstPipeline) Start() error {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return nil
	}
	return p.startInner()
}

// startInner passe le pipeline en PLAYING. Le silenceMu doit être tenu et
// silence_begin doit avoir été appelé. p.mu doit être tenu par l'appelant.
func (p *GstPipeline) startInner() error {
	if C.start_pipeline_inner(p.pipeline) == 0 {
		return fmt.Errorf("impossible de démarrer le pipeline GStreamer")
	}
	p.running = true
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.watchBus() }()
	if p.appsink != nil {
		p.wg.Add(1)
		go func() { defer p.wg.Done(); p.loop() }()
	}
	return nil
}

// StartAll démarre plusieurs pipelines en parallèle sous un silencing global unique.
//
// Le problème des démarrages concurrents individuels : chaque goroutine ferait
// dup(1) → dup2(/dev/null, 1) → get_state (~1s) → dup2(saved, 1). Si G2 fait
// dup(1) pendant que G1 a déjà redirigé fd 1 vers /dev/null, G2 sauvegarde
// /dev/null et le restaure ensuite — ce qui supprime définitivement stdout.
//
// StartAll résout ce problème en n'appliquant qu'un seul dup2 pour l'ensemble
// des démarrages : tous les get_state bloquants s'exécutent en parallèle à
// l'intérieur de cette unique fenêtre de silencing, puis 100 ms supplémentaires
// couvrent les threads Nvidia qui écrivent encore après le retour de get_state.
//
// Retourne les erreurs indexées sur la slice d'entrée (nil = succès).
func StartAll(pipelines []*GstPipeline) []error {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)

	errs := make([]error, len(pipelines))
	var sg sync.WaitGroup
	for i, gp := range pipelines {
		i, gp := i, gp
		sg.Add(1)
		go func() {
			defer sg.Done()
			gp.mu.Lock()
			defer gp.mu.Unlock()
			if gp.running {
				return
			}
			errs[i] = gp.startInner()
		}()
	}
	sg.Wait()
	// Marge pour les threads de drivers Nvidia qui écrivent encore après
	// que gst_element_get_state ait signalé l'état PLAYING.
	time.Sleep(100 * time.Millisecond)
	return errs
}

// SendEOS injecte un événement EOS à la source du pipeline.
// Les goroutines internes s'arrêtent naturellement après la propagation.
func (p *GstPipeline) SendEOS() {
	C.send_eos(p.pipeline)
}

// WaitDrain attend que les goroutines internes s'arrêtent après un EOS.
// En cas de timeout, le contexte est annulé pour forcer l'arrêt.
func (p *GstPipeline) WaitDrain(timeout time.Duration) {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		p.cancel()
		p.wg.Wait()
	}
}

// SetNull place le pipeline en GST_STATE_NULL (libère les ressources matérielles).
// Les drivers Nvidia émettent des messages lors de la libération qui sont supprimés.
func (p *GstPipeline) SetNull() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd) // defer garanti même en cas de panique CGo
	C.gst_element_set_state(p.pipeline, C.GST_STATE_NULL)
}

// Stop arrête proprement le pipeline : EOS → drain → NULL.
func (p *GstPipeline) Stop() {
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	p.SendEOS()
	p.WaitDrain(10 * time.Second)
	p.SetNull()
}

// Free libère les ressources GStreamer. À appeler après Stop.
func (p *GstPipeline) Free() {
	silenceMu.Lock()
	defer silenceMu.Unlock()
	var out, errfd C.int
	C.silence_begin(&out, &errfd)
	defer C.silence_end(out, errfd)
	if p.appsink != nil {
		C.gst_object_unref(C.gpointer(p.appsink))
		p.appsink = nil
	}
	if p.pipeline != nil {
		C.gst_object_unref(C.gpointer(p.pipeline))
		p.pipeline = nil
	}
}

// watchBus surveille le bus GStreamer et loggue les erreurs et avertissements.
// S'arrête à l'EOS ou l'annulation du contexte.
func (p *GstPipeline) watchBus() {
	bus := C.get_bus(p.pipeline)
	if bus == nil {
		logger.Warn("[gst] Impossible d'accéder au bus du pipeline")
		return
	}
	defer C.gst_object_unref(C.gpointer(bus))

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}
		var cMsg, cDbg *C.char
		ret := C.pop_bus_message(bus, &cMsg, &cDbg)
		msg, dbg := "", ""
		if cMsg != nil {
			msg = C.GoString(cMsg)
			C.g_free(C.gpointer(unsafe.Pointer(cMsg)))
		}
		if cDbg != nil {
			dbg = C.GoString(cDbg)
			C.g_free(C.gpointer(unsafe.Pointer(cDbg)))
		}
		switch ret {
		case 1:
			logger.Error("[gst] Erreur pipeline : %s — %s", msg, dbg)
			p.mu.Lock()
			cb := p.onError
			p.mu.Unlock()
			if cb != nil {
				go cb()
			}
			return
		case 2:
			logger.Warn("[gst] Avertissement pipeline : %s — %s", msg, dbg)
		case 3:
			// logger.Info("[gst] EOS reçu sur le bus")
			return
		}
	}
}

// loop tire les buffers de l'appsink et les envoie sur le canal frames.
// Les frames sont abandonnées si le consommateur est trop lent (non bloquant).
func (p *GstPipeline) loop() {
	defer func() {
		if p.frames != nil {
			close(p.frames)
		}
	}()

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		var sample *C.GstSample
		flowRet := C.pull_sample(p.appsink, &sample)

		if flowRet == C.GST_FLOW_EOS {
			// logger.Info("[gst] Appsink EOS")
			return
		}
		if flowRet == C.GST_FLOW_CUSTOM_ERROR {
			continue // timeout 1s, aucune frame disponible
		}

		buf := C.gst_sample_get_buffer(sample)
		if buf == nil {
			C.gst_sample_unref(sample)
			continue
		}

		var mapInfo C.GstMapInfo
		if C.gst_buffer_map(buf, &mapInfo, C.GST_MAP_READ) == C.gboolean(0) {
			C.gst_sample_unref(sample)
			continue
		}

		data := C.GoBytes(unsafe.Pointer(mapInfo.data), C.int(mapInfo.size))
		dur := time.Duration(C.buf_duration(buf))
		if dur <= 0 {
			dur = time.Second / 30
		}

		C.gst_buffer_unmap(buf, &mapInfo)
		C.gst_sample_unref(sample)

		if len(data) == 0 {
			continue
		}

		select {
		case p.frames <- Frame{Data: data, Duration: dur}:
		default: // consommateur trop lent : frame abandonnée
		}
	}
}
