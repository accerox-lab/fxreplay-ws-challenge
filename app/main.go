package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

//go:embed client/index.html
var indexHTML []byte

// Cross-pod broadcast: WebSocket connections are sticky to one pod. To deliver
// a message sent on pod A to a client connected to pod B, every pod publishes
// to a shared Redis channel and subscribes to it. Local fan-out happens
// in-process to avoid Redis round-trip per client.

const redisChannel = "ws-broadcast"

type wireMsg struct {
	Sender string `json:"sender"`
	Body   string `json:"body"`
	Ts     int64  `json:"ts"`
}

type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	podID   string
}

type client struct {
	conn *websocket.Conn
	send chan []byte
	id   string
}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}

	mActiveConns = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ws_active_connections",
		Help: "Currently open WebSocket connections on this pod.",
	})
	mMessagesIn = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ws_messages_received_total",
		Help: "WebSocket frames received from clients.",
	})
	mMessagesOut = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ws_messages_sent_total",
		Help: "WebSocket frames sent to clients.",
	})
	mPublishErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ws_redis_publish_errors_total",
		Help: "Failures publishing to the Redis broadcast channel.",
	})
	mBroadcastLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "ws_broadcast_latency_seconds",
		Help:    "Time from Redis receive to local fan-out start.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12),
	})
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	podID := getenv("POD_NAME", "local")
	addr := ":" + getenv("PORT", "8080")
	redisAddr := getenv("REDIS_ADDR", "redis:6379")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("redis ping failed", "err", err, "addr", redisAddr)
		os.Exit(1)
	}

	h := &hub{clients: make(map[*client]struct{}), podID: podID}

	// Subscribe loop: receives every broadcast and fans out to local clients.
	pubsub := rdb.Subscribe(ctx, redisChannel)
	go func() {
		ch := pubsub.Channel()
		for msg := range ch {
			start := time.Now()
			h.localFanout([]byte(msg.Payload))
			mBroadcastLatency.Observe(time.Since(start).Seconds())
		}
	}()

	// Readiness flips to false on shutdown so the Service stops sending new
	// connections during the drain window.
	var ready atomic.Bool
	ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(h, rdb, w, r)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", addr, "pod", podID, "redis", redisAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutdown signal received, draining")

	// 1. Stop being Ready so the Service yanks us out of endpoints.
	ready.Store(false)

	// 2. Give the Service / kube-proxy time to propagate the readiness change
	//    before we slam connections closed.
	preStopGrace := envDuration("PRESTOP_DRAIN", 10*time.Second)
	time.Sleep(preStopGrace)

	// 3. Send a close frame to every connected client so they reconnect to a
	//    healthy pod immediately instead of waiting for TCP timeout.
	h.closeAll()

	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	_ = pubsub.Close()
	_ = rdb.Close()
	slog.Info("shutdown complete")
}

func serveWS(h *hub, rdb *redis.Client, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("upgrade failed", "err", err)
		return
	}
	conn.SetReadLimit(64 * 1024)

	c := &client{conn: conn, send: make(chan []byte, 64), id: r.RemoteAddr}
	h.add(c)
	mActiveConns.Inc()
	slog.Info("client connected", "id", c.id, "total", h.count())

	go c.writeLoop()
	c.readLoop(h, rdb)

	mActiveConns.Dec()
	slog.Info("client disconnected", "id", c.id)
}

func (c *client) readLoop(h *hub, rdb *redis.Client) {
	defer func() {
		h.remove(c)
		_ = c.conn.Close()
		close(c.send)
	}()
	_ = c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(70 * time.Second))
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		mMessagesIn.Inc()
		payload, _ := json.Marshal(wireMsg{Sender: h.podID + "/" + c.id, Body: string(raw), Ts: time.Now().UnixMilli()})
		if err := rdb.Publish(context.Background(), redisChannel, payload).Err(); err != nil {
			mPublishErrors.Inc()
			slog.Warn("redis publish failed", "err", err)
		}
	}
}

func (c *client) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseGoingAway, "bye"))
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
			mMessagesOut.Inc()
		case <-ping.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

func (h *hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *hub) localFanout(payload []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- payload:
		default:
			// Drop slow consumers rather than block the whole broadcast.
		}
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseServiceRestart, "shutting down"),
			time.Now().Add(2*time.Second),
		)
		_ = c.conn.Close()
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}
