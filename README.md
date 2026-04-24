RaceCast Emitter — Go port

This repository is a Go translation of the original TypeScript project. It keeps a similar structure and emits UPS telemetry as JSON lines on stdout.

Quick run (local):

1. Build:

```bash
go build ./...
```

2. Run:

```bash
LIVEKIT_TLS=true LIVEKIT_DOMAIN=example.com LIVEKIT_ROOM=test LIVEKIT_IDENTITY=bot ./racecast-emitter
```

The UPS implementation in `scripts.RunUPS` is a simulated device. To integrate real I2C hardware implement a concrete UPS reader and replace the simulation.
