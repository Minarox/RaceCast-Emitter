package livekit

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/protocol/livekit"
	lkauth "github.com/livekit/protocol/auth"
	protoLogger "github.com/livekit/protocol/logger"

	"racecast-emitter/internal/logger"
)

// nullLogger implémente protoLogger.Logger en ignorant tous les messages.
// Utilisé pour supprimer les logs internes de pion/WebRTC.
type nullLogger struct{}

func (nullLogger) Debugw(_ string, _ ...any)         {}
func (nullLogger) Infow(_ string, _ ...any)          {}
func (nullLogger) Warnw(_ string, _ error, _ ...any) {}
func (nullLogger) Errorw(_ string, _ error, _ ...any) {}
func (n nullLogger) WithValues(_ ...any) protoLogger.Logger    { return n }
func (n nullLogger) WithUnlikelyValues(_ ...any) protoLogger.UnlikelyLogger {
	return protoLogger.NewUnlikelyLogger(n)
}
func (n nullLogger) WithName(_ string) protoLogger.Logger       { return n }
func (n nullLogger) WithComponent(_ string) protoLogger.Logger  { return n }
func (n nullLogger) WithCallDepth(_ int) protoLogger.Logger     { return n }
func (n nullLogger) WithItemSampler() protoLogger.Logger        { return n }
func (n nullLogger) WithoutSampler() protoLogger.Logger         { return n }
func (n nullLogger) WithDeferredValues() (protoLogger.Logger, protoLogger.DeferredFieldResolver) {
	return n, nullResolver{}
}

type nullResolver struct{}

func (nullResolver) Resolve(_ ...any) {}
func (nullResolver) Reset()           {}

// Config contient les paramètres de connexion à LiveKit, lus depuis les
// variables d'environnement (chargées depuis .env par le package env).
type Config struct {
	TLS      bool   // RC_LIVEKIT_TLS
	Domain   string // RC_LIVEKIT_DOMAIN
	APIKey   string // RC_LIVEKIT_API_KEY
	Secret   string // RC_LIVEKIT_API_SECRET
	Room     string // RC_LIVEKIT_ROOM
	Identity string // RC_LIVEKIT_IDENTITY
}

// LoadConfig lit la configuration LiveKit depuis les variables d'environnement.
func LoadConfig() (Config, error) {
	cfg := Config{
		Domain:   os.Getenv("RC_LIVEKIT_DOMAIN"),
		APIKey:   os.Getenv("RC_LIVEKIT_API_KEY"),
		Secret:   os.Getenv("RC_LIVEKIT_API_SECRET"),
		Room:     os.Getenv("RC_LIVEKIT_ROOM"),
		Identity: os.Getenv("RC_LIVEKIT_IDENTITY"),
	}

	tls := os.Getenv("RC_LIVEKIT_TLS")
	cfg.TLS = tls == "" || tls == "true" || tls == "1"

	var missing []string
	if cfg.Domain == "" {
		missing = append(missing, "RC_LIVEKIT_DOMAIN")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "RC_LIVEKIT_API_KEY")
	}
	if cfg.Secret == "" {
		missing = append(missing, "RC_LIVEKIT_API_SECRET")
	}
	if cfg.Room == "" {
		missing = append(missing, "RC_LIVEKIT_ROOM")
	}
	if cfg.Identity == "" {
		missing = append(missing, "RC_LIVEKIT_IDENTITY")
	}

	if len(missing) > 0 {
		return cfg, fmt.Errorf("variables d'environnement manquantes : %v", missing)
	}
	return cfg, nil
}

// serverURL construit l'URL WebSocket du serveur LiveKit.
func (c Config) serverURL() string {
	scheme := "wss"
	if !c.TLS {
		scheme = "ws"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Domain)
}

// apiURL construit l'URL HTTP du serveur LiveKit.
func (c Config) apiURL() string {
	scheme := "https"
	if !c.TLS {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, c.Domain)
}

// token génère un jeton JWT pour rejoindre la room.
func (c Config) token() (string, error) {
	at := lkauth.NewAccessToken(c.APIKey, c.Secret)
	grant := &lkauth.VideoGrant{
		RoomJoin: true,
		Room:     c.Room,
	}
	at.SetVideoGrant(grant).
		SetIdentity(c.Identity).
		SetValidFor(24 * time.Hour)

	return at.ToJWT()
}

// ensureRoom crée la room si elle n'existe pas encore, avec un DepartureTimeout
// d'une journée (la room persiste même sans participants).
func ensureRoom(cfg Config) error {
	client := lksdk.NewRoomServiceClient(cfg.apiURL(), cfg.APIKey, cfg.Secret)

	_, err := client.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
		Name:             cfg.Room,
		DepartureTimeout: uint32((24 * time.Hour).Seconds()),
	})
	if err != nil {
		// La room existe déjà — pas une erreur fatale
		logger.Info("[livekit] Room \"%s\" déjà existante ou créée", cfg.Room)
		return nil
	}

	logger.Info("[livekit] Room \"%s\" créée (DepartureTimeout=24h)", cfg.Room)
	return nil
}

// Connect se connecte à la room LiveKit et retourne le client Room.
// La room est créée automatiquement si elle n'existe pas.
func Connect(cfg Config) (*lksdk.Room, error) {
	if err := ensureRoom(cfg); err != nil {
		return nil, fmt.Errorf("création de la room : %w", err)
	}

	token, err := cfg.token()
	if err != nil {
		return nil, fmt.Errorf("génération du token : %w", err)
	}

	// Pion écrit ses logs internes via le logger standard Go (log.Print*).
	// On redirige vers io.Discard pour les supprimer complètement.
	log.SetOutput(io.Discard)

	room, err := lksdk.ConnectToRoomWithToken(cfg.serverURL(), token, &lksdk.RoomCallback{},
		lksdk.WithLogger(nullLogger{}),
	)
	if err != nil {
		return nil, fmt.Errorf("connexion à la room : %w", err)
	}

	logger.Info("[livekit] Connecté à la room \"%s\" en tant que \"%s\"", cfg.Room, cfg.Identity)
	return room, nil
}
